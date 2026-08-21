package emscripten

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"

	"github.com/moriyoshi/pglite-go/vfs"
)

// WASI errno values
const (
	wasiSuccess    = 0
	wasiBadf       = 8
	wasiInval      = 28
	wasiNosys      = 52
)

// InstantiateWASIWithCapture is like InstantiateWASI but captures stdout to a buffer.
func InstantiateWASIWithCapture(ctx context.Context, r wazero.Runtime, fs *vfs.FS, stdoutBuf *[]byte) error {
	return instantiateWASIImpl(ctx, r, fs, nil, stdoutBuf)
}

// InstantiateWASIWithStdin provides stdin data from a byte slice.
func InstantiateWASIWithStdin(ctx context.Context, r wazero.Runtime, fs *vfs.FS, stdinData []byte) error {
	return instantiateWASIImpl(ctx, r, fs, stdinData, nil)
}

// InstantiateWASIWithStdinAndCapture provides stdin data and captures stdout.
func InstantiateWASIWithStdinAndCapture(ctx context.Context, r wazero.Runtime, fs *vfs.FS, stdinData []byte, stdoutBuf *[]byte) error {
	return instantiateWASIImpl(ctx, r, fs, stdinData, stdoutBuf)
}

// InstantiateWASI creates a custom wasi_snapshot_preview1 module that
// delegates file operations to our VFS instead of the host filesystem.
func InstantiateWASI(ctx context.Context, r wazero.Runtime, fs *vfs.FS) error {
	return instantiateWASIImpl(ctx, r, fs, nil, nil)
}

// InstantiateWASIReturning is like InstantiateWASI but returns the WASIInstance for stdout control.
func InstantiateWASIReturning(ctx context.Context, r wazero.Runtime, fs *vfs.FS) (*WASIInstance, error) {
	return instantiateWASIFull(ctx, r, fs, nil, nil)
}

// WASIInstance provides access to the WASI state for runtime stdout override.
type WASIInstance struct {
	impl *wasiImpl
}

// SetStdoutCapture enables capturing stdout to the given buffer.
// Pass nil to restore normal stdout.
func (w *WASIInstance) SetStdoutCapture(buf *[]byte) {
	w.impl.StdoutOverride = buf
}

func instantiateWASIImpl(ctx context.Context, r wazero.Runtime, fs *vfs.FS, stdinData []byte, stdoutCapture *[]byte) error {
	_, err := instantiateWASIFull(ctx, r, fs, stdinData, stdoutCapture)
	return err
}

func instantiateWASIFull(ctx context.Context, r wazero.Runtime, fs *vfs.FS, stdinData []byte, stdoutCapture *[]byte) (*WASIInstance, error) {
	builder := r.NewHostModuleBuilder("wasi_snapshot_preview1")

	stdout := io.Writer(os.Stdout)
	if stdoutCapture != nil {
		stdout = &bufWriter{buf: stdoutCapture}
	}
	wasi := &wasiImpl{fs: fs, stdout: stdout, stderr: os.Stderr, stdinData: stdinData}

	builder.NewFunctionBuilder().
		WithGoModuleFunction(wasi.clockTimeGet(), []api.ValueType{api.ValueTypeI32, api.ValueTypeI64, api.ValueTypeI32}, []api.ValueType{api.ValueTypeI32}).
		Export("clock_time_get")

	builder.NewFunctionBuilder().
		WithGoModuleFunction(wasi.environGet(), []api.ValueType{api.ValueTypeI32, api.ValueTypeI32}, []api.ValueType{api.ValueTypeI32}).
		Export("environ_get")

	builder.NewFunctionBuilder().
		WithGoModuleFunction(wasi.environSizesGet(), []api.ValueType{api.ValueTypeI32, api.ValueTypeI32}, []api.ValueType{api.ValueTypeI32}).
		Export("environ_sizes_get")

	builder.NewFunctionBuilder().
		WithGoModuleFunction(wasi.fdClose(), []api.ValueType{api.ValueTypeI32}, []api.ValueType{api.ValueTypeI32}).
		Export("fd_close")

	builder.NewFunctionBuilder().
		WithGoModuleFunction(wasi.fdFdstatGet(), []api.ValueType{api.ValueTypeI32, api.ValueTypeI32}, []api.ValueType{api.ValueTypeI32}).
		Export("fd_fdstat_get")

	builder.NewFunctionBuilder().
		WithGoModuleFunction(wasi.fdPread(), []api.ValueType{api.ValueTypeI32, api.ValueTypeI32, api.ValueTypeI32, api.ValueTypeI64, api.ValueTypeI32}, []api.ValueType{api.ValueTypeI32}).
		Export("fd_pread")

	builder.NewFunctionBuilder().
		WithGoModuleFunction(wasi.fdPwrite(), []api.ValueType{api.ValueTypeI32, api.ValueTypeI32, api.ValueTypeI32, api.ValueTypeI64, api.ValueTypeI32}, []api.ValueType{api.ValueTypeI32}).
		Export("fd_pwrite")

	builder.NewFunctionBuilder().
		WithGoModuleFunction(wasi.fdRead(), []api.ValueType{api.ValueTypeI32, api.ValueTypeI32, api.ValueTypeI32, api.ValueTypeI32}, []api.ValueType{api.ValueTypeI32}).
		Export("fd_read")

	builder.NewFunctionBuilder().
		WithGoModuleFunction(wasi.fdSeek(), []api.ValueType{api.ValueTypeI32, api.ValueTypeI64, api.ValueTypeI32, api.ValueTypeI32}, []api.ValueType{api.ValueTypeI32}).
		Export("fd_seek")

	builder.NewFunctionBuilder().
		WithGoModuleFunction(wasi.fdSync(), []api.ValueType{api.ValueTypeI32}, []api.ValueType{api.ValueTypeI32}).
		Export("fd_sync")

	builder.NewFunctionBuilder().
		WithGoModuleFunction(wasi.fdWrite(), []api.ValueType{api.ValueTypeI32, api.ValueTypeI32, api.ValueTypeI32, api.ValueTypeI32}, []api.ValueType{api.ValueTypeI32}).
		Export("fd_write")

	builder.NewFunctionBuilder().
		WithGoModuleFunction(wasi.procExit(), []api.ValueType{api.ValueTypeI32}, nil).
		Export("proc_exit")

	builder.NewFunctionBuilder().
		WithGoModuleFunction(wasi.randomGet(), []api.ValueType{api.ValueTypeI32, api.ValueTypeI32}, []api.ValueType{api.ValueTypeI32}).
		Export("random_get")

	_, err := builder.Instantiate(ctx)
	if err != nil {
		return nil, err
	}
	return &WASIInstance{impl: wasi}, nil
}

// bufWriter captures writes to a byte slice.
type bufWriter struct {
	buf *[]byte
}

func (w *bufWriter) Write(p []byte) (n int, err error) {
	*w.buf = append(*w.buf, p...)
	return len(p), nil
}

type wasiImpl struct {
	fs        *vfs.FS
	stdout    io.Writer
	stderr    io.Writer
	envVars   []string
	stdinData []byte
	stdinPos  int
	// StdoutOverride, if non-nil, captures stdout instead of writing to stdout
	StdoutOverride *[]byte
}

func defaultEnvVars() []string {
	return []string{
		"PGDATA=/tmp/pglite/data",
		"HOME=/home/web_user",
		"USER=web_user",
		"PATH=/pglite/bin",
		"LC_ALL=C",
		"LANG=C",
	}
}

func (w *wasiImpl) clockTimeGet() api.GoModuleFunc {
	return api.GoModuleFunc(func(ctx context.Context, mod api.Module, stack []uint64) {
		clockID := api.DecodeU32(stack[0])
		// precision := stack[1]
		resultPtr := api.DecodeU32(stack[2])

		var nanos uint64
		switch clockID {
		case 0: // CLOCK_REALTIME
			nanos = uint64(time.Now().UnixNano())
		case 1: // CLOCK_MONOTONIC
			nanos = uint64(time.Now().UnixNano())
		default:
			stack[0] = wasiInval
			return
		}

		mod.Memory().WriteUint64Le(resultPtr, nanos)
		stack[0] = wasiSuccess
	})
}

func (w *wasiImpl) environGet() api.GoModuleFunc {
	return api.GoModuleFunc(func(ctx context.Context, mod api.Module, stack []uint64) {
		environPtr := api.DecodeU32(stack[0])
		environBufPtr := api.DecodeU32(stack[1])
		mem := mod.Memory()
		vars := w.envVars
		if len(vars) == 0 {
			vars = defaultEnvVars()
		}
		tracef("[environ_get] writing %d vars\n", len(vars))
		bufOffset := environBufPtr
		for i, v := range vars {
			// Write pointer to the env string
			mem.WriteUint32Le(environPtr+uint32(i*4), bufOffset)
			// Write the env string (null-terminated)
			mem.Write(bufOffset, append([]byte(v), 0))
			bufOffset += uint32(len(v) + 1)
		}
		stack[0] = wasiSuccess
	})
}

func (w *wasiImpl) environSizesGet() api.GoModuleFunc {
	return api.GoModuleFunc(func(ctx context.Context, mod api.Module, stack []uint64) {
		countPtr := api.DecodeU32(stack[0])
		sizePtr := api.DecodeU32(stack[1])
		vars := w.envVars
		if len(vars) == 0 {
			vars = defaultEnvVars()
		}
		totalSize := uint32(0)
		for _, v := range vars {
			totalSize += uint32(len(v) + 1) // +1 for null terminator
		}
		mod.Memory().WriteUint32Le(countPtr, uint32(len(vars)))
		mod.Memory().WriteUint32Le(sizePtr, totalSize)
		stack[0] = wasiSuccess
	})
}

func (w *wasiImpl) fdClose() api.GoModuleFunc {
	return api.GoModuleFunc(func(ctx context.Context, mod api.Module, stack []uint64) {
		fd := api.DecodeI32(stack[0])
		if fd <= 2 {
			stack[0] = wasiSuccess
			return
		}
		if err := w.fs.Close(fd); err != nil {
			stack[0] = wasiBadf
			return
		}
		stack[0] = wasiSuccess
	})
}

func (w *wasiImpl) fdFdstatGet() api.GoModuleFunc {
	return api.GoModuleFunc(func(ctx context.Context, mod api.Module, stack []uint64) {
		fd := api.DecodeI32(stack[0])
		buf := api.DecodeU32(stack[1])

		// Write a minimal fdstat structure (24 bytes)
		var fdstat [24]byte
		if fd <= 2 {
			fdstat[0] = 2 // filetype = character device
		} else {
			_, ok := w.fs.GetFD(fd)
			if !ok {
				stack[0] = wasiBadf
				return
			}
			fdstat[0] = 4 // filetype = regular file
		}
		// fdflags at offset 2 = 0
		// rights_base at offset 8 = all rights
		binary.LittleEndian.PutUint64(fdstat[8:], 0xFFFFFFFFFFFFFFFF)
		// rights_inheriting at offset 16 = all rights
		binary.LittleEndian.PutUint64(fdstat[16:], 0xFFFFFFFFFFFFFFFF)

		mod.Memory().Write(buf, fdstat[:])
		stack[0] = wasiSuccess
	})
}

func (w *wasiImpl) fdRead() api.GoModuleFunc {
	return api.GoModuleFunc(func(ctx context.Context, mod api.Module, stack []uint64) {
		fd := api.DecodeI32(stack[0])
		iovsPtr := api.DecodeU32(stack[1])
		iovsLen := api.DecodeU32(stack[2])
		nreadPtr := api.DecodeU32(stack[3])

		if fd == 0 {
			// Read from stdinData if available
			if w.stdinData != nil && w.stdinPos < len(w.stdinData) {
				totalRead := uint32(0)
				for i := uint32(0); i < iovsLen && w.stdinPos < len(w.stdinData); i++ {
					bufPtr, _ := mod.Memory().ReadUint32Le(iovsPtr + i*8)
					bufLen, _ := mod.Memory().ReadUint32Le(iovsPtr + i*8 + 4)
					n := copy(make([]byte, bufLen), w.stdinData[w.stdinPos:])
					if n > int(bufLen) {
						n = int(bufLen)
					}
					remaining := len(w.stdinData) - w.stdinPos
					if remaining < n {
						n = remaining
					}
					mod.Memory().Write(bufPtr, w.stdinData[w.stdinPos:w.stdinPos+n])
					w.stdinPos += n
					totalRead += uint32(n)
				}
				mod.Memory().WriteUint32Le(nreadPtr, totalRead)
				stack[0] = wasiSuccess
				return
			}
			// EOF
			mod.Memory().WriteUint32Le(nreadPtr, 0)
			stack[0] = wasiSuccess
			return
		}

		totalRead := uint32(0)
		for i := uint32(0); i < iovsLen; i++ {
			bufPtr, _ := mod.Memory().ReadUint32Le(iovsPtr + i*8)
			bufLen, _ := mod.Memory().ReadUint32Le(iovsPtr + i*8 + 4)

			if bufLen == 0 {
				continue
			}

			// Special handling for /dev/urandom - return random data
			of, ok := w.fs.GetFD(fd)
			if ok && of.Path == "/dev/urandom" {
				randomBytes := make([]byte, bufLen)
				rand.Read(randomBytes)
				mod.Memory().Write(bufPtr, randomBytes)
				totalRead += bufLen
				continue
			}

			readBuf := make([]byte, bufLen)
			n, err := w.fs.Read(fd, readBuf)
			if err != nil {
				if totalRead > 0 {
					break
				}
				mod.Memory().WriteUint32Le(nreadPtr, 0)
				stack[0] = wasiBadf
				return
			}
			if n > 0 {
				mod.Memory().Write(bufPtr, readBuf[:n])
			}
			totalRead += uint32(n)
			if n < int(bufLen) {
				break // short read
			}
		}

		mod.Memory().WriteUint32Le(nreadPtr, totalRead)
		stack[0] = wasiSuccess
	})
}

func (w *wasiImpl) fdWrite() api.GoModuleFunc {
	return api.GoModuleFunc(func(ctx context.Context, mod api.Module, stack []uint64) {
		fd := api.DecodeI32(stack[0])
		iovsPtr := api.DecodeU32(stack[1])
		iovsLen := api.DecodeU32(stack[2])
		nwrittenPtr := api.DecodeU32(stack[3])

		totalWritten := uint32(0)
		for i := uint32(0); i < iovsLen; i++ {
			bufPtr, _ := mod.Memory().ReadUint32Le(iovsPtr + i*8)
			bufLen, _ := mod.Memory().ReadUint32Le(iovsPtr + i*8 + 4)

			if bufLen == 0 {
				continue
			}

			data, ok := mod.Memory().Read(bufPtr, bufLen)
			if !ok {
				break
			}

			if fd == 1 {
				if w.StdoutOverride != nil {
					*w.StdoutOverride = append(*w.StdoutOverride, data...)
				} else {
					w.stdout.Write(data)
				}
				totalWritten += bufLen
			} else if fd == 2 {
				w.stderr.Write(data)
				totalWritten += bufLen
			} else {
				n, err := w.fs.Write(fd, data)
				if err != nil {
					break
				}
				totalWritten += uint32(n)
			}
		}

		mod.Memory().WriteUint32Le(nwrittenPtr, totalWritten)
		stack[0] = wasiSuccess
	})
}

func (w *wasiImpl) fdSeek() api.GoModuleFunc {
	return api.GoModuleFunc(func(ctx context.Context, mod api.Module, stack []uint64) {
		fd := api.DecodeI32(stack[0])
		offset := int64(stack[1])
		whence := api.DecodeU32(stack[2])
		newOffsetPtr := api.DecodeU32(stack[3])

		newOffset, err := w.fs.Seek(fd, offset, int(whence))
		if err != nil {
			stack[0] = wasiBadf
			return
		}
		mod.Memory().WriteUint64Le(newOffsetPtr, uint64(newOffset))
		stack[0] = wasiSuccess
	})
}

func (w *wasiImpl) fdSync() api.GoModuleFunc {
	return api.GoModuleFunc(func(ctx context.Context, mod api.Module, stack []uint64) {
		stack[0] = wasiSuccess // no-op
	})
}

func (w *wasiImpl) fdPread() api.GoModuleFunc {
	return api.GoModuleFunc(func(ctx context.Context, mod api.Module, stack []uint64) {
		fd := api.DecodeI32(stack[0])
		iovsPtr := api.DecodeU32(stack[1])
		iovsLen := api.DecodeU32(stack[2])
		offset := int64(stack[3])
		nreadPtr := api.DecodeU32(stack[4])

		// Save current position, seek, read, restore
		oldPos, _ := w.fs.Seek(fd, 0, 1) // SEEK_CUR
		w.fs.Seek(fd, offset, 0)          // SEEK_SET

		totalRead := uint32(0)
		for i := uint32(0); i < iovsLen; i++ {
			bufPtr, _ := mod.Memory().ReadUint32Le(iovsPtr + i*8)
			bufLen, _ := mod.Memory().ReadUint32Le(iovsPtr + i*8 + 4)
			readBuf := make([]byte, bufLen)
			n, _ := w.fs.Read(fd, readBuf)
			if n > 0 {
				mod.Memory().Write(bufPtr, readBuf[:n])
			}
			totalRead += uint32(n)
			if n < int(bufLen) {
				break
			}
		}

		w.fs.Seek(fd, oldPos, 0) // restore
		mod.Memory().WriteUint32Le(nreadPtr, totalRead)
		stack[0] = wasiSuccess
	})
}

func (w *wasiImpl) fdPwrite() api.GoModuleFunc {
	return api.GoModuleFunc(func(ctx context.Context, mod api.Module, stack []uint64) {
		fd := api.DecodeI32(stack[0])
		iovsPtr := api.DecodeU32(stack[1])
		iovsLen := api.DecodeU32(stack[2])
		offset := int64(stack[3])
		nwrittenPtr := api.DecodeU32(stack[4])

		oldPos, _ := w.fs.Seek(fd, 0, 1)
		w.fs.Seek(fd, offset, 0)

		totalWritten := uint32(0)
		for i := uint32(0); i < iovsLen; i++ {
			bufPtr, _ := mod.Memory().ReadUint32Le(iovsPtr + i*8)
			bufLen, _ := mod.Memory().ReadUint32Le(iovsPtr + i*8 + 4)
			data, ok := mod.Memory().Read(bufPtr, bufLen)
			if !ok {
				break
			}
			n, _ := w.fs.Write(fd, data)
			totalWritten += uint32(n)
		}

		w.fs.Seek(fd, oldPos, 0)
		mod.Memory().WriteUint32Le(nwrittenPtr, totalWritten)
		stack[0] = wasiSuccess
	})
}

func (w *wasiImpl) procExit() api.GoModuleFunc {
	return api.GoModuleFunc(func(ctx context.Context, mod api.Module, stack []uint64) {
		code := api.DecodeU32(stack[0])
		tracef("[wasi] proc_exit(%d)\n", code)
		panic(fmt.Sprintf("proc_exit(%d)", code))
	})
}

func (w *wasiImpl) randomGet() api.GoModuleFunc {
	return api.GoModuleFunc(func(ctx context.Context, mod api.Module, stack []uint64) {
		buf := api.DecodeU32(stack[0])
		bufLen := api.DecodeU32(stack[1])

		randomBytes := make([]byte, bufLen)
		rand.Read(randomBytes)
		mod.Memory().Write(buf, randomBytes)
		stack[0] = wasiSuccess
	})
}
