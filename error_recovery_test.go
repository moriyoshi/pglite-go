//go:build !wazero && !aot

package pglite

import (
	"bufio"
	"net"
	"strings"
	"testing"
)

// TestSQLErrorRecovers verifies that a SQL error surfaces as a proper PostgreSQL
// error (not a raw wasm exit(100) trap) and leaves the session alive for the
// next query, on the direct Query path.
func TestSQLErrorRecovers(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	if _, err := db.Query("SELECT 1"); err != nil {
		t.Fatalf("pre-error query: %v", err)
	}

	_, err := db.Query("SELECT * FROM no_such_table")
	if err == nil {
		t.Fatalf("expected an error for missing table")
	}
	if !strings.Contains(err.Error(), "no_such_table") {
		t.Fatalf("expected a PostgreSQL relation error, got: %v", err)
	}

	// The session must survive the error.
	r, err := db.Query("SELECT 2")
	if err != nil {
		t.Fatalf("post-error query: %v", err)
	}
	if len(r.Rows) != 1 || r.Rows[0][0] != int64(2) {
		t.Fatalf("post-error query returned %v", r.Rows)
	}

	// A syntax error must recover too.
	if _, err := db.Query("SELCT bogus"); err == nil {
		t.Fatalf("expected a syntax error")
	}
	if _, err := db.Query("SELECT 3"); err != nil {
		t.Fatalf("post-syntax-error query: %v", err)
	}
}

// TestServeConnSQLError verifies that over a real socket a SQL error is relayed
// as an ErrorResponse + ReadyForQuery and the connection stays open for further
// queries, rather than being torn down.
func TestServeConnSQLError(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	srvErr := make(chan error, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			srvErr <- err
			return
		}
		srvErr <- db.ServeConn(c)
	}()

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	br := bufio.NewReader(conn)

	if _, err := conn.Write(startupMessage("postgres", db.database)); err != nil {
		t.Fatalf("write startup: %v", err)
	}
	drainToReady(t, br)

	// A failing query must come back as ErrorResponse ('E') then
	// ReadyForQuery ('Z'), not close the connection.
	if _, err := conn.Write(queryMessage("SELECT * FROM no_such_table")); err != nil {
		t.Fatalf("write bad query: %v", err)
	}
	sawError := false
	for {
		typ, payload := readBackendMessage(t, br)
		if typ == 'E' {
			sawError = true
			if msg := decodeError(payload); !strings.Contains(msg, "no_such_table") {
				t.Fatalf("unexpected error payload: %s", msg)
			}
		}
		if typ == 'Z' {
			break
		}
	}
	if !sawError {
		t.Fatalf("no ErrorResponse relayed to client")
	}

	// The connection must still be usable.
	if _, err := conn.Write(queryMessage("SELECT 2")); err != nil {
		t.Fatalf("write follow-up query: %v", err)
	}
	resp := drainToReady(t, br)
	if _, ok := resp['D']; !ok {
		t.Fatalf("no DataRow for follow-up query after error")
	}

	conn.Write([]byte{'X', 0, 0, 0, 4})
	conn.Close()
	if err := <-srvErr; err != nil {
		t.Fatalf("ServeConn: %v", err)
	}
}
