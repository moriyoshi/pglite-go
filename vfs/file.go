package vfs

import (
	"fmt"
	"io"
	"io/fs"
	"time"
)

// File is the union of the standard file interfaces the VFS needs to service a
// PostgreSQL workload. fs.File provides Stat/Read/Close (so any File is a valid
// io/fs handle), while io.ReaderAt, io.WriterAt and io.Seeker add the random
// access and positioned writes the database relies on — pread/pwrite at
// arbitrary offsets and lseek. Every open handle in the overlay implements it,
// which is why a lower layer whose files already satisfy it (e.g. *os.File, or a
// bytes-backed bundle file) can be used directly with zero copies.
type File interface {
	fs.File
	io.ReaderAt
	io.WriterAt
	io.Seeker
}

// memHandle is a handle over an in-memory Node in the writable layer. Positioned
// I/O (ReadAt/WriteAt) is authoritative; the overlay tracks the per-fd offset
// itself, so the embedded off is only used when a caller drives Read/Seek
// directly through the File interface.
type memHandle struct {
	node *Node
	name string
	off  int64
}

func (h *memHandle) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, fmt.Errorf("readat: negative offset")
	}
	data := h.node.Data
	if off >= int64(len(data)) {
		return 0, io.EOF
	}
	n := copy(p, data[off:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

// WriteAt writes at off, growing the backing buffer with 2× capacity
// amortization so that append-heavy workloads (WAL, heap extension) stay off an
// O(n²) reallocation path.
func (h *memHandle) WriteAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, fmt.Errorf("writeat: negative offset")
	}
	node := h.node
	needed := off + int64(len(p))
	if needed > int64(len(node.Data)) {
		if needed > int64(cap(node.Data)) {
			newCap := int64(cap(node.Data)) * 2
			if newCap < needed {
				newCap = needed
			}
			if newCap < 4096 {
				newCap = 4096
			}
			grown := make([]byte, needed, newCap)
			copy(grown, node.Data)
			node.Data = grown
		} else {
			node.Data = node.Data[:needed]
		}
	}
	copy(node.Data[off:], p)
	node.ModTime = time.Now()
	return len(p), nil
}

func (h *memHandle) Read(p []byte) (int, error) {
	n, err := h.ReadAt(p, h.off)
	h.off += int64(n)
	return n, err
}

func (h *memHandle) Seek(offset int64, whence int) (int64, error) {
	var abs int64
	switch whence {
	case io.SeekStart:
		abs = offset
	case io.SeekCurrent:
		abs = h.off + offset
	case io.SeekEnd:
		abs = int64(len(h.node.Data)) + offset
	default:
		return 0, fmt.Errorf("seek: invalid whence %d", whence)
	}
	if abs < 0 {
		return 0, fmt.Errorf("seek: negative offset")
	}
	h.off = abs
	return abs, nil
}

func (h *memHandle) Stat() (fs.FileInfo, error) { return &nodeInfo{node: h.node, name: h.name}, nil }
func (h *memHandle) Close() error               { return nil }

// roBufHandle is a read-only File backed by a byte slice. It is the fallback for
// a lower-layer fs.File that does not itself satisfy the File union (no ReaderAt
// /Seeker): the file is read fully into memory once when opened. Bundle and
// os.DirFS sources hand back Files directly, so this path is only for arbitrary
// fs.FS lowers.
type roBufHandle struct {
	data []byte
	off  int64
	info fs.FileInfo
}

func (h *roBufHandle) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, fmt.Errorf("readat: negative offset")
	}
	if off >= int64(len(h.data)) {
		return 0, io.EOF
	}
	n := copy(p, h.data[off:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

func (h *roBufHandle) WriteAt([]byte, int64) (int, error) {
	return 0, fs.ErrPermission
}

func (h *roBufHandle) Read(p []byte) (int, error) {
	n, err := h.ReadAt(p, h.off)
	h.off += int64(n)
	return n, err
}

func (h *roBufHandle) Seek(offset int64, whence int) (int64, error) {
	var abs int64
	switch whence {
	case io.SeekStart:
		abs = offset
	case io.SeekCurrent:
		abs = h.off + offset
	case io.SeekEnd:
		abs = int64(len(h.data)) + offset
	default:
		return 0, fmt.Errorf("seek: invalid whence %d", whence)
	}
	if abs < 0 {
		return 0, fmt.Errorf("seek: negative offset")
	}
	h.off = abs
	return abs, nil
}

func (h *roBufHandle) Stat() (fs.FileInfo, error) {
	if h.info != nil {
		return h.info, nil
	}
	return nil, fmt.Errorf("stat: no info")
}
func (h *roBufHandle) Close() error { return nil }

// nodeInfo adapts a Node to fs.FileInfo.
type nodeInfo struct {
	node *Node
	name string
}

func (i *nodeInfo) Name() string       { return i.name }
func (i *nodeInfo) Size() int64        { return i.node.Size() }
func (i *nodeInfo) ModTime() time.Time { return i.node.ModTime }
func (i *nodeInfo) IsDir() bool        { return i.node.Type == FileTypeDirectory }
func (i *nodeInfo) Sys() any           { return i.node }

func (i *nodeInfo) Mode() fs.FileMode {
	m := fs.FileMode(i.node.Mode & 0o777)
	switch i.node.Type {
	case FileTypeDirectory:
		m |= fs.ModeDir
	case FileTypeSymlink:
		m |= fs.ModeSymlink
	}
	return m
}
