//go:build !wazero

package pglite

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"

	"github.com/bytecodealliance/wasmtime-go/v34"

	emcompat "github.com/moriyoshi/pglite-go/emscripten"
	"github.com/moriyoshi/pglite-go/vfs"
)

// The default backend runs PGlite on wasmtime (CGo). backend_wazero.go provides
// the pure-Go alternative behind -tags wazero; the rest of the library is
// written against these aliases and constructors plus emcompat.Runtime.

type wasmEngine = *wasmtime.Engine
type wasmModule = *wasmtime.Module

func newEngine() wasmEngine { return wasmtime.NewEngine() }
func closeEngine(e wasmEngine) {
	if e != nil {
		e.Close()
	}
}

func compileModule(e wasmEngine, wasm []byte, name string) (wasmModule, error) {
	m, _, err := compileCached(e, wasm, name)
	return m, err
}

func newRuntimeFromModule(ctx context.Context, e wasmEngine, m wasmModule, fs *vfs.FS, stdin []byte, capture *[]byte) (emcompat.Runtime, error) {
	return emcompat.NewWTRuntimeFromModule(ctx, e, m, fs, stdin, capture)
}

func newRuntimeFromWasm(ctx context.Context, e wasmEngine, wasm []byte, fs *vfs.FS, stdin []byte, capture *[]byte) (emcompat.Runtime, error) {
	return emcompat.NewWTRuntime(ctx, e, wasm, fs, stdin, capture)
}

// compileCached compiles wasm to a native module, caching the serialized result
// on disk (keyed by content hash) so the expensive compile is paid only once.
func compileCached(engine wasmEngine, wasm []byte, name string) (*wasmtime.Module, bool, error) {
	dir := ""
	if d, err := os.UserCacheDir(); err == nil {
		dir = filepath.Join(d, "pglite-go", "wasmtime")
	}
	sum := sha256.Sum256(wasm)
	path := ""
	if dir != "" {
		path = filepath.Join(dir, fmt.Sprintf("%s-%s.cwasm", name, hex.EncodeToString(sum[:8])))
		if mod, err := wasmtime.NewModuleDeserializeFile(engine, path); err == nil {
			return mod, true, nil
		}
	}
	mod, err := wasmtime.NewModule(engine, wasm)
	if err != nil {
		return nil, false, err
	}
	if path != "" {
		if blob, serr := mod.Serialize(); serr == nil {
			_ = os.MkdirAll(dir, 0o755)
			tmp := path + ".tmp"
			if os.WriteFile(tmp, blob, 0o644) == nil {
				_ = os.Rename(tmp, path)
			}
		}
	}
	return mod, false, nil
}
