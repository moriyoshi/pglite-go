//go:build !wazero && !aot

package pglite

import (
	"bufio"
	"encoding/binary"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
)

// openTestDB opens an ephemeral in-memory cluster from the repo's wasm assets.
func openTestDB(t *testing.T) *DB {
	t.Helper()
	dir, _ := filepath.Abs("wasm")
	if _, err := os.Stat(filepath.Join(dir, "pglite.wasm")); err != nil {
		t.Skipf("wasm assets not found at %s: %v", dir, err)
	}
	db, err := Open(Config{WasmDir: dir, Ephemeral: true})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	return db
}

// readBackendMessage reads one typed backend message off a socket.
func readBackendMessage(t *testing.T, br *bufio.Reader) (byte, []byte) {
	t.Helper()
	var hdr [5]byte
	if _, err := io.ReadFull(br, hdr[:]); err != nil {
		t.Fatalf("read header: %v", err)
	}
	n := binary.BigEndian.Uint32(hdr[1:5])
	payload := make([]byte, n-4)
	if _, err := io.ReadFull(br, payload); err != nil {
		t.Fatalf("read payload: %v", err)
	}
	return hdr[0], payload
}

// drainToReady reads messages until ReadyForQuery ('Z'), returning them keyed
// by type (last one wins) and failing on an ErrorResponse ('E').
func drainToReady(t *testing.T, br *bufio.Reader) map[byte][]byte {
	t.Helper()
	msgs := map[byte][]byte{}
	for {
		typ, payload := readBackendMessage(t, br)
		if typ == 'E' {
			t.Fatalf("server error: %s", decodeError(payload))
		}
		msgs[typ] = payload
		if typ == 'Z' {
			return msgs
		}
	}
}

// TestServeConn drives ServeConn as a real PostgreSQL client would: a full
// startup handshake over a TCP socket, then a simple query and an extended
// (parameterized) query, asserting the decoded wire responses.
func TestServeConn(t *testing.T) {
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
	// Closing conn first (defers run LIFO) unblocks ServeConn's read so it
	// releases db.mu before the deferred db.Close() tries to acquire it.
	defer conn.Close()
	br := bufio.NewReader(conn)

	// Startup handshake.
	if _, err := conn.Write(startupMessage("postgres", db.database)); err != nil {
		t.Fatalf("write startup: %v", err)
	}
	start := drainToReady(t, br)
	if _, ok := start['R']; !ok {
		t.Fatalf("no AuthenticationOk in startup reply")
	}
	if _, ok := start['K']; !ok {
		t.Fatalf("no BackendKeyData in startup reply")
	}

	// Simple query.
	if _, err := conn.Write(queryMessage("SELECT 42 AS n, 'hi'::text AS s")); err != nil {
		t.Fatalf("write query: %v", err)
	}
	resp := drainToReady(t, br)
	names, oids, formats := decodeRowDesc(resp['T'])
	if len(names) != 2 || names[0] != "n" || names[1] != "s" {
		t.Fatalf("unexpected columns: %v", names)
	}
	row := decodeDataRow(resp['D'], oids, formats)
	// decodeText maps by OID, so int4 still decodes to int64 even over the
	// simple protocol's text format.
	if got := row[0]; got != int64(42) {
		t.Fatalf("col n = %v (%T), want int64(42)", got, got)
	}
	if got := row[1]; got != "hi" {
		t.Fatalf("col s = %v, want \"hi\"", got)
	}

	// Extended (parameterized) query: Parse/Bind/Describe/Execute/Sync in one
	// write, exercising ServeConn's message batching.
	var ext []byte
	ext = append(ext, parseMessage("SELECT $1::int + 1 AS v")...)
	ext = append(ext, bindMessage([]string{"100"}, nil, []int16{1})...) // request binary int4
	ext = append(ext, describePortalMessage()...)
	ext = append(ext, executeMessage()...)
	ext = append(ext, syncMessage()...)
	if _, err := conn.Write(ext); err != nil {
		t.Fatalf("write extended: %v", err)
	}
	resp = drainToReady(t, br)
	_, oids, formats = decodeRowDesc(resp['T'])
	row = decodeDataRow(resp['D'], oids, formats)
	if got := row[0]; got != int64(101) {
		t.Fatalf("col v = %v (%T), want int64(101)", got, got)
	}

	// Terminate, then confirm ServeConn returned cleanly.
	conn.Write([]byte{'X', 0, 0, 0, 4})
	conn.Close()
	if err := <-srvErr; err != nil {
		t.Fatalf("ServeConn: %v", err)
	}

	// The shared backend survives the client disconnect (Terminate was not
	// forwarded): a normal query still works.
	if r, err := db.Query("SELECT 7"); err != nil {
		t.Fatalf("post-serve query: %v", err)
	} else if len(r.Rows) != 1 {
		t.Fatalf("post-serve query returned %d rows", len(r.Rows))
	}
}
