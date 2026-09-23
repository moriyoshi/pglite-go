//go:build aot

package emscripten

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"strings"
)

// A2GRuntime drives a PGlite module that goccy/wasm2go has transpiled to Go
// ahead of time (via internal/wasmpass + `go generate -tags aot`). There is no
// wasm engine: the module is Go code. A2GRuntime implements Runtime, so the
// wire-protocol, initdb, and open orchestration in wire.go/cluster.go/pglite.go
// run against it unchanged behind -tags aot — the same way they run against the
// wasmtime and wazero backends.
//
// This file is engine- and type-agnostic: it talks to the transpiled module
// only through AOTModule. The generated package ships a small shim (emitted by
// the generate step) that implements AOTModule over its concrete *base.Module,
// so this runtime is shared by both the pglite and initdb transpiled packages.

// AOTModule is the surface a transpiled PGlite package exposes to A2GRuntime.
type AOTModule interface {
	// Memory returns the module's linear memory (wasm2go exports it as []byte).
	Memory() []byte
	// ApplyDataRelocs runs __wasm_apply_data_relocs.
	ApplyDataRelocs()
	// CallCtors runs __wasm_call_ctors (must follow ApplyDataRelocs).
	CallCtors()
	// Call invokes an exported function by name. Arguments and the result use
	// wasm value lanes (i32 in the low 32 bits; f64 via math.Float64bits). A void
	// export returns 0. The generated shim switches over the known export roots.
	Call(name string, args []uint64) uint64
	// RegisterRW installs the socket read/write callbacks into the indirect
	// function table (m.T0) and returns the base index (base+0=read, base+1=write).
	// It lives in the generated shim because only that code knows the concrete
	// func(*base.Module, …) table-entry signatures.
	RegisterRW(read func(dst []byte) int, write func(src []byte)) uint32
	// RegisterInitdb installs the system/popen/pclose callbacks and returns the
	// base index.
	RegisterInitdb(cb *InitdbCallbacks) uint32
	// SP/SetSP read and write the __stack_pointer global (for argv marshaling).
	SP() int32
	SetSP(int32)
	// SetStdout/SetStderr/SetStdoutCapture retarget the VFS-backed WASI streams.
	SetStdout(io.Writer)
	SetStderr(io.Writer)
	SetStdoutCapture(*[]byte)
}

// aotThrewAddr is the linear-memory address of Emscripten's __THREW__ flag. A
// nonzero value after a top-level export returns means a longjmp unwound all the
// way out — the cooperative-unwind guards internal/wasmpass inserts. (Value for
// PGlite 0.5.x; keep in sync with wasmpass.DefaultConfig().ThrewAddr.)
const aotThrewAddr = 2980948

type A2GRuntime struct {
	mod AOTModule
}

var _ Runtime = (*A2GRuntime)(nil)

// NewA2GRuntime wraps an already-instantiated transpiled module. backend_aot.go
// builds the module via the generated shim's constructor (which wires the
// env/WASI host adapter over the vfs.FS, stdin, and capture buffer) and passes
// it here.
func NewA2GRuntime(ctx context.Context, mod AOTModule) (*A2GRuntime, error) {
	return &A2GRuntime{mod: mod}, nil
}

func aotLanes(args []interface{}) []uint64 {
	out := make([]uint64, len(args))
	for i, a := range args {
		switch v := a.(type) {
		case int32:
			out[i] = uint64(uint32(v))
		case uint32:
			out[i] = uint64(v)
		case int64:
			out[i] = uint64(v)
		default:
			panic(fmt.Sprintf("aot: unsupported export arg type %T", a))
		}
	}
	return out
}

// ApplyDataRelocs runs the module's data relocations and then its C++ static
// constructors — the correct Emscripten order (relocs before ctors), which a
// wasm engine would run at/just after instantiation.
func (rt *A2GRuntime) ApplyDataRelocs(ctx context.Context) error {
	rt.mod.ApplyDataRelocs()
	rt.mod.CallCtors()
	return nil
}

func (rt *A2GRuntime) CallExport(name string, args ...interface{}) (res interface{}, err error) {
	defer func() {
		if r := recover(); r != nil {
			if s, ok := isExit(r); ok {
				res, err = int32(0), fmt.Errorf("%s during %s", s, name)
				return
			}
			panic(r)
		}
	}()
	return int32(rt.mod.Call(name, aotLanes(args))), nil
}

// CallExportRaw calls the export, then reports whether a longjmp unwound out by
// reading (and clearing) __THREW__ — the cooperative equivalent of the engine
// backends detecting a longjmp trap.
func (rt *A2GRuntime) CallExportRaw(name string, args ...interface{}) (result interface{}, longjmp bool, err error) {
	defer func() {
		if r := recover(); r != nil {
			if s, ok := isExit(r); ok {
				result, longjmp, err = int32(0), false, fmt.Errorf("%s during %s", s, name)
				return
			}
			panic(r)
		}
	}()
	res := rt.mod.Call(name, aotLanes(args))
	mem := rt.mod.Memory()
	longjmp = binary.LittleEndian.Uint32(mem[aotThrewAddr:]) != 0
	if longjmp {
		binary.LittleEndian.PutUint32(mem[aotThrewAddr:], 0)   // __THREW__
		binary.LittleEndian.PutUint32(mem[aotThrewAddr+4:], 0) // __threwValue
	}
	return int32(res), longjmp, nil
}

// CallMain marshals argv onto the shadow stack (below __stack_pointer, the way a
// C runtime lays out argv) and calls __main_argc_argv(argc, argvPtr). No malloc
// export is needed; postgres main copies argv early.
// isExit reports whether a recovered panic value is an emscripten exit()/
// proc_exit() (a genuine termination, distinct from the cooperative longjmp).
func isExit(r any) (string, bool) {
	s := fmt.Sprint(r)
	if strings.HasPrefix(s, "exit(") || strings.HasPrefix(s, "proc_exit(") {
		return s, true
	}
	return s, false
}

func (rt *A2GRuntime) CallMain(ctx context.Context, args []string) (code int32, err error) {
	// In PGlite mode main() completes startup then exits/longjmps while the
	// backend stays alive; exit() panics up through the generated frames, so
	// catch it here and report it as a (non-fatal) error, matching the engine
	// backends. A non-exit panic is a real crash and re-propagates.
	defer func() {
		if r := recover(); r != nil {
			if s, ok := isExit(r); ok {
				err = fmt.Errorf("main %s", s)
				return
			}
			panic(r)
		}
	}()
	mem := rt.mod.Memory()
	sp := uint32(rt.mod.SP())

	// String bytes (NUL-terminated), top-down.
	ptrs := make([]uint32, len(args))
	for i := len(args) - 1; i >= 0; i-- {
		b := append([]byte(args[i]), 0)
		sp -= uint32(len(b))
		copy(mem[sp:], b)
		ptrs[i] = sp
	}
	sp &^= 15 // align

	// argv pointer array (argc+1, NUL-terminated).
	sp -= uint32((len(args) + 1) * 4)
	sp &^= 15
	argv := sp
	for i, p := range ptrs {
		binary.LittleEndian.PutUint32(mem[argv+uint32(i*4):], p)
	}
	binary.LittleEndian.PutUint32(mem[argv+uint32(len(args)*4):], 0)

	rt.mod.SetSP(int32(sp))
	res := rt.mod.Call("__main_argc_argv", []uint64{uint64(uint32(len(args))), uint64(argv)})
	return int32(res), nil
}

func (rt *A2GRuntime) RegisterRWCallbacks(cb RWCallbacks) (uint32, error) {
	return rt.mod.RegisterRW(cb.Read, cb.Write), nil
}

func (rt *A2GRuntime) RegisterInitdbCallbacks(cb *InitdbCallbacks) (uint32, error) {
	return rt.mod.RegisterInitdb(cb), nil
}

// FindStdoutFILE locates musl's static __stdout_FILE in guest memory by its
// initialized signature (flags=5, fd=1, buf_size=1024, lock=-1) — the same scan
// the engine backends use, over the []byte directly.
func (rt *A2GRuntime) FindStdoutFILE() uint32 {
	mem := rt.mod.Memory()
	size := uint32(len(mem))
	const (
		offFlags   = 0
		offBufSize = 48
		offFD      = 60
		offLock    = 76
		fileMin    = 84
	)
	u32 := func(p uint32) uint32 { return binary.LittleEndian.Uint32(mem[p : p+4]) }
	i32 := func(p uint32) int32 { return int32(u32(p)) }
	for p := uint32(0); p+fileMin <= size; p += 4 {
		if u32(p+offFlags) == 5 && i32(p+offFD) == 1 &&
			i32(p+offLock) == -1 && i32(p+offBufSize) == 1024 {
			return p
		}
	}
	return 0
}

func (rt *A2GRuntime) SetStdout(w io.Writer)        { rt.mod.SetStdout(w) }
func (rt *A2GRuntime) SetStderr(w io.Writer)        { rt.mod.SetStderr(w) }
func (rt *A2GRuntime) SetStdoutCapture(buf *[]byte) { rt.mod.SetStdoutCapture(buf) }
