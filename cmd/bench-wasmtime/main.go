//go:build !wazero

// Command bench-wasmtime measures how long wasmtime takes to compile
// pglite.wasm, for comparison against wazero's compile time.
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
	fmt.Printf("module: %s (%d bytes)\n", path, len(wasm))

	engine := wasmtime.NewEngine()

	// In-memory compile (Cranelift).
	t0 := time.Now()
	mod, err := wasmtime.NewModule(engine, wasm)
	if err != nil {
		fmt.Printf("compile FAILED: %v\n", err)
		os.Exit(1)
	}
	compile := time.Since(t0)
	fmt.Printf("wasmtime NewModule (compile): %.2fs\n", compile.Seconds())

	// Serialize (AOT artifact) to measure size.
	ser, err := mod.Serialize()
	if err == nil {
		fmt.Printf("serialized (.cwasm) size: %d bytes (%.1f MB)\n", len(ser), float64(len(ser))/1e6)
	}

	// Report imports so we know what the host layer must provide.
	imps := mod.Imports()
	byModule := map[string]int{}
	for _, imp := range imps {
		if m := imp.Module(); m != "" {
			byModule[m]++
		}
	}
	fmt.Printf("imports by module: %v (total %d)\n", byModule, len(imps))
}
