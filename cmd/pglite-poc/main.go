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

// dataPersistDir returns the host directory where the initialized PostgreSQL
// data directory is persisted between runs.
func dataPersistDir() string {
	if dir, err := os.UserCacheDir(); err == nil {
		return dir + "/pglite-go/data"
	}
	return ".pglite-data"
}

// isPersistedCluster reports whether persistDir holds a previously-initialized
// cluster (identified by the presence of PG_VERSION).
func isPersistedCluster(persistDir string) bool {
	_, err := os.Stat(persistDir + "/PG_VERSION")
	return err == nil
}

// runInitdbAndPersist runs initdb and, on success, saves the resulting data
// directory to persistDir so subsequent starts can skip initdb.
func runInitdbAndPersist(ctx context.Context, fs *vfs.FS, initdbWasm, postgresWasm []byte, dataDir, persistDir string) {
	// initdb currently exits 1 on the post-bootstrap collation import (a known
	// WASM-locale limitation), but the cluster it produces is otherwise valid.
	// So persist based on the presence of the essential cluster files in the
	// VFS rather than on initdb's exit code.
	if err := runInitdb(ctx, fs, initdbWasm, postgresWasm); err != nil {
		fmt.Fprintf(os.Stderr, "initdb reported: %v (continuing if cluster looks valid)\n", err)
	}
	for _, f := range []string{"/PG_VERSION", "/global/pg_control"} {
		if _, err := fs.Stat(dataDir + f); err != nil {
			fmt.Fprintf(os.Stderr, "not persisting: cluster incomplete (missing %s)\n", f)
			return
		}
	}
	if err := fs.SaveSubtree(dataDir, persistDir); err != nil {
		fmt.Fprintf(os.Stderr, "warning: failed to persist data dir to %s: %v\n", persistDir, err)
	} else {
		fmt.Printf("Persisted data dir to %s\n", persistDir)
	}
}

// wasmCacheDir returns a stable, per-user directory for wazero's persistent
// compilation cache. Falls back to a repo-local dir if the OS cache dir is
// unavailable.
func wasmCacheDir() string {
	if dir, err := os.UserCacheDir(); err == nil {
		return dir + "/pglite-go/wazero"
	}
	return ".wazero-cache"
}

func newRuntime(ctx context.Context) wazero.Runtime {
	config := wazero.NewRuntimeConfig().WithCompilationCache(compilationCache)
	return wazero.NewRuntimeWithConfig(ctx, config)
}

func main() {
	wasmDir := "wasm"
	if len(os.Args) > 1 {
		wasmDir = os.Args[1]
	}

	// Persist the AOT-compiled native code to disk so the ~90s compile of
	// pglite.wasm (8.7MB) is paid only once, not on every process restart.
	// Cache contents are wazero-version-specific; the dir is created if absent.
	cacheDir := wasmCacheDir()
	if c, err := wazero.NewCompilationCacheWithDir(cacheDir); err == nil {
		compilationCache = c
	} else {
		fmt.Fprintf(os.Stderr, "warning: file cache at %s unavailable (%v); using in-memory cache\n", cacheDir, err)
		compilationCache = wazero.NewCompilationCache()
	}
	defer compilationCache.Close(context.Background())

	fs := vfs.New()
	fs.LoadManifest(wasmDir+"/pglite.manifest.json", wasmDir+"/pglite.data")
	fs.MkdirAll("/tmp/pglite/data", 0o700)
	fs.MkdirAll("/dev", 0o755)
	fs.WriteFile("/dev/null", nil, 0o666)
	fs.WriteFile("/dev/urandom", nil, 0o666)
	fs.MkdirAll("/home/web_user", 0o755)

	// Back linear memory with an mmap allocator (on supported platforms) so
	// memory.grow reslices the reserved mapping instead of realloc+memmove'ing
	// the whole linear memory on every heap growth. The allocator is read from
	// the context at instantiation time, so wrap the base context once here.
	ctx := withMemoryAllocator(context.Background())
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

	// Persist the initdb-created data dir to the host so initdb (the ~9s
	// dominant startup cost) runs only once. If a saved data dir exists, load
	// it and skip initdb entirely — turning startup into the ~1.6s
	// "open existing DB" path.
	const dataDir = "/tmp/pglite/data"
	persistDir := dataPersistDir()
	if isPersistedCluster(persistDir) {
		fmt.Printf("=== Phase 1: Loading persisted data dir from %s (skipping initdb) ===\n", persistDir)
		if err := fs.LoadSubtree(persistDir, dataDir); err != nil {
			fmt.Fprintf(os.Stderr, "load persisted data dir failed (%v); falling back to initdb\n", err)
			runInitdbAndPersist(ctx, fs, initdbWasm, postgresWasm, dataDir, persistDir)
		}
	} else {
		fmt.Println("=== Phase 1: Running initdb ===")
		runInitdbAndPersist(ctx, fs, initdbWasm, postgresWasm, dataDir, persistDir)
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
	if err := runPostgres(ctx, fs, postgresWasm); err != nil {
		fmt.Fprintf(os.Stderr, "postgres error: %v\n", err)
	}
}

// runPostgresCmd runs a postgres subcommand. If stdoutCapture is non-nil, stdout is captured.
// insertDataDir adds -D /tmp/pglite/data to postgres args.
// For --boot and --single, -D must come before the flag.
// For --check, we skip (returning 0 from OnSystem instead).
// For -V, -D is not needed.
func insertDataDir(args []string) []string {
	for _, a := range args {
		if a == "-D" {
			return args
		}
	}
	// Don't add -D for version check
	for _, a := range args {
		if a == "-V" || a == "--version" {
			return args
		}
	}
	// Use -D at the end. For --single mode, insert before the database name.
	// For --boot, append at end (no database name follows).
	for i, a := range args {
		if a == "--single" {
			// Find the database name (last non-flag arg)
			// Insert -D before the database name
			for j := len(args) - 1; j > i; j-- {
				if !strings.HasPrefix(args[j], "-") {
					result := make([]string, 0, len(args)+2)
					result = append(result, args[:j]...)
					result = append(result, "-D", "/tmp/pglite/data")
					result = append(result, args[j:]...)
					return result
				}
			}
		}
	}
	// For other modes (--boot), just append
	return append(args, "-D", "/tmp/pglite/data")
}

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
		wazero.NewModuleConfig().WithName("postgres").
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
		wazero.NewModuleConfig().WithName("postgres").
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

	wasiInst, _ := emcompat.InstantiateWASIReturning(ctx, r, fs)
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
	var pendingPgArgs []string
	var capturedStdout *[]byte

	cb.OnSystem = func(ctx context.Context, cmd string) int32 {
		prog, args := emcompat.ParseSystemCommand(cmd)
		if strings.Contains(prog, "postgres") {
			// For --check commands, return 0 (success) to skip probing
			// This accepts the first proposed value for max_connections/shared_buffers
			for _, a := range args {
				if a == "--check" {
					return 0
				}
			}
			args = insertDataDir(args)
			fullArgs := append([]string{prog}, args...)
			fmt.Printf("[system] %s\n", strings.Join(fullArgs, " "))
			result := runPostgresCmd(ctx, fs, postgresWasm, fullArgs, nil)
			fmt.Printf("[system] -> %d\n", result)
			return result
		}
		return -1
	}

	cb.OnPopen = func(ctx context.Context, cmd string, mode string) int32 {
		fmt.Printf("[popen raw] %q\n", cmd)
		prog, args := emcompat.ParseSystemCommand(cmd)
		if !strings.Contains(prog, "postgres") {
			return 0
		}
		args = insertDataDir(args)
		fullArgs := append([]string{prog}, args...)
		fmt.Printf("[popen %s] %s\n", mode, strings.Join(fullArgs, " "))

		if mode == "r" {
			var captured []byte
			lastPgResult = runPostgresCmd(ctx, fs, postgresWasm, fullArgs, &captured)
			fs.WriteFile(popenFile, captured, 0o644)
			return cb.Fopen(ctx, popenFile, "r")
		} else if mode == "w" {
			// Capture stdout to a buffer - initdb writes bootstrap SQL via printf/puts
			pendingPgArgs = fullArgs
			var captured []byte
			capturedStdout = &captured
			wasiInst.SetStdoutCapture(capturedStdout)
			// Return stdout FILE* (1 in Emscripten convention? Actually return the
			// static stdout FILE struct address from initdb globals)
			// Use global[5] which is the stdout FILE pointer
			return 29560 // stdout FILE* address (from the disassembly)
		}
		return 0
	}

	cb.OnPclose = func(ctx context.Context, stream int32) int32 {
		if len(pendingPgArgs) > 0 {
			// Flush stdout before reading captured data
			if fflush := cb.Module.ExportedFunction("fflush"); fflush != nil {
				fflush.Call(ctx, uint64(stream))
			}

			// Stop capturing stdout
			wasiInst.SetStdoutCapture(nil)

			args := pendingPgArgs
			pendingPgArgs = nil

			// Write captured stdout to the pipe file
			if capturedStdout != nil {
				fs.WriteFile(popenFile, *capturedStdout, 0o644)
				fmt.Printf("[pclose] captured %d bytes of SQL\n", len(*capturedStdout))
				capturedStdout = nil
			}

			fmt.Printf("[pclose] running: %s\n", strings.Join(args, " "))
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

// runPostgresWithSQL runs postgres in single-user mode with SQL as stdin
func runPostgresWithSQL(ctx context.Context, fs *vfs.FS, postgresWasm []byte, args []string, sql string, stdoutBuf *[]byte) int32 {
	r := newRuntime(ctx)
	defer r.Close(ctx)

	emcompat.InstantiateWASIWithStdinAndCapture(ctx, r, fs, []byte(sql), stdoutBuf)

	compiled, err := emcompat.PrepareAndCompilePrepatched(ctx, r, postgresWasm, fs)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[pg-sql] prepare: %v\n", err)
		return -1
	}

	mod, err := r.InstantiateModule(ctx, compiled,
		wazero.NewModuleConfig().WithName("postgres").
			WithStdout(os.Stdout).WithStderr(os.Stderr))
	if err != nil {
		fmt.Fprintf(os.Stderr, "[pg-sql] instantiate: %v\n", err)
		return -1
	}
	defer mod.Close(ctx)

	return callMain(ctx, mod, args)
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
		wazero.NewModuleConfig().WithName("pglite").
			WithStdout(os.Stdout).WithStderr(os.Stderr))
	if err != nil {
		return fmt.Errorf("instantiate: %w", err)
	}
	defer mod.Close(ctx)

	if relocs := mod.ExportedFunction("__wasm_apply_data_relocs"); relocs != nil {
		relocs.Call(ctx)
	}

	// Run postgres in single-user mode with a test SQL query as stdin
	sql := "SELECT 1 + 1 AS result;\n"
	fmt.Printf("Executing SQL: %s", sql)

	args := []string{"/pglite/bin/postgres", "--single", "-D", "/tmp/pglite/data", "template1"}
	fmt.Printf("Running: %s\n", strings.Join(args, " "))

	// Provide SQL via stdin
	var stdoutBuf []byte
	result := runPostgresWithSQL(ctx, fs, postgresWasm, args, sql, &stdoutBuf)
	fmt.Printf("postgres returned: %d\n", result)
	if len(stdoutBuf) > 0 {
		fmt.Printf("Output:\n%s\n", string(stdoutBuf))
	}
	return nil
}
