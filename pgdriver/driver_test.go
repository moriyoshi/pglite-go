//go:build !wazero

package pgdriver_test

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	_ "github.com/moriyoshi/pglite-go/pgdriver"
)

// wasmDir locates the repo's wasm assets relative to this package.
func wasmDir(t *testing.T) string {
	dir := filepath.Join("..", "wasm")
	if _, err := os.Stat(filepath.Join(dir, "pglite.wasm")); err != nil {
		t.Skipf("wasm assets not found at %s (run scripts/download-wasm.sh): %v", dir, err)
	}
	abs, _ := filepath.Abs(dir)
	return abs
}

// TestDriverCRUD exercises the "pglite" database/sql driver end to end:
// DDL, parameterized inserts, typed scanning, and NULL handling — all against
// one in-memory cluster shared across statements.
func TestDriverCRUD(t *testing.T) {
	db, err := sql.Open("pglite", "dir="+wasmDir(t)+" ephemeral=true")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	db.SetMaxOpenConns(1)
	defer db.Close()

	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.Exec(q, args...); err != nil {
			t.Fatalf("exec %q: %v", q, err)
		}
	}
	exec("CREATE TABLE t (id int primary key, name text, score float8, active bool)")
	exec("INSERT INTO t VALUES ($1,$2,$3,$4)", 1, "alice", 9.5, true)
	exec("INSERT INTO t VALUES ($1,$2,$3,$4)", 2, "bob", nil, false)

	// RowsAffected comes from the wire CommandComplete tag.
	res, err := db.Exec("UPDATE t SET active = true")
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if n, _ := res.RowsAffected(); n != 2 {
		t.Errorf("UPDATE RowsAffected = %d, want 2", n)
	}
	exec("UPDATE t SET active = false WHERE id = 2") // restore for the checks below

	rows, err := db.Query("SELECT id, name, score, active FROM t ORDER BY id")
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()

	type row struct {
		id     int
		name   string
		score  sql.NullFloat64
		active bool
	}
	var got []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.name, &r.score, &r.active); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got = append(got, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}

	if len(got) != 2 {
		t.Fatalf("got %d rows, want 2", len(got))
	}
	if got[0] != (row{1, "alice", sql.NullFloat64{Float64: 9.5, Valid: true}, true}) {
		t.Errorf("row0 = %+v", got[0])
	}
	if got[1].id != 2 || got[1].name != "bob" || got[1].score.Valid || got[1].active {
		t.Errorf("row1 = %+v (want id=2 name=bob score=NULL active=false)", got[1])
	}

	var n int
	if err := db.QueryRow("SELECT count(*) FROM t WHERE active = $1", true).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Errorf("count(active) = %d, want 1", n)
	}
}

// TestBinaryResultFormats checks that parameterized queries (extended protocol)
// decode binary-format values into the right Go types across int/float/bool/bytea.
func TestBinaryResultFormats(t *testing.T) {
	db, err := sql.Open("pglite", "dir="+wasmDir(t)+" ephemeral=true")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	db.SetMaxOpenConns(1)
	defer db.Close()

	// $1 forces the extended protocol (binary result formats).
	var (
		i2  int16
		i4  int32
		i8  int64
		f4  float32
		f8  float64
		b   bool
		raw []byte
	)
	row := db.QueryRow(`SELECT 32000::int2, 70000::int4, 5000000000::int8,
		1.5::float4, 2.25::float8, true, '\xdeadbeef'::bytea WHERE $1`, true)
	if err := row.Scan(&i2, &i4, &i8, &f4, &f8, &b, &raw); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if i2 != 32000 || i4 != 70000 || i8 != 5000000000 {
		t.Errorf("ints = %d,%d,%d", i2, i4, i8)
	}
	if f4 != 1.5 || f8 != 2.25 {
		t.Errorf("floats = %v,%v", f4, f8)
	}
	if !b {
		t.Errorf("bool = %v", b)
	}
	if string(raw) != "\xde\xad\xbe\xef" {
		t.Errorf("bytea = % x, want de ad be ef", raw)
	}
}

// TestDriverTransaction verifies real transactions on the persistent backend:
// commit persists, rollback discards.
func TestDriverTransaction(t *testing.T) {
	db, err := sql.Open("pglite", "dir="+wasmDir(t)+" ephemeral=true")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	db.SetMaxOpenConns(1) // single backend = single connection
	defer db.Close()

	if _, err := db.Exec("CREATE TABLE acct (id int, bal int)"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("INSERT INTO acct VALUES (1, 100)"); err != nil {
		t.Fatal(err)
	}

	count := func() (n int) {
		if err := db.QueryRow("SELECT bal FROM acct WHERE id = 1").Scan(&n); err != nil {
			t.Fatal(err)
		}
		return
	}

	// Rollback discards.
	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := tx.Exec("UPDATE acct SET bal = 999 WHERE id = 1"); err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if got := count(); got != 100 {
		t.Errorf("after rollback bal = %d, want 100", got)
	}

	// Commit persists.
	tx, err = db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := tx.Exec("UPDATE acct SET bal = 250 WHERE id = 1"); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if got := count(); got != 250 {
		t.Errorf("after commit bal = %d, want 250", got)
	}
}
