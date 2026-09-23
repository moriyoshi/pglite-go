// Command wasmpass rewrites the PGlite Emscripten wasm into a standalone module
// that goccy/wasm2go can translate into a self-contained, panic-free Go package.
//
// Usage:
//
//	wasmpass -i pglite.wasm -o pglite.standalone.wasm
//
// It strips non-root exports, internalizes the dynamic-linking imports, folds
// segment offsets, and inserts the cooperative longjmp-unwind guards. See
// internal/wasmpass for the transforms.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/moriyoshi/pglite-go/internal/wasmpass"
)

func main() {
	in := flag.String("i", "", "input wasm file (required)")
	out := flag.String("o", "", "output wasm file (required)")
	config := flag.String("config", "pglite", "module config: pglite|initdb")
	flag.Parse()
	if *in == "" || *out == "" {
		flag.Usage()
		os.Exit(2)
	}
	cfg := wasmpass.DefaultConfig()
	if *config == "initdb" {
		cfg = wasmpass.DefaultInitdbConfig()
	}
	raw, err := os.ReadFile(*in)
	if err != nil {
		fatal(err)
	}
	res, st, err := wasmpass.Transform(raw, cfg)
	if err != nil {
		fatal(err)
	}
	if err := os.WriteFile(*out, res, 0o644); err != nil {
		fatal(err)
	}
	fmt.Fprintf(os.Stderr,
		"wasmpass: stripped %d exports, %d may-longjmp funcs, %d sites instrumented; wrote %s (%d bytes)\n",
		st.ExportsStripped, st.MayLongjmpFuncs, st.InstrumentedSites, *out, len(res))
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "wasmpass:", err)
	os.Exit(1)
}
