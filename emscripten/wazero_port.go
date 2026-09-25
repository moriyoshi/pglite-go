//go:build wazero && !aot

// Package emscripten — wazero backend (pure Go, no CGo; -tags wazero).
//
// WZRuntime is the wazero counterpart of WTRuntime: it instantiates one PGlite
// module wired with the shared host layer (WASI, env, invoke_*/longjmp via
// wazero's built-in emscripten support) and implements the Runtime interface so
// the wire-protocol and initdb code drives it identically to the wasmtime path.
//
// Two mechanisms differ from wasmtime and are worth noting:
//   - Host callbacks (RW hooks, initdb system/popen/pclose) can't be appended to
//     the function table via a runtime API. Instead a tiny generated bridge
//     module imports the shared table and uses a static element segment to place
//     wrapper functions at a reserved base index above the module's own entries
//     (callbackBase, derived from the dylink table size).
//   - A longjmp out of an exported call (PostgresMainLoopOnce in wire mode)
//     surfaces as an error whose message contains "_emscripten_throw_longjmp"
//     (wazero wraps the panic thrown by its built-in _emscripten_throw_longjmp).
package emscripten

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"strings"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"

	"github.com/moriyoshi/pglite-go/vfs"
)

// wazero surfaces a longjmp as this substring (see internal/emscripten).
const wzLongjmpMarker = "_emscripten_throw_longjmp"

// Compile-time check that the wazero backend satisfies the shared interface.
var _ Runtime = (*WZRuntime)(nil)

// WZRuntime holds a wazero runtime with one instantiated PGlite/PostgreSQL
// module and the host layer around it.
type WZRuntime struct {
	ctx  context.Context
	r    wazero.Runtime
	wasi *WASIInstance
	mod  api.Module
	mem  api.Memory
	// callbackBase is the first table index above the module's own indirect
	// functions; nextCB hands out slots from there for callback bridges.
	callbackBase uint32
	nextCB       uint32
}

// NewWZRuntime patches raw wasm and instantiates it. stdin (optional) feeds fd 0;
// capture (optional) redirects fd 1 into a buffer.
func NewWZRuntime(ctx context.Context, cache wazero.CompilationCache, wasmBytes []byte, fs *vfs.FS, stdin []byte, capture *[]byte) (*WZRuntime, error) {
	patched, err := PatchWasmImports(wasmBytes)
	if err != nil {
		return nil, fmt.Errorf("patch imports: %w", err)
	}
	return NewWZRuntimeFromModule(ctx, cache, patched, fs, stdin, capture)
}

// NewWZRuntimeFromModule instantiates already-patched wasm (the wazero "module"
// is the patched byte slice; native code is cached via the shared cache).
func NewWZRuntimeFromModule(ctx context.Context, cache wazero.CompilationCache, patchedWasm []byte, fs *vfs.FS, stdin []byte, capture *[]byte) (*WZRuntime, error) {
	_, tableSize, err := ParseDylink(patchedWasm)
	if err != nil {
		return nil, fmt.Errorf("dylink: %w", err)
	}

	r := wazero.NewRuntimeWithConfig(ctx, wazero.NewRuntimeConfig().WithCompilationCache(cache))
	wasi, err := instantiateWASIFull(ctx, r, fs, stdin, capture)
	if err != nil {
		r.Close(ctx)
		return nil, fmt.Errorf("wasi: %w", err)
	}
	compiled, err := PrepareAndCompilePrepatched(ctx, r, patchedWasm, fs)
	if err != nil {
		r.Close(ctx)
		return nil, fmt.Errorf("prepare: %w", err)
	}
	mod, err := r.InstantiateModule(ctx, compiled, wazero.NewModuleConfig().WithName("pglite"))
	if err != nil {
		r.Close(ctx)
		return nil, fmt.Errorf("instantiate: %w", err)
	}
	base := tableSize + 1 // leave the module's own entries (and slot 0) untouched
	return &WZRuntime{
		ctx: ctx, r: r, wasi: wasi, mod: mod, mem: mod.Memory(),
		callbackBase: base, nextCB: base,
	}, nil
}

// Close releases the runtime.
func (rt *WZRuntime) Close() { rt.r.Close(rt.ctx) }

func encodeArg(a interface{}) uint64 {
	switch v := a.(type) {
	case int32:
		return uint64(uint32(v))
	case uint32:
		return uint64(v)
	case int64:
		return uint64(v)
	case uint64:
		return v
	case int:
		return uint64(uint32(int32(v)))
	case float64:
		return api.EncodeF64(v)
	case float32:
		return api.EncodeF32(v)
	default:
		return 0
	}
}

func (rt *WZRuntime) callRaw(name string, args ...interface{}) ([]uint64, error) {
	f := rt.mod.ExportedFunction(name)
	if f == nil {
		return nil, fmt.Errorf("export %q not found", name)
	}
	stack := make([]uint64, len(args))
	for i, a := range args {
		stack[i] = encodeArg(a)
	}
	return f.Call(rt.ctx, stack...)
}

// CallExport calls an export and returns its first result as int32 (the form the
// wire/initdb callers expect via asI32), or nil if it returns nothing.
func (rt *WZRuntime) CallExport(name string, args ...interface{}) (interface{}, error) {
	res, err := rt.callRaw(name, args...)
	if err != nil {
		return nil, err
	}
	if len(res) > 0 {
		return int32(res[0]), nil
	}
	return nil, nil
}

// CallExportRaw reports whether the call unwound via longjmp (so the caller can
// drive PostgresMainLongJmp) rather than failing.
func (rt *WZRuntime) CallExportRaw(name string, args ...interface{}) (interface{}, bool, error) {
	res, err := rt.callRaw(name, args...)
	if err != nil {
		if strings.Contains(err.Error(), wzLongjmpMarker) {
			return nil, true, nil
		}
		return nil, false, err
	}
	if len(res) > 0 {
		return int32(res[0]), false, nil
	}
	return nil, false, nil
}

// CallMain sets up argv on the Emscripten stack and calls __main_argc_argv. In
// PGlite the entry point typically exits/longjmps rather than returning cleanly,
// so a non-nil error is normal; the caller (runCmd/startWire) interprets it. The
// error string contains "proc_exit(N)" for a normal exit(N).
func (rt *WZRuntime) CallMain(_ context.Context, args []string) (int32, error) {
	main := rt.mod.ExportedFunction("__main_argc_argv")
	stackAlloc := rt.mod.ExportedFunction("_emscripten_stack_alloc")
	if main == nil || stackAlloc == nil {
		return -1, fmt.Errorf("missing __main_argc_argv or _emscripten_stack_alloc export")
	}
	alloc := func(n int) (uint32, error) {
		r, err := stackAlloc.Call(rt.ctx, uint64(n))
		if err != nil {
			return 0, err
		}
		return uint32(r[0]), nil
	}
	argvPtr, err := alloc((len(args) + 1) * 4)
	if err != nil {
		return -1, err
	}
	for i, a := range args {
		p, err := alloc(len(a) + 1)
		if err != nil {
			return -1, err
		}
		rt.mem.Write(p, append([]byte(a), 0))
		rt.mem.WriteUint32Le(argvPtr+uint32(i*4), p)
	}
	rt.mem.WriteUint32Le(argvPtr+uint32(len(args)*4), 0)

	res, err := main.Call(rt.ctx, uint64(len(args)), uint64(argvPtr))
	if err != nil {
		return -1, err
	}
	if len(res) > 0 {
		return int32(res[0]), nil
	}
	return 0, nil
}

// ApplyDataRelocs runs __wasm_apply_data_relocs if the module exports it.
func (rt *WZRuntime) ApplyDataRelocs(_ context.Context) error {
	if f := rt.mod.ExportedFunction("__wasm_apply_data_relocs"); f != nil {
		_, err := f.Call(rt.ctx)
		return err
	}
	return nil
}

func (rt *WZRuntime) SetStdout(w io.Writer)        { rt.wasi.SetStdout(w) }
func (rt *WZRuntime) SetStderr(w io.Writer)        { rt.wasi.SetStderr(w) }
func (rt *WZRuntime) SetStdoutCapture(buf *[]byte) { rt.wasi.SetStdoutCapture(buf) }

// FindStdoutFILE locates musl's static stdout FILE struct by its initialized
// signature (flags=5, buf_size=1024, fd=1, lock=-1). See the wasmtime port.
func (rt *WZRuntime) FindStdoutFILE() uint32 {
	size := rt.mem.Size()
	mem, ok := rt.mem.Read(0, size)
	if !ok {
		return 0
	}
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

// RegisterRWCallbacks installs the PGlite read/write hooks at the next free table
// slots via a generated bridge module, and returns the base index.
func (rt *WZRuntime) RegisterRWCallbacks(cb RWCallbacks) (uint32, error) {
	base := rt.nextCB

	host := rt.r.NewHostModuleBuilder("rw_host")
	host.NewFunctionBuilder().
		WithGoModuleFunction(api.GoModuleFunc(func(ctx context.Context, mod api.Module, stack []uint64) {
			ptr, max := api.DecodeU32(stack[0]), api.DecodeU32(stack[1])
			tmp := make([]byte, max)
			n := cb.Read(tmp)
			if n > 0 {
				mod.Memory().Write(ptr, tmp[:n])
			}
			stack[0] = api.EncodeI32(int32(n))
		}), []api.ValueType{api.ValueTypeI32, api.ValueTypeI32}, []api.ValueType{api.ValueTypeI32}).
		Export("handle_read")
	host.NewFunctionBuilder().
		WithGoModuleFunction(api.GoModuleFunc(func(ctx context.Context, mod api.Module, stack []uint64) {
			ptr, length := api.DecodeU32(stack[0]), api.DecodeU32(stack[1])
			if buf, ok := mod.Memory().Read(ptr, length); ok {
				cb.Write(buf)
			}
			stack[0] = api.EncodeI32(int32(length))
		}), []api.ValueType{api.ValueTypeI32, api.ValueTypeI32}, []api.ValueType{api.ValueTypeI32}).
		Export("handle_write")
	if _, err := host.Instantiate(rt.ctx); err != nil {
		return 0, fmt.Errorf("instantiate rw_host: %w", err)
	}

	bridge := buildRWBridgeModule(base)
	compiled, err := rt.r.CompileModule(rt.ctx, bridge)
	if err != nil {
		return 0, fmt.Errorf("compile rw bridge: %w", err)
	}
	if _, err := rt.r.InstantiateModule(rt.ctx, compiled, wazero.NewModuleConfig().WithName("rw_bridge")); err != nil {
		return 0, fmt.Errorf("instantiate rw bridge: %w", err)
	}
	rt.nextCB += 2
	return base, nil
}

// RegisterInitdbCallbacks installs initdb's system/popen/pclose hooks at the next
// free table slots and returns the base index.
func (rt *WZRuntime) RegisterInitdbCallbacks(cb *InitdbCallbacks) (uint32, error) {
	base := rt.nextCB
	if _, err := instantiateInitdbCallbacksInto(rt.ctx, rt.r, base, cb); err != nil {
		return 0, err
	}
	cb.SetModule(rt.mod)
	rt.nextCB += 3
	return base, nil
}

// buildRWBridgeModule generates a wasm module that imports the two host RW
// functions and the shared table, and places wrapper functions at baseIndex and
// baseIndex+1 via an active element segment.
func buildRWBridgeModule(baseIndex uint32) []byte {
	var buf []byte
	buf = append(buf, 0x00, 0x61, 0x73, 0x6d)
	buf = append(buf, 0x01, 0x00, 0x00, 0x00)

	// Type section: 1 type, (i32, i32) -> (i32)
	typePayload := []byte{1, 0x60, 2, 0x7f, 0x7f, 1, 0x7f}
	buf = append(buf, encodeSection(1, typePayload)...)

	// Import section: 2 host functions + memory + table
	var importPayload []byte
	importPayload = append(importPayload, encodeLEB128U(4)...)
	for _, name := range []string{"handle_read", "handle_write"} {
		importPayload = append(importPayload, encodeName("rw_host")...)
		importPayload = append(importPayload, encodeName(name)...)
		importPayload = append(importPayload, 0x00, 0x00) // func, type 0
	}
	importPayload = append(importPayload, encodeName("env.extras")...)
	importPayload = append(importPayload, encodeName("memory")...)
	importPayload = append(importPayload, 0x02, 0x00, 0x00) // memory, no max, min 0
	importPayload = append(importPayload, encodeName("env.extras")...)
	importPayload = append(importPayload, encodeName("__indirect_function_table")...)
	importPayload = append(importPayload, 0x01, 0x70, 0x00, 0x00) // table, funcref, no max, min 0
	buf = append(buf, encodeSection(2, importPayload)...)

	// Function section: 2 wrapper functions, both type 0
	buf = append(buf, encodeSection(3, []byte{2, 0, 0})...)

	// Element section: place the 2 wrappers at baseIndex
	var elemPayload []byte
	elemPayload = append(elemPayload, encodeLEB128U(1)...) // 1 segment
	elemPayload = append(elemPayload, 0x00)                // active, table 0
	elemPayload = append(elemPayload, 0x41)                // i32.const
	elemPayload = append(elemPayload, encodeLEB128S(int64(baseIndex))...)
	elemPayload = append(elemPayload, 0x0b)                // end
	elemPayload = append(elemPayload, encodeLEB128U(2)...) // 2 func refs
	// Func indices: 2 imported (0,1) + 2 defined (2,3)
	elemPayload = append(elemPayload, encodeLEB128U(2)...) // wrapper_read
	elemPayload = append(elemPayload, encodeLEB128U(3)...) // wrapper_write
	buf = append(buf, encodeSection(9, elemPayload)...)

	// Code section: 2 wrapper bodies that forward to the imported host funcs
	var codePayload []byte
	codePayload = append(codePayload, encodeLEB128U(2)...)
	// wrapper_read: local.get 0, local.get 1, call 0 (handle_read), end
	readBody := []byte{0, 0x20, 0x00, 0x20, 0x01, 0x10, 0x00, 0x0b}
	codePayload = append(codePayload, encodeLEB128U(uint64(len(readBody)))...)
	codePayload = append(codePayload, readBody...)
	// wrapper_write: local.get 0, local.get 1, call 1 (handle_write), end
	writeBody := []byte{0, 0x20, 0x00, 0x20, 0x01, 0x10, 0x01, 0x0b}
	codePayload = append(codePayload, encodeLEB128U(uint64(len(writeBody)))...)
	codePayload = append(codePayload, writeBody...)
	buf = append(buf, encodeSection(10, codePayload)...)

	return buf
}
