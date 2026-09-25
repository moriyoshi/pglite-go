//go:build aot

package pglite

import (
	"fmt"
	"testing"
)

// TestAOTQuery warm-loads a pre-initialized cluster and runs a real query through
// the AOT (pure-Go, wasm2go) backend — the full path: Open -> LoadSubtree ->
// startWire (ApplyDataRelocs, RegisterRWCallbacks->T0, CallMain, pgl_startPGlite,
// the PostgresMainLoopOnce wire loop with cooperative longjmp) -> Query.
func TestAOTQuery(t *testing.T) {
	db, err := Open(Config{
		WasmDir:    "wasm",
		PersistDir: "/tmp/aot-warm",
		Database:   "template1",
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	rows, err := db.Query("SELECT 42")
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	t.Logf("columns=%v command=%q rows=%v", rows.Columns, rows.Command, rows.Rows)
	if len(rows.Rows) != 1 || len(rows.Rows[0]) != 1 {
		t.Fatalf("unexpected shape: %v", rows.Rows)
	}
	if got := fmt.Sprint(rows.Rows[0][0]); got != "42" {
		t.Fatalf("got %q, want 42", got)
	}
	t.Log("LIVE QUERY through the pure-Go AOT backend")
}
