//go:build wazero

package pglite

import (
	"context"
	"os"

	"github.com/tetratelabs/wazero"

	emcompat "github.com/moriyoshi/pglite-go/emscripten"
	"github.com/moriyoshi/pglite-go/vfs"
)

// The -tags wazero backend runs PGlite on the pure-Go wazero runtime (no CGo).
// The wazero "engine" is a persistent compilation cache plus a base context
// carrying the mmap linear-memory allocator; a "module" is the import-patched
// wasm, which each runtime compiles through the shared cache (fast after the
// first cold compile). backend_wasmtime.go is the default.

type wazeroEngine struct {
	cache wazero.CompilationCache
	ctx   context.Context // carries the mmap memory allocator (experimental)
}

type wasmEngine = *wazeroEngine
type wasmModule = []byte

func newEngine() wasmEngine {
	var cache wazero.CompilationCache
	if d, err := os.UserCacheDir(); err == nil {
		if c, cerr := wazero.NewCompilationCacheWithDir(d + "/pglite-go/wazero"); cerr == nil {
			cache = c
		}
	}
	if cache == nil {
		cache = wazero.NewCompilationCache()
	}
	return &wazeroEngine{cache: cache, ctx: withMemoryAllocator(context.Background())}
}

func closeEngine(e wasmEngine) {
	if e != nil && e.cache != nil {
		e.cache.Close(context.Background())
	}
}

// compileModule patches the wasm imports once; native compilation happens
// per-runtime through the shared cache.
func compileModule(e wasmEngine, wasm []byte, name string) (wasmModule, error) {
	return emcompat.PatchWasmImports(wasm)
}

func newRuntimeFromModule(ctx context.Context, e wasmEngine, m wasmModule, fs *vfs.FS, stdin []byte, capture *[]byte) (emcompat.Runtime, error) {
	return emcompat.NewWZRuntimeFromModule(e.ctx, e.cache, m, fs, stdin, capture)
}

func newRuntimeFromWasm(ctx context.Context, e wasmEngine, wasm []byte, fs *vfs.FS, stdin []byte, capture *[]byte) (emcompat.Runtime, error) {
	return emcompat.NewWZRuntime(e.ctx, e.cache, wasm, fs, stdin, capture)
}
