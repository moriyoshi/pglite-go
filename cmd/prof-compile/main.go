// Command prof-compile CPU-profiles wazero's compilation of pglite.wasm to
// locate the bottleneck. Usage: prof-compile <out.pprof>
package main

import (
	"context"
	"fmt"
	"os"
	"runtime/pprof"
	"time"

	"github.com/tetratelabs/wazero"

	emcompat "github.com/moriyoshi/pglite-go/emscripten"
)

func main() {
	out := "/tmp/compile.pprof"
	if len(os.Args) > 1 {
		out = os.Args[1]
	}
	ctx := context.Background()
	raw, err := os.ReadFile("wasm/pglite.wasm")
	if err != nil {
		panic(err)
	}
	patched, err := emcompat.PatchWasmImports(raw)
	if err != nil {
		panic(err)
	}

	f, err := os.Create(out)
	if err != nil {
		panic(err)
	}
	defer f.Close()

	cache := wazero.NewCompilationCache()
	defer cache.Close(ctx)
	r := wazero.NewRuntimeWithConfig(ctx, wazero.NewRuntimeConfig().WithCompilationCache(cache))
	defer r.Close(ctx)

	if err := pprof.StartCPUProfile(f); err != nil {
		panic(err)
	}
	t0 := time.Now()
	if _, err := r.CompileModule(ctx, patched); err != nil {
		panic(err)
	}
	pprof.StopCPUProfile()
	fmt.Printf("compiled in %.2fs -> profile %s\n", time.Since(t0).Seconds(), out)
}
