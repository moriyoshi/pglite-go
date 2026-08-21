package pglite

import (
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"path"
	"strings"
	"time"
)

// manifestEntry describes a file in the Emscripten preloaded data bundle.
// Offsets are float64 because the file_packager manifest can render them either
// as integers (204) or as floats (5093000.0).
type manifestEntry struct {
	Filename string  `json:"filename"`
	Start    float64 `json:"start"`
	End      float64 `json:"end"`
}

// bundleFS is a read-only fs.FS over an Emscripten .data bundle: a single byte
// blob plus a manifest of [start,end) ranges. It is the VFS lower layer, so the
// install is served with zero copies — each file reads a sub-slice of the blob.
type bundleFS struct {
	data  []byte
	nodes map[string]*bundleNode // keyed by clean io/fs name; "." is the root
}

type bundleNode struct {
	name       string
	isDir      bool
	mode       fs.FileMode
	modTime    time.Time
	start, end int                    // regular files
	children   map[string]*bundleNode // directories
}

// loadBundleFS builds a bundleFS from the manifest JSON and data blob served by
// src (pglite.manifest.json + pglite.data), where src is any io/fs — a host
// directory, an embed.FS, or a downloaded cache dir.
func loadBundleFS(src fs.FS) (fs.FS, error) {
	manifestBytes, err := fs.ReadFile(src, "pglite.manifest.json")
	if err != nil {
		return nil, fmt.Errorf("read manifest: %w", err)
	}
	var entries []manifestEntry
	if err := json.Unmarshal(manifestBytes, &entries); err != nil {
		return nil, fmt.Errorf("parse manifest: %w", err)
	}
	data, err := fs.ReadFile(src, "pglite.data")
	if err != nil {
		return nil, fmt.Errorf("read data: %w", err)
	}

	root := &bundleNode{name: ".", isDir: true, mode: fs.ModeDir | 0o755, modTime: time.Unix(0, 0), children: map[string]*bundleNode{}}
	b := &bundleFS{data: data, nodes: map[string]*bundleNode{".": root}}

	for _, e := range entries {
		start, end := int(e.Start), int(e.End)
		if start < 0 || end > len(data) || start > end {
			return nil, fmt.Errorf("entry %s out of bounds [%d:%d] in %d bytes", e.Filename, start, end, len(data))
		}
		// Executable bit for binaries and shared objects.
		mode := fs.FileMode(0o644)
		if strings.Contains(e.Filename, "/bin/") || strings.HasSuffix(e.Filename, ".so") {
			mode = 0o755
		}
		rel := strings.TrimPrefix(path.Clean("/"+e.Filename), "/")
		b.insert(rel, start, end, mode)
	}
	return b, nil
}

// insert adds a file at rel, creating parent directory nodes as needed.
func (b *bundleFS) insert(rel string, start, end int, mode fs.FileMode) {
	parts := strings.Split(rel, "/")
	parent := b.nodes["."]
	prefix := ""
	for _, part := range parts[:len(parts)-1] {
		if prefix == "" {
			prefix = part
		} else {
			prefix += "/" + part
		}
		child, ok := parent.children[part]
		if !ok {
			child = &bundleNode{name: part, isDir: true, mode: fs.ModeDir | 0o755, modTime: time.Unix(0, 0), children: map[string]*bundleNode{}}
			parent.children[part] = child
			b.nodes[prefix] = child
		}
		parent = child
	}
	base := parts[len(parts)-1]
	n := &bundleNode{name: base, mode: mode, modTime: time.Unix(0, 0), start: start, end: end}
	parent.children[base] = n
	b.nodes[rel] = n
}

func (b *bundleFS) Open(name string) (fs.File, error) {
	n, ok := b.nodes[name]
	if !ok {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
	}
	if n.isDir {
		return &bundleDir{node: n}, nil
	}
	return &bundleFile{fs: b, node: n}, nil
}

func (b *bundleFS) Stat(name string) (fs.FileInfo, error) {
	n, ok := b.nodes[name]
	if !ok {
		return nil, &fs.PathError{Op: "stat", Path: name, Err: fs.ErrNotExist}
	}
	return &bundleInfo{n}, nil
}

func (b *bundleFS) ReadDir(name string) ([]fs.DirEntry, error) {
	n, ok := b.nodes[name]
	if !ok || !n.isDir {
		return nil, &fs.PathError{Op: "readdir", Path: name, Err: fs.ErrNotExist}
	}
	entries := make([]fs.DirEntry, 0, len(n.children))
	for _, c := range n.children {
		entries = append(entries, fs.FileInfoToDirEntry(&bundleInfo{c}))
	}
	return entries, nil
}

// bundleInfo is the fs.FileInfo for a bundle node.
type bundleInfo struct{ n *bundleNode }

func (i *bundleInfo) Name() string { return i.n.name }
func (i *bundleInfo) Size() int64 {
	if i.n.isDir {
		return 0
	}
	return int64(i.n.end - i.n.start)
}
func (i *bundleInfo) Mode() fs.FileMode  { return i.n.mode }
func (i *bundleInfo) ModTime() time.Time { return i.n.modTime }
func (i *bundleInfo) IsDir() bool        { return i.n.isDir }
func (i *bundleInfo) Sys() any           { return nil }

// bundleFile is a read-only File over a sub-slice of the bundle blob. It
// satisfies the vfs.File union, so the overlay serves reads straight from it.
type bundleFile struct {
	fs   *bundleFS
	node *bundleNode
	off  int64
}

func (f *bundleFile) bytes() []byte { return f.fs.data[f.node.start:f.node.end] }

func (f *bundleFile) ReadAt(p []byte, off int64) (int, error) {
	data := f.bytes()
	if off < 0 {
		return 0, fmt.Errorf("readat: negative offset")
	}
	if off >= int64(len(data)) {
		return 0, io.EOF
	}
	n := copy(p, data[off:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

func (f *bundleFile) WriteAt([]byte, int64) (int, error) { return 0, fs.ErrPermission }

func (f *bundleFile) Read(p []byte) (int, error) {
	n, err := f.ReadAt(p, f.off)
	f.off += int64(n)
	return n, err
}

func (f *bundleFile) Seek(offset int64, whence int) (int64, error) {
	var abs int64
	switch whence {
	case io.SeekStart:
		abs = offset
	case io.SeekCurrent:
		abs = f.off + offset
	case io.SeekEnd:
		abs = int64(len(f.bytes())) + offset
	default:
		return 0, fmt.Errorf("seek: invalid whence %d", whence)
	}
	if abs < 0 {
		return 0, fmt.Errorf("seek: negative offset")
	}
	f.off = abs
	return abs, nil
}

func (f *bundleFile) Stat() (fs.FileInfo, error) { return &bundleInfo{f.node}, nil }
func (f *bundleFile) Close() error               { return nil }

// bundleDir is a minimal directory handle (bundle dirs are read via ReadDirFS).
type bundleDir struct {
	node    *bundleNode
	entries []fs.DirEntry
	pos     int
}

func (d *bundleDir) Stat() (fs.FileInfo, error) { return &bundleInfo{d.node}, nil }
func (d *bundleDir) Read([]byte) (int, error) {
	return 0, &fs.PathError{Op: "read", Path: d.node.name, Err: fmt.Errorf("is a directory")}
}
func (d *bundleDir) Close() error { return nil }

func (d *bundleDir) ReadDir(n int) ([]fs.DirEntry, error) {
	if d.entries == nil {
		d.entries = make([]fs.DirEntry, 0, len(d.node.children))
		for _, c := range d.node.children {
			d.entries = append(d.entries, fs.FileInfoToDirEntry(&bundleInfo{c}))
		}
	}
	if n <= 0 {
		rest := d.entries[d.pos:]
		d.pos = len(d.entries)
		return rest, nil
	}
	if d.pos >= len(d.entries) {
		return nil, io.EOF
	}
	end := d.pos + n
	if end > len(d.entries) {
		end = len(d.entries)
	}
	rest := d.entries[d.pos:end]
	d.pos = end
	return rest, nil
}
