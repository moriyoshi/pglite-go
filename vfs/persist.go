package vfs

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
)

// SaveSubtree mirrors the VFS subtree rooted at vfsPath onto the host filesystem
// at hostDir. Regular files, directories and symlinks are preserved along with
// their permission bits. hostDir is created if absent. The data directory lives
// entirely in the writable upper layer, so this walks the upper node tree.
//
// This is used to persist the initdb-created data directory so that initdb (the
// dominant startup cost) runs only once per data directory rather than on every
// process start.
func (v *FS) SaveSubtree(vfsPath, hostDir string) error {
	v.mu.RLock()
	defer v.mu.RUnlock()

	root, ok := v.inUpper(v.resolve(vfsPath))
	if !ok {
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
	return v.saveNode(root, hostDir)
}

// saveNode writes the children of a directory node into hostDir. Must hold the
// read lock.
func (v *FS) saveNode(dir *Node, hostDir string) error {
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
			if err := v.saveNode(child, target); err != nil {
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
		// fileTypeWhiteout entries are internal and never persisted.
	}
	return nil
}

// LoadSubtree materializes an fs.FS (previously written by SaveSubtree, wrapped
// with os.DirFS) into the writable upper layer at vfsPath. Existing VFS entries
// under vfsPath are left in place unless overwritten by a source entry of the
// same name.
func (v *FS) LoadSubtree(src fs.FS, vfsPath string) error {
	v.mu.Lock()
	defer v.mu.Unlock()

	base := v.resolve(vfsPath)
	if _, err := v.upper.mkdirAll(relName(base), 0o700); err != nil {
		return fmt.Errorf("LoadSubtree: mkdir %s: %w", base, err)
	}

	return fs.WalkDir(src, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return fmt.Errorf("LoadSubtree: walk %s: %w", p, err)
		}
		if p == "." {
			return nil
		}
		abs := path.Join(base, p)
		info, err := d.Info()
		if err != nil {
			return fmt.Errorf("LoadSubtree: stat %s: %w", p, err)
		}
		switch {
		case d.Type()&fs.ModeSymlink != 0:
			target, err := fs.ReadLink(src, p)
			if err != nil {
				return fmt.Errorf("LoadSubtree: readlink %s: %w", p, err)
			}
			if _, err := v.upper.place(relName(abs), &Node{
				Type:    FileTypeSymlink,
				Target:  target,
				Mode:    0o777,
				ModTime: info.ModTime(),
			}); err != nil {
				return err
			}
		case d.IsDir():
			// Normalize to 0700: a valid, strict PostgreSQL data-dir mode.
			if _, err := v.upper.mkdirAll(relName(abs), 0o700); err != nil {
				return err
			}
		default:
			f, err := src.Open(p)
			if err != nil {
				return fmt.Errorf("LoadSubtree: open %s: %w", p, err)
			}
			data := make([]byte, info.Size())
			n, rerr := io.ReadFull(f, data)
			f.Close()
			if rerr != nil && rerr != io.EOF && rerr != io.ErrUnexpectedEOF {
				return fmt.Errorf("LoadSubtree: read %s: %w", p, rerr)
			}
			if _, err := v.upper.place(relName(abs), &Node{
				Type:    FileTypeRegular,
				Data:    data[:n],
				Mode:    0o600,
				ModTime: info.ModTime(),
			}); err != nil {
				return err
			}
		}
		return nil
	})
}
