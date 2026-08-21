package vfs

import (
	"fmt"
	"io/fs"
	"strings"
	"time"
)

// FileType represents the type of a VFS node.
type FileType int

const (
	FileTypeRegular FileType = iota
	FileTypeDirectory
	FileTypeSymlink
	// fileTypeWhiteout marks a name as deleted in the writable (upper) layer so
	// it hides a same-named entry in the read-only (lower) layer. Whiteout nodes
	// never escape the overlay — callers only ever see the three exported types.
	fileTypeWhiteout
)

// Node represents a file or directory in the writable layer.
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
	// dataSize carries the size of a lower-backed regular node whose bytes are
	// not resident in Data (a synthesized stat result). For in-memory files Data
	// is authoritative and this is ignored.
	dataSize int64
}

// Size reports the logical size used for stat: byte length for regular files,
// target length for symlinks, 0 for directories.
func (n *Node) Size() int64 {
	switch n.Type {
	case FileTypeSymlink:
		return int64(len(n.Target))
	case FileTypeRegular:
		if n.Data != nil {
			return int64(len(n.Data))
		}
		return n.dataSize
	default:
		return 0
	}
}

// WritableFS is the interface an overlay's upper layer satisfies: the standard
// read-only io/fs surface plus the mutating operations the VFS needs. The
// default implementation (memFS) is in-memory; an os-dir-backed implementation
// whose handles are *os.File (already a File) could be swapped in later.
type WritableFS interface {
	fs.FS
	OpenFile(name string, flag int, perm fs.FileMode) (File, error)
	Mkdir(name string, perm fs.FileMode) error
	Remove(name string) error
	Rename(oldName, newName string) error
	Symlink(target, linkName string) error
}

// memFS is the in-memory writable layer. Names are io/fs-style: slash-separated,
// unrooted, with "." denoting the root. Symlink resolution and cwd are the
// overlay's concern, so memFS lookups never follow symlinks.
type memFS struct {
	root *Node
}

func newMemFS() *memFS {
	return &memFS{root: &Node{
		Type:     FileTypeDirectory,
		Children: make(map[string]*Node),
		Mode:     0o755,
		ModTime:  time.Now(),
	}}
}

func splitName(name string) []string {
	if name == "." || name == "" || name == "/" {
		return nil
	}
	name = strings.Trim(name, "/")
	parts := strings.Split(name, "/")
	out := parts[:0]
	for _, p := range parts {
		if p != "" && p != "." {
			out = append(out, p)
		}
	}
	return out
}

// get returns the raw node at name (no symlink following). ok is false if any
// component is missing or a non-final component is not a directory.
func (m *memFS) get(name string) (*Node, bool) {
	n := m.root
	for _, part := range splitName(name) {
		if n.Type != FileTypeDirectory {
			return nil, false
		}
		c, ok := n.Children[part]
		if !ok {
			return nil, false
		}
		n = c
	}
	return n, true
}

// mkdirAll creates name and all missing parents as directories, returning the
// leaf. A whiteout in the path is replaced by a fresh directory.
func (m *memFS) mkdirAll(name string, mode uint32) (*Node, error) {
	n := m.root
	for _, part := range splitName(name) {
		if n.Type != FileTypeDirectory {
			return nil, fmt.Errorf("%s: not a directory", part)
		}
		c, ok := n.Children[part]
		if !ok || c.Type == fileTypeWhiteout {
			c = &Node{
				Name:     part,
				Type:     FileTypeDirectory,
				Children: make(map[string]*Node),
				Parent:   n,
				Mode:     mode,
				ModTime:  time.Now(),
			}
			n.Children[part] = c
		} else if c.Type != FileTypeDirectory {
			return nil, fmt.Errorf("%s: not a directory", part)
		}
		n = c
	}
	return n, nil
}

// place attaches child under the directory named by dir(name)/base(name),
// creating parent directories as needed.
func (m *memFS) place(name string, child *Node) (*Node, error) {
	parts := splitName(name)
	if len(parts) == 0 {
		return nil, fmt.Errorf("cannot place root")
	}
	parentName := strings.Join(parts[:len(parts)-1], "/")
	parent, err := m.mkdirAll(parentName, 0o755)
	if err != nil {
		return nil, err
	}
	base := parts[len(parts)-1]
	child.Name = base
	child.Parent = parent
	parent.Children[base] = child
	return child, nil
}

// --- WritableFS interface (memFS is a valid writable io/fs) ---

func (m *memFS) Open(name string) (fs.File, error) {
	n, ok := m.get(name)
	if !ok || n.Type == fileTypeWhiteout {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
	}
	return &memHandle{node: n, name: pathBase(name)}, nil
}

func (m *memFS) OpenFile(name string, flag int, perm fs.FileMode) (File, error) {
	n, ok := m.get(name)
	if !ok || n.Type == fileTypeWhiteout {
		if flag&int(O_CREAT) == 0 {
			return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
		}
		nn := &Node{Type: FileTypeRegular, Mode: uint32(perm.Perm()), ModTime: time.Now()}
		if _, err := m.place(name, nn); err != nil {
			return nil, err
		}
		n = nn
	} else if flag&int(O_TRUNC) != 0 && n.Type == FileTypeRegular {
		n.Data = nil
	}
	return &memHandle{node: n, name: pathBase(name)}, nil
}

func (m *memFS) Mkdir(name string, perm fs.FileMode) error {
	_, err := m.mkdirAll(name, uint32(perm.Perm()))
	return err
}

func (m *memFS) Remove(name string) error {
	n, ok := m.get(name)
	if !ok || n.Type == fileTypeWhiteout {
		return &fs.PathError{Op: "remove", Path: name, Err: fs.ErrNotExist}
	}
	if n.Type == FileTypeDirectory && len(n.Children) > 0 {
		return &fs.PathError{Op: "remove", Path: name, Err: fmt.Errorf("directory not empty")}
	}
	if n.Parent != nil {
		delete(n.Parent.Children, n.Name)
	}
	return nil
}

func (m *memFS) Rename(oldName, newName string) error {
	n, ok := m.get(oldName)
	if !ok || n.Type == fileTypeWhiteout {
		return &fs.PathError{Op: "rename", Path: oldName, Err: fs.ErrNotExist}
	}
	if n.Parent != nil {
		delete(n.Parent.Children, n.Name)
	}
	_, err := m.place(newName, n)
	return err
}

func (m *memFS) Symlink(target, linkName string) error {
	_, err := m.place(linkName, &Node{
		Type:    FileTypeSymlink,
		Target:  target,
		Mode:    0o777,
		ModTime: time.Now(),
	})
	return err
}

func pathBase(name string) string {
	parts := splitName(name)
	if len(parts) == 0 {
		return "."
	}
	return parts[len(parts)-1]
}
