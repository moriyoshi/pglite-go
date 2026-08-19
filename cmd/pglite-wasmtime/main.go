//go:build wasmtime

// Command pglite-wasmtime runs pglite.wasm on wasmtime (instead of wazero) to
// validate the execution port. Starts with the simplest subcommand, `postgres
// -V`, which still exercises instantiation, data relocs, C++ ctors (via
// invoke_/longjmp), and WASI stdout.
//
//	go run -tags wasmtime ./cmd/pglite-wasmtime
package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/bytecodealliance/wasmtime-go/v34"

	emcompat "github.com/moriyoshi/pglite-go/emscripten"
	"github.com/moriyoshi/pglite-go/vfs"
)

func main() {
	wasmDir := "wasm"
	if len(os.Args) > 1 {
		wasmDir = os.Args[1]
	}

	fs := vfs.New()
	if err := fs.LoadManifest(wasmDir+"/pglite.manifest.json", wasmDir+"/pglite.data"); err != nil {
		fmt.Fprintf(os.Stderr, "load manifest: %v\n", err)
	}
	fs.MkdirAll("/tmp/pglite/data", 0o700)
	fs.MkdirAll("/dev", 0o755)
	fs.WriteFile("/dev/null", nil, 0o666)
	fs.WriteFile("/dev/urandom", nil, 0o666)
	fs.MkdirAll("/home/web_user", 0o755)

	ctx := context.Background()
	wasm, err := os.ReadFile(wasmDir + "/pglite.wasm")
	if err != nil {
		panic(err)
	}
	engine := wasmtime.NewEngine()

	fmt.Println("=== postgres -V on wasmtime ===")
	code := run(ctx, engine, wasm, fs, []string{"/pglite/bin/postgres", "-V"}, nil, nil)
	fmt.Printf("postgres -V exit: %d\n", code)
}

// run instantiates a fresh module and calls __main_argc_argv with args.
func run(ctx context.Context, engine *wasmtime.Engine, wasm []byte, fs *vfs.FS, args []string, stdin []byte, stdoutCapture *[]byte) int32 {
	t0 := time.Now()
	rt, err := emcompat.NewWTRuntime(ctx, engine, wasm, fs, stdin, stdoutCapture)
	if err != nil {
		fmt.Fprintf(os.Stderr, "NewWTRuntime: %v\n", err)
		return -1
	}
	fmt.Printf("[wasmtime] compile+instantiate: %.3fs\n", time.Since(t0).Seconds())

	if err := rt.ApplyDataRelocs(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "data relocs: %v\n", err)
	}

	code, err := rt.CallMain(ctx, args)
	if err != nil {
		msg := err.Error()
		if strings.Contains(msg, "exit(0)") {
			return 0
		}
		fmt.Fprintf(os.Stderr, "[main] %v\n", err)
		if strings.Contains(msg, "exit(") {
			return 1
		}
		return -1
	}
	return code
}
