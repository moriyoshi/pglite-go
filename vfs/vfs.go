// Package vfs implements a virtual filesystem for use with Emscripten-compiled
// WASM modules. It is an overlay of two io/fs-shaped layers: a read-only lower
// layer (any fs.FS — e.g. the PostgreSQL install bundle, served with zero
// copies) and a writable upper layer (an in-memory WritableFS by default). The
// overlay owns the cross-cutting state — the open-fd table, the current working
// directory, path and symlink resolution, copy-up, and whiteouts — while the
// layers only store bytes.
package vfs

import (
	"fmt"
	"io"
	"io/fs"
	"path"
	"strings"
	"sync"
	"time"
)

// FS is an overlay filesystem: a writable upper layer stacked on a read-only
// lower layer.
type FS struct {
	mu     sync.RWMutex
	upper  *memFS
	lower  fs.FS
	cwd    string
	fds    map[int32]*OpenFile
	nextFD int32
}

// OpenFile represents an open file descriptor.
type OpenFile struct {
	// handle backs positioned I/O (nil for directory descriptors and for the
	// placeholder standard-stream copies handed out by Dup).
	handle File
	// node is the entry the descriptor refers to: the live upper node, or a
	// synthesized node for a lower-backed file. Used by Fstat and directory reads.
	node *Node
	// Offset is the file position, or — for directory descriptors — the readdir
	// cursor advanced by getdents.
	Offset int64
	Flags  int32
	Path   string
}

// New creates an overlay filesystem with the given read-only lower layer (may be
// nil for none) and a fresh in-memory writable upper layer.
func New(lower fs.FS) *FS {
	return &FS{
		upper:  newMemFS(),
		lower:  lower,
		cwd:    "/",
		fds:    make(map[int32]*OpenFile),
		nextFD: 3, // 0=stdin, 1=stdout, 2=stderr
	}
}

// resolve turns p into a cleaned absolute path based on cwd.
func (v *FS) resolve(p string) string {
	if !path.IsAbs(p) {
		p = path.Join(v.cwd, p)
	}
	return path.Clean(p)
}

// relName converts an absolute VFS path to an io/fs-style name (unrooted, "."
// for root) for handing to a layer.
func relName(abs string) string {
	r := strings.TrimPrefix(path.Clean(abs), "/")
	if r == "" {
		return "."
	}
	return r
}

// inUpper returns the live upper node at abs, if present and not a whiteout.
func (v *FS) inUpper(abs string) (*Node, bool) {
	n, ok := v.upper.get(relName(abs))
	if !ok || n.Type == fileTypeWhiteout {
		return nil, false
	}
	return n, true
}

// upperWhiteout reports whether abs is whited-out in the upper layer.
func (v *FS) upperWhiteout(abs string) bool {
	n, ok := v.upper.get(relName(abs))
	return ok && n.Type == fileTypeWhiteout
}

// lowerInfo lstats abs in the lower layer (no symlink following on the final
// component when the lower supports it).
func (v *FS) lowerInfo(abs string) (fs.FileInfo, bool) {
	if v.lower == nil {
		return nil, false
	}
	info, err := fs.Lstat(v.lower, relName(abs))
	if err != nil {
		return nil, false
	}
	return info, true
}

// synthNode builds a transient *Node describing a lower-backed entry.
func (v *FS) synthNode(abs string, info fs.FileInfo) *Node {
	n := &Node{
		Name:    path.Base(abs),
		Mode:    uint32(info.Mode().Perm()),
		ModTime: info.ModTime(),
	}
	switch {
	case info.Mode()&fs.ModeSymlink != 0:
		n.Type = FileTypeSymlink
		if target, err := fs.ReadLink(v.lower, relName(abs)); err == nil {
			n.Target = target
		}
	case info.IsDir():
		n.Type = FileTypeDirectory
	default:
		n.Type = FileTypeRegular
		n.dataSize = info.Size()
	}
	return n
}

// statNode returns the entry at abs without following a final symlink, checking
// the upper layer then falling through to the lower. fromUpper reports which
// layer answered.
func (v *FS) statNode(abs string) (node *Node, fromUpper bool, ok bool) {
	if n, up := v.inUpper(abs); up {
		return n, true, true
	}
	if v.upperWhiteout(abs) {
		return nil, false, false
	}
	if info, ok := v.lowerInfo(abs); ok {
		return v.synthNode(abs, info), false, true
	}
	return nil, false, false
}

// eval walks abs component by component, following symlinks on intermediate
// components (and on the final one when followFinal is set), spanning both
// layers. It returns the resolved node and its resolved absolute path.
func (v *FS) eval(abs string, followFinal bool, depth int) (*Node, string, bool) {
	if depth > 40 {
		return nil, abs, false
	}
	abs = path.Clean(abs)
	if abs == "/" {
		n, _, ok := v.statNode("/")
		return n, "/", ok
	}
	parts := strings.Split(strings.TrimPrefix(abs, "/"), "/")
	cur := ""
	var node *Node
	for i, part := range parts {
		if part == "" {
			continue
		}
		next := cur + "/" + part
		n, _, ok := v.statNode(next)
		if !ok {
			return nil, next, false
		}
		isLast := i == len(parts)-1
		if n.Type == FileTypeSymlink && (!isLast || followFinal) {
			target := n.Target
			if !path.IsAbs(target) {
				target = path.Join(cur, target)
			}
			rn, rabs, ok := v.eval(target, true, depth+1)
			if !ok {
				return nil, next, false
			}
			n, next = rn, rabs
		}
		node, cur = n, next
	}
	if node == nil {
		node, _, _ = v.statNode("/")
		cur = "/"
	}
	return node, cur, node != nil
}

// lowerHas reports whether abs exists in the lower layer (following symlinks).
func (v *FS) lowerHas(abs string) bool {
	if v.lower == nil {
		return false
	}
	_, err := fs.Stat(v.lower, relName(abs))
	return err == nil
}

// readLowerAll reads the full contents of a lower-layer regular file.
func (v *FS) readLowerAll(abs string, size int64) ([]byte, error) {
	f, err := v.lower.Open(relName(abs))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data := make([]byte, size)
	n, err := io.ReadFull(f, data)
	if err == io.EOF || err == io.ErrUnexpectedEOF {
		return data[:n], nil
	}
	if err != nil {
		return nil, err
	}
	return data, nil
}

// copyUp ensures abs exists in the upper layer and returns its upper node. For a
// lower-backed regular file the bytes are copied up (unless empty is set, used
// by O_TRUNC to avoid a pointless read).
func (v *FS) copyUp(abs string, node *Node, empty bool) (*Node, error) {
	if un, ok := v.inUpper(abs); ok {
		return un, nil
	}
	nn := &Node{Type: node.Type, Mode: node.Mode, ModTime: node.ModTime}
	switch node.Type {
	case FileTypeDirectory:
		nn.Children = make(map[string]*Node)
	case FileTypeSymlink:
		nn.Target = node.Target
	default:
		if !empty {
			data, err := v.readLowerAll(abs, node.Size())
			if err != nil {
				return nil, err
			}
			nn.Data = data
		}
	}
	return v.upper.place(relName(abs), nn)
}

// openLowerHandle opens a read-only handle over a lower-layer file, using the
// source's own File when it satisfies the union and buffering otherwise.
func (v *FS) openLowerHandle(abs string, node *Node) (File, error) {
	f, err := v.lower.Open(relName(abs))
	if err != nil {
		return nil, err
	}
	if uf, ok := f.(File); ok {
		return uf, nil
	}
	// Fallback: read fully into memory.
	defer f.Close()
	data := make([]byte, node.Size())
	n, err := io.ReadFull(f, data)
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return nil, err
	}
	info, _ := f.Stat()
	return &roBufHandle{data: data[:n], info: info}, nil
}

// ManifestEntry / LoadManifest were removed: the install bundle is now supplied
// as an fs.FS lower layer (see the manifestFS adapter in package pglite).

// MkdirAll creates a directory and all parents in the upper layer.
func (v *FS) MkdirAll(p string, mode uint32) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	_, err := v.upper.mkdirAll(relName(v.resolve(p)), mode)
	return err
}

// WriteFile creates or overwrites a file in the upper layer with the given data.
func (v *FS) WriteFile(p string, data []byte, mode uint32) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	abs := v.resolve(p)
	_, err := v.upper.place(relName(abs), &Node{
		Type:    FileTypeRegular,
		Data:    data,
		Mode:    mode,
		ModTime: time.Now(),
	})
	return err
}

// Chdir changes the current working directory.
func (v *FS) Chdir(p string) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	abs := v.resolve(p)
	node, _, ok := v.eval(abs, true, 0)
	if !ok {
		return fmt.Errorf("chdir %s: no such directory", abs)
	}
	if node.Type != FileTypeDirectory {
		return fmt.Errorf("chdir %s: not a directory", abs)
	}
	v.cwd = abs
	return nil
}

// Getcwd returns the current working directory.
func (v *FS) Getcwd() string {
	v.mu.RLock()
	defer v.mu.RUnlock()
	return v.cwd
}

// Open flags (matching Linux values used by Emscripten)
const (
	O_RDONLY    = 0
	O_WRONLY    = 1
	O_RDWR      = 2
	O_CREAT     = 0o100
	O_TRUNC     = 0o1000
	O_APPEND    = 0o2000
	O_EXCL      = 0o200
	O_DIRECTORY = 0o200000

	AT_FDCWD = -100
)

// Open opens a file and returns a file descriptor.
func (v *FS) Open(p string, flags int32, mode uint32) (int32, error) {
	v.mu.Lock()
	defer v.mu.Unlock()

	abs := v.resolve(p)
	node, resolved, ok := v.eval(abs, true, 0)
	writable := flags&(O_WRONLY|O_RDWR) != 0

	if !ok {
		if flags&O_CREAT == 0 {
			return -1, fmt.Errorf("open %s: no such file", abs)
		}
		nn := &Node{Type: FileTypeRegular, Mode: mode, ModTime: time.Now()}
		created, err := v.upper.place(relName(abs), nn)
		if err != nil {
			return -1, err
		}
		node, resolved = created, abs
	} else {
		if node.Type == FileTypeRegular && flags&O_TRUNC != 0 {
			up, err := v.copyUp(resolved, node, true)
			if err != nil {
				return -1, err
			}
			up.Data = nil
			node = up
		} else if writable && node.Type == FileTypeRegular {
			up, err := v.copyUp(resolved, node, false)
			if err != nil {
				return -1, err
			}
			node = up
		}
	}

	var handle File
	if node.Type == FileTypeRegular {
		if _, up := v.inUpper(resolved); up {
			handle = &memHandle{node: node, name: path.Base(resolved)}
		} else {
			h, err := v.openLowerHandle(resolved, node)
			if err != nil {
				return -1, err
			}
			handle = h
		}
	}

	fd := v.nextFD
	v.nextFD++
	of := &OpenFile{handle: handle, node: node, Flags: flags, Path: resolved}
	if flags&O_APPEND != 0 && node.Type == FileTypeRegular {
		of.Offset = node.Size()
	}
	v.fds[fd] = of
	return fd, nil
}

// Close closes a file descriptor.
func (v *FS) Close(fd int32) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	of, ok := v.fds[fd]
	if !ok {
		return fmt.Errorf("close: bad fd %d", fd)
	}
	if of.handle != nil {
		of.handle.Close()
	}
	delete(v.fds, fd)
	return nil
}

// Read reads from a file descriptor at its current offset.
// Note: WASM is single-threaded so we skip locking for read operations.
func (v *FS) Read(fd int32, buf []byte) (int, error) {
	of, ok := v.fds[fd]
	if !ok {
		return 0, fmt.Errorf("read: bad fd %d", fd)
	}
	if of.node == nil || of.node.Type != FileTypeRegular || of.handle == nil {
		return 0, fmt.Errorf("read: not a regular file")
	}
	n, err := of.handle.ReadAt(buf, of.Offset)
	of.Offset += int64(n)
	if err == io.EOF {
		return n, nil
	}
	return n, err
}

// Write writes to a file descriptor at its current offset.
// Note: WASM is single-threaded so we skip locking for write operations.
func (v *FS) Write(fd int32, data []byte) (int, error) {
	of, ok := v.fds[fd]
	if !ok {
		return 0, fmt.Errorf("write: bad fd %d", fd)
	}
	if of.node == nil || of.node.Type != FileTypeRegular || of.handle == nil {
		return 0, fmt.Errorf("write: not a regular file")
	}
	n, err := of.handle.WriteAt(data, of.Offset)
	of.Offset += int64(n)
	return n, err
}

// Pread reads at an explicit offset without disturbing the file position.
func (v *FS) Pread(fd int32, buf []byte, offset int64) (int, error) {
	of, ok := v.fds[fd]
	if !ok || of.handle == nil {
		return 0, fmt.Errorf("pread: bad fd %d", fd)
	}
	n, err := of.handle.ReadAt(buf, offset)
	if err == io.EOF {
		return n, nil
	}
	return n, err
}

// Pwrite writes at an explicit offset without disturbing the file position.
func (v *FS) Pwrite(fd int32, data []byte, offset int64) (int, error) {
	of, ok := v.fds[fd]
	if !ok || of.handle == nil {
		return 0, fmt.Errorf("pwrite: bad fd %d", fd)
	}
	return of.handle.WriteAt(data, offset)
}

// Seek seeks within a file descriptor.
func (v *FS) Seek(fd int32, offset int64, whence int) (int64, error) {
	v.mu.Lock()
	defer v.mu.Unlock()

	of, ok := v.fds[fd]
	if !ok {
		return 0, fmt.Errorf("seek: bad fd %d", fd)
	}

	var newOffset int64
	switch whence {
	case 0: // SEEK_SET
		newOffset = offset
	case 1: // SEEK_CUR
		newOffset = of.Offset + offset
	case 2: // SEEK_END
		size := int64(0)
		if of.node != nil {
			size = of.node.Size()
		}
		newOffset = size + offset
	default:
		return 0, fmt.Errorf("seek: invalid whence %d", whence)
	}

	if newOffset < 0 {
		return 0, fmt.Errorf("seek: negative offset")
	}
	of.Offset = newOffset
	return newOffset, nil
}

// Stat returns information about a file (follows symlinks).
func (v *FS) Stat(p string) (*Node, error) {
	v.mu.RLock()
	defer v.mu.RUnlock()
	node, _, ok := v.eval(v.resolve(p), true, 0)
	if !ok {
		return nil, fmt.Errorf("stat %s: no such file", p)
	}
	return node, nil
}

// Lstat returns information about a file (does NOT follow the final symlink).
func (v *FS) Lstat(p string) (*Node, error) {
	v.mu.RLock()
	defer v.mu.RUnlock()
	node, _, ok := v.eval(v.resolve(p), false, 0)
	if !ok {
		return nil, fmt.Errorf("lstat %s: no such file", p)
	}
	return node, nil
}

// Fstat returns information about an open file.
func (v *FS) Fstat(fd int32) (*Node, error) {
	v.mu.RLock()
	defer v.mu.RUnlock()
	of, ok := v.fds[fd]
	if !ok || of.node == nil {
		return nil, fmt.Errorf("fstat: bad fd %d", fd)
	}
	return of.node, nil
}

// Readdir returns the merged names of entries in the directory an fd refers to.
func (v *FS) Readdir(fd int32) ([]string, error) {
	v.mu.RLock()
	defer v.mu.RUnlock()
	of, ok := v.fds[fd]
	if !ok || of.node == nil {
		return nil, fmt.Errorf("readdir: bad fd %d", fd)
	}
	if of.node.Type != FileTypeDirectory {
		return nil, fmt.Errorf("readdir: not a directory")
	}
	return v.readdirNames(of.Path), nil
}

// readdirNames returns the union of upper and lower entries under abs, hiding
// whiteouts and de-duplicating names.
func (v *FS) readdirNames(abs string) []string {
	seen := make(map[string]bool)
	white := make(map[string]bool)
	var names []string
	if un, ok := v.inUpper(abs); ok && un.Type == FileTypeDirectory {
		for name, c := range un.Children {
			if c.Type == fileTypeWhiteout {
				white[name] = true
				continue
			}
			seen[name] = true
			names = append(names, name)
		}
	}
	if v.lower != nil {
		if entries, err := fs.ReadDir(v.lower, relName(abs)); err == nil {
			for _, e := range entries {
				name := e.Name()
				if seen[name] || white[name] {
					continue
				}
				names = append(names, name)
			}
		}
	}
	return names
}

// removeChild deletes abs from the upper layer and, if a same-named lower entry
// would otherwise reappear, records a whiteout.
func (v *FS) removeChild(abs string) error {
	if un, ok := v.inUpper(abs); ok && un.Parent != nil {
		delete(un.Parent.Children, un.Name)
	}
	if v.lowerHas(abs) {
		if _, err := v.upper.place(relName(abs), &Node{Type: fileTypeWhiteout, ModTime: time.Now()}); err != nil {
			return err
		}
	}
	return nil
}

// Unlink removes a file.
func (v *FS) Unlink(p string) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	abs := v.resolve(p)
	node, _, ok := v.eval(abs, false, 0)
	if !ok {
		return fmt.Errorf("unlink %s: no such file", abs)
	}
	if node.Type == FileTypeDirectory {
		return fmt.Errorf("unlink %s: is a directory", abs)
	}
	return v.removeChild(abs)
}

// Rmdir removes an empty directory.
func (v *FS) Rmdir(p string) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	abs := v.resolve(p)
	node, _, ok := v.eval(abs, false, 0)
	if !ok {
		return fmt.Errorf("rmdir %s: not found", abs)
	}
	if node.Type != FileTypeDirectory {
		return fmt.Errorf("rmdir %s: not a directory", abs)
	}
	if len(v.readdirNames(abs)) > 0 {
		return fmt.Errorf("rmdir %s: not empty", abs)
	}
	return v.removeChild(abs)
}

// Rename renames a file or directory.
func (v *FS) Rename(oldPath, newPath string) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	oldAbs := v.resolve(oldPath)
	newAbs := v.resolve(newPath)

	node, _, ok := v.eval(oldAbs, false, 0)
	if !ok {
		return fmt.Errorf("rename: %s not found", oldAbs)
	}
	up, err := v.copyUp(oldAbs, node, false)
	if err != nil {
		return err
	}
	if up.Parent != nil {
		delete(up.Parent.Children, up.Name)
	}
	if v.lowerHas(oldAbs) {
		if _, err := v.upper.place(relName(oldAbs), &Node{Type: fileTypeWhiteout, ModTime: time.Now()}); err != nil {
			return err
		}
	}
	if _, err := v.upper.place(relName(newAbs), up); err != nil {
		return err
	}
	return nil
}

// Access checks if a file exists and is accessible.
func (v *FS) Access(p string) error {
	v.mu.RLock()
	defer v.mu.RUnlock()
	if _, _, ok := v.eval(v.resolve(p), true, 0); !ok {
		return fmt.Errorf("access %s: no such file", p)
	}
	return nil
}

// Truncate truncates the file behind fd to length.
func (v *FS) Truncate(fd int32, length int64) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	of, ok := v.fds[fd]
	if !ok {
		return fmt.Errorf("truncate: bad fd %d", fd)
	}
	if of.node == nil || of.node.Type != FileTypeRegular {
		return fmt.Errorf("truncate: not a regular file")
	}
	// Ensure the entry is upper-backed before mutating its bytes.
	up, err := v.copyUp(of.Path, of.node, false)
	if err != nil {
		return err
	}
	of.node = up
	of.handle = &memHandle{node: up, name: path.Base(of.Path)}
	if int64(len(up.Data)) > length {
		up.Data = up.Data[:length]
	} else if int64(len(up.Data)) < length {
		grown := make([]byte, length)
		copy(grown, up.Data)
		up.Data = grown
	}
	return nil
}

// GetFD returns the OpenFile for a file descriptor.
func (v *FS) GetFD(fd int32) (*OpenFile, bool) {
	v.mu.RLock()
	defer v.mu.RUnlock()
	of, ok := v.fds[fd]
	return of, ok
}

// Symlink creates a symbolic link in the upper layer.
func (v *FS) Symlink(target, linkPath string) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	abs := v.resolve(linkPath)
	_, err := v.upper.place(relName(abs), &Node{
		Type:    FileTypeSymlink,
		Target:  target,
		Mode:    0o777,
		ModTime: time.Now(),
	})
	return err
}

// Dup duplicates a file descriptor.
func (v *FS) Dup(oldFD int32) (int32, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	of, ok := v.fds[oldFD]
	if !ok {
		// Standard streams (0=stdin, 1=stdout, 2=stderr) are serviced by the
		// WASI layer, not the VFS fd table, so they never appear in v.fds.
		// PostgreSQL's set_max_safe_fds() probes descriptor availability by
		// dup()ing stderr many times and immediately closing the copies; hand
		// out a placeholder descriptor so the probe (and any close) succeeds.
		if oldFD >= 0 && oldFD <= 2 {
			newFD := v.nextFD
			v.nextFD++
			v.fds[newFD] = &OpenFile{Path: fmt.Sprintf("/dev/std%d", oldFD)}
			return newFD, nil
		}
		return -1, fmt.Errorf("dup: bad fd %d", oldFD)
	}
	newFD := v.nextFD
	v.nextFD++
	v.fds[newFD] = &OpenFile{
		handle: of.handle,
		node:   of.node,
		Offset: of.Offset,
		Flags:  of.Flags,
		Path:   of.Path,
	}
	return newFD, nil
}
