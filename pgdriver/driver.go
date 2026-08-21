//go:build !wazero

// Package pgdriver registers a database/sql driver named "pglite" backed by the
// embedded PGlite/PostgreSQL library. Import it for its side effect:
//
//	import _ "github.com/moriyoshi/pglite-go/pgdriver"
//	db, err := sql.Open("pglite", "dir=./wasm database=template1")
//
// DSN is a space-separated list of key=value pairs:
//
//	dir=<path>        directory with pglite.wasm/initdb.wasm/manifest/data (default "wasm")
//	database=<name>   database to connect to (default "template1")
//	persist=<path>    host dir to persist the cluster (default: user cache dir)
//	ephemeral=true    keep the cluster only in memory
//
// The backend is a single, persistent connection, so real transactions work
// (db.Begin/tx.Commit/tx.Rollback) — but callers must serialize access with
// db.SetMaxOpenConns(1). Bind parameters ($1, $2, …) are interpolated
// client-side with proper escaping; values arrive as text and RowsAffected is
// not reported (single-user backend limitation).
package pgdriver

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"time"

	pglite "github.com/moriyoshi/pglite-go"
)

func init() { sql.Register("pglite", drv{}) }

type drv struct{}

// clusters caches one *pglite.DB per DSN so pooled connections share the single
// in-memory cluster (query execution is serialized inside the DB).
var (
	mu       sync.Mutex
	clusters = map[string]*pglite.DB{}
)

func (drv) Open(dsn string) (driver.Conn, error) {
	mu.Lock()
	defer mu.Unlock()
	if db, ok := clusters[dsn]; ok {
		return &conn{db: db}, nil
	}
	cfg, err := parseDSN(dsn)
	if err != nil {
		return nil, err
	}
	db, err := pglite.Open(cfg)
	if err != nil {
		return nil, err
	}
	clusters[dsn] = db
	return &conn{db: db}, nil
}

func parseDSN(dsn string) (pglite.Config, error) {
	var cfg pglite.Config
	for _, tok := range strings.Fields(dsn) {
		k, v, ok := strings.Cut(tok, "=")
		if !ok {
			return cfg, fmt.Errorf("pglite: bad DSN token %q (want key=value)", tok)
		}
		switch k {
		case "dir":
			cfg.WasmDir = v
		case "database":
			cfg.Database = v
		case "persist":
			cfg.PersistDir = v
		case "ephemeral":
			cfg.Ephemeral = v == "true" || v == "1"
		default:
			return cfg, fmt.Errorf("pglite: unknown DSN key %q", k)
		}
	}
	return cfg, nil
}

type conn struct{ db *pglite.DB }

func (c *conn) Prepare(query string) (driver.Stmt, error) {
	return &stmt{c: c, query: query}, nil
}
func (c *conn) Close() error { return nil } // the shared cluster outlives the conn

// Begin starts a transaction on the persistent backend session. Because the
// backend is a single connection, callers must serialize access with
// db.SetMaxOpenConns(1) (PGlite is likewise single-connection); otherwise a
// concurrent statement could land inside another connection's transaction.
func (c *conn) Begin() (driver.Tx, error) {
	if _, err := c.db.Exec("BEGIN"); err != nil {
		return nil, err
	}
	return &tx{c: c}, nil
}

type tx struct{ c *conn }

func (t *tx) Commit() error   { _, err := t.c.db.Exec("COMMIT"); return err }
func (t *tx) Rollback() error { _, err := t.c.db.Exec("ROLLBACK"); return err }

func (c *conn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	q, err := interpolate(query, args)
	if err != nil {
		return nil, err
	}
	r, err := c.db.Query(q)
	if err != nil {
		return nil, err
	}
	return &rows{r: r}, nil
}

func (c *conn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	q, err := interpolate(query, args)
	if err != nil {
		return nil, err
	}
	if _, err := c.db.Query(q); err != nil {
		return nil, err
	}
	return result{}, nil
}

type stmt struct {
	c     *conn
	query string
}

func (s *stmt) Close() error  { return nil }
func (s *stmt) NumInput() int { return -1 }
func (s *stmt) Exec(args []driver.Value) (driver.Result, error) {
	return s.c.ExecContext(context.Background(), s.query, named(args))
}
func (s *stmt) Query(args []driver.Value) (driver.Rows, error) {
	return s.c.QueryContext(context.Background(), s.query, named(args))
}

func named(vs []driver.Value) []driver.NamedValue {
	out := make([]driver.NamedValue, len(vs))
	for i, v := range vs {
		out[i] = driver.NamedValue{Ordinal: i + 1, Value: v}
	}
	return out
}

// result reports unknown affected rows (the single-user backend does not emit a
// command tag). LastInsertId is unsupported (PostgreSQL has no such concept).
type result struct{}

func (result) LastInsertId() (int64, error) {
	return 0, fmt.Errorf("pglite: LastInsertId is not supported")
}
func (result) RowsAffected() (int64, error) { return 0, nil }

type rows struct {
	r   *pglite.Rows
	pos int
}

func (rs *rows) Columns() []string { return rs.r.Columns }
func (rs *rows) Close() error      { return nil }
func (rs *rows) Next(dest []driver.Value) error {
	if rs.pos >= len(rs.r.Rows) {
		return io.EOF
	}
	row := rs.r.Rows[rs.pos]
	rs.pos++
	for i := range dest {
		if i >= len(row) || row[i] == nil {
			dest[i] = nil
			continue
		}
		dest[i] = convert(rs.r.TypeOIDs[i], *row[i])
	}
	return nil
}

// convert maps a text value + type OID to a database/sql driver.Value.
func convert(oid uint32, s string) driver.Value {
	switch oid {
	case 21, 23, 20, 26: // int2, int4, int8, oid
		if v, err := strconv.ParseInt(s, 10, 64); err == nil {
			return v
		}
	case 700, 701: // float4, float8
		if v, err := strconv.ParseFloat(s, 64); err == nil {
			return v
		}
	case 16: // bool
		return s == "t" || s == "true"
	case 17: // bytea (rendered as \x hex)
		if strings.HasPrefix(s, `\x`) {
			if b, err := hexDecode(s[2:]); err == nil {
				return b
			}
		}
	}
	return s // text and everything else as a string; Scan converts as needed
}

func hexDecode(s string) ([]byte, error) {
	if len(s)%2 != 0 {
		return nil, fmt.Errorf("odd hex length")
	}
	b := make([]byte, len(s)/2)
	for i := 0; i < len(b); i++ {
		hi, err1 := hexNibble(s[2*i])
		lo, err2 := hexNibble(s[2*i+1])
		if err1 != nil || err2 != nil {
			return nil, fmt.Errorf("bad hex")
		}
		b[i] = hi<<4 | lo
	}
	return b, nil
}

func hexNibble(c byte) (byte, error) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', nil
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, nil
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, nil
	}
	return 0, fmt.Errorf("bad nibble")
}

// interpolate substitutes $1,$2,… placeholders with escaped SQL literals, since
// the single-user backend has no server-side parameter binding.
func interpolate(query string, args []driver.NamedValue) (string, error) {
	if len(args) == 0 {
		return query, nil
	}
	byOrd := make(map[int]driver.Value, len(args))
	for _, a := range args {
		byOrd[a.Ordinal] = a.Value
	}
	var b strings.Builder
	for i := 0; i < len(query); i++ {
		if query[i] != '$' || i+1 >= len(query) || query[i+1] < '1' || query[i+1] > '9' {
			b.WriteByte(query[i])
			continue
		}
		j := i + 1
		for j < len(query) && query[j] >= '0' && query[j] <= '9' {
			j++
		}
		ord, _ := strconv.Atoi(query[i+1 : j])
		v, ok := byOrd[ord]
		if !ok {
			return "", fmt.Errorf("pglite: missing argument for $%d", ord)
		}
		lit, err := literal(v)
		if err != nil {
			return "", err
		}
		b.WriteString(lit)
		i = j - 1
	}
	return b.String(), nil
}

func literal(v driver.Value) (string, error) {
	switch x := v.(type) {
	case nil:
		return "NULL", nil
	case int64:
		return strconv.FormatInt(x, 10), nil
	case float64:
		return strconv.FormatFloat(x, 'g', -1, 64), nil
	case bool:
		if x {
			return "TRUE", nil
		}
		return "FALSE", nil
	case []byte:
		return `'\x` + toHex(x) + `'::bytea`, nil
	case string:
		return quote(x), nil
	case time.Time:
		return quote(x.Format("2006-01-02 15:04:05.999999-07")), nil
	default:
		return "", fmt.Errorf("pglite: unsupported argument type %T", v)
	}
}

func quote(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

func toHex(b []byte) string {
	const hexdig = "0123456789abcdef"
	out := make([]byte, len(b)*2)
	for i, c := range b {
		out[2*i] = hexdig[c>>4]
		out[2*i+1] = hexdig[c&0xf]
	}
	return string(out)
}

var _ driver.QueryerContext = (*conn)(nil)
var _ driver.ExecerContext = (*conn)(nil)
