package pglite

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
)

// ServeConn speaks the PostgreSQL v3 frontend/backend protocol to a single
// client over conn, relaying the client's messages to the embedded backend and
// the backend's responses back to the client. It lets ordinary PostgreSQL
// clients (psql, pgx, lib/pq, JDBC, …) talk to the in-process cluster over a
// real socket — e.g. in an Accept loop:
//
//	ln, _ := net.Listen("tcp", "127.0.0.1:5432")
//	for {
//		c, err := ln.Accept()
//		if err != nil {
//			return err
//		}
//		go db.ServeConn(c)
//	}
//
// The embedded cluster has exactly one persistent backend session, so ServeConn
// does not open a new one per client: it answers the startup handshake itself
// (AuthenticationOk, a synthetic set of ParameterStatus values, BackendKeyData,
// ReadyForQuery — authentication is always trust) and then shuttles protocol
// messages to and from that single shared session. Consequently ServeConn holds
// the DB's backend exclusively for the entire lifetime of conn: other ServeConn
// calls and Query/Exec/QueryParams block until the served client disconnects.
//
// A client Terminate ('X') ends only that client; it is never forwarded to the
// shared backend. ServeConn closes conn before returning.
func (db *DB) ServeConn(conn net.Conn) error {
	defer conn.Close()

	db.mu.Lock()
	defer db.mu.Unlock()
	if db.wire == nil {
		return errors.New("pglite: DB is closed")
	}

	br := bufio.NewReader(conn)

	// Startup phase: negotiate SSL/GSS (declined), then answer the StartupMessage.
	if err := db.serveStartup(conn, br); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return nil
		}
		return err
	}

	// Message relay. Extended-protocol messages (Parse/Bind/Describe/Execute/
	// Close) are buffered and submitted to the backend as one batch when a
	// terminator (Sync, Flush, simple Query, …) arrives, matching how the
	// backend expects a pipelined request — see wireConn.extendedQuery.
	var batch []byte
	for {
		typ, payload, err := readFrontendMessage(br)
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return nil
			}
			return err
		}
		if typ == 'X' { // Terminate: drop this client; never kill the shared session.
			return nil
		}
		batch = append(batch, frame(typ, payload)...)
		switch typ {
		case 'P', 'B', 'D', 'E', 'C': // accumulate until a terminator
			continue
		}
		out, err := db.wire.exec(batch)
		batch = batch[:0]
		if err != nil {
			return err
		}
		if _, err := conn.Write(out); err != nil {
			return err
		}
	}
}

// serveStartup consumes SSL/GSS negotiation and the StartupMessage, then emits
// the backend's startup reply. SSL and GSS are declined; a CancelRequest is a
// no-op (the single-session backend has nothing to cancel).
func (db *DB) serveStartup(conn net.Conn, br *bufio.Reader) error {
	for {
		var lenbuf [4]byte
		if _, err := io.ReadFull(br, lenbuf[:]); err != nil {
			return err
		}
		n := binary.BigEndian.Uint32(lenbuf[:])
		if n < 8 || n > 1<<20 {
			return fmt.Errorf("pglite: bad startup packet length %d", n)
		}
		body := make([]byte, n-4)
		if _, err := io.ReadFull(br, body); err != nil {
			return err
		}
		switch code := binary.BigEndian.Uint32(body[:4]); code {
		case 80877103, 80877104: // SSLRequest / GSSENCRequest
			if _, err := conn.Write([]byte{'N'}); err != nil { // unsupported
				return err
			}
		case 80877102: // CancelRequest
			return io.EOF // nothing to cancel; just drop the connection
		case 196608: // protocol 3.0 StartupMessage
			return db.sendStartupResponse(conn)
		default:
			conn.Write(frame('E', errorBody("FATAL", "0A000",
				fmt.Sprintf("unsupported protocol/request code 0x%08x", code))))
			return fmt.Errorf("pglite: unsupported startup code 0x%08x", code)
		}
	}
}

func (db *DB) sendStartupResponse(conn net.Conn) error {
	var buf []byte
	buf = append(buf, frame('R', binary.BigEndian.AppendUint32(nil, 0))...) // AuthenticationOk
	for _, kv := range db.startupParams() {
		var p []byte
		p = append(p, kv[0]...)
		p = append(p, 0)
		p = append(p, kv[1]...)
		p = append(p, 0)
		buf = append(buf, frame('S', p)...) // ParameterStatus
	}
	kd := binary.BigEndian.AppendUint32(nil, 1) // BackendKeyData: dummy pid+secret
	kd = binary.BigEndian.AppendUint32(kd, 1)
	buf = append(buf, frame('K', kd)...)
	buf = append(buf, frame('Z', []byte{'I'})...) // ReadyForQuery, idle
	_, err := conn.Write(buf)
	return err
}

// startupParams is the ParameterStatus set reported to a connecting client.
// server_version is read from the live backend (clients such as pgx parse it);
// the rest are fixed because the embedded cluster always runs UTF-8/UTC/trust.
func (db *DB) startupParams() [][2]string {
	serverVersion := "18.0"
	if r, err := db.wire.simpleQuery("SHOW server_version"); err == nil &&
		len(r.Rows) == 1 && len(r.Rows[0]) == 1 {
		if s, ok := r.Rows[0][0].(string); ok && s != "" {
			serverVersion = s
		}
	}
	return [][2]string{
		{"server_version", serverVersion},
		{"server_encoding", "UTF8"},
		{"client_encoding", "UTF8"},
		{"DateStyle", "ISO, MDY"},
		{"TimeZone", "UTC"},
		{"IntervalStyle", "postgres"},
		{"integer_datetimes", "on"},
		{"standard_conforming_strings", "on"},
		{"is_superuser", "on"},
		{"session_authorization", "postgres"},
		{"application_name", ""},
	}
}

// readFrontendMessage reads one typed frontend message (type byte + int32
// length + payload) and returns its type and payload. frame reconstructs the
// exact bytes read.
func readFrontendMessage(br *bufio.Reader) (byte, []byte, error) {
	var hdr [5]byte
	if _, err := io.ReadFull(br, hdr[:]); err != nil {
		return 0, nil, err
	}
	n := binary.BigEndian.Uint32(hdr[1:5])
	if n < 4 || n > 1<<30 {
		return 0, nil, fmt.Errorf("pglite: bad message length %d", n)
	}
	payload := make([]byte, n-4)
	if _, err := io.ReadFull(br, payload); err != nil {
		return 0, nil, err
	}
	return hdr[0], payload, nil
}

// errorBody builds an ErrorResponse payload (S severity, C SQLSTATE, M message).
func errorBody(severity, code, message string) []byte {
	var b []byte
	for _, f := range []struct {
		code byte
		val  string
	}{{'S', severity}, {'C', code}, {'M', message}} {
		b = append(b, f.code)
		b = append(b, f.val...)
		b = append(b, 0)
	}
	return append(b, 0)
}
