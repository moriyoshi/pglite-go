//go:build !wazero

// Package emscripten — wasmtime backend (the default).
//
// This file re-hosts the exact same Go syscall/WASI/runtime closures used by the
// wazero path (all api.GoModuleFunc) on top of wasmtime-go, via small adapters
// that implement wazero's api.Module/api.Memory over wasmtime memory. It also
// re-implements the Emscripten invoke_*/longjmp trampolines (which wazero
// provided built-in) using wasmtime's trap-and-resume behavior.
//
// Built by default; excluded from the pure-Go build via `-tags wazero`.
// Run: go run ./cmd/pglite
package emscripten

import (
	"context"
	"fmt"
	"io"
	"math"
	"os"
	"strings"

	"github.com/bytecodealliance/wasmtime-go/v34"
	"github.com/tetratelabs/wazero/api"

	"github.com/moriyoshi/pglite-go/vfs"
)

const longjmpSentinel = "__emscripten_longjmp__"
const heapBase = defaultHeapBase

// WTRuntime holds a wasmtime instance wired with our host layer.
type WTRuntime struct {
	ctx      context.Context
	engine   *wasmtime.Engine
	store    *wasmtime.Store
	module   *wasmtime.Module
	instance *wasmtime.Instance
	memory   *wasmtime.Memory
	table    *wasmtime.Table
	fs       *vfs.FS
	wasi     *wasiImpl
}

// NewWTRuntime compiles the given (unpatched) pglite.wasm and wires all host
// imports. Convenience wrapper around NewWTRuntimeFromModule that compiles the
// bytes each call — prefer compiling once and reusing the module across
// subcommands (see NewWTRuntimeFromModule).
func NewWTRuntime(ctx context.Context, engine *wasmtime.Engine, wasmBytes []byte, fs *vfs.FS, stdinData []byte, stdoutCapture *[]byte) (*WTRuntime, error) {
	mod, err := wasmtime.NewModule(engine, wasmBytes)
	if err != nil {
		return nil, fmt.Errorf("compile: %w", err)
	}
	return NewWTRuntimeFromModule(ctx, engine, mod, fs, stdinData, stdoutCapture)
}

// NewWTRuntimeFromModule wires host imports for an already-compiled module and
// instantiates it. A wasmtime.Module can be instantiated into many stores, so
// compiling once and reusing it across subcommands avoids recompiling.
func NewWTRuntimeFromModule(ctx context.Context, engine *wasmtime.Engine, mod *wasmtime.Module, fs *vfs.FS, stdinData []byte, stdoutCapture *[]byte) (*WTRuntime, error) {
	rt := &WTRuntime{ctx: ctx, engine: engine, fs: fs}
	store := wasmtime.NewStore(engine)
	rt.store = store
	rt.module = mod

	// Derive the imported memory and table types from the module itself
	// (pglite.wasm and initdb.wasm differ in min sizes).
	memType := wasmtime.NewMemoryType(2048, true, 32768, false)
	tableMin := uint32(6097)
	for _, imp := range mod.Imports() {
		if mt := imp.Type().MemoryType(); mt != nil {
			hasMax, max := mt.Maximum()
			memType = wasmtime.NewMemoryType(uint32(mt.Minimum()), hasMax, uint32(max), false)
		}
		if tt := imp.Type().TableType(); tt != nil {
			tableMin = tt.Minimum()
		}
	}
	memory, err := wasmtime.NewMemory(store, memType)
	if err != nil {
		return nil, fmt.Errorf("memory: %w", err)
	}
	rt.memory = memory
	table, err := wasmtime.NewTable(store, wasmtime.NewTableType(wasmtime.NewValType(wasmtime.KindFuncref), tableMin, false, 0), wasmtime.ValFuncref(nil))
	if err != nil {
		return nil, fmt.Errorf("table: %w", err)
	}
	rt.table = table

	// Globals.
	i32 := wasmtime.NewValType(wasmtime.KindI32)
	mkGlobal := func(mutable bool, v int32) *wasmtime.Global {
		g, gerr := wasmtime.NewGlobal(store, wasmtime.NewGlobalType(i32, mutable), wasmtime.ValI32(v))
		if gerr != nil {
			panic(gerr)
		}
		return g
	}
	globals := map[string]*wasmtime.Global{
		"__memory_base":   mkGlobal(false, defaultMemoryBase),
		"__stack_pointer": mkGlobal(true, defaultStackPointer),
		"__table_base":    mkGlobal(false, defaultTableBase),
		"__heap_base":     mkGlobal(true, heapBase),
	}

	// The named GoModuleFunc implementations, reused verbatim from the wazero path.
	syscalls := NewSyscallHandler(fs).Register()
	rt.wasi = newWASIImpl(fs, stdinData, stdoutCapture)
	wasiFuncs := rt.wasi.goModuleFuncs()

	// Build externs in the module's exact import order.
	externs := make([]wasmtime.AsExtern, 0, len(mod.Imports()))
	for _, imp := range mod.Imports() {
		name := ""
		if imp.Name() != nil {
			name = *imp.Name()
		}
		ty := imp.Type()
		switch {
		case ty.MemoryType() != nil:
			externs = append(externs, memory)
		case ty.TableType() != nil:
			externs = append(externs, table)
		case ty.GlobalType() != nil:
			g, ok := globals[name]
			if !ok {
				return nil, fmt.Errorf("unknown global import %q", name)
			}
			externs = append(externs, g)
		case ty.FuncType() != nil:
			ft := ty.FuncType()
			externs = append(externs, rt.hostFunc(imp.Module(), name, ft, syscalls, wasiFuncs))
		default:
			return nil, fmt.Errorf("unhandled import kind for %q", name)
		}
	}

	inst, err := wasmtime.NewInstance(store, mod, externs)
	if err != nil {
		return nil, fmt.Errorf("instantiate: %w", err)
	}
	rt.instance = inst
	return rt, nil
}

// hostFunc resolves a single func import to a wasmtime.Func.
func (rt *WTRuntime) hostFunc(module, name string, ft *wasmtime.FuncType, syscalls, wasiFuncs map[string]api.GoModuleFunc) *wasmtime.Func {
	// Emscripten invoke_* trampolines.
	if strings.HasPrefix(name, "invoke_") {
		return rt.makeInvoke(ft)
	}
	// longjmp: return a distinctively-messaged trap that invoke_* catches.
	if name == "_emscripten_throw_longjmp" {
		return wasmtime.NewFunc(rt.store, ft, func(_ *wasmtime.Caller, _ []wasmtime.Val) ([]wasmtime.Val, *wasmtime.Trap) {
			return nil, wasmtime.NewTrap(longjmpSentinel)
		})
	}
	if name == "emscripten_notify_memory_growth" {
		return wasmtime.NewFunc(rt.store, ft, func(_ *wasmtime.Caller, _ []wasmtime.Val) ([]wasmtime.Val, *wasmtime.Trap) {
			return nil, nil
		})
	}
	// Resolve the named GoModuleFunc: WASI, syscall, or Emscripten runtime.
	var goFn api.GoModuleFunc
	if module == "wasi_snapshot_preview1" {
		goFn = wasiFuncs[name]
	} else if impl, ok := syscalls[name]; ok {
		goFn = impl
	} else {
		goFn = createHostFunc(name, funcSig{params: valTypes(ft.Params()), results: valTypes(ft.Results())})
	}
	if goFn == nil {
		goFn = makeStub(name, funcSig{params: valTypes(ft.Params()), results: valTypes(ft.Results())})
	}
	return rt.bridge(ft, goFn)
}

// bridge wraps a wazero GoModuleFunc as a wasmtime.Func, matching wazero's
// stack calling convention (params in, results overwrite from index 0).
func (rt *WTRuntime) bridge(ft *wasmtime.FuncType, goFn api.GoModuleFunc) *wasmtime.Func {
	nparams := len(ft.Params())
	results := ft.Results()
	stackSize := nparams
	if len(results) > stackSize {
		stackSize = len(results)
	}
	if stackSize == 0 {
		stackSize = 1
	}
	return wasmtime.NewFunc(rt.store, ft, func(caller *wasmtime.Caller, args []wasmtime.Val) (out []wasmtime.Val, trap *wasmtime.Trap) {
		stack := make([]uint64, stackSize)
		for i, a := range args {
			stack[i] = valToStack(a)
		}
		mod := &wtModule{rt: rt, caller: caller}
		defer func() {
			if r := recover(); r != nil {
				trap = wasmtime.NewTrap(fmt.Sprintf("%v", r))
			}
		}()
		goFn(rt.ctx, mod, stack)
		out = make([]wasmtime.Val, len(results))
		for i := range results {
			out[i] = stackToVal(results[i].Kind(), stack[i])
		}
		return out, nil
	})
}

// makeInvoke implements invoke_XX(index, args...) with longjmp handling.
func (rt *WTRuntime) makeInvoke(ft *wasmtime.FuncType) *wasmtime.Func {
	return wasmtime.NewFunc(rt.store, ft, func(caller *wasmtime.Caller, args []wasmtime.Val) ([]wasmtime.Val, *wasmtime.Trap) {
		idx := args[0].I32()
		callArgs := make([]interface{}, len(args)-1)
		for i, a := range args[1:] {
			callArgs[i] = a.Get()
		}

		// sp = emscripten_stack_get_current()
		sp, trap := callExportI32(caller, "emscripten_stack_get_current")
		if trap != nil {
			return nil, trap
		}

		fnVal, err := rt.table.Get(rt.store, uint64(uint32(idx)))
		if err != nil {
			return nil, wasmtime.NewTrap(fmt.Sprintf("invoke: bad table index %d: %v", idx, err))
		}
		fn := fnVal.Funcref()
		if fn == nil {
			return nil, wasmtime.NewTrap(fmt.Sprintf("invoke: nil funcref at %d", idx))
		}

		res, err := fn.Call(rt.store, callArgs...)
		if err != nil {
			// On longjmp: restore stack + setThrew(1,0), swallow. Else propagate.
			if t, ok := err.(*wasmtime.Trap); ok && strings.Contains(t.Message(), longjmpSentinel) {
				_, _ = callExport(caller, "_emscripten_stack_restore", sp)
				_, _ = callExport(caller, "setThrew", int32(1), int32(0))
				return zeroResults(ft), nil
			}
			if t, ok := err.(*wasmtime.Trap); ok {
				return nil, t
			}
			return nil, wasmtime.NewTrap(err.Error())
		}
		return packResults(ft, res), nil
	})
}

// --- wazero api.Module / api.Memory adapters over wasmtime ---

// wtModule embeds api.Module (nil) so wazero's sealing marker method is
// promoted; the methods we actually use are overridden below.
type wtModule struct {
	api.Module
	rt     *WTRuntime
	caller *wasmtime.Caller
}

// storelike returns the caller when inside a host call, else the runtime store.
func (m *wtModule) storelike() wasmtime.Storelike {
	if m.caller != nil {
		return m.caller
	}
	return m.rt.store
}

func (m *wtModule) Memory() api.Memory {
	if m.caller != nil {
		if ext := m.caller.GetExport("memory"); ext != nil {
			return &wtMemory{mem: ext.Memory(), store: m.caller}
		}
	}
	return &wtMemory{mem: m.rt.memory, store: m.storelike()}
}

func (m *wtModule) ExportedFunction(name string) api.Function {
	var f *wasmtime.Func
	if m.caller != nil {
		if ext := m.caller.GetExport(name); ext != nil {
			f = ext.Func()
		}
	} else {
		f = m.rt.instance.GetFunc(m.rt.store, name)
	}
	if f == nil {
		return nil
	}
	return &wtFunction{fn: f, store: m.storelike(), name: name}
}

func (m *wtModule) Name() string { return "pglite" }

type wtFunction struct {
	api.Function
	fn    *wasmtime.Func
	store wasmtime.Storelike
	name  string
}

func (f *wtFunction) Definition() api.FunctionDefinition { return wtFuncDef{name: f.name} }

func (f *wtFunction) Call(_ context.Context, params ...uint64) ([]uint64, error) {
	ft := f.fn.Type(f.store)
	args := make([]interface{}, len(params))
	for i, p := range params {
		args[i] = stackToVal(ft.Params()[i].Kind(), p).Get()
	}
	res, err := f.fn.Call(f.store, args...)
	if err != nil {
		return nil, err
	}
	return ifaceResultsToStack(ft, res), nil
}

func (f *wtFunction) CallWithStack(_ context.Context, stack []uint64) error {
	ft := f.fn.Type(f.store)
	args := make([]interface{}, len(ft.Params()))
	for i := range ft.Params() {
		args[i] = stackToVal(ft.Params()[i].Kind(), stack[i]).Get()
	}
	res, err := f.fn.Call(f.store, args...)
	if err != nil {
		return err
	}
	out := ifaceResultsToStack(ft, res)
	copy(stack, out)
	return nil
}

type wtFuncDef struct {
	api.FunctionDefinition
	name string
}

func (d wtFuncDef) Name() string { return d.name }

type wtMemory struct {
	api.Memory
	mem   *wasmtime.Memory
	store wasmtime.Storelike
}

func (m *wtMemory) data() []byte { return m.mem.UnsafeData(m.store) }

func (m *wtMemory) Size() uint32 { return uint32(len(m.data())) }

func (m *wtMemory) Grow(deltaPages uint32) (uint32, bool) {
	prev, err := m.mem.Grow(m.store, uint64(deltaPages))
	if err != nil {
		return 0, false
	}
	return uint32(prev), true
}

func (m *wtMemory) ReadByte(off uint32) (byte, bool) {
	d := m.data()
	if off >= uint32(len(d)) {
		return 0, false
	}
	return d[off], true
}

func (m *wtMemory) Read(off, n uint32) ([]byte, bool) {
	d := m.data()
	if off+n > uint32(len(d)) {
		return nil, false
	}
	return d[off : off+n], true
}

func (m *wtMemory) Write(off uint32, v []byte) bool {
	d := m.data()
	if off+uint32(len(v)) > uint32(len(d)) {
		return false
	}
	copy(d[off:], v)
	return true
}

func (m *wtMemory) WriteByte(off uint32, v byte) bool {
	d := m.data()
	if off >= uint32(len(d)) {
		return false
	}
	d[off] = v
	return true
}

func (m *wtMemory) WriteString(off uint32, v string) bool { return m.Write(off, []byte(v)) }

func (m *wtMemory) ReadUint16Le(off uint32) (uint16, bool) {
	b, ok := m.Read(off, 2)
	if !ok {
		return 0, false
	}
	return uint16(b[0]) | uint16(b[1])<<8, true
}
func (m *wtMemory) ReadUint32Le(off uint32) (uint32, bool) {
	b, ok := m.Read(off, 4)
	if !ok {
		return 0, false
	}
	return uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16 | uint32(b[3])<<24, true
}
func (m *wtMemory) ReadUint64Le(off uint32) (uint64, bool) {
	b, ok := m.Read(off, 8)
	if !ok {
		return 0, false
	}
	var v uint64
	for i := 0; i < 8; i++ {
		v |= uint64(b[i]) << (8 * i)
	}
	return v, true
}
func (m *wtMemory) ReadFloat32Le(off uint32) (float32, bool) {
	v, ok := m.ReadUint32Le(off)
	return math.Float32frombits(v), ok
}
func (m *wtMemory) ReadFloat64Le(off uint32) (float64, bool) {
	v, ok := m.ReadUint64Le(off)
	return math.Float64frombits(v), ok
}
func (m *wtMemory) WriteUint16Le(off uint32, v uint16) bool {
	return m.Write(off, []byte{byte(v), byte(v >> 8)})
}
func (m *wtMemory) WriteUint32Le(off, v uint32) bool {
	return m.Write(off, []byte{byte(v), byte(v >> 8), byte(v >> 16), byte(v >> 24)})
}
func (m *wtMemory) WriteUint64Le(off uint32, v uint64) bool {
	b := make([]byte, 8)
	for i := 0; i < 8; i++ {
		b[i] = byte(v >> (8 * i))
	}
	return m.Write(off, b)
}
func (m *wtMemory) WriteFloat32Le(off uint32, v float32) bool {
	return m.WriteUint32Le(off, math.Float32bits(v))
}
func (m *wtMemory) WriteFloat64Le(off uint32, v float64) bool {
	return m.WriteUint64Le(off, math.Float64bits(v))
}
func (m *wtMemory) Definition() api.MemoryDefinition { return nil }

// --- helpers ---

func valToStack(v wasmtime.Val) uint64 {
	switch v.Kind() {
	case wasmtime.KindI32:
		return uint64(uint32(v.I32()))
	case wasmtime.KindI64:
		return uint64(v.I64())
	case wasmtime.KindF32:
		return uint64(math.Float32bits(v.F32()))
	case wasmtime.KindF64:
		return math.Float64bits(v.F64())
	}
	return 0
}

func stackToVal(kind wasmtime.ValKind, s uint64) wasmtime.Val {
	switch kind {
	case wasmtime.KindI32:
		return wasmtime.ValI32(int32(uint32(s)))
	case wasmtime.KindI64:
		return wasmtime.ValI64(int64(s))
	case wasmtime.KindF32:
		return wasmtime.ValF32(math.Float32frombits(uint32(s)))
	case wasmtime.KindF64:
		return wasmtime.ValF64(math.Float64frombits(s))
	}
	return wasmtime.ValI32(0)
}

func valTypes(ts []*wasmtime.ValType) []api.ValueType {
	out := make([]api.ValueType, len(ts))
	for i, t := range ts {
		switch t.Kind() {
		case wasmtime.KindI64:
			out[i] = api.ValueTypeI64
		case wasmtime.KindF32:
			out[i] = api.ValueTypeF32
		case wasmtime.KindF64:
			out[i] = api.ValueTypeF64
		default:
			out[i] = api.ValueTypeI32
		}
	}
	return out
}

func zeroResults(ft *wasmtime.FuncType) []wasmtime.Val {
	out := make([]wasmtime.Val, len(ft.Results()))
	for i, r := range ft.Results() {
		out[i] = stackToVal(r.Kind(), 0)
	}
	return out
}

func packResults(ft *wasmtime.FuncType, res interface{}) []wasmtime.Val {
	results := ft.Results()
	out := make([]wasmtime.Val, len(results))
	switch len(results) {
	case 0:
		return out
	case 1:
		out[0] = ifaceToVal(results[0].Kind(), res)
	default:
		vals, _ := res.([]wasmtime.Val)
		for i := range results {
			if i < len(vals) {
				out[i] = vals[i]
			} else {
				out[i] = stackToVal(results[i].Kind(), 0)
			}
		}
	}
	return out
}

func ifaceToVal(kind wasmtime.ValKind, v interface{}) wasmtime.Val {
	switch x := v.(type) {
	case int32:
		return wasmtime.ValI32(x)
	case int64:
		return wasmtime.ValI64(x)
	case float32:
		return wasmtime.ValF32(x)
	case float64:
		return wasmtime.ValF64(x)
	case wasmtime.Val:
		return x
	}
	return stackToVal(kind, 0)
}

func ifaceResultsToStack(ft *wasmtime.FuncType, res interface{}) []uint64 {
	results := ft.Results()
	out := make([]uint64, len(results))
	switch len(results) {
	case 0:
	case 1:
		out[0] = valToStack(ifaceToVal(results[0].Kind(), res))
	default:
		if vals, ok := res.([]wasmtime.Val); ok {
			for i := range results {
				if i < len(vals) {
					out[i] = valToStack(vals[i])
				}
			}
		}
	}
	return out
}

func callExport(caller *wasmtime.Caller, name string, args ...interface{}) (interface{}, *wasmtime.Trap) {
	ext := caller.GetExport(name)
	if ext == nil || ext.Func() == nil {
		return nil, wasmtime.NewTrap(name + " not exported")
	}
	res, err := ext.Func().Call(caller, args...)
	if err != nil {
		if t, ok := err.(*wasmtime.Trap); ok {
			return nil, t
		}
		return nil, wasmtime.NewTrap(err.Error())
	}
	return res, nil
}

// ModuleAdapter returns an instance-backed api.Module for use by Go-side
// orchestration (e.g. InitdbCallbacks) outside of host callbacks.
func (rt *WTRuntime) ModuleAdapter() api.Module { return &wtModule{rt: rt} }

// CallExport calls an exported function by name with the given Go-typed args
// (int32/int64/float64) and returns wasmtime's raw result (nil / value / []Val).
func (rt *WTRuntime) CallExport(name string, args ...interface{}) (interface{}, error) {
	f := rt.instance.GetFunc(rt.store, name)
	if f == nil {
		return nil, fmt.Errorf("export %q not found", name)
	}
	return f.Call(rt.store, args...)
}

// RegisterInitdbCallbacks appends the system/popen/pclose callbacks to the
// shared table (growing it so they can't collide with initdb's own functions)
// and points cb.memory/Module at this instance. Returns the base table index;
// initdb is told to use them via pgl_set_system_fn(base+0), etc.
func (rt *WTRuntime) RegisterInitdbCallbacks(cb *InitdbCallbacks) (uint32, error) {
	cb.SetModule(rt.ModuleAdapter())

	base := uint32(rt.table.Size(rt.store))
	if _, err := rt.table.Grow(rt.store, 3, wasmtime.ValFuncref(nil)); err != nil {
		return 0, fmt.Errorf("grow table: %w", err)
	}

	readStr := func(caller *wasmtime.Caller, ptr int32) string {
		mem := rt.memory
		if e := caller.GetExport("memory"); e != nil {
			mem = e.Memory()
		}
		data := mem.UnsafeData(caller)
		var b []byte
		for p := uint32(uint32(ptr)); p < uint32(len(data)) && data[p] != 0; p++ {
			b = append(b, data[p])
		}
		return string(b)
	}

	i32 := wasmtime.NewValType(wasmtime.KindI32)
	ft1 := wasmtime.NewFuncType([]*wasmtime.ValType{i32}, []*wasmtime.ValType{i32})
	ft2 := wasmtime.NewFuncType([]*wasmtime.ValType{i32, i32}, []*wasmtime.ValType{i32})

	systemFn := wasmtime.NewFunc(rt.store, ft1, func(caller *wasmtime.Caller, args []wasmtime.Val) ([]wasmtime.Val, *wasmtime.Trap) {
		ret := int32(-1)
		if cb.OnSystem != nil {
			ret = cb.OnSystem(rt.ctx, readStr(caller, args[0].I32()))
		}
		return []wasmtime.Val{wasmtime.ValI32(ret)}, nil
	})
	popenFn := wasmtime.NewFunc(rt.store, ft2, func(caller *wasmtime.Caller, args []wasmtime.Val) ([]wasmtime.Val, *wasmtime.Trap) {
		ret := int32(0)
		if cb.OnPopen != nil {
			ret = cb.OnPopen(rt.ctx, readStr(caller, args[0].I32()), readStr(caller, args[1].I32()))
		}
		return []wasmtime.Val{wasmtime.ValI32(ret)}, nil
	})
	pcloseFn := wasmtime.NewFunc(rt.store, ft1, func(_ *wasmtime.Caller, args []wasmtime.Val) ([]wasmtime.Val, *wasmtime.Trap) {
		ret := int32(0)
		if cb.OnPclose != nil {
			ret = cb.OnPclose(rt.ctx, args[0].I32())
		}
		return []wasmtime.Val{wasmtime.ValI32(ret)}, nil
	})

	for i, fn := range []*wasmtime.Func{systemFn, popenFn, pcloseFn} {
		if err := rt.table.Set(rt.store, uint64(base)+uint64(i), wasmtime.ValFuncref(fn)); err != nil {
			return 0, fmt.Errorf("table.Set %d: %w", base+uint32(i), err)
		}
	}
	return base, nil
}

// RWCallbacks are the PGlite socket read/write hooks. Read fills dst with
// client→server bytes (non-blocking; returns the count, possibly 0). Write
// receives a copy of server→client bytes.
type RWCallbacks struct {
	Read  func(dst []byte) int
	Write func(src []byte)
}

// RegisterRWCallbacks places the read/write callbacks into the shared table and
// returns the base index; pass base+0 (read) and base+1 (write) to
// pgl_set_rw_cbs. The callbacks operate directly on guest memory.
func (rt *WTRuntime) RegisterRWCallbacks(cb RWCallbacks) (uint32, error) {
	base := uint32(rt.table.Size(rt.store))
	if _, err := rt.table.Grow(rt.store, 2, wasmtime.ValFuncref(nil)); err != nil {
		return 0, fmt.Errorf("grow table: %w", err)
	}
	i32 := wasmtime.NewValType(wasmtime.KindI32)
	ft := wasmtime.NewFuncType([]*wasmtime.ValType{i32, i32}, []*wasmtime.ValType{i32})

	mem := func(caller *wasmtime.Caller) []byte {
		if e := caller.GetExport("memory"); e != nil {
			return e.Memory().UnsafeData(caller)
		}
		return rt.memory.UnsafeData(caller)
	}
	readFn := wasmtime.NewFunc(rt.store, ft, func(caller *wasmtime.Caller, args []wasmtime.Val) ([]wasmtime.Val, *wasmtime.Trap) {
		ptr, max := uint32(args[0].I32()), uint32(args[1].I32())
		data := mem(caller)
		n := 0
		if int(ptr) <= len(data) {
			end := ptr + max
			if end > uint32(len(data)) {
				end = uint32(len(data))
			}
			n = cb.Read(data[ptr:end])
		}
		return []wasmtime.Val{wasmtime.ValI32(int32(n))}, nil
	})
	writeFn := wasmtime.NewFunc(rt.store, ft, func(caller *wasmtime.Caller, args []wasmtime.Val) ([]wasmtime.Val, *wasmtime.Trap) {
		ptr, length := uint32(args[0].I32()), uint32(args[1].I32())
		data := mem(caller)
		if int(ptr)+int(length) <= len(data) {
			cb.Write(data[ptr : ptr+length])
		}
		return []wasmtime.Val{wasmtime.ValI32(int32(length))}, nil
	})
	for i, fn := range []*wasmtime.Func{readFn, writeFn} {
		if err := rt.table.Set(rt.store, uint64(base)+uint64(i), wasmtime.ValFuncref(fn)); err != nil {
			return 0, fmt.Errorf("table.Set %d: %w", base+uint32(i), err)
		}
	}
	return base, nil
}

// CallExportRaw calls an exported function and reports whether it trapped with
// the Emscripten longjmp sentinel (so the caller can invoke PostgresMainLongJmp
// and continue), distinguishing that from a real error.
func (rt *WTRuntime) CallExportRaw(name string, args ...interface{}) (res interface{}, longjmp bool, err error) {
	f := rt.instance.GetFunc(rt.store, name)
	if f == nil {
		return nil, false, fmt.Errorf("export %q not found", name)
	}
	res, err = f.Call(rt.store, args...)
	if err != nil {
		if t, ok := err.(*wasmtime.Trap); ok && strings.Contains(t.Message(), longjmpSentinel) {
			return nil, true, nil
		}
	}
	return res, false, err
}

// ApplyDataRelocs runs __wasm_apply_data_relocs if the module exports it.
func (rt *WTRuntime) ApplyDataRelocs(_ context.Context) error {
	f := rt.instance.GetFunc(rt.store, "__wasm_apply_data_relocs")
	if f == nil {
		return nil
	}
	_, err := f.Call(rt.store)
	return err
}

// CallMain sets up argv on the Emscripten stack and calls __main_argc_argv.
func (rt *WTRuntime) CallMain(_ context.Context, args []string) (int32, error) {
	main := rt.instance.GetFunc(rt.store, "__main_argc_argv")
	stackAlloc := rt.instance.GetFunc(rt.store, "_emscripten_stack_alloc")
	if main == nil || stackAlloc == nil {
		return -1, fmt.Errorf("missing __main_argc_argv or _emscripten_stack_alloc export")
	}

	alloc := func(n int) (uint32, error) {
		r, err := stackAlloc.Call(rt.store, int32(n))
		if err != nil {
			return 0, err
		}
		return uint32(r.(int32)), nil
	}

	argvPtr, err := alloc((len(args) + 1) * 4)
	if err != nil {
		return -1, err
	}
	strPtrs := make([]uint32, len(args))
	for i, a := range args {
		p, err := alloc(len(a) + 1)
		if err != nil {
			return -1, err
		}
		strPtrs[i] = p
	}

	mem := rt.memory.UnsafeData(rt.store)
	putU32 := func(off, v uint32) {
		mem[off] = byte(v)
		mem[off+1] = byte(v >> 8)
		mem[off+2] = byte(v >> 16)
		mem[off+3] = byte(v >> 24)
	}
	for i, a := range args {
		copy(mem[strPtrs[i]:], append([]byte(a), 0))
		putU32(argvPtr+uint32(i*4), strPtrs[i])
	}
	putU32(argvPtr+uint32(len(args)*4), 0)

	res, err := main.Call(rt.store, int32(len(args)), int32(argvPtr))
	if err != nil {
		return -1, err
	}
	return res.(int32), nil
}

// newWASIImpl builds the same wasiImpl the wazero path uses.
func newWASIImpl(fs *vfs.FS, stdinData []byte, stdoutCapture *[]byte) *wasiImpl {
	stdout := io.Writer(os.Stdout)
	if stdoutCapture != nil {
		stdout = &bufWriter{buf: stdoutCapture}
	}
	return &wasiImpl{fs: fs, stdout: stdout, stderr: os.Stderr, stdinData: stdinData, StdoutOverride: stdoutCapture}
}

// SetStdoutCapture toggles dynamic stdout capture on the running instance.
func (rt *WTRuntime) SetStdoutCapture(buf *[]byte) { rt.wasi.StdoutOverride = buf }

// SetStdout redirects fd 1 to w. Set before CallMain.
func (rt *WTRuntime) SetStdout(w io.Writer) { rt.wasi.stdout = w }

// SetStderr redirects fd 2 to w. Set before CallMain.
func (rt *WTRuntime) SetStderr(w io.Writer) { rt.wasi.stderr = w }

// goModuleFuncs returns the 13 WASI functions by import name.
func (w *wasiImpl) goModuleFuncs() map[string]api.GoModuleFunc {
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

func callExportI32(caller *wasmtime.Caller, name string) (int32, *wasmtime.Trap) {
	res, trap := callExport(caller, name)
	if trap != nil {
		return 0, trap
	}
	if v, ok := res.(int32); ok {
		return v, nil
	}
	return 0, nil
}
