package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/tetratelabs/wazero"
	wazero_api "github.com/tetratelabs/wazero/api"

	emcompat "github.com/moriyoshi/pglite-go/emscripten"
	"github.com/moriyoshi/pglite-go/vfs"
)

// Global compilation cache shared across all runtimes
var compilationCache wazero.CompilationCache

func newRuntime(ctx context.Context) wazero.Runtime {
	config := wazero.NewRuntimeConfig().WithCompilationCache(compilationCache)
	return wazero.NewRuntimeWithConfig(ctx, config)
}

func main() {
	wasmDir := "wasm"
	if len(os.Args) > 1 {
		wasmDir = os.Args[1]
	}

	compilationCache = wazero.NewCompilationCache()
	defer compilationCache.Close(context.Background())

	fs := vfs.New()
	fs.LoadManifest(wasmDir+"/pglite.manifest.json", wasmDir+"/pglite.data")
	fs.MkdirAll("/tmp/pglite/data", 0o700)
	fs.MkdirAll("/dev", 0o755)
	fs.WriteFile("/dev/null", nil, 0o666)
	fs.WriteFile("/dev/urandom", nil, 0o666)
	fs.MkdirAll("/home/web_user", 0o755)

	ctx := context.Background()
	rawPostgresWasm, _ := os.ReadFile(wasmDir + "/pglite.wasm")
	rawInitdbWasm, _ := os.ReadFile(wasmDir + "/initdb.wasm")

	// Pre-patch both WASM binaries
	postgresWasm, _ := emcompat.PatchWasmImports(rawPostgresWasm)
	initdbWasm, _ := emcompat.PatchWasmImports(rawInitdbWasm)
	// Pre-compile to warm the cache
	fmt.Print("Pre-compiling WASM... ")
	preR := newRuntime(ctx)
	preR.CompileModule(ctx, postgresWasm)
	preR.CompileModule(ctx, initdbWasm)
	preR.Close(ctx)
	fmt.Println("done")

	fmt.Println("=== Phase 1: Running initdb ===")
	err := runInitdb(ctx, fs, initdbWasm, postgresWasm)
	if err != nil {
		fmt.Fprintf(os.Stderr, "initdb error: %v\n", err)
	}

	// Check if initdb created the expected files
	for _, path := range []string{"/tmp/pglite/data/global/pg_control", "/tmp/pglite/data/PG_VERSION", "/tmp/pglite/data/postgresql.conf"} {
		if _, err := fs.Stat(path); err != nil {
			fmt.Printf("  MISSING: %s\n", path)
		} else {
			fmt.Printf("  EXISTS: %s\n", path)
		}
	}

	fmt.Println("\n=== Phase 2: Starting PostgreSQL ===")
	err = runPostgres(ctx, fs, postgresWasm)
	if err != nil {
		fmt.Fprintf(os.Stderr, "postgres error: %v\n", err)
	}
}

// runPostgresCmd runs a postgres subcommand. If stdoutCapture is non-nil, stdout is captured.
func runPostgresCmd(ctx context.Context, fs *vfs.FS, postgresWasm []byte, args []string, stdoutCapture *[]byte) int32 {
	r := newRuntime(ctx)
	defer r.Close(ctx)

	if stdoutCapture != nil {
		emcompat.InstantiateWASIWithCapture(ctx, r, fs, stdoutCapture)
	} else {
		emcompat.InstantiateWASI(ctx, r, fs)
	}

	compiled, err := emcompat.PrepareAndCompilePrepatched(ctx, r, postgresWasm, fs)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[pg] prepare: %v\n", err)
		return -1
	}

	mod, err := r.InstantiateModule(ctx, compiled,
		wazero.NewModuleConfig().WithName("postgres").WithStartFunctions().
			WithStdout(os.Stdout).WithStderr(os.Stderr))
	if err != nil {
		fmt.Fprintf(os.Stderr, "[pg] instantiate: %v\n", err)
		return -1
	}
	defer mod.Close(ctx)

	if relocs := mod.ExportedFunction("__wasm_apply_data_relocs"); relocs != nil {
		relocs.Call(ctx)
	}

	return callMain(ctx, mod, args)
}

func callMain(ctx context.Context, mod wazero_api.Module, args []string) int32 {
	mem := mod.Memory()
	mainFn := mod.ExportedFunction("__main_argc_argv")
	stackAlloc := mod.ExportedFunction("_emscripten_stack_alloc")
	if mainFn == nil || stackAlloc == nil {
		return -1
	}

	argvResult, _ := stackAlloc.Call(ctx, uint64((len(args)+1)*4))
	argvPtr := uint32(argvResult[0])
	for i, arg := range args {
		strResult, _ := stackAlloc.Call(ctx, uint64(len(arg)+1))
		strPtr := uint32(strResult[0])
		mem.Write(strPtr, append([]byte(arg), 0))
		mem.WriteUint32Le(argvPtr+uint32(i*4), strPtr)
	}
	mem.WriteUint32Le(argvPtr+uint32(len(args)*4), 0)

	results, err := mainFn.Call(ctx, uint64(len(args)), uint64(argvPtr))
	if err != nil {
		errStr := err.Error()
		if strings.Contains(errStr, "exit(0)") {
			return 0
		}
		if strings.Contains(errStr, "exit(") {
			// Extract exit code
			fmt.Fprintf(os.Stderr, "[main] %v\n", err)
			return 1
		}
		// _abort_js or other fatal error
		fmt.Fprintf(os.Stderr, "[main] %v\n", err)
		return -1
	}
	return int32(results[0])
}

// runPostgresWithStdin runs postgres with stdin fed from a VFS file
func runPostgresWithStdin(ctx context.Context, fs *vfs.FS, postgresWasm []byte, args []string, stdinFile string) int32 {
	// Read the stdin data from VFS
	node, err := fs.Stat(stdinFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[pg-stdin] can't stat %s: %v\n", stdinFile, err)
		return -1
	}
	stdinData := make([]byte, len(node.Data))
	copy(stdinData, node.Data)

	r := newRuntime(ctx)
	defer r.Close(ctx)

	// Create WASI with stdin from stdinData
	emcompat.InstantiateWASIWithStdin(ctx, r, fs, stdinData)

	compiled, err := emcompat.PrepareAndCompilePrepatched(ctx, r, postgresWasm, fs)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[pg-stdin] prepare: %v\n", err)
		return -1
	}

	mod, err := r.InstantiateModule(ctx, compiled,
		wazero.NewModuleConfig().WithName("postgres").WithStartFunctions().
			WithStdout(os.Stdout).WithStderr(os.Stderr))
	if err != nil {
		fmt.Fprintf(os.Stderr, "[pg-stdin] instantiate: %v\n", err)
		return -1
	}
	defer mod.Close(ctx)

	if relocs := mod.ExportedFunction("__wasm_apply_data_relocs"); relocs != nil {
		relocs.Call(ctx)
	}

	return callMain(ctx, mod, args)
}

func runInitdb(ctx context.Context, fs *vfs.FS, initdbWasm, postgresWasm []byte) error {
	r := newRuntime(ctx)
	defer r.Close(ctx)

	emcompat.InstantiateWASI(ctx, r, fs)
	compiled, err := emcompat.PrepareAndCompilePrepatched(ctx, r, initdbWasm, fs)
	if err != nil {
		return fmt.Errorf("prepare: %w", err)
	}

	// Callbacks at table[20..22]
	const cbBase = 20
	cb, err := emcompat.InstantiateInitdbCallbacks(ctx, r, cbBase)
	if err != nil {
		return fmt.Errorf("callbacks: %w", err)
	}

	const popenFile = "/tmp/.popen_output"
	var lastPgResult int32
	var pendingPgArgs []string // args for deferred popen('w') execution

	cb.OnSystem = func(ctx context.Context, cmd string) int32 {
		prog, args := emcompat.ParseSystemCommand(cmd)
		if strings.Contains(prog, "postgres") {
			fullArgs := append([]string{prog}, args...)
			fmt.Printf("[system] %s\n", strings.Join(fullArgs, " "))
			result := runPostgresCmd(ctx, fs, postgresWasm, fullArgs, nil)
			fmt.Printf("[system] -> %d\n", result)
			return result
		}
		return -1
	}

	cb.OnPopen = func(ctx context.Context, cmd string, mode string) int32 {
		prog, args := emcompat.ParseSystemCommand(cmd)
		if !strings.Contains(prog, "postgres") {
			return 0
		}
		fullArgs := append([]string{prog}, args...)
		fmt.Printf("[popen %s] %s\n", mode, strings.Join(fullArgs, " "))

		if mode == "r" {
			var captured []byte
			lastPgResult = runPostgresCmd(ctx, fs, postgresWasm, fullArgs, &captured)
			fs.WriteFile(popenFile, captured, 0o644)
			return cb.Fopen(ctx, popenFile, "r")
		} else if mode == "w" {
			// Defer postgres execution - initdb will write SQL via stdout
			// Redirect initdb's stdout to the pipe file using pgl_freopen
			pendingPgArgs = fullArgs
			freopen := cb.Module.ExportedFunction("pgl_freopen")
			if freopen != nil {
				stackAlloc := cb.Module.ExportedFunction("_emscripten_stack_alloc")
				pathR, _ := stackAlloc.Call(ctx, uint64(len(popenFile)+1))
				cb.Module.Memory().Write(uint32(pathR[0]), append([]byte(popenFile), 0))
				modeR, _ := stackAlloc.Call(ctx, 2)
				cb.Module.Memory().Write(uint32(modeR[0]), []byte("w\x00"))
				res, _ := freopen.Call(ctx, pathR[0], modeR[0], 1) // 1 = stdout
				fmt.Printf("[popen w] freopen stdout -> %d\n", int32(res[0]))
				return int32(res[0]) // Return the FILE* from freopen
			}
			return 0
		}
		return 0
	}

	cb.OnPclose = func(ctx context.Context, stream int32) int32 {
		cb.Fclose(ctx, stream)
		if len(pendingPgArgs) > 0 {
			// Flush the FILE* before reading - call fflush on initdb module
			if fflush := cb.Module.ExportedFunction("fflush"); fflush != nil {
				fflush.Call(ctx, uint64(stream))
			}

			args := pendingPgArgs
			pendingPgArgs = nil

			// Check how much data was written to the pipe
			if node, err := fs.Stat(popenFile); err == nil {
				fmt.Printf("[pclose] pipe has %d bytes of SQL\n", len(node.Data))
			}
			fmt.Printf("[pclose] running: %s\n", strings.Join(args, " "))
			// Read the written data and provide it as stdin to postgres
			lastPgResult = runPostgresWithStdin(ctx, fs, postgresWasm, args, popenFile)
			fmt.Printf("[pclose] -> %d\n", lastPgResult)
		}
		return lastPgResult
	}

	mod, err := r.InstantiateModule(ctx, compiled,
		wazero.NewModuleConfig().WithName("initdb").
			WithStdout(os.Stdout).WithStderr(os.Stderr))
	if err != nil {
		return fmt.Errorf("instantiate: %w", err)
	}
	defer mod.Close(ctx)

	cb.SetModule(mod)

	// Register callbacks
	for _, reg := range []struct{ name string; idx uint64 }{
		{"pgl_set_system_fn", cbBase + 0},
		{"pgl_set_popen_fn", cbBase + 1},
		{"pgl_set_pclose_fn", cbBase + 2},
	} {
		if fn := mod.ExportedFunction(reg.name); fn != nil {
			fn.Call(ctx, reg.idx)
		}
	}
	fmt.Println("Callbacks registered")

	args := []string{
		"/pglite/bin/initdb",
		"--allow-group-access",
		"--encoding=UTF8",
		"--locale=C.UTF-8",
		"--locale-provider=libc",
		"--auth=trust",
		"-D", "/tmp/pglite/data",
		"-U", "web_user",
	}

	fmt.Printf("Running: %s\n", strings.Join(args, " "))
	result := callMain(ctx, mod, args)
	if result != 0 {
		return fmt.Errorf("initdb exited with %d", result)
	}
	fmt.Println("initdb completed successfully")
	return nil
}

func runPostgres(ctx context.Context, fs *vfs.FS, postgresWasm []byte) error {
	r := newRuntime(ctx)
	defer r.Close(ctx)

	emcompat.InstantiateWASI(ctx, r, fs)
	compiled, err := emcompat.PrepareAndCompilePrepatched(ctx, r, postgresWasm, fs)
	if err != nil {
		return fmt.Errorf("prepare: %w", err)
	}

	mod, err := r.InstantiateModule(ctx, compiled,
		wazero.NewModuleConfig().WithName("pglite").WithStartFunctions().
			WithStdout(os.Stdout).WithStderr(os.Stderr))
	if err != nil {
		return fmt.Errorf("instantiate: %w", err)
	}
	defer mod.Close(ctx)

	if relocs := mod.ExportedFunction("__wasm_apply_data_relocs"); relocs != nil {
		relocs.Call(ctx)
	}

	args := []string{"/pglite/bin/postgres", "--single", "-D", "/tmp/pglite/data", "template1"}
	fmt.Printf("Running: %s\n", strings.Join(args, " "))
	result := callMain(ctx, mod, args)
	if result != 0 {
		return fmt.Errorf("postgres exited with %d", result)
	}
	return nil
}
