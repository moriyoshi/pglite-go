//go:build wasmtime

// Command pglite-wasmtime runs the full PGlite flow (initdb + a SQL query) on
// wasmtime instead of wazero, to validate the execution port end-to-end and
// benchmark it against the wazero PoC.
//
//	go run -tags wasmtime ./cmd/pglite-wasmtime
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/bytecodealliance/wasmtime-go/v34"

	emcompat "github.com/moriyoshi/pglite-go/emscripten"
	"github.com/moriyoshi/pglite-go/vfs"
)

const dataDir = "/tmp/pglite/data"

var (
	engine     *wasmtime.Engine
	postgresMod *wasmtime.Module // compiled once, reused across all subcommands
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
	fs.MkdirAll(dataDir, 0o700)
	fs.MkdirAll("/dev", 0o755)
	fs.WriteFile("/dev/null", nil, 0o666)
	fs.WriteFile("/dev/urandom", nil, 0o666)
	fs.MkdirAll("/home/web_user", 0o755)

	ctx := context.Background()
	postgresWasm, _ := os.ReadFile(wasmDir + "/pglite.wasm")
	initdbWasm, _ := os.ReadFile(wasmDir + "/initdb.wasm")

	engine = wasmtime.NewEngine()

	total := time.Now()
	comp := time.Now()
	var cerr error
	var cached bool
	if postgresMod, cached, cerr = compileCached(engine, postgresWasm, "pglite"); cerr != nil {
		fmt.Fprintf(os.Stderr, "compile pglite.wasm: %v\n", cerr)
		os.Exit(1)
	}
	how := "compiled"
	if cached {
		how = "loaded from cache"
	}
	fmt.Printf("[wasmtime] %s pglite.wasm in %.2fs (reused across subcommands)\n", how, time.Since(comp).Seconds())

	fmt.Println("=== Phase 1: initdb (on wasmtime) ===")
	if err := runInitdb(ctx, fs, initdbWasm, postgresWasm); err != nil {
		fmt.Fprintf(os.Stderr, "initdb: %v\n", err)
	}
	for _, p := range []string{dataDir + "/global/pg_control", dataDir + "/PG_VERSION", dataDir + "/postgresql.conf"} {
		if _, err := fs.Stat(p); err != nil {
			fmt.Printf("  MISSING: %s\n", p)
		} else {
			fmt.Printf("  EXISTS: %s\n", p)
		}
	}

	fmt.Println("\n=== Phase 2: SELECT 1+1 (on wasmtime) ===")
	sql := "SELECT 1 + 1 AS result;\n"
	args := []string{"/pglite/bin/postgres", "--single", "-D", dataDir, "template1"}
	var out []byte
	code := runWithStdin(ctx, fs, postgresWasm, args, []byte(sql), &out)
	fmt.Printf("postgres returned: %d\n", code)
	if len(out) > 0 {
		fmt.Printf("Output:\n%s\n", string(out))
	}
	fmt.Printf("\n[wasmtime] total wall time: %.2fs\n", time.Since(total).Seconds())
}

// runCmd runs a postgres subcommand in a fresh wasmtime instance, reusing the
// once-compiled postgres module (the wasm arg is ignored for postgres).
func runCmd(ctx context.Context, fs *vfs.FS, wasm []byte, args []string, stdin []byte, stdoutCapture *[]byte) int32 {
	rt, err := emcompat.NewWTRuntimeFromModule(ctx, engine, postgresMod, fs, stdin, stdoutCapture)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[pg] runtime: %v\n", err)
		return -1
	}
	if err := rt.ApplyDataRelocs(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "[pg] relocs: %v\n", err)
	}
	code, err := rt.CallMain(ctx, args)
	if err != nil {
		msg := err.Error()
		if strings.Contains(msg, "exit(0)") {
			return 0
		}
		if strings.Contains(msg, "exit(") {
			return 1
		}
		fmt.Fprintf(os.Stderr, "[pg] main: %v\n", err)
		return -1
	}
	return code
}

func runWithStdin(ctx context.Context, fs *vfs.FS, wasm []byte, args []string, stdin []byte, out *[]byte) int32 {
	return runCmd(ctx, fs, wasm, args, stdin, out)
}

// insertDataDir mirrors cmd/pglite-poc: -D placement is order-sensitive.
func insertDataDir(args []string) []string {
	for _, a := range args {
		if a == "-D" {
			return args
		}
	}
	for _, a := range args {
		if a == "-V" || a == "--version" {
			return args
		}
	}
	for i, a := range args {
		if a == "--single" {
			for j := len(args) - 1; j > i; j-- {
				if !strings.HasPrefix(args[j], "-") {
					result := make([]string, 0, len(args)+2)
					result = append(result, args[:j]...)
					result = append(result, "-D", dataDir)
					result = append(result, args[j:]...)
					return result
				}
			}
		}
	}
	return append(args, "-D", dataDir)
}

func runInitdb(ctx context.Context, fs *vfs.FS, initdbWasm, postgresWasm []byte) error {
	rt, err := emcompat.NewWTRuntime(ctx, engine, initdbWasm, fs, nil, nil)
	if err != nil {
		return fmt.Errorf("runtime: %w", err)
	}
	if err := rt.ApplyDataRelocs(ctx); err != nil {
		return fmt.Errorf("relocs: %w", err)
	}

	cb := &emcompat.InitdbCallbacks{}
	cbBase, err := rt.RegisterInitdbCallbacks(cb)
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
			for _, a := range args {
				if a == "--check" {
					return 0
				}
			}
			args = insertDataDir(args)
			full := append([]string{prog}, args...)
			fmt.Printf("[system] %s\n", strings.Join(full, " "))
			r := runCmd(ctx, fs, postgresWasm, full, nil, nil)
			fmt.Printf("[system] -> %d\n", r)
			return r
		}
		return -1
	}

	cb.OnPopen = func(ctx context.Context, cmd string, mode string) int32 {
		prog, args := emcompat.ParseSystemCommand(cmd)
		if !strings.Contains(prog, "postgres") {
			return 0
		}
		args = insertDataDir(args)
		full := append([]string{prog}, args...)
		fmt.Printf("[popen %s] %s\n", mode, strings.Join(full, " "))

		if mode == "r" {
			var captured []byte
			lastPgResult = runCmd(ctx, fs, postgresWasm, full, nil, &captured)
			fs.WriteFile(popenFile, captured, 0o644)
			return cb.Fopen(ctx, popenFile, "r")
		}
		// mode "w": capture initdb's stdout (bootstrap SQL) until pclose.
		pendingPgArgs = full
		var captured []byte
		capturedStdout = &captured
		rt.SetStdoutCapture(capturedStdout)
		return 29560 // stdout FILE* address (from disassembly, same binary)
	}

	cb.OnPclose = func(ctx context.Context, stream int32) int32 {
		if len(pendingPgArgs) > 0 {
			if fflush := cb.Module.ExportedFunction("fflush"); fflush != nil {
				fflush.Call(ctx, uint64(stream))
			}
			rt.SetStdoutCapture(nil)
			args := pendingPgArgs
			pendingPgArgs = nil
			if capturedStdout != nil {
				fs.WriteFile(popenFile, *capturedStdout, 0o644)
				fmt.Printf("[pclose] captured %d bytes of SQL\n", len(*capturedStdout))
				capturedStdout = nil
			}
			fmt.Printf("[pclose] running: %s\n", strings.Join(args, " "))
			lastPgResult = runCmd(ctx, fs, postgresWasm, args, mustRead(fs, popenFile), nil)
			fmt.Printf("[pclose] -> %d\n", lastPgResult)
		}
		return lastPgResult
	}

	for _, reg := range []struct {
		name string
		idx  int32
	}{
		{"pgl_set_system_fn", int32(cbBase) + 0},
		{"pgl_set_popen_fn", int32(cbBase) + 1},
		{"pgl_set_pclose_fn", int32(cbBase) + 2},
	} {
		if _, err := rt.CallExport(reg.name, reg.idx); err != nil {
			fmt.Fprintf(os.Stderr, "register %s: %v\n", reg.name, err)
		}
	}

	args := []string{
		"/pglite/bin/initdb", "--allow-group-access", "--encoding=UTF8",
		"--locale=C.UTF-8", "--locale-provider=libc", "--auth=trust",
		"-D", dataDir, "-U", "web_user",
	}
	fmt.Printf("Running: %s\n", strings.Join(args, " "))
	code, err := rt.CallMain(ctx, args)
	if err != nil && !strings.Contains(err.Error(), "exit(0)") {
		fmt.Fprintf(os.Stderr, "initdb main: %v\n", err)
	}
	fmt.Printf("initdb exit: %d\n", code)
	return nil
}

// compileCached compiles wasm to a wasmtime.Module, persisting the serialized
// native code to disk so subsequent runs deserialize (fast) instead of
// recompiling. The cache key includes the wasm content hash; wasmtime's blob
// additionally self-validates the runtime version + host CPU, so a stale/foreign
// blob is rejected and we transparently recompile. Returns (module, fromCache).
func compileCached(engine *wasmtime.Engine, wasm []byte, name string) (*wasmtime.Module, bool, error) {
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
				_ = os.Rename(tmp, path) // atomic publish
			}
		}
	}
	return mod, false, nil
}

func mustRead(fs *vfs.FS, path string) []byte {
	n, err := fs.Stat(path)
	if err != nil {
		return nil
	}
	b := make([]byte, len(n.Data))
	copy(b, n.Data)
	return b
}
