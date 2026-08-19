package vfs

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
)

// SaveSubtree mirrors the VFS subtree rooted at vfsPath onto the host
// filesystem at hostDir. Regular files, directories and symlinks are
// preserved along with their permission bits. hostDir is created if absent.
//
// This is used to persist the initdb-created data directory so that initdb
// (the dominant startup cost) runs only once per data directory rather than
// on every process start.
func (fs *FS) SaveSubtree(vfsPath, hostDir string) error {
	fs.mu.RLock()
	defer fs.mu.RUnlock()

	root := fs.lookupNoFollow(fs.resolve(vfsPath))
	if root == nil {
		return fmt.Errorf("SaveSubtree: %s not found in VFS", vfsPath)
	}
	if root.Type != FileTypeDirectory {
		return fmt.Errorf("SaveSubtree: %s is not a directory", vfsPath)
	}

	// Start from a clean target so deletions in the VFS are reflected.
	if err := os.RemoveAll(hostDir); err != nil {
		return fmt.Errorf("SaveSubtree: clear %s: %w", hostDir, err)
	}
	if err := os.MkdirAll(hostDir, 0o700); err != nil {
		return fmt.Errorf("SaveSubtree: mkdir %s: %w", hostDir, err)
	}
	return fs.saveNode(root, hostDir)
}

// saveNode writes the children of a directory node into hostDir. Must hold the
// read lock.
func (fs *FS) saveNode(dir *Node, hostDir string) error {
	// Deterministic order keeps on-disk output stable across runs.
	names := make([]string, 0, len(dir.Children))
	for name := range dir.Children {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		child := dir.Children[name]
		target := filepath.Join(hostDir, name)
		switch child.Type {
		case FileTypeDirectory:
			// Use fixed 0700 on the host, not the VFS mode: initdb marks some
			// dirs read-only, which would prevent us from writing children into
			// them or reading them back. On reload the VFS mode is normalized to
			// a valid strict PostgreSQL permission (0700 dir / 0600 file).
			if err := os.MkdirAll(target, 0o700); err != nil {
				return fmt.Errorf("mkdir %s: %w", target, err)
			}
			if err := fs.saveNode(child, target); err != nil {
				return err
			}
		case FileTypeRegular:
			// Fixed 0600 on the host so the file is always readable on reload,
			// regardless of the (possibly read-restricted) VFS mode.
			if err := os.WriteFile(target, child.Data, 0o600); err != nil {
				return fmt.Errorf("write %s: %w", target, err)
			}
		case FileTypeSymlink:
			// Remove any stale entry then recreate the link.
			_ = os.Remove(target)
			if err := os.Symlink(child.Target, target); err != nil {
				return fmt.Errorf("symlink %s: %w", target, err)
			}
		}
	}
	return nil
}

// LoadSubtree mirrors a host directory (previously written by SaveSubtree) back
// into the VFS subtree rooted at vfsPath. Existing VFS entries under vfsPath are
// left in place unless overwritten by a host entry of the same name.
func (fs *FS) LoadSubtree(hostDir, vfsPath string) error {
	fs.mu.Lock()
	defer fs.mu.Unlock()

	vfsPath = fs.resolve(vfsPath)
	if err := fs.mkdirAllLocked(vfsPath, 0o700); err != nil {
		return fmt.Errorf("LoadSubtree: mkdir %s: %w", vfsPath, err)
	}

	entries, err := os.ReadDir(hostDir)
	if err != nil {
		return fmt.Errorf("LoadSubtree: read %s: %w", hostDir, err)
	}
	for _, e := range entries {
		if err := fs.loadEntry(filepath.Join(hostDir, e.Name()), path.Join(vfsPath, e.Name())); err != nil {
			return err
		}
	}
	return nil
}

// loadEntry loads a single host path into the VFS at vfsPath. Must hold the
// write lock.
func (fs *FS) loadEntry(hostPath, vfsPath string) error {
	info, err := os.Lstat(hostPath)
	if err != nil {
		return fmt.Errorf("lstat %s: %w", hostPath, err)
	}

	parent := fs.lookup(path.Dir(vfsPath))
	if parent == nil || parent.Type != FileTypeDirectory {
		return fmt.Errorf("loadEntry: parent of %s missing", vfsPath)
	}
	base := path.Base(vfsPath)

	switch {
	case info.Mode()&os.ModeSymlink != 0:
		target, err := os.Readlink(hostPath)
		if err != nil {
			return fmt.Errorf("readlink %s: %w", hostPath, err)
		}
		parent.Children[base] = &Node{
			Name:    base,
			Type:    FileTypeSymlink,
			Target:  target,
			Parent:  parent,
			Mode:    0o777,
			ModTime: info.ModTime(),
		}
	case info.IsDir():
		node, ok := parent.Children[base]
		if !ok || node.Type != FileTypeDirectory {
			node = &Node{
				Name:     base,
				Type:     FileTypeDirectory,
				Children: make(map[string]*Node),
				Parent:   parent,
				// Normalize to 0700: a valid, strict PostgreSQL data-dir mode.
				Mode:    0o700,
				ModTime: info.ModTime(),
			}
			parent.Children[base] = node
		}
		entries, err := os.ReadDir(hostPath)
		if err != nil {
			return fmt.Errorf("read %s: %w", hostPath, err)
		}
		for _, e := range entries {
			if err := fs.loadEntry(filepath.Join(hostPath, e.Name()), path.Join(vfsPath, e.Name())); err != nil {
				return err
			}
		}
	default:
		data, err := os.ReadFile(hostPath)
		if err != nil {
			return fmt.Errorf("read %s: %w", hostPath, err)
		}
		parent.Children[base] = &Node{
			Name:    base,
			Type:    FileTypeRegular,
			Data:    data,
			Parent:  parent,
			Mode:    0o600,
			ModTime: info.ModTime(),
		}
	}
	return nil
}
