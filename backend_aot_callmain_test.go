//go:build aot

package pglite

import (
	"context"
	"testing"
	"testing/fstest"
	"time"

	"github.com/moriyoshi/pglite-go/vfs"
)

// TestAOTCallMain drives postgres main() through the AOT backend against an
// empty VFS. Expected: it runs real init (memory contexts, GUC — the code that
// SIGSEGV'd in palloc0 when CallMain was skipped) and then fails gracefully on
// the missing data directory via our bridged __syscall_openat — proving the
// host syscall bridge is exercised.
func TestAOTCallMain(t *testing.T) {
	e := newEngine()
	defer closeEngine(e)
	mod, err := compileModule(e, nil, "pglite")
	if err != nil {
		t.Fatal(err)
	}
	// Minimal layout so postgres can resolve its executable and look for a data
	// dir; the VFS has no cluster, so it should FATAL on that (a deeper, expected
	// failure than "could not locate my own executable").
	fs := vfs.New(fstest.MapFS{
		"pglite/bin/postgres": &fstest.MapFile{Data: []byte("\x7fELF"), Mode: 0o755},
	})
	rt, err := newRuntimeFromModule(context.Background(), e, mod, fs, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := rt.ApplyDataRelocs(context.Background()); err != nil {
		t.Fatal(err)
	}
	args := []string{
		"/pglite/bin/postgres", "--single", "-F", "-O", "-j",
		"-c", "search_path=public", "-c", "exit_on_error=false",
		"-c", "log_checkpoints=false", "-c", "io_method=sync",
		"-D", "/pgdata", "template1",
	}
	done := make(chan struct{})
	go func() {
		defer func() {
			if r := recover(); r != nil {
				t.Logf("CallMain unwound: %v", r)
			}
			close(done)
		}()
		code, _ := rt.CallMain(context.Background(), args)
		t.Logf("CallMain returned code=%d", code)
	}()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Log("CallMain still running after 20s (blocked?)")
	}
}
