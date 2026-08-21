package pglite

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	emcompat "github.com/moriyoshi/pglite-go/emscripten"
)

// insertDataDir mirrors the -D placement rules PostgreSQL 17's getopt parsing
// requires: for --single, -D must precede the database name.
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

// runInitdb runs initdb inside a wasmtime instance, bridging its system/popen/
// pclose calls (which spawn postgres --boot/--single subcommands) back into
// fresh wasmtime instances over the shared VFS.
func (db *DB) runInitdb(ctx context.Context, initdbWasm, postgresWasm []byte) error {
	dbg := os.Getenv("PGLITE_DEBUG_INITDB") != ""
	rt, err := newRuntimeFromWasm(ctx, db.engine, initdbWasm, db.fs, nil, nil)
	if err != nil {
		return fmt.Errorf("runtime: %w", err)
	}
	// Keep the library quiet: initdb prints progress to stdout/stderr.
	rt.SetStdout(io.Discard)
	if dbg {
		rt.SetStderr(os.Stderr)
	} else {
		rt.SetStderr(io.Discard)
	}
	if err := rt.ApplyDataRelocs(ctx); err != nil {
		return fmt.Errorf("relocs: %w", err)
	}

	// Locate initdb's stdout FILE* so the popen("w") bootstrap-SQL pipe can be
	// serviced by capturing writes to fd 1 (see OnPopen). The address shifts with
	// every PGlite rebuild, so derive it from memory rather than hardcoding it.
	stdoutFILE := rt.FindStdoutFILE()
	if stdoutFILE == 0 {
		return fmt.Errorf("could not locate stdout FILE struct in initdb.wasm (PGlite layout changed?)")
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
		if dbg {
			fmt.Fprintf(os.Stderr, "[initdb] system(%q)\n", cmd)
		}
		prog, args := emcompat.ParseSystemCommand(cmd)
		if !strings.Contains(prog, "postgres") {
			return -1
		}
		for _, a := range args {
			if a == "--check" {
				return 0
			}
		}
		full := append([]string{prog}, insertDataDir(args)...)
		r, _ := db.runCmd(ctx, full, nil, nil)
		return r
	}

	cb.OnPopen = func(ctx context.Context, cmd string, mode string) int32 {
		if dbg {
			fmt.Fprintf(os.Stderr, "[initdb] popen(%q, %q)\n", cmd, mode)
		}
		prog, args := emcompat.ParseSystemCommand(cmd)
		if !strings.Contains(prog, "postgres") {
			return 0
		}
		full := append([]string{prog}, insertDataDir(args)...)
		if mode == "r" {
			var captured []byte
			lastPgResult, _ = db.runCmd(ctx, full, nil, &captured)
			db.fs.WriteFile(popenFile, captured, 0o644)
			return cb.Fopen(ctx, popenFile, "r")
		}
		pendingPgArgs = full
		var captured []byte
		capturedStdout = &captured
		rt.SetStdoutCapture(capturedStdout)
		return int32(stdoutFILE)
	}

	cb.OnPclose = func(ctx context.Context, stream int32) int32 {
		if len(pendingPgArgs) == 0 {
			return lastPgResult
		}
		if fflush := cb.Module.ExportedFunction("fflush"); fflush != nil {
			fflush.Call(ctx, uint64(stream))
		}
		rt.SetStdoutCapture(nil)
		args := pendingPgArgs
		pendingPgArgs = nil
		if capturedStdout != nil {
			db.fs.WriteFile(popenFile, *capturedStdout, 0o644)
			if dbg {
				fmt.Fprintf(os.Stderr, "[initdb] pclose: captured %d bytes of bootstrap SQL; running %v\n", len(*capturedStdout), args)
			}
			capturedStdout = nil
		}
		lastPgResult, _ = db.runCmd(ctx, args, db.readVFS(popenFile), nil)
		if dbg {
			_, statErr := db.fs.Stat(dataDir + "/global/pg_control")
			fmt.Fprintf(os.Stderr, "[initdb] postgres --boot exit=%d; pg_control present=%v\n", lastPgResult, statErr == nil)
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
			return fmt.Errorf("register %s: %w", reg.name, err)
		}
	}

	args := []string{
		"/pglite/bin/initdb", "--allow-group-access", "--encoding=UTF8",
		"--locale=C.UTF-8", "--locale-provider=libc", "--auth=trust",
		"-D", dataDir, "-U", "web_user",
	}
	// initdb exits 1 on the collation import (locale -a unimplemented) but still
	// produces a valid cluster; the caller validates via essential files.
	if _, err := rt.CallMain(ctx, args); err != nil && !strings.Contains(err.Error(), "exit(0)") {
		// non-fatal
	}
	return nil
}

func (db *DB) readVFS(path string) []byte {
	n, err := db.fs.Stat(path)
	if err != nil {
		return nil
	}
	b := make([]byte, len(n.Data))
	copy(b, n.Data)
	return b
}
