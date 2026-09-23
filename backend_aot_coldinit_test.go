//go:build aot

package pglite

import (
	"fmt"
	"testing"
)

// TestAOTColdInit exercises the full cold path through the pure-Go AOT backend:
// an empty persist dir means no cluster exists, so Open runs initdb (transpiled
// internal/initdbwasm) — which spawns `postgres --boot` in fresh AOT pgwasm
// instances over the shared VFS to bootstrap the catalog — then starts the wire
// backend and runs a query. No pre-existing cluster, no CGo, no wasm engine.
func TestAOTColdInit(t *testing.T) {
	dir := t.TempDir() // empty -> not a persisted cluster -> initdb runs
	db, err := Open(Config{
		WasmDir:    "wasm",
		PersistDir: dir,
		Database:   "template1",
	})
	if err != nil {
		t.Fatalf("Open (cold init): %v", err)
	}
	defer db.Close()

	rows, err := db.Query("SELECT 42")
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	t.Logf("cols=%v cmd=%q rows=%v", rows.Columns, rows.Command, rows.Rows)
	if len(rows.Rows) != 1 || len(rows.Rows[0]) != 1 || fmt.Sprint(rows.Rows[0][0]) != "42" {
		t.Fatalf("unexpected result: %v", rows.Rows)
	}
	t.Log("COLD INIT + QUERY through the pure-Go AOT backend (initdb created the cluster)")
}
