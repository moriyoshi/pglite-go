# pglite-go

Embedded PostgreSQL for Go — no external server, no libpq, no separate process.

It runs [PGlite](https://github.com/electric-sql/pglite) (real PostgreSQL 17.5
compiled to WebAssembly) on the [wasmtime](https://github.com/bytecodealliance/wasmtime-go)
runtime, with a hand-written Emscripten/WASI host layer and an in-memory virtual
filesystem. Queries run over the actual PostgreSQL v3 **wire protocol** against one
persistent in-process backend, so you get typed results, `RowsAffected`, server-side
prepared statements, and transactions — through the standard `database/sql` API.

```go
import (
    "database/sql"
    _ "github.com/moriyoshi/pglite-go/pgdriver"
)

db, _ := sql.Open("pglite", "dir=./wasm")
db.SetMaxOpenConns(1) // one embedded backend = one connection
defer db.Close()

db.Exec(`CREATE TABLE users (id int primary key, name text)`)
db.Exec(`INSERT INTO users VALUES ($1, $2)`, 1, "alice")

var name string
db.QueryRow(`SELECT name FROM users WHERE id = $1`, 1).Scan(&name) // "alice"
```

## Status

A working embedded database, not a toy: the `database/sql` driver passes CRUD,
parameterized queries, NULLs, `RowsAffected`, and `Begin`/`Commit`/`Rollback` under
`-race`, and [`jmoiron/sqlx`](https://github.com/jmoiron/sqlx) works unchanged. It
began as a proof of concept; see [`JOURNAL.md`](JOURNAL.md) for the full development
history (runtime benchmarks, the wire-protocol reverse-engineering, perf work).

## Requirements

- **Go 1.25+** with **CGo enabled** (the default). `wasmtime-go` vendors a prebuilt
  `libwasmtime`, so there is nothing else to install.
- The PGlite **wasm assets**. Fetch them once:

  ```sh
  ./scripts/update-wasm.sh        # downloads @electric-sql/pglite@0.4.2 -> ./wasm
  ```

  This writes `pglite.wasm`, `initdb.wasm`, `pglite.manifest.json`, and `pglite.data`
  into `wasm/` (set `PGLITE_VERSION` to pin a different release). Point the
  driver/library at that directory.

## Usage

### database/sql (+ sqlx)

```go
import (
    "database/sql"
    _ "github.com/moriyoshi/pglite-go/pgdriver"

    "github.com/jmoiron/sqlx"
)

db, _ := sql.Open("pglite", "dir=./wasm database=template1")
db.SetMaxOpenConns(1)

// sqlx wraps any database/sql driver:
dbx := sqlx.NewDb(db, "pglite")
var users []User
dbx.Select(&users, `SELECT id, name FROM users WHERE active = $1`, true)
```

**DSN** is a space-separated list of `key=value` pairs:

| key | meaning | default |
|-----|---------|---------|
| `dir` | directory holding the wasm assets | `wasm` |
| `database` | database to connect to | `template1` |
| `persist` | host dir to save/restore the cluster | user cache dir |
| `ephemeral` | keep the cluster only in memory (`true`) | `false` |

### Lower-level library

```go
import pglite "github.com/moriyoshi/pglite-go"

db, _ := pglite.Open(pglite.Config{WasmDir: "wasm"})
defer db.Close()

rows, _ := db.Query("SELECT 1 + 1 AS n, 'hi'::text AS s")
// rows.Columns, rows.TypeOIDs, rows.Rows ([][]any of typed Go values), rows.AffectedRows

// Server-side prepared statement (extended protocol):
rows, _ = db.QueryParams("SELECT $1::int + $2::int", []string{"2", "3"}, []bool{false, false})
```

## How it works

- **Backend:** `Open` initializes a data directory (`initdb`) or loads a persisted
  one, keeps it in an in-memory VFS, and starts a single PostgreSQL backend driven
  over PGlite's `pgl_set_rw_cbs` + `PostgresMainLoopOnce` wire-protocol hooks.
- **Queries** feed v3 protocol messages to that live backend and decode the responses
  (RowDescription, DataRow, CommandComplete, …). Session state — the open transaction,
  temp tables, `SET`, prepared statements — persists across calls.
- **Persistence:** on close (or `Sync`) the cluster is saved to the host under the
  cache dir; the next `Open` loads it and skips `initdb`. Compiled native code is
  cached to a `.cwasm` on disk. So a warm start is a fraction of a second.

## Performance

On an idle x86-64 Mac (see `JOURNAL.md` for methodology):

| | |
|---|---|
| First run (compile + initdb + persist) | ~2.7 s |
| Warm start (`.cwasm` cache + skip initdb) | ~0.2 s |
| Query latency (warm, simple `SELECT`) | ~150 µs |

## Limitations

- **Single connection.** The embedded backend is one connection (as in PGlite); use
  `db.SetMaxOpenConns(1)`. The library serializes internally.
- **Result types.** Common numeric/bool/bytea columns are received in binary format
  and decoded to typed Go values (`int64`, `float64`, `bool`, `[]byte`); other types
  arrive as text strings, which `Scan` converts as usual. Binary applies to
  parameterized (extended-protocol) queries; plain `Query` uses the text protocol.
- **No `COPY` / `LISTEN`/`NOTIFY` yet.**
- **CGo required.** The wire-protocol library runs on wasmtime (CGo). A pure-Go
  `-tags wazero` build exists (`cmd/pglite-poc`) but uses an older single-user query
  path and does **not** include this driver/library.

## Layout

```
.                     pglite package — Open/Query/Exec/QueryParams (the library)
pgdriver/             database/sql driver, registered as "pglite"
emscripten/           Emscripten + WASI host layer (wasmtime backend, syscalls, wire hooks)
vfs/                  in-memory virtual filesystem
cmd/pglite/           demo using the library
scripts/update-wasm.sh     fetches/refreshes the vendored PGlite wasm assets
```

## Credits

Built on [PGlite](https://github.com/electric-sql/pglite) by ElectricSQL and
[wasmtime](https://github.com/bytecodealliance/wasmtime) by the Bytecode Alliance.
