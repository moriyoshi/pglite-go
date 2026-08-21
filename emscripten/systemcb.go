package emscripten

import (
	"context"
	"fmt"
	"strings"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
)

// InitdbCallbacks holds the state for initdb's system/popen/pclose callbacks.
type InitdbCallbacks struct {
	// OnSystem is called for system() calls. Returns exit code.
	OnSystem func(ctx context.Context, cmd string) int32
	// OnPopen is called for popen() calls. Returns a FILE pointer.
	OnPopen func(ctx context.Context, cmd string, mode string) int32
	// OnPclose is called for pclose() calls. Returns exit code.
	OnPclose func(ctx context.Context, stream int32) int32
	// memory reference (set after module instantiation)
	memory api.Memory
	// Module reference for calling fopen/fclose on the initdb module
	Module api.Module
}

// SetModule sets the module reference for memory access and fopen calls.
func (h *InitdbCallbacks) SetModule(mod api.Module) {
	h.Module = mod
	h.memory = mod.Memory()
}

// Fopen calls the initdb module's exported fopen function to create a FILE*.
func (h *InitdbCallbacks) Fopen(ctx context.Context, path, mode string) int32 {
	if h.Module == nil {
		return 0
	}
	fopenFn := h.Module.ExportedFunction("fopen")
	stackAlloc := h.Module.ExportedFunction("_emscripten_stack_alloc")
	if fopenFn == nil || stackAlloc == nil {
		return 0
	}

	// Allocate strings on the WASM stack
	pathR, _ := stackAlloc.Call(ctx, uint64(len(path)+1))
	h.memory.Write(uint32(pathR[0]), append([]byte(path), 0))
	modeR, _ := stackAlloc.Call(ctx, uint64(len(mode)+1))
	h.memory.Write(uint32(modeR[0]), append([]byte(mode), 0))

	results, err := fopenFn.Call(ctx, pathR[0], modeR[0])
	if err != nil || len(results) == 0 {
		return 0
	}
	return int32(results[0])
}

// Fclose calls the initdb module's exported fclose function.
func (h *InitdbCallbacks) Fclose(ctx context.Context, fp int32) int32 {
	if h.Module == nil {
		return -1
	}
	fcloseFn := h.Module.ExportedFunction("fclose")
	if fcloseFn == nil {
		return -1
	}
	results, err := fcloseFn.Call(ctx, uint64(fp))
	if err != nil {
		return -1
	}
	return int32(results[0])
}

func (h *InitdbCallbacks) readCString(ptr uint32) string {
	if h.memory == nil {
		return ""
	}
	var buf []byte
	for {
		b, ok := h.memory.ReadByte(ptr)
		if !ok || b == 0 {
			break
		}
		buf = append(buf, b)
		ptr++
	}
	return string(buf)
}

// buildCallbackBridgeModule creates a WASM module with THREE callback functions
// placed in the shared table at consecutive indices starting at baseIndex.
// - baseIndex+0: system callback (i32) -> (i32)
// - baseIndex+1: popen callback (i32, i32) -> (i32)
// - baseIndex+2: pclose callback (i32) -> (i32)
func buildCallbackBridgeModule(baseIndex uint32) []byte {
	var buf []byte
	buf = append(buf, 0x00, 0x61, 0x73, 0x6d)
	buf = append(buf, 0x01, 0x00, 0x00, 0x00)

	// Type section: 2 types
	// type[0]: (i32) -> (i32) - for system and pclose
	// type[1]: (i32, i32) -> (i32) - for popen
	var typePayload []byte
	typePayload = append(typePayload, 2) // 2 types
	// type[0]: (i32) -> (i32)
	typePayload = append(typePayload, 0x60, 1, 0x7f, 1, 0x7f)
	// type[1]: (i32, i32) -> (i32)
	typePayload = append(typePayload, 0x60, 2, 0x7f, 0x7f, 1, 0x7f)
	buf = append(buf, encodeSection(1, typePayload)...)

	// Import section: 3 host functions + memory + table
	var importPayload []byte
	importPayload = append(importPayload, encodeLEB128U(5)...)
	// Import host functions
	for _, f := range []struct {
		name    string
		typeIdx byte
	}{
		{"handle_system", 0},
		{"handle_popen", 1},
		{"handle_pclose", 0},
	} {
		importPayload = append(importPayload, encodeName("initdb_host")...)
		importPayload = append(importPayload, encodeName(f.name)...)
		importPayload = append(importPayload, 0x00, f.typeIdx) // func, type index
	}
	// Import memory
	importPayload = append(importPayload, encodeName("env.extras")...)
	importPayload = append(importPayload, encodeName("memory")...)
	importPayload = append(importPayload, 0x02, 0x00, 0x00) // memory, no max, min 0
	// Import table
	importPayload = append(importPayload, encodeName("env.extras")...)
	importPayload = append(importPayload, encodeName("__indirect_function_table")...)
	importPayload = append(importPayload, 0x01, 0x70, 0x00, 0x00) // table, funcref, no max, min 0
	buf = append(buf, encodeSection(2, importPayload)...)

	// Function section: 3 wrapper functions
	buf = append(buf, encodeSection(3, []byte{3, 0, 1, 0})...) // 3 funcs: type0, type1, type0

	// Element section: place 3 functions at baseIndex
	var elemPayload []byte
	elemPayload = append(elemPayload, encodeLEB128U(1)...) // 1 segment
	elemPayload = append(elemPayload, 0x00)                // active, table 0
	elemPayload = append(elemPayload, 0x41)                // i32.const
	elemPayload = append(elemPayload, encodeLEB128S(int64(baseIndex))...)
	elemPayload = append(elemPayload, 0x0b)                // end
	elemPayload = append(elemPayload, encodeLEB128U(3)...) // 3 function refs
	// Func indices: 3 imported + 3 defined = indices 3, 4, 5
	elemPayload = append(elemPayload, encodeLEB128U(3)...) // wrapper_system
	elemPayload = append(elemPayload, encodeLEB128U(4)...) // wrapper_popen
	elemPayload = append(elemPayload, encodeLEB128U(5)...) // wrapper_pclose
	buf = append(buf, encodeSection(9, elemPayload)...)

	// Code section: 3 wrapper function bodies
	var codePayload []byte
	codePayload = append(codePayload, encodeLEB128U(3)...)
	// wrapper_system: call handle_system(arg0)
	body0 := []byte{0, 0x20, 0x00, 0x10, 0x00, 0x0b} // 0 locals, local.get 0, call 0, end
	codePayload = append(codePayload, encodeLEB128U(uint64(len(body0)))...)
	codePayload = append(codePayload, body0...)
	// wrapper_popen: call handle_popen(arg0, arg1)
	body1 := []byte{0, 0x20, 0x00, 0x20, 0x01, 0x10, 0x01, 0x0b} // local.get 0, local.get 1, call 1, end
	codePayload = append(codePayload, encodeLEB128U(uint64(len(body1)))...)
	codePayload = append(codePayload, body1...)
	// wrapper_pclose: call handle_pclose(arg0)
	body2 := []byte{0, 0x20, 0x00, 0x10, 0x02, 0x0b} // local.get 0, call 2, end
	codePayload = append(codePayload, encodeLEB128U(uint64(len(body2)))...)
	codePayload = append(codePayload, body2...)
	buf = append(buf, encodeSection(10, codePayload)...)

	return buf
}

// InstantiateInitdbCallbacks sets up system/popen/pclose callbacks for initdb.
// Returns the callback handler and the base table index.
func InstantiateInitdbCallbacks(
	ctx context.Context,
	r wazero.Runtime,
	baseIndex uint32,
) (*InitdbCallbacks, error) {
	return instantiateInitdbCallbacksInto(ctx, r, baseIndex, &InitdbCallbacks{})
}

// instantiateInitdbCallbacksInto wires the given handler's system/popen/pclose
// hooks into a bridge placed at baseIndex, so the WZRuntime path can install an
// externally-owned InitdbCallbacks (matching the wasmtime RegisterInitdbCallbacks
// contract).
func instantiateInitdbCallbacksInto(
	ctx context.Context,
	r wazero.Runtime,
	baseIndex uint32,
	handler *InitdbCallbacks,
) (*InitdbCallbacks, error) {
	// Register host functions
	hostBuilder := r.NewHostModuleBuilder("initdb_host")

	hostBuilder.NewFunctionBuilder().
		WithGoModuleFunction(api.GoModuleFunc(func(ctx context.Context, mod api.Module, stack []uint64) {
			cmdPtr := api.DecodeU32(stack[0])
			cmd := handler.readCString(cmdPtr)
			if handler.OnSystem != nil {
				stack[0] = api.EncodeI32(handler.OnSystem(ctx, cmd))
			} else {
				stack[0] = api.EncodeI32(-1)
			}
		}), []api.ValueType{api.ValueTypeI32}, []api.ValueType{api.ValueTypeI32}).
		Export("handle_system")

	hostBuilder.NewFunctionBuilder().
		WithGoModuleFunction(api.GoModuleFunc(func(ctx context.Context, mod api.Module, stack []uint64) {
			cmdPtr := api.DecodeU32(stack[0])
			modePtr := api.DecodeU32(stack[1])
			cmd := handler.readCString(cmdPtr)
			mode := handler.readCString(modePtr)
			if handler.OnPopen != nil {
				stack[0] = api.EncodeI32(handler.OnPopen(ctx, cmd, mode))
			} else {
				stack[0] = 0 // NULL
			}
		}), []api.ValueType{api.ValueTypeI32, api.ValueTypeI32}, []api.ValueType{api.ValueTypeI32}).
		Export("handle_popen")

	hostBuilder.NewFunctionBuilder().
		WithGoModuleFunction(api.GoModuleFunc(func(ctx context.Context, mod api.Module, stack []uint64) {
			stream := api.DecodeI32(stack[0])
			if handler.OnPclose != nil {
				stack[0] = api.EncodeI32(handler.OnPclose(ctx, stream))
			} else {
				stack[0] = 0
			}
		}), []api.ValueType{api.ValueTypeI32}, []api.ValueType{api.ValueTypeI32}).
		Export("handle_pclose")

	_, err := hostBuilder.Instantiate(ctx)
	if err != nil {
		return nil, fmt.Errorf("instantiate initdb_host: %w", err)
	}

	// Build and instantiate the bridge WASM module
	bridgeWasm := buildCallbackBridgeModule(baseIndex)
	compiled, err := r.CompileModule(ctx, bridgeWasm)
	if err != nil {
		return nil, fmt.Errorf("compile callback bridge: %w", err)
	}
	_, err = r.InstantiateModule(ctx, compiled, wazero.NewModuleConfig().WithName("initdb_bridge"))
	if err != nil {
		return nil, fmt.Errorf("instantiate callback bridge: %w", err)
	}

	return handler, nil
}

// ParseSystemCommand parses a shell command string, stripping I/O redirections.
func ParseSystemCommand(cmd string) (string, []string) {
	cmd = strings.TrimSpace(cmd)
	var program string
	var rest string

	if strings.HasPrefix(cmd, "\"") {
		end := strings.Index(cmd[1:], "\"")
		if end >= 0 {
			program = cmd[1 : end+1]
			rest = strings.TrimSpace(cmd[end+2:])
		} else {
			program = cmd[1:]
		}
	} else {
		parts := strings.SplitN(cmd, " ", 2)
		program = parts[0]
		if len(parts) > 1 {
			rest = parts[1]
		}
	}

	// Parse args, stopping at the first shell operator (like PGlite's getArgs)
	var args []string
	fields := strings.Fields(rest)
	for _, f := range fields {
		// Stop at shell operators: <, >, |, ;, &, etc.
		if f == "<" || f == ">" || f == ">>" || f == "|" || f == ";" ||
			f == "&&" || f == "||" || f == "&" ||
			strings.HasPrefix(f, "2>") || strings.HasPrefix(f, "1>") ||
			strings.HasPrefix(f, ">/") || strings.HasPrefix(f, "</") ||
			f == "2>&1" || f == "1>&2" {
			break
		}
		// Strip quotes
		f = strings.Trim(f, "\"'")
		if f != "" {
			args = append(args, f)
		}
	}
	return program, args
}
