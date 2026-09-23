//go:build aot

package emscripten

import (
	"context"
	"io"
	"os"

	"github.com/moriyoshi/pglite-go/vfs"
	"github.com/tetratelabs/wazero/api"
)

// AOTDispatch resolves and invokes the shared host handlers (syscall + WASI, plus
// the createHostFunc/makeStub fallbacks) for the AOT backend. The generated env
// adapter (internal/genaot) holds one and routes each non-invoke import through
// Invoke, so all the host logic in emsyscall.go/syscall.go/wasi.go/systemcb.go is
// reused verbatim.
type AOTDispatch struct {
	syscalls map[string]api.GoModuleFunc
	wasi     map[string]api.GoModuleFunc
	cache    map[string]api.GoModuleFunc
	w        *wasiImpl
}

// SetStdout/SetStderr/SetStdoutCapture retarget the VFS-backed WASI streams.
// SetStdoutCapture is what makes initdb's popen("w") bootstrap-SQL capture work.
func (d *AOTDispatch) SetStdout(w io.Writer)        { d.w.stdout = w }
func (d *AOTDispatch) SetStderr(w io.Writer)        { d.w.stderr = w }
func (d *AOTDispatch) SetStdoutCapture(buf *[]byte) { d.w.StdoutOverride = buf }

// NewAOTDispatch builds the handler maps over the given VFS/stdin/capture. Note:
// this is the emscripten (VFS-backed) WASI, replacing wasm2go's host-backed
// DefaultWASI, so the guest sees the in-memory filesystem.
func NewAOTDispatch(fs *vfs.FS, stdin []byte, capture *[]byte) *AOTDispatch {
	w := &wasiImpl{
		fs:             fs,
		stdinData:      stdin,
		StdoutOverride: capture,
		stdout:         os.Stdout,
		stderr:         os.Stderr,
	}
	return &AOTDispatch{
		syscalls: NewSyscallHandler(fs).Register(),
		wasi:     w.aotFuncs(),
		cache:    map[string]api.GoModuleFunc{},
		w:        w,
	}
}

// aotFuncs mirrors wasiImpl.goModuleFuncs (which lives in the wasmtime-tagged
// port file and is thus unavailable under -tags aot).
func (w *wasiImpl) aotFuncs() map[string]api.GoModuleFunc {
	return map[string]api.GoModuleFunc{
		"clock_time_get":    w.clockTimeGet(),
		"environ_get":       w.environGet(),
		"environ_sizes_get": w.environSizesGet(),
		"fd_close":          w.fdClose(),
		"fd_fdstat_get":     w.fdFdstatGet(),
		"fd_pread":          w.fdPread(),
		"fd_pwrite":         w.fdPwrite(),
		"fd_read":           w.fdRead(),
		"fd_seek":           w.fdSeek(),
		"fd_sync":           w.fdSync(),
		"fd_write":          w.fdWrite(),
		"proc_exit":         w.procExit(),
		"random_get":        w.randomGet(),
	}
}

// Invoke resolves the handler for the wasm import name and runs it against mod
// with stack. params/results are the wasm valtype bytes (for the
// createHostFunc/makeStub fallback used by non-syscall Emscripten runtime funcs).
func (d *AOTDispatch) Invoke(name string, mod api.Module, stack []uint64, params, results []byte) {
	h, ok := d.cache[name]
	if !ok {
		if f, o := d.syscalls[name]; o {
			h = f
		} else if f, o := d.wasi[name]; o {
			h = f
		} else {
			h = createHostFunc(name, funcSig{params: params, results: results})
		}
		if h == nil {
			h = makeStub(name, funcSig{params: params, results: results})
		}
		d.cache[name] = h
	}
	h(context.Background(), mod, stack)
}
