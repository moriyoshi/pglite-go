// Package vfs implements an in-memory virtual filesystem for use with
// Emscripten-compiled WASM modules.
package vfs

import (
	"encoding/json"
	"fmt"
	"os"
	"path"
	"strings"
	"sync"
	"time"
)

// FileType represents the type of a VFS node.
type FileType int

const (
	FileTypeRegular FileType = iota
	FileTypeDirectory
	FileTypeSymlink
)

// Node represents a file or directory in the VFS.
type Node struct {
	Name     string
	Type     FileType
	Data     []byte
	Children map[string]*Node
	Parent   *Node
	Mode     uint32
	ModTime  time.Time
	// For symlinks
	Target string
}

// FS is an in-memory filesystem.
type FS struct {
	mu   sync.RWMutex
	root *Node
	cwd  string
	// Open file descriptors
	fds     map[int32]*OpenFile
	nextFD  int32
}

// OpenFile represents an open file descriptor.
type OpenFile struct {
	Node   *Node
	Offset int64
	Flags  int32
	Path   string
}

// New creates a new empty filesystem.
func New() *FS {
	return &FS{
		root: &Node{
			Name:     "",
			Type:     FileTypeDirectory,
			Children: make(map[string]*Node),
			Mode:     0o755,
			ModTime:  time.Now(),
		},
		cwd:    "/",
		fds:    make(map[int32]*OpenFile),
		nextFD: 3, // 0=stdin, 1=stdout, 2=stderr
	}
}

// ManifestEntry describes a file in the Emscripten preloaded data bundle.
type ManifestEntry struct {
	Filename string `json:"filename"`
	Start    int    `json:"start"`
	End      int    `json:"end"`
}

// LoadManifest loads files from an Emscripten .data bundle using a manifest.
func (fs *FS) LoadManifest(manifestPath, dataPath string) error {
	manifestBytes, err := os.ReadFile(manifestPath)
	if err != nil {
		return fmt.Errorf("read manifest: %w", err)
	}
	var entries []ManifestEntry
	if err := json.Unmarshal(manifestBytes, &entries); err != nil {
		return fmt.Errorf("parse manifest: %w", err)
	}

	dataBytes, err := os.ReadFile(dataPath)
	if err != nil {
		return fmt.Errorf("read data: %w", err)
	}

	for _, entry := range entries {
		if entry.Start > len(dataBytes) || entry.End > len(dataBytes) {
			return fmt.Errorf("entry %s out of bounds [%d:%d] in %d bytes", entry.Filename, entry.Start, entry.End, len(dataBytes))
		}
		data := make([]byte, entry.End-entry.Start)
		copy(data, dataBytes[entry.Start:entry.End])

		// Set executable permission for files in bin/ and .so files
		mode := uint32(0o644)
		if strings.Contains(entry.Filename, "/bin/") || strings.HasSuffix(entry.Filename, ".so") {
			mode = 0o755
		}

		if err := fs.WriteFile(entry.Filename, data, mode); err != nil {
			return fmt.Errorf("write %s: %w", entry.Filename, err)
		}
	}
	return nil
}

// resolve resolves a path to an absolute path based on cwd.
func (fs *FS) resolve(p string) string {
	if !path.IsAbs(p) {
		p = path.Join(fs.cwd, p)
	}
	return path.Clean(p)
}

// lookup finds a node by path. Must hold at least a read lock.
func (fs *FS) lookup(p string) *Node {
	p = fs.resolve(p)
	if p == "/" {
		return fs.root
	}

	parts := strings.Split(strings.TrimPrefix(p, "/"), "/")
	node := fs.root
	for _, part := range parts {
		if part == "" {
			continue
		}
		if node.Type != FileTypeDirectory {
			return nil
		}
		child, ok := node.Children[part]
		if !ok {
			return nil
		}
		// Follow symlinks
		if child.Type == FileTypeSymlink {
			target := child.Target
			if !path.IsAbs(target) {
				// Resolve relative to parent directory
				parentPath := fs.nodePath(node)
				target = path.Join(parentPath, target)
			}
			child = fs.lookup(target)
			if child == nil {
				return nil
			}
		}
		node = child
	}
	return node
}

// nodePath returns the absolute path to a node.
func (fs *FS) nodePath(node *Node) string {
	if node == fs.root {
		return "/"
	}
	var parts []string
	for n := node; n != nil && n != fs.root; n = n.Parent {
		parts = append([]string{n.Name}, parts...)
	}
	return "/" + strings.Join(parts, "/")
}

// MkdirAll creates a directory and all parents.
func (fs *FS) MkdirAll(p string, mode uint32) error {
	fs.mu.Lock()
	defer fs.mu.Unlock()

	p = fs.resolve(p)
	parts := strings.Split(strings.TrimPrefix(p, "/"), "/")
	node := fs.root
	for _, part := range parts {
		if part == "" {
			continue
		}
		child, ok := node.Children[part]
		if ok {
			if child.Type != FileTypeDirectory {
				return fmt.Errorf("%s: not a directory", part)
			}
			node = child
		} else {
			child = &Node{
				Name:     part,
				Type:     FileTypeDirectory,
				Children: make(map[string]*Node),
				Parent:   node,
				Mode:     mode,
				ModTime:  time.Now(),
			}
			node.Children[part] = child
			node = child
		}
	}
	return nil
}

// WriteFile creates or overwrites a file with the given data.
func (fs *FS) WriteFile(p string, data []byte, mode uint32) error {
	fs.mu.Lock()
	defer fs.mu.Unlock()

	p = fs.resolve(p)
	dir := path.Dir(p)
	base := path.Base(p)

	// Ensure parent directories exist (unlock/relock not needed since we hold the lock)
	if err := fs.mkdirAllLocked(dir, 0o755); err != nil {
		return err
	}

	parent := fs.lookup(dir)
	if parent == nil {
		return fmt.Errorf("parent directory %s not found", dir)
	}

	parent.Children[base] = &Node{
		Name:    base,
		Type:    FileTypeRegular,
		Data:    data,
		Parent:  parent,
		Mode:    mode,
		ModTime: time.Now(),
	}
	return nil
}

func (fs *FS) mkdirAllLocked(p string, mode uint32) error {
	p = fs.resolve(p)
	parts := strings.Split(strings.TrimPrefix(p, "/"), "/")
	node := fs.root
	for _, part := range parts {
		if part == "" {
			continue
		}
		child, ok := node.Children[part]
		if ok {
			if child.Type != FileTypeDirectory {
				return fmt.Errorf("%s: not a directory", part)
			}
			node = child
		} else {
			child = &Node{
				Name:     part,
				Type:     FileTypeDirectory,
				Children: make(map[string]*Node),
				Parent:   node,
				Mode:     mode,
				ModTime:  time.Now(),
			}
			node.Children[part] = child
			node = child
		}
	}
	return nil
}

// Chdir changes the current working directory.
func (fs *FS) Chdir(p string) error {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	p = fs.resolve(p)
	node := fs.lookup(p)
	if node == nil {
		return fmt.Errorf("chdir %s: no such directory", p)
	}
	if node.Type != FileTypeDirectory {
		return fmt.Errorf("chdir %s: not a directory", p)
	}
	fs.cwd = p
	return nil
}

// Getcwd returns the current working directory.
func (fs *FS) Getcwd() string {
	fs.mu.RLock()
	defer fs.mu.RUnlock()
	return fs.cwd
}

// Open flags (matching Linux values used by Emscripten)
const (
	O_RDONLY = 0
	O_WRONLY = 1
	O_RDWR   = 2
	O_CREAT  = 0o100
	O_TRUNC  = 0o1000
	O_APPEND = 0o2000
	O_EXCL   = 0o200
	O_DIRECTORY = 0o200000

	AT_FDCWD = -100
)

// Open opens a file and returns a file descriptor.
func (fs *FS) Open(p string, flags int32, mode uint32) (int32, error) {
	fs.mu.Lock()
	defer fs.mu.Unlock()

	p = fs.resolve(p)
	node := fs.lookup(p)

	if node == nil {
		if flags&O_CREAT == 0 {
			return -1, fmt.Errorf("open %s: no such file", p)
		}
		// Create the file
		dir := path.Dir(p)
		base := path.Base(p)
		parent := fs.lookup(dir)
		if parent == nil {
			return -1, fmt.Errorf("open %s: parent directory not found", p)
		}
		node = &Node{
			Name:    base,
			Type:    FileTypeRegular,
			Data:    nil,
			Parent:  parent,
			Mode:    mode,
			ModTime: time.Now(),
		}
		parent.Children[base] = node
	}

	if flags&O_TRUNC != 0 && node.Type == FileTypeRegular {
		node.Data = nil
	}

	fd := fs.nextFD
	fs.nextFD++
	fs.fds[fd] = &OpenFile{
		Node:   node,
		Offset: 0,
		Flags:  flags,
		Path:   p,
	}

	if flags&O_APPEND != 0 && node.Type == FileTypeRegular {
		fs.fds[fd].Offset = int64(len(node.Data))
	}

	return fd, nil
}

// Close closes a file descriptor.
func (fs *FS) Close(fd int32) error {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if _, ok := fs.fds[fd]; !ok {
		return fmt.Errorf("close: bad fd %d", fd)
	}
	delete(fs.fds, fd)
	return nil
}

// Read reads from a file descriptor.
func (fs *FS) Read(fd int32, buf []byte) (int, error) {
	fs.mu.Lock()
	defer fs.mu.Unlock()

	of, ok := fs.fds[fd]
	if !ok {
		return 0, fmt.Errorf("read: bad fd %d", fd)
	}
	if of.Node.Type != FileTypeRegular {
		return 0, fmt.Errorf("read: not a regular file")
	}

	data := of.Node.Data
	if of.Offset >= int64(len(data)) {
		return 0, nil // EOF
	}

	n := copy(buf, data[of.Offset:])
	of.Offset += int64(n)
	return n, nil
}

// Write writes to a file descriptor.
func (fs *FS) Write(fd int32, data []byte) (int, error) {
	fs.mu.Lock()
	defer fs.mu.Unlock()

	of, ok := fs.fds[fd]
	if !ok {
		return 0, fmt.Errorf("write: bad fd %d", fd)
	}
	if of.Node.Type != FileTypeRegular {
		return 0, fmt.Errorf("write: not a regular file")
	}

	node := of.Node
	offset := of.Offset

	// Extend file if necessary
	needed := offset + int64(len(data))
	if needed > int64(len(node.Data)) {
		newData := make([]byte, needed)
		copy(newData, node.Data)
		node.Data = newData
	}
	copy(node.Data[offset:], data)
	of.Offset = offset + int64(len(data))
	return len(data), nil
}

// Seek seeks within a file descriptor.
func (fs *FS) Seek(fd int32, offset int64, whence int) (int64, error) {
	fs.mu.Lock()
	defer fs.mu.Unlock()

	of, ok := fs.fds[fd]
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
		newOffset = int64(len(of.Node.Data)) + offset
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
func (fs *FS) Stat(p string) (*Node, error) {
	fs.mu.RLock()
	defer fs.mu.RUnlock()
	p = fs.resolve(p)
	node := fs.lookup(p)
	if node == nil {
		return nil, fmt.Errorf("stat %s: no such file", p)
	}
	return node, nil
}

// Lstat returns information about a file (does NOT follow symlinks).
func (fs *FS) Lstat(p string) (*Node, error) {
	fs.mu.RLock()
	defer fs.mu.RUnlock()
	p = fs.resolve(p)
	node := fs.lookupNoFollow(p)
	if node == nil {
		return nil, fmt.Errorf("lstat %s: no such file", p)
	}
	return node, nil
}

// lookupNoFollow is like lookup but doesn't follow the final symlink component.
func (fs *FS) lookupNoFollow(p string) *Node {
	p = fs.resolve(p)
	if p == "/" {
		return fs.root
	}
	parts := strings.Split(strings.TrimPrefix(p, "/"), "/")
	node := fs.root
	for i, part := range parts {
		if part == "" {
			continue
		}
		if node.Type != FileTypeDirectory {
			return nil
		}
		child, ok := node.Children[part]
		if !ok {
			return nil
		}
		// Follow symlinks for intermediate components, but NOT the last one
		if child.Type == FileTypeSymlink && i < len(parts)-1 {
			target := child.Target
			if !path.IsAbs(target) {
				parentPath := fs.nodePath(node)
				target = path.Join(parentPath, target)
			}
			child = fs.lookup(target)
			if child == nil {
				return nil
			}
		}
		node = child
	}
	return node
}

// Fstat returns information about an open file.
func (fs *FS) Fstat(fd int32) (*Node, error) {
	fs.mu.RLock()
	defer fs.mu.RUnlock()
	of, ok := fs.fds[fd]
	if !ok {
		return nil, fmt.Errorf("fstat: bad fd %d", fd)
	}
	return of.Node, nil
}

// Readdir returns the names of entries in a directory.
func (fs *FS) Readdir(fd int32) ([]string, error) {
	fs.mu.RLock()
	defer fs.mu.RUnlock()
	of, ok := fs.fds[fd]
	if !ok {
		return nil, fmt.Errorf("readdir: bad fd %d", fd)
	}
	if of.Node.Type != FileTypeDirectory {
		return nil, fmt.Errorf("readdir: not a directory")
	}
	names := make([]string, 0, len(of.Node.Children))
	for name := range of.Node.Children {
		names = append(names, name)
	}
	return names, nil
}

// Unlink removes a file.
func (fs *FS) Unlink(p string) error {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	p = fs.resolve(p)
	dir := path.Dir(p)
	base := path.Base(p)
	parent := fs.lookup(dir)
	if parent == nil {
		return fmt.Errorf("unlink %s: parent not found", p)
	}
	child, ok := parent.Children[base]
	if !ok {
		return fmt.Errorf("unlink %s: no such file", p)
	}
	if child.Type == FileTypeDirectory {
		return fmt.Errorf("unlink %s: is a directory", p)
	}
	delete(parent.Children, base)
	return nil
}

// Rename renames a file or directory.
func (fs *FS) Rename(oldPath, newPath string) error {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	oldPath = fs.resolve(oldPath)
	newPath = fs.resolve(newPath)

	oldDir := path.Dir(oldPath)
	oldBase := path.Base(oldPath)
	newDir := path.Dir(newPath)
	newBase := path.Base(newPath)

	oldParent := fs.lookup(oldDir)
	if oldParent == nil {
		return fmt.Errorf("rename: %s not found", oldDir)
	}
	node, ok := oldParent.Children[oldBase]
	if !ok {
		return fmt.Errorf("rename: %s not found", oldPath)
	}

	newParent := fs.lookup(newDir)
	if newParent == nil {
		return fmt.Errorf("rename: %s not found", newDir)
	}

	delete(oldParent.Children, oldBase)
	node.Name = newBase
	node.Parent = newParent
	newParent.Children[newBase] = node
	return nil
}

// Access checks if a file exists and is accessible.
func (fs *FS) Access(p string) error {
	fs.mu.RLock()
	defer fs.mu.RUnlock()
	p = fs.resolve(p)
	if fs.lookup(p) == nil {
		return fmt.Errorf("access %s: no such file", p)
	}
	return nil
}

// Truncate truncates a file to the given length.
func (fs *FS) Truncate(fd int32, length int64) error {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	of, ok := fs.fds[fd]
	if !ok {
		return fmt.Errorf("truncate: bad fd %d", fd)
	}
	node := of.Node
	if node.Type != FileTypeRegular {
		return fmt.Errorf("truncate: not a regular file")
	}
	if int64(len(node.Data)) > length {
		node.Data = node.Data[:length]
	} else if int64(len(node.Data)) < length {
		newData := make([]byte, length)
		copy(newData, node.Data)
		node.Data = newData
	}
	return nil
}

// GetFD returns the OpenFile for a file descriptor.
func (fs *FS) GetFD(fd int32) (*OpenFile, bool) {
	fs.mu.RLock()
	defer fs.mu.RUnlock()
	of, ok := fs.fds[fd]
	return of, ok
}

// Rmdir removes an empty directory.
func (fs *FS) Rmdir(p string) error {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	p = fs.resolve(p)
	dir := path.Dir(p)
	base := path.Base(p)
	parent := fs.lookup(dir)
	if parent == nil {
		return fmt.Errorf("rmdir %s: parent not found", p)
	}
	child, ok := parent.Children[base]
	if !ok {
		return fmt.Errorf("rmdir %s: not found", p)
	}
	if child.Type != FileTypeDirectory {
		return fmt.Errorf("rmdir %s: not a directory", p)
	}
	if len(child.Children) > 0 {
		return fmt.Errorf("rmdir %s: not empty", p)
	}
	delete(parent.Children, base)
	return nil
}

// Symlink creates a symbolic link.
func (fs *FS) Symlink(target, linkPath string) error {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	linkPath = fs.resolve(linkPath)
	dir := path.Dir(linkPath)
	base := path.Base(linkPath)
	parent := fs.lookup(dir)
	if parent == nil {
		return fmt.Errorf("symlink: parent %s not found", dir)
	}
	parent.Children[base] = &Node{
		Name:    base,
		Type:    FileTypeSymlink,
		Target:  target,
		Parent:  parent,
		Mode:    0o777,
		ModTime: time.Now(),
	}
	return nil
}

// Dup duplicates a file descriptor.
func (fs *FS) Dup(oldFD int32) (int32, error) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	of, ok := fs.fds[oldFD]
	if !ok {
		return -1, fmt.Errorf("dup: bad fd %d", oldFD)
	}
	newFD := fs.nextFD
	fs.nextFD++
	fs.fds[newFD] = &OpenFile{
		Node:   of.Node,
		Offset: of.Offset,
		Flags:  of.Flags,
		Path:   of.Path,
	}
	return newFD, nil
}
