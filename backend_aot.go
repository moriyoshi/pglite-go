//go:build aot

package pglite

import (
	"context"
	"errors"

	emcompat "github.com/moriyoshi/pglite-go/emscripten"
	"github.com/moriyoshi/pglite-go/vfs"
)

// The -tags aot backend runs PGlite as pure Go, transpiled ahead-of-time by
// goccy/wasm2go — no wasm engine, no CGo, no runtime .wasm asset. It implements
// the same backend contract as backend_wasmtime.go / backend_wazero.go, so
// pglite.go / wire.go / cluster.go use it unchanged.
//
// The ~200 MB of transpiled Go lives in a companion module rather than here, so
// this module stays thin. That module registers its constructors via a driver
// pattern (like database/sql) — enable the AOT backend by blank-importing it:
//
//	import (
//	    "github.com/moriyoshi/pglite-go"
//	    _ "github.com/moriyoshi/pglite-go-aot" // registers the AOT backend
//	)
//	// build with -tags aot
//
// The companion imports this module's emscripten/vfs packages; this module never
// imports the companion, so there is no module cycle. See docs/wasm2go-migration.md.

// AOTFactory instantiates a transpiled module over the given VFS/stdin/capture
// and returns it as an emcompat.AOTModule. The companion module supplies one for
// the postgres module and one for initdb.
type AOTFactory func(fs *vfs.FS, stdin []byte, capture *[]byte) emcompat.AOTModule

var (
	aotPglite AOTFactory
	aotInitdb AOTFactory
)

// RegisterAOT installs the transpiled-module constructors. The companion module
// github.com/moriyoshi/pglite-go-aot calls it from an init(); user code enables
// the backend by blank-importing that module.
func RegisterAOT(pglite, initdb AOTFactory) {
	aotPglite, aotInitdb = pglite, initdb
}

var errAOTNotRegistered = errors.New(
	"pglite: AOT backend not registered — blank-import github.com/moriyoshi/pglite-go-aot and build with -tags aot")

// There is no runtime engine to manage in the AOT backend.
type aotEngine struct{}

type wasmEngine = *aotEngine

// A "module" only records which transpiled package to instantiate; the wasm
// bytes handed to compileModule/newRuntimeFromWasm are ignored (the code was
// transpiled at build time).
type wasmModule struct{ which string }

func newEngine() wasmEngine    { return &aotEngine{} }
func closeEngine(e wasmEngine) {}

func compileModule(e wasmEngine, wasm []byte, name string) (wasmModule, error) {
	return wasmModule{which: name}, nil
}

// newRuntimeFromModule is the postgres path (pglite.go compiles with name
// "pglite"), backed by the companion's transpiled postgres module.
func newRuntimeFromModule(ctx context.Context, e wasmEngine, m wasmModule, fs *vfs.FS, stdin []byte, capture *[]byte) (emcompat.Runtime, error) {
	if aotPglite == nil {
		return nil, errAOTNotRegistered
	}
	return emcompat.NewA2GRuntime(ctx, aotPglite(fs, stdin, capture))
}

// newRuntimeFromWasm is the initdb path (cluster.go). The wasm bytes are ignored;
// initdb is transpiled in the companion module.
func newRuntimeFromWasm(ctx context.Context, e wasmEngine, wasm []byte, fs *vfs.FS, stdin []byte, capture *[]byte) (emcompat.Runtime, error) {
	if aotInitdb == nil {
		return nil, errAOTNotRegistered
	}
	return emcompat.NewA2GRuntime(ctx, aotInitdb(fs, stdin, capture))
}
