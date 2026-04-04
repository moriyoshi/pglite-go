// Package emscripten provides host function implementations for Emscripten-compiled
// WASM modules, specifically the syscall layer that wazero's built-in emscripten
// support does not cover.
package emscripten

import (
	"context"
	"fmt"
	"time"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/imports/emscripten"

	"github.com/moriyoshi/pglite-go/vfs"
)

const (
	// Values matching PGlite's Emscripten build output.
	// These are extracted from the JS glue code (pglite.js).
	// Both must be 0: Emscripten's linker generates module globals with pre-relocation
	// values (data offsets). The JS runtime's relocateExports adds __memory_base to
	// global values, but we can't modify globals after instantiation in wazero.
	// Setting both to 0 avoids the need for relocation entirely.
	defaultMemoryBase   = 0
	defaultTableBase    = 0
	defaultStackPointer = 10937088 // same as __heap_base in the JS
	defaultHeapBase     = 10937088
)

// PrepareAndCompile patches the WASM binary to redirect non-function imports
// from "env" to "env.extras", then compiles it and sets up all host modules.
// Returns the compiled module ready for instantiation.
// The provided VFS is used for filesystem syscalls.
// PrepareAndCompile patches the WASM binary to redirect non-function imports
// from "env" to "env.extras", then compiles it and sets up all host modules.
// Returns the compiled module ready for instantiation.
// The provided VFS is used for filesystem syscalls.
// WASI must be set up separately before calling this (use InstantiateWASI).
func PrepareAndCompile(ctx context.Context, r wazero.Runtime, wasmBytes []byte, fs *vfs.FS) (wazero.CompiledModule, error) {
	// Patch the WASM binary
	patched, err := PatchWasmImports(wasmBytes)
	if err != nil {
		return nil, fmt.Errorf("patch WASM: %w", err)
	}
	return PrepareAndCompilePrepatched(ctx, r, patched, fs)
}

// PrepareAndCompilePrepatched is like PrepareAndCompile but takes already-patched WASM.
func PrepareAndCompilePrepatched(ctx context.Context, r wazero.Runtime, patchedWasm []byte, fs *vfs.FS) (wazero.CompiledModule, error) {
	compiled, err := r.CompileModule(ctx, patchedWasm)
	if err != nil {
		return nil, fmt.Errorf("compile WASM: %w", err)
	}

	// Step 3: Set up GOT.mem module
	gotMemWasm := buildGOTMemModule(map[string]int32{
		"__heap_base": defaultHeapBase,
	})
	gotMemCompiled, err := r.CompileModule(ctx, gotMemWasm)
	if err != nil {
		return nil, fmt.Errorf("compile GOT.mem: %w", err)
	}
	_, err = r.InstantiateModule(ctx, gotMemCompiled, wazero.NewModuleConfig().WithName("GOT.mem"))
	if err != nil {
		return nil, fmt.Errorf("instantiate GOT.mem: %w", err)
	}

	// Step 4: Set up env.extras module (globals, memory, table)
	envExtrasWasm := buildEnvExtrasModule(
		2048,  // min pages (128MB)
		32768, // max pages (2GB)
		6098,  // table min size (table_base=1 + dylink tablesize=6097)
		[]globalDef{
			{name: "__memory_base", valType: 0x7f, mutable: false, initI32: defaultMemoryBase},
			{name: "__stack_pointer", valType: 0x7f, mutable: true, initI32: defaultStackPointer},
			{name: "__table_base", valType: 0x7f, mutable: false, initI32: defaultTableBase},
		},
	)
	envExtrasCompiled, err := r.CompileModule(ctx, envExtrasWasm)
	if err != nil {
		return nil, fmt.Errorf("compile env.extras: %w", err)
	}
	_, err = r.InstantiateModule(ctx, envExtrasCompiled, wazero.NewModuleConfig().WithName("env.extras"))
	if err != nil {
		return nil, fmt.Errorf("instantiate env.extras: %w", err)
	}

	// Step 5: Set up "env" module with all host functions
	syscallHandler := NewSyscallHandler(fs)
	envBuilder := r.NewHostModuleBuilder("env")
	registerAllEnvFunctions(envBuilder, compiled, syscallHandler)

	// Add wazero's built-in Emscripten support (invoke_*, longjmp)
	exporter, err := emscripten.NewFunctionExporterForModule(compiled)
	if err != nil {
		return nil, fmt.Errorf("emscripten exporter: %w", err)
	}
	exporter.ExportFunctions(envBuilder)

	_, err = envBuilder.Instantiate(ctx)
	if err != nil {
		return nil, fmt.Errorf("instantiate env: %w", err)
	}

	return compiled, nil
}

// registerAllEnvFunctions registers Go implementations for all env function
// imports, excluding invoke_* (handled by wazero's emscripten package).
func registerAllEnvFunctions(builder wazero.HostModuleBuilder, compiled wazero.CompiledModule, syscallHandler *SyscallHandler) {
	// Get real syscall implementations
	syscallImpls := syscallHandler.Register()

	for _, imp := range compiled.ImportedFunctions() {
		moduleName, name, _ := imp.Import()
		if moduleName != "env" {
			continue
		}
		// Skip invoke_* and _emscripten_throw_longjmp - handled by wazero
		if len(name) > 7 && name[:7] == "invoke_" {
			continue
		}
		if name == "_emscripten_throw_longjmp" {
			continue
		}
		if name == "emscripten_notify_memory_growth" {
			continue
		}

		sig := funcSig{
			params:  imp.ParamTypes(),
			results: imp.ResultTypes(),
		}

		// Check if we have a real syscall implementation
		var fn api.GoModuleFunc
		if impl, ok := syscallImpls[name]; ok {
			fn = impl
		} else {
			fn = createHostFunc(name, sig)
		}

		tracingFn := fn

		builder.NewFunctionBuilder().
			WithGoModuleFunction(tracingFn, sig.params, sig.results).
			Export(name)
	}
}

// buildEnvExtrasModule builds a WASM binary that defines and exports
// memory, table, and globals for the "env.extras" module.
func buildEnvExtrasModule(memMinPages, memMaxPages, tableMinSize uint32, globals []globalDef) []byte {
	var buf []byte
	buf = append(buf, 0x00, 0x61, 0x73, 0x6d) // \0asm
	buf = append(buf, 0x01, 0x00, 0x00, 0x00) // version 1

	// Table section (id=4)
	var tablePayload []byte
	tablePayload = append(tablePayload, encodeLEB128U(1)...) // 1 table
	tablePayload = append(tablePayload, 0x70)                // funcref
	tablePayload = append(tablePayload, 0x00)                // no max
	tablePayload = append(tablePayload, encodeLEB128U(uint64(tableMinSize))...)
	buf = append(buf, encodeSection(4, tablePayload)...)

	// Memory section (id=5)
	var memPayload []byte
	memPayload = append(memPayload, encodeLEB128U(1)...) // 1 memory
	memPayload = append(memPayload, 0x01)                // has max
	memPayload = append(memPayload, encodeLEB128U(uint64(memMinPages))...)
	memPayload = append(memPayload, encodeLEB128U(uint64(memMaxPages))...)
	buf = append(buf, encodeSection(5, memPayload)...)

	// Global section (id=6)
	var globalPayload []byte
	globalPayload = append(globalPayload, encodeLEB128U(uint64(len(globals)))...)
	for _, g := range globals {
		globalPayload = append(globalPayload, g.valType)
		if g.mutable {
			globalPayload = append(globalPayload, 0x01)
		} else {
			globalPayload = append(globalPayload, 0x00)
		}
		globalPayload = append(globalPayload, 0x41) // i32.const
		globalPayload = append(globalPayload, encodeLEB128S(int64(g.initI32))...)
		globalPayload = append(globalPayload, 0x0b) // end
	}
	buf = append(buf, encodeSection(6, globalPayload)...)

	// Export section (id=7)
	numExports := len(globals) + 2 // +2 for memory and table
	var exportPayload []byte
	exportPayload = append(exportPayload, encodeLEB128U(uint64(numExports))...)

	// Export table
	exportPayload = append(exportPayload, encodeName("__indirect_function_table")...)
	exportPayload = append(exportPayload, 0x01) // table export
	exportPayload = append(exportPayload, encodeLEB128U(0)...)

	// Export memory
	exportPayload = append(exportPayload, encodeName("memory")...)
	exportPayload = append(exportPayload, 0x02) // memory export
	exportPayload = append(exportPayload, encodeLEB128U(0)...)

	// Export globals
	for i, g := range globals {
		exportPayload = append(exportPayload, encodeName(g.name)...)
		exportPayload = append(exportPayload, 0x03) // global export
		exportPayload = append(exportPayload, encodeLEB128U(uint64(i))...)
	}

	buf = append(buf, encodeSection(7, exportPayload)...)

	return buf
}

// createHostFunc returns a Go implementation for the given function.
func createHostFunc(name string, sig funcSig) api.GoModuleFunc {
	switch name {
	case "emscripten_date_now":
		return api.GoModuleFunc(func(ctx context.Context, mod api.Module, stack []uint64) {
			stack[0] = api.EncodeF64(float64(time.Now().UnixMilli()))
		})
	case "emscripten_get_now":
		getNowCount := 0
		return api.GoModuleFunc(func(ctx context.Context, mod api.Module, stack []uint64) {
			getNowCount++
			if getNowCount <= 3 || getNowCount%10000 == 0 {
				fmt.Printf("[emscripten_get_now] call #%d\n", getNowCount)
			}
			stack[0] = api.EncodeF64(float64(time.Now().UnixMicro()) / 1000.0)
		})
	case "emscripten_get_heap_max":
		return api.GoModuleFunc(func(ctx context.Context, mod api.Module, stack []uint64) {
			stack[0] = api.EncodeU32(2 * 1024 * 1024 * 1024)
		})
	case "emscripten_resize_heap":
		return api.GoModuleFunc(func(ctx context.Context, mod api.Module, stack []uint64) {
			requestedSize := api.DecodeU32(stack[0])
			mem := mod.Memory()
			currentPages := mem.Size() / 65536
			neededPages := (requestedSize + 65535) / 65536
			if neededPages > currentPages {
				grew, ok := mem.Grow(neededPages - currentPages)
				if !ok || grew == 0 {
					stack[0] = 0 // failure
					return
				}
			}
			stack[0] = 1 // success
		})
	case "_abort_js":
		return api.GoModuleFunc(func(ctx context.Context, mod api.Module, stack []uint64) {
			// Use sys.ExitError to gracefully abort (wazero catches this)
			panic(fmt.Errorf("_abort_js"))
		})
	case "__assert_fail":
		return api.GoModuleFunc(func(ctx context.Context, mod api.Module, stack []uint64) {
			condition := readCString(mod.Memory(), api.DecodeU32(stack[0]))
			file := readCString(mod.Memory(), api.DecodeU32(stack[1]))
			line := api.DecodeU32(stack[2])
			function := readCString(mod.Memory(), api.DecodeU32(stack[3]))
			panic(fmt.Sprintf("assert failed: %s at %s:%d in %s", condition, file, line, function))
		})
	case "exit":
		return api.GoModuleFunc(func(ctx context.Context, mod api.Module, stack []uint64) {
			code := api.DecodeU32(stack[0])
			fmt.Printf("[emscripten] exit(%d) called\n", code)
			panic(fmt.Sprintf("exit(%d)", code))
		})
	case "_emscripten_runtime_keepalive_clear":
		return api.GoModuleFunc(func(ctx context.Context, mod api.Module, stack []uint64) {})
	case "__call_sighandler":
		return api.GoModuleFunc(func(ctx context.Context, mod api.Module, stack []uint64) {})
	case "_localtime_js":
		return api.GoModuleFunc(func(ctx context.Context, mod api.Module, stack []uint64) {
			t := int64(stack[0])
			tmPtr := api.DecodeU32(stack[1])
			tm := time.Unix(t, 0).Local()
			writeTm(mod.Memory(), tmPtr, tm)
		})
	case "_gmtime_js":
		return api.GoModuleFunc(func(ctx context.Context, mod api.Module, stack []uint64) {
			t := int64(stack[0])
			tmPtr := api.DecodeU32(stack[1])
			tm := time.Unix(t, 0).UTC()
			writeTm(mod.Memory(), tmPtr, tm)
		})
	case "_mktime_js":
		return api.GoModuleFunc(func(ctx context.Context, mod api.Module, stack []uint64) {
			tmPtr := api.DecodeU32(stack[0])
			mem := mod.Memory()
			sec, _ := mem.ReadUint32Le(tmPtr + 0)
			min, _ := mem.ReadUint32Le(tmPtr + 4)
			hour, _ := mem.ReadUint32Le(tmPtr + 8)
			mday, _ := mem.ReadUint32Le(tmPtr + 12)
			mon, _ := mem.ReadUint32Le(tmPtr + 16)
			year, _ := mem.ReadUint32Le(tmPtr + 20)
			t := time.Date(int(year)+1900, time.Month(int(mon)+1), int(mday),
				int(hour), int(min), int(sec), 0, time.Local)
			stack[0] = api.EncodeF64(float64(t.Unix()))
		})
	case "_tzset_js":
		return api.GoModuleFunc(func(ctx context.Context, mod api.Module, stack []uint64) {
			// timezone, daylight, tzname, std_name
			tzPtr := api.DecodeU32(stack[0])
			daylightPtr := api.DecodeU32(stack[1])
			stdNamePtr := api.DecodeU32(stack[2])
			dstNamePtr := api.DecodeU32(stack[3])
			mem := mod.Memory()
			_, offset := time.Now().Zone()
			mem.WriteUint32Le(tzPtr, uint32(-offset))
			mem.WriteUint32Le(daylightPtr, 0)
			mem.Write(stdNamePtr, []byte("UTC\x00"))
			mem.Write(dstNamePtr, []byte("UTC\x00"))
		})
	case "_mmap_js":
		return api.GoModuleFunc(func(ctx context.Context, mod api.Module, stack []uint64) {
			length := api.DecodeU32(stack[0])
			// prot := api.DecodeI32(stack[1])
			// flags := api.DecodeI32(stack[2])
			// fd := api.DecodeI32(stack[3])
			// offset := int64(stack[4])
			allocatedPtr := api.DecodeU32(stack[5])
			addrPtr := api.DecodeU32(stack[6])

			// Allocate aligned memory via the module's memalign
			memalign := mod.ExportedFunction("emscripten_builtin_memalign")
			if memalign == nil {
				memalign = mod.ExportedFunction("malloc")
			}
			if memalign == nil {
				stack[0] = api.EncodeI32(-ENOSYS)
				return
			}

			var ptr uint32
			if memalign.Definition().Name() == "emscripten_builtin_memalign" {
				results, err := memalign.Call(ctx, 65536, uint64(length))
				if err != nil || results[0] == 0 {
					stack[0] = api.EncodeI32(-ENOSYS)
					return
				}
				ptr = uint32(results[0])
			} else {
				results, err := memalign.Call(ctx, uint64(length))
				if err != nil || results[0] == 0 {
					stack[0] = api.EncodeI32(-ENOSYS)
					return
				}
				ptr = uint32(results[0])
			}

			// Zero the allocated memory
			zeros := make([]byte, length)
			mod.Memory().Write(ptr, zeros)

			// Write output parameters
			mod.Memory().WriteUint32Le(allocatedPtr, 1) // allocated = true
			mod.Memory().WriteUint32Le(addrPtr, ptr)

			stack[0] = 0 // success
		})
	case "_munmap_js":
		return api.GoModuleFunc(func(ctx context.Context, mod api.Module, stack []uint64) {
			// No-op for now - memory is freed when the module is closed
			stack[0] = 0
		})
	case "_setitimer_js":
		return api.GoModuleFunc(func(ctx context.Context, mod api.Module, stack []uint64) {
			which := api.DecodeI32(stack[0])
			timeout := api.DecodeF64(stack[1])
			fmt.Printf("[setitimer] which=%d timeout=%f\n", which, timeout)
			stack[0] = 0
		})
	case "getaddrinfo":
		return api.GoModuleFunc(func(ctx context.Context, mod api.Module, stack []uint64) {
			// Return EAI_FAIL (-1) - network not available
			stack[0] = api.EncodeI32(-1)
		})
	case "getnameinfo":
		return api.GoModuleFunc(func(ctx context.Context, mod api.Module, stack []uint64) {
			stack[0] = api.EncodeI32(-1)
		})
	default:
		return makeStub(name, sig)
	}
}

var hostCallCounter int64
var invokeCallCounter int64

func makeStub(name string, sig funcSig) api.GoModuleFunc {
	return api.GoModuleFunc(func(ctx context.Context, mod api.Module, stack []uint64) {
		hostCallCounter++
		if hostCallCounter%100000 == 0 {
			fmt.Printf("[host] %d calls, last stub: %s\n", hostCallCounter, name)
		}
		for i := range sig.results {
			stack[len(sig.params)+i] = 0
		}
	})
}

// writeTm writes a struct tm to WASM memory at the given pointer.
// Emscripten struct tm layout (all i32):
// offset 0: tm_sec, 4: tm_min, 8: tm_hour, 12: tm_mday, 16: tm_mon,
// 20: tm_year, 24: tm_wday, 28: tm_yday, 32: tm_isdst
func writeTm(mem api.Memory, ptr uint32, t time.Time) {
	mem.WriteUint32Le(ptr+0, uint32(t.Second()))
	mem.WriteUint32Le(ptr+4, uint32(t.Minute()))
	mem.WriteUint32Le(ptr+8, uint32(t.Hour()))
	mem.WriteUint32Le(ptr+12, uint32(t.Day()))
	mem.WriteUint32Le(ptr+16, uint32(t.Month()-1))
	mem.WriteUint32Le(ptr+20, uint32(t.Year()-1900))
	mem.WriteUint32Le(ptr+24, uint32(t.Weekday()))
	mem.WriteUint32Le(ptr+28, uint32(t.YearDay()-1))
	isdst := uint32(0)
	if t.IsDST() {
		isdst = 1
	}
	mem.WriteUint32Le(ptr+32, isdst)
}

// readCString reads a null-terminated C string from WASM memory.
func readCString(mem api.Memory, offset uint32) string {
	var buf []byte
	for {
		b, ok := mem.ReadByte(offset)
		if !ok || b == 0 {
			break
		}
		buf = append(buf, b)
		offset++
	}
	return string(buf)
}
