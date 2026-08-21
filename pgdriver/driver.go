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
//	download=true     fetch the wasm artifacts from jsDelivr if no local source
//
// Queries run over the PostgreSQL wire protocol against one persistent backend,
// so bind parameters ($1, $2, …) are real server-side prepared statements
// (extended protocol), RowsAffected comes from the command tag, and
// transactions work (db.Begin/tx.Commit/tx.Rollback). The backend is a single
// connection (as in PGlite), so callers must serialize access with
// db.SetMaxOpenConns(1). Result values are decoded from their text encodings.
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
		case "download":
			cfg.Download = v == "true" || v == "1"
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

func (c *conn) run(query string, args []driver.NamedValue) (*pglite.Rows, error) {
	if len(args) == 0 {
		return c.db.Query(query)
	}
	params, isNull, err := encodeParams(args)
	if err != nil {
		return nil, err
	}
	// The query keeps its $N placeholders; params bind server-side (extended
	// protocol = a real prepared statement).
	return c.db.QueryParams(query, params, isNull)
}

func (c *conn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	r, err := c.run(query, args)
	if err != nil {
		return nil, err
	}
	return &rows{r: r}, nil
}

func (c *conn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	r, err := c.run(query, args)
	if err != nil {
		return nil, err
	}
	return result{affected: r.AffectedRows}, nil
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

// result carries the affected-row count from the CommandComplete tag.
// LastInsertId is unsupported (PostgreSQL has no such concept).
type result struct{ affected int64 }

func (result) LastInsertId() (int64, error) {
	return 0, fmt.Errorf("pglite: LastInsertId is not supported")
}
func (r result) RowsAffected() (int64, error) { return r.affected, nil }

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
		// The library already decodes each value to a typed Go value
		// (int64/float64/bool/[]byte/string/nil) — all valid driver.Values.
		if i < len(row) {
			dest[i] = row[i]
		} else {
			dest[i] = nil
		}
	}
	return nil
}

// encodeParams renders bind arguments (ordered by $N) as their PostgreSQL text
// encodings for the extended-protocol Bind message; a nil value becomes NULL.
func encodeParams(args []driver.NamedValue) (params []string, isNull []bool, err error) {
	ordered := make([]driver.Value, len(args))
	for _, a := range args {
		if a.Ordinal < 1 || a.Ordinal > len(args) {
			return nil, nil, fmt.Errorf("pglite: bad parameter ordinal %d", a.Ordinal)
		}
		ordered[a.Ordinal-1] = a.Value
	}
	params = make([]string, len(ordered))
	isNull = make([]bool, len(ordered))
	for i, v := range ordered {
		switch x := v.(type) {
		case nil:
			isNull[i] = true
		case int64:
			params[i] = strconv.FormatInt(x, 10)
		case float64:
			params[i] = strconv.FormatFloat(x, 'g', -1, 64)
		case bool:
			if x {
				params[i] = "t"
			} else {
				params[i] = "f"
			}
		case string:
			params[i] = x
		case []byte:
			params[i] = `\x` + toHex(x) // bytea text input
		case time.Time:
			params[i] = x.Format("2006-01-02 15:04:05.999999-07")
		default:
			return nil, nil, fmt.Errorf("pglite: unsupported argument type %T", v)
		}
	}
	return params, isNull, nil
}

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
