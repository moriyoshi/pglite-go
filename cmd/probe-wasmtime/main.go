//go:build !wazero

// Command probe-wasmtime wires the minimal externs pglite.wasm imports
// (memory, table, globals) with no-op function stubs, then attempts to
// instantiate and measure instantiation time + how far the start function gets.
package main

import (
	"fmt"
	"os"
	"time"

	"github.com/bytecodealliance/wasmtime-go/v34"
)

func main() {
	path := "wasm/pglite.wasm"
	if len(os.Args) > 1 {
		path = os.Args[1]
	}
	wasm, err := os.ReadFile(path)
	if err != nil {
		panic(err)
	}

	engine := wasmtime.NewEngine()
	mod, err := wasmtime.NewModule(engine, wasm)
	if err != nil {
		panic(err)
	}
	store := wasmtime.NewStore(engine)

	// Provide the imported memory: min=2048 pages (128MB), max=32768 (2GB).
	memType := wasmtime.NewMemoryType(2048, true, 32768, false)
	memory, err := wasmtime.NewMemory(store, memType)
	if err != nil {
		panic(err)
	}

	// Provide the imported function table: funcref, min=6097.
	tableType := wasmtime.NewTableType(wasmtime.NewValType(wasmtime.KindFuncref), 6097, false, 0)
	table, err := wasmtime.NewTable(store, tableType, wasmtime.ValFuncref(nil))
	if err != nil {
		panic(err)
	}

	// Globals.
	i32 := wasmtime.NewValType(wasmtime.KindI32)
	mkGlobal := func(mutable bool, v int32) *wasmtime.Global {
		g, gerr := wasmtime.NewGlobal(store, wasmtime.NewGlobalType(i32, mutable), wasmtime.ValI32(v))
		if gerr != nil {
			panic(gerr)
		}
		return g
	}
	const heapBase = 10937088
	globals := map[string]*wasmtime.Global{
		"__memory_base":   mkGlobal(false, 0),
		"__stack_pointer": mkGlobal(true, heapBase),
		"__table_base":    mkGlobal(false, 0),
		"__heap_base":     mkGlobal(true, heapBase),
	}

	// Build externs in the exact order the module imports them.
	var called string
	stubCount := 0
	imports := mod.Imports()
	externs := make([]wasmtime.AsExtern, 0, len(imports))
	for _, imp := range imports {
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
				panic("unknown global import: " + name)
			}
			externs = append(externs, g)
		case ty.FuncType() != nil:
			ft := ty.FuncType()
			fname := name
			// No-op stub: records the first call, returns zero values.
			fn := wasmtime.NewFunc(store, ft, func(_ *wasmtime.Caller, _ []wasmtime.Val) ([]wasmtime.Val, *wasmtime.Trap) {
				if called == "" {
					called = fname
				}
				results := make([]wasmtime.Val, len(ft.Results()))
				for i, rt := range ft.Results() {
					switch rt.Kind() {
					case wasmtime.KindI32:
						results[i] = wasmtime.ValI32(0)
					case wasmtime.KindI64:
						results[i] = wasmtime.ValI64(0)
					case wasmtime.KindF64:
						results[i] = wasmtime.ValF64(0)
					case wasmtime.KindF32:
						results[i] = wasmtime.ValF32(0)
					default:
						results[i] = wasmtime.ValI32(0)
					}
				}
				return results, nil
			})
			stubCount++
			externs = append(externs, fn)
		default:
			panic("unhandled import kind for " + name)
		}
	}
	fmt.Printf("wired %d externs (%d func stubs)\n", len(externs), stubCount)

	t0 := time.Now()
	inst, err := wasmtime.NewInstance(store, mod, externs)
	dur := time.Since(t0)
	if err != nil {
		fmt.Printf("instantiate returned after %.3fs: %v\n", dur.Seconds(), err)
		fmt.Printf("first host func called during start: %q\n", called)
		return
	}
	fmt.Printf("INSTANTIATED in %.3fs\n", dur.Seconds())
	fmt.Printf("first host func called during start: %q\n", called)
	_ = inst
}
