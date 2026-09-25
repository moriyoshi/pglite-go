//go:build aot

package pglite

import (
	"context"

	emcompat "github.com/moriyoshi/pglite-go/emscripten"
	initdbwasm "github.com/moriyoshi/pglite-go/internal/initdbwasm"
	pgwasm "github.com/moriyoshi/pglite-go/internal/pgwasm"
	"github.com/moriyoshi/pglite-go/vfs"
)

// The -tags aot backend runs PGlite as pure Go, transpiled ahead-of-time by
// goccy/wasm2go — no wasm engine, no CGo, no runtime .wasm asset. It implements
// the same backend contract as backend_wasmtime.go / backend_wazero.go, so
// pglite.go / wire.go / cluster.go use it unchanged.
//
// The transpiled packages (internal/pgwasm for the postgres module,
// internal/initdbwasm for initdb) are build artifacts and are NOT committed —
// treat them like the .wasm assets. Produce them locally with:
//
//	go generate ./...            # fetch pglite.wasm + initdb.wasm  (existing)
//	go generate -tags aot ./...  # internal/wasmpass + wasm2go -> internal/{pgwasm,initdbwasm}
//	go build -tags aot ./...
//
// Each generate step emits, alongside the wasm2go output, a small shim
// (NewAOT + the AOTModule methods) and the auto-generated env/WASI host adapter
// that bridges the module's EnvImports to the shared handlers in this package's
// emscripten/ layer. See docs/wasm2go-migration.md.

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
// "pglite"), backed by the transpiled internal/pgwasm package.
func newRuntimeFromModule(ctx context.Context, e wasmEngine, m wasmModule, fs *vfs.FS, stdin []byte, capture *[]byte) (emcompat.Runtime, error) {
	// NewAOT (generated shim) wires the env/WASI host adapter over fs/stdin/capture,
	// instantiates the transpiled module, and returns it as an emcompat.AOTModule.
	mod := pgwasm.NewAOT(fs, stdin, capture)
	return emcompat.NewA2GRuntime(ctx, mod)
}

// newRuntimeFromWasm is the initdb path (cluster.go). The wasm bytes are ignored;
// initdb is transpiled into internal/initdbwasm.
func newRuntimeFromWasm(ctx context.Context, e wasmEngine, wasm []byte, fs *vfs.FS, stdin []byte, capture *[]byte) (emcompat.Runtime, error) {
	mod := initdbwasm.NewAOT(fs, stdin, capture)
	return emcompat.NewA2GRuntime(ctx, mod)
}
