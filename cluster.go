//go:build !wazero

package pglite

import (
	"context"
	"fmt"
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
	rt, err := emcompat.NewWTRuntime(ctx, db.engine, initdbWasm, db.fs, nil, nil)
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
		return 29560 // stdout FILE* address (from the initdb disassembly)
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
			capturedStdout = nil
		}
		lastPgResult, _ = db.runCmd(ctx, args, db.readVFS(popenFile), nil)
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
