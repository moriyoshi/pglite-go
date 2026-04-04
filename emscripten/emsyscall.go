package emscripten

import (
	"context"
	"encoding/binary"
	"fmt"
	"time"

	"github.com/tetratelabs/wazero/api"

	"github.com/moriyoshi/pglite-go/vfs"
)

// Errno values matching Emscripten's WASI-based errno mapping
// These are __WASI_ERRNO_* values, NOT Linux values!
const (
	EACCES    = 2
	EBADF     = 8
	EEXIST    = 20
	EFBIG     = 22
	EINVAL    = 28
	EIO       = 29
	EISDIR    = 31
	ENOENT    = 44
	ENOSYS    = 52
	ENOTDIR   = 54
	ENOTEMPTY = 55
)

// SyscallHandler holds state for Emscripten syscall implementations.
type SyscallHandler struct {
	FS *vfs.FS
}

// NewSyscallHandler creates a new syscall handler with the given filesystem.
func NewSyscallHandler(fs *vfs.FS) *SyscallHandler {
	return &SyscallHandler{FS: fs}
}

// readCStringFromMem reads a null-terminated string from WASM memory.
func readCStringFromMem(mem api.Memory, ptr uint32) string {
	var buf []byte
	for {
		b, ok := mem.ReadByte(ptr)
		if !ok || b == 0 {
			break
		}
		buf = append(buf, b)
		ptr++
	}
	return string(buf)
}

// writeToMem writes bytes to WASM memory.
func writeToMem(mem api.Memory, ptr uint32, data []byte) bool {
	return mem.Write(ptr, data)
}

// Register registers all syscall implementations on the given handler map.
func (h *SyscallHandler) Register() map[string]api.GoModuleFunc {
	return map[string]api.GoModuleFunc{
		"__syscall_openat":     h.syscallOpenat(),
		"__syscall_fstat64":    h.syscallFstat64(),
		"__syscall_stat64":     h.syscallStat64(),
		"__syscall_lstat64":    h.syscallLstat64(),
		"__syscall_newfstatat": h.syscallNewfstatat(),
		"__syscall_getcwd":     h.syscallGetcwd(),
		"__syscall_chdir":      h.syscallChdir(),
		"__syscall_mkdirat":    h.syscallMkdirat(),
		"__syscall_unlinkat":   h.syscallUnlinkat(),
		"__syscall_renameat":   h.syscallRenameat(),
		"__syscall_faccessat":  h.syscallFaccessat(),
		"__syscall_fcntl64":    h.syscallFcntl64(),
		"__syscall_ioctl":      h.syscallIoctl(),
		"__syscall_dup":        h.syscallDup(),
		"__syscall_dup3":       h.syscallDup3(),
		"__syscall_fchmod":     h.syscallFchmod(),
		"__syscall_chmod":      h.syscallChmod(),
		"__syscall_fchown32":   h.syscallFchown32(),
		"__syscall_fchownat":   h.syscallFchownat(),
		"__syscall_fchmodat2":  h.syscallFchmodat2(),
		"__syscall_rmdir":      h.syscallRmdir(),
		"__syscall_getdents64": h.syscallGetdents64(),
		"__syscall_readlinkat": h.syscallReadlinkat(),
		"__syscall_symlinkat":  h.syscallSymlinkat(),
		"__syscall_fdatasync":  h.syscallFdatasync(),
		"__syscall_ftruncate64": h.syscallFtruncate64(),
		"__syscall_truncate64": h.syscallTruncate64(),
		"__syscall_pipe":       h.syscallPipe(),
		"__syscall_fadvise64":  h.syscallFadvise64(),
		"__syscall_fallocate":  h.syscallFallocate(),
		"__syscall_statfs64":   h.syscallStatfs64(),
		"__syscall_utimensat":  h.syscallUtimensat(),
	}
}

func (h *SyscallHandler) resolvePathAt(mem api.Memory, dirfd int32, pathPtr uint32) string {
	p := readCStringFromMem(mem, pathPtr)
	if len(p) > 0 && p[0] == '/' {
		return p
	}
	if dirfd == vfs.AT_FDCWD {
		return p // will be resolved relative to cwd by the VFS
	}
	// TODO: resolve relative to dirfd
	return p
}

func (h *SyscallHandler) syscallOpenat() api.GoModuleFunc {
	return api.GoModuleFunc(func(ctx context.Context, mod api.Module, stack []uint64) {
		dirfd := api.DecodeI32(stack[0])
		pathPtr := api.DecodeU32(stack[1])
		flags := api.DecodeI32(stack[2])
		mode := api.DecodeU32(stack[3])

		p := h.resolvePathAt(mod.Memory(), dirfd, pathPtr)
		fd, err := h.FS.Open(p, flags, mode)
		if err != nil {
			stack[0] = api.EncodeI32(-ENOENT)
			return
		}
		stack[0] = api.EncodeI32(fd)
	})
}

// writeStat64 writes an Emscripten struct stat to WASM memory.
// Layout from Emscripten's musl for wasm32 (96 bytes total):
//
//	offset  0: st_dev      (uint32, 4 bytes)
//	offset  4: st_mode     (uint32, 4 bytes)
//	offset  8: st_nlink    (uint32, 4 bytes)
//	offset 12: st_uid      (uint32, 4 bytes)
//	offset 16: st_gid      (uint32, 4 bytes)
//	offset 20: st_rdev     (uint32, 4 bytes)
//	offset 24: st_size     (int64,  8 bytes)
//	offset 32: st_blksize  (int32,  4 bytes)
//	offset 36: st_blocks   (int32,  4 bytes)
//	offset 40: st_atim_sec (int64,  8 bytes)
//	offset 48: st_atim_nsec(int32+pad, 8 bytes)
//	offset 56: st_mtim_sec (int64,  8 bytes)
//	offset 64: st_mtim_nsec(int32+pad, 8 bytes)
//	offset 72: st_ctim_sec (int64,  8 bytes)
//	offset 80: st_ctim_nsec(int32+pad, 8 bytes)
//	offset 88: st_ino      (uint64, 8 bytes)
func writeStat64(mem api.Memory, buf uint32, node *vfs.Node) {
	var statBuf [96]byte

	var mode uint32
	var size uint64
	switch node.Type {
	case vfs.FileTypeDirectory:
		mode = 0o40000 | (node.Mode & 0o7777) // S_IFDIR
		size = 4096
	case vfs.FileTypeRegular:
		mode = 0o100000 | (node.Mode & 0o7777) // S_IFREG
		size = uint64(len(node.Data))
	case vfs.FileTypeSymlink:
		mode = 0o120000 | (node.Mode & 0o7777) // S_IFLNK
		size = uint64(len(node.Target))
	}

	binary.LittleEndian.PutUint32(statBuf[0:], 1)    // st_dev
	binary.LittleEndian.PutUint32(statBuf[4:], mode)  // st_mode
	binary.LittleEndian.PutUint32(statBuf[8:], 1)     // st_nlink
	binary.LittleEndian.PutUint64(statBuf[24:], size)  // st_size
	binary.LittleEndian.PutUint32(statBuf[32:], 4096)  // st_blksize
	blocks := int32((size + 511) / 512)
	binary.LittleEndian.PutUint32(statBuf[36:], uint32(blocks)) // st_blocks

	ts := node.ModTime.Unix()
	binary.LittleEndian.PutUint64(statBuf[40:], uint64(ts)) // st_atim.tv_sec
	binary.LittleEndian.PutUint64(statBuf[56:], uint64(ts)) // st_mtim.tv_sec
	binary.LittleEndian.PutUint64(statBuf[72:], uint64(ts)) // st_ctim.tv_sec

	// Use a hash of the node pointer as inode number to ensure uniqueness
	ino := uint64(buf) ^ uint64(mode)
	if ino == 0 {
		ino = 1
	}
	binary.LittleEndian.PutUint64(statBuf[88:], ino) // st_ino

	mem.Write(buf, statBuf[:])
}

func (h *SyscallHandler) syscallFstat64() api.GoModuleFunc {
	return api.GoModuleFunc(func(ctx context.Context, mod api.Module, stack []uint64) {
		fd := api.DecodeI32(stack[0])
		buf := api.DecodeU32(stack[1])

		node, err := h.FS.Fstat(fd)
		if err != nil {
			stack[0] = api.EncodeI32(-EBADF)
			return
		}
		writeStat64(mod.Memory(), buf, node)
		stack[0] = 0
	})
}

func (h *SyscallHandler) syscallStat64() api.GoModuleFunc {
	return api.GoModuleFunc(func(ctx context.Context, mod api.Module, stack []uint64) {
		pathPtr := api.DecodeU32(stack[0])
		buf := api.DecodeU32(stack[1])

		p := readCStringFromMem(mod.Memory(), pathPtr)
		node, err := h.FS.Stat(p)
		if err != nil {
			fmt.Printf("[syscall] stat64(%q, pathPtr=0x%x) -> ENOENT\n", p, pathPtr)
			stack[0] = api.EncodeI32(-ENOENT)
			return
		}
		writeStat64(mod.Memory(), buf, node)
		stack[0] = 0
	})
}

func (h *SyscallHandler) syscallLstat64() api.GoModuleFunc {
	return api.GoModuleFunc(func(ctx context.Context, mod api.Module, stack []uint64) {
		pathPtr := api.DecodeU32(stack[0])
		buf := api.DecodeU32(stack[1])

		p := readCStringFromMem(mod.Memory(), pathPtr)
		node, err := h.FS.Lstat(p)
		if err != nil {
			stack[0] = api.EncodeI32(-ENOENT)
			return
		}
		writeStat64(mod.Memory(), buf, node)
		stack[0] = 0
	})
}

func (h *SyscallHandler) syscallNewfstatat() api.GoModuleFunc {
	return api.GoModuleFunc(func(ctx context.Context, mod api.Module, stack []uint64) {
		dirfd := api.DecodeI32(stack[0])
		pathPtr := api.DecodeU32(stack[1])
		buf := api.DecodeU32(stack[2])
		// flags := api.DecodeI32(stack[3])

		p := h.resolvePathAt(mod.Memory(), dirfd, pathPtr)
		node, err := h.FS.Stat(p)
		if err != nil {
			fmt.Printf("[syscall] newfstatat(%s) -> ENOENT\n", p)
			stack[0] = api.EncodeI32(-ENOENT)
			return
		}
		writeStat64(mod.Memory(), buf, node)
		stack[0] = 0
	})
}

func (h *SyscallHandler) syscallGetcwd() api.GoModuleFunc {
	return api.GoModuleFunc(func(ctx context.Context, mod api.Module, stack []uint64) {
		buf := api.DecodeU32(stack[0])
		size := api.DecodeU32(stack[1])

		cwd := h.FS.Getcwd()
		if uint32(len(cwd)+1) > size {
			stack[0] = api.EncodeI32(-EINVAL)
			return
		}
		writeToMem(mod.Memory(), buf, append([]byte(cwd), 0))
		// Return length of the string written (including null terminator)
		stack[0] = api.EncodeI32(int32(len(cwd) + 1))
	})
}

func (h *SyscallHandler) syscallChdir() api.GoModuleFunc {
	return api.GoModuleFunc(func(ctx context.Context, mod api.Module, stack []uint64) {
		pathPtr := api.DecodeU32(stack[0])
		p := readCStringFromMem(mod.Memory(), pathPtr)
		if err := h.FS.Chdir(p); err != nil {
			stack[0] = api.EncodeI32(-ENOENT)
			return
		}
		stack[0] = 0
	})
}

func (h *SyscallHandler) syscallMkdirat() api.GoModuleFunc {
	return api.GoModuleFunc(func(ctx context.Context, mod api.Module, stack []uint64) {
		dirfd := api.DecodeI32(stack[0])
		pathPtr := api.DecodeU32(stack[1])
		mode := api.DecodeU32(stack[2])

		p := h.resolvePathAt(mod.Memory(), dirfd, pathPtr)
		_ = p
		if err := h.FS.MkdirAll(p, mode); err != nil {
			stack[0] = api.EncodeI32(-EEXIST)
			return
		}
		stack[0] = 0
	})
}

func (h *SyscallHandler) syscallUnlinkat() api.GoModuleFunc {
	return api.GoModuleFunc(func(ctx context.Context, mod api.Module, stack []uint64) {
		dirfd := api.DecodeI32(stack[0])
		pathPtr := api.DecodeU32(stack[1])
		flags := api.DecodeI32(stack[2])

		p := h.resolvePathAt(mod.Memory(), dirfd, pathPtr)
		var err error
		if flags&0x200 != 0 { // AT_REMOVEDIR
			err = h.FS.Rmdir(p)
		} else {
			err = h.FS.Unlink(p)
		}
		if err != nil {
			stack[0] = api.EncodeI32(-ENOENT)
			return
		}
		stack[0] = 0
	})
}

func (h *SyscallHandler) syscallRenameat() api.GoModuleFunc {
	return api.GoModuleFunc(func(ctx context.Context, mod api.Module, stack []uint64) {
		oldDirfd := api.DecodeI32(stack[0])
		oldPathPtr := api.DecodeU32(stack[1])
		newDirfd := api.DecodeI32(stack[2])
		newPathPtr := api.DecodeU32(stack[3])

		oldPath := h.resolvePathAt(mod.Memory(), oldDirfd, oldPathPtr)
		newPath := h.resolvePathAt(mod.Memory(), newDirfd, newPathPtr)

		if err := h.FS.Rename(oldPath, newPath); err != nil {
			stack[0] = api.EncodeI32(-ENOENT)
			return
		}
		stack[0] = 0
	})
}

func (h *SyscallHandler) syscallFaccessat() api.GoModuleFunc {
	return api.GoModuleFunc(func(ctx context.Context, mod api.Module, stack []uint64) {
		dirfd := api.DecodeI32(stack[0])
		pathPtr := api.DecodeU32(stack[1])
		mode := api.DecodeI32(stack[2])
		// flags := api.DecodeI32(stack[3])

		p := h.resolvePathAt(mod.Memory(), dirfd, pathPtr)
		node, err := h.FS.Stat(p)
		if err != nil {
			stack[0] = api.EncodeI32(-ENOENT)
			return
		}
		// Check execute permission (X_OK = 1)
		if mode&1 != 0 && node.Mode&0o111 == 0 {
			stack[0] = api.EncodeI32(-EACCES)
			return
		}
		stack[0] = 0
	})
}

func (h *SyscallHandler) syscallFcntl64() api.GoModuleFunc {
	return api.GoModuleFunc(func(ctx context.Context, mod api.Module, stack []uint64) {
		// fd := api.DecodeI32(stack[0])
		// cmd := api.DecodeI32(stack[1])
		// Mostly no-op for PoC
		stack[0] = 0
	})
}

func (h *SyscallHandler) syscallIoctl() api.GoModuleFunc {
	return api.GoModuleFunc(func(ctx context.Context, mod api.Module, stack []uint64) {
		stack[0] = api.EncodeI32(-ENOSYS)
	})
}

func (h *SyscallHandler) syscallDup() api.GoModuleFunc {
	return api.GoModuleFunc(func(ctx context.Context, mod api.Module, stack []uint64) {
		oldFD := api.DecodeI32(stack[0])
		newFD, err := h.FS.Dup(oldFD)
		if err != nil {
			stack[0] = api.EncodeI32(-EBADF)
			return
		}
		stack[0] = api.EncodeI32(newFD)
	})
}

func (h *SyscallHandler) syscallDup3() api.GoModuleFunc {
	return api.GoModuleFunc(func(ctx context.Context, mod api.Module, stack []uint64) {
		oldFD := api.DecodeI32(stack[0])
		newFD := api.DecodeI32(stack[1])
		// flags := api.DecodeI32(stack[2])
		_ = newFD
		fd, err := h.FS.Dup(oldFD)
		if err != nil {
			stack[0] = api.EncodeI32(-EBADF)
			return
		}
		stack[0] = api.EncodeI32(fd)
	})
}

func (h *SyscallHandler) syscallFchmod() api.GoModuleFunc {
	return api.GoModuleFunc(func(ctx context.Context, mod api.Module, stack []uint64) {
		stack[0] = 0 // no-op
	})
}

func (h *SyscallHandler) syscallChmod() api.GoModuleFunc {
	return api.GoModuleFunc(func(ctx context.Context, mod api.Module, stack []uint64) {
		stack[0] = 0 // no-op
	})
}

func (h *SyscallHandler) syscallFchown32() api.GoModuleFunc {
	return api.GoModuleFunc(func(ctx context.Context, mod api.Module, stack []uint64) {
		stack[0] = 0 // no-op
	})
}

func (h *SyscallHandler) syscallFchownat() api.GoModuleFunc {
	return api.GoModuleFunc(func(ctx context.Context, mod api.Module, stack []uint64) {
		stack[0] = 0 // no-op
	})
}

func (h *SyscallHandler) syscallFchmodat2() api.GoModuleFunc {
	return api.GoModuleFunc(func(ctx context.Context, mod api.Module, stack []uint64) {
		stack[0] = 0 // no-op
	})
}

func (h *SyscallHandler) syscallRmdir() api.GoModuleFunc {
	return api.GoModuleFunc(func(ctx context.Context, mod api.Module, stack []uint64) {
		pathPtr := api.DecodeU32(stack[0])
		p := readCStringFromMem(mod.Memory(), pathPtr)
		if err := h.FS.Rmdir(p); err != nil {
			stack[0] = api.EncodeI32(-ENOENT)
			return
		}
		stack[0] = 0
	})
}

func (h *SyscallHandler) syscallGetdents64() api.GoModuleFunc {
	return api.GoModuleFunc(func(ctx context.Context, mod api.Module, stack []uint64) {
		fd := api.DecodeI32(stack[0])
		buf := api.DecodeU32(stack[1])
		bufSize := api.DecodeU32(stack[2])

		entries, err := h.FS.Readdir(fd)
		if err != nil {
			stack[0] = api.EncodeI32(-EBADF)
			return
		}

		// Use the file descriptor's offset to track which entries have been returned
		of, ok := h.FS.GetFD(fd)
		if !ok {
			stack[0] = api.EncodeI32(-EBADF)
			return
		}
		startIdx := int(of.Offset)

		allEntries := append([]string{".", ".."}, entries...)

		// If we've already returned all entries, return 0 (EOF)
		if startIdx >= len(allEntries) {
			stack[0] = 0
			return
		}

		offset := uint32(0)
		idx := startIdx
		for idx < len(allEntries) {
			name := allEntries[idx]
			recLen := uint16(8 + 8 + 2 + 1 + len(name) + 1)
			recLen = (recLen + 7) &^ 7

			if offset+uint32(recLen) > bufSize {
				break
			}

			var rec [280]byte
			binary.LittleEndian.PutUint64(rec[0:], uint64(idx+1)) // d_ino (non-zero)
			binary.LittleEndian.PutUint64(rec[8:], uint64(idx+1)) // d_off (next position)
			binary.LittleEndian.PutUint16(rec[16:], recLen)
			rec[18] = 8 // DT_REG default
			copy(rec[19:], name)
			rec[19+len(name)] = 0

			mod.Memory().Write(buf+offset, rec[:recLen])
			offset += uint32(recLen)
			idx++
		}

		// Update the offset to track how many entries we've returned
		of.Offset = int64(idx)

		stack[0] = api.EncodeI32(int32(offset))
	})
}

func (h *SyscallHandler) syscallReadlinkat() api.GoModuleFunc {
	return api.GoModuleFunc(func(ctx context.Context, mod api.Module, stack []uint64) {
		dirfd := api.DecodeI32(stack[0])
		pathPtr := api.DecodeU32(stack[1])
		buf := api.DecodeU32(stack[2])
		bufSize := api.DecodeU32(stack[3])

		p := h.resolvePathAt(mod.Memory(), dirfd, pathPtr)

		// Handle /proc/self/exe specially - PostgreSQL uses this to find its binary
		if p == "/proc/self/exe" {
			target := "/pglite/bin/postgres"
			n := len(target)
			if uint32(n) > bufSize {
				n = int(bufSize)
			}
			mod.Memory().Write(buf, []byte(target[:n]))
			stack[0] = api.EncodeI32(int32(n))
			return
		}

		// Check if it's a symlink in our VFS
		// Note: Stat follows symlinks, so we can't use it for lstat behavior.
		// For non-/proc/self/exe paths, just return EINVAL (not a symlink)
		stack[0] = api.EncodeI32(-EINVAL)
	})
}

func (h *SyscallHandler) syscallSymlinkat() api.GoModuleFunc {
	return api.GoModuleFunc(func(ctx context.Context, mod api.Module, stack []uint64) {
		stack[0] = 0 // no-op
	})
}

func (h *SyscallHandler) syscallFdatasync() api.GoModuleFunc {
	return api.GoModuleFunc(func(ctx context.Context, mod api.Module, stack []uint64) {
		stack[0] = 0 // no-op for in-memory FS
	})
}

func (h *SyscallHandler) syscallFtruncate64() api.GoModuleFunc {
	return api.GoModuleFunc(func(ctx context.Context, mod api.Module, stack []uint64) {
		fd := api.DecodeI32(stack[0])
		length := int64(stack[1])
		if err := h.FS.Truncate(fd, length); err != nil {
			stack[0] = api.EncodeI32(-EBADF)
			return
		}
		stack[0] = 0
	})
}

func (h *SyscallHandler) syscallTruncate64() api.GoModuleFunc {
	return api.GoModuleFunc(func(ctx context.Context, mod api.Module, stack []uint64) {
		stack[0] = api.EncodeI32(-ENOSYS)
	})
}

func (h *SyscallHandler) syscallPipe() api.GoModuleFunc {
	return api.GoModuleFunc(func(ctx context.Context, mod api.Module, stack []uint64) {
		pipefd := api.DecodeU32(stack[0])

		// Create two "pipe" file descriptors backed by in-memory buffers
		h.FS.WriteFile("/tmp/.pipe_r", nil, 0o600)
		h.FS.WriteFile("/tmp/.pipe_w", nil, 0o600)
		rfd, _ := h.FS.Open("/tmp/.pipe_r", 0, 0o600)
		wfd, _ := h.FS.Open("/tmp/.pipe_w", 0, 0o600)

		mem := mod.Memory()
		mem.WriteUint32Le(pipefd, uint32(rfd))
		mem.WriteUint32Le(pipefd+4, uint32(wfd))

		stack[0] = 0
	})
}

func (h *SyscallHandler) syscallFadvise64() api.GoModuleFunc {
	return api.GoModuleFunc(func(ctx context.Context, mod api.Module, stack []uint64) {
		stack[0] = 0 // no-op
	})
}

func (h *SyscallHandler) syscallFallocate() api.GoModuleFunc {
	return api.GoModuleFunc(func(ctx context.Context, mod api.Module, stack []uint64) {
		stack[0] = 0 // no-op
	})
}

func (h *SyscallHandler) syscallStatfs64() api.GoModuleFunc {
	return api.GoModuleFunc(func(ctx context.Context, mod api.Module, stack []uint64) {
		// pathPtr := api.DecodeU32(stack[0])
		// size := api.DecodeU32(stack[1])
		buf := api.DecodeU32(stack[2])

		// Write a minimal statfs structure
		var statfs [64]byte
		binary.LittleEndian.PutUint32(statfs[0:], 0x9fa0) // f_type = TMPFS_MAGIC
		binary.LittleEndian.PutUint32(statfs[4:], 4096)    // f_bsize
		binary.LittleEndian.PutUint64(statfs[8:], 1<<30)   // f_blocks
		binary.LittleEndian.PutUint64(statfs[16:], 1<<29)  // f_bfree
		binary.LittleEndian.PutUint64(statfs[24:], 1<<29)  // f_bavail
		mod.Memory().Write(buf, statfs[:])
		stack[0] = 0
	})
}

func (h *SyscallHandler) syscallUtimensat() api.GoModuleFunc {
	return api.GoModuleFunc(func(ctx context.Context, mod api.Module, stack []uint64) {
		stack[0] = 0 // no-op
		_ = time.Now()
	})
}
