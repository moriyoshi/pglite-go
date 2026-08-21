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
- The PGlite **wasm assets** (`pglite.wasm`, `initdb.wasm`, `pglite.data`,
  `pglite.manifest.json`). They are not committed; obtain them in whichever way suits
  your build:

  ```sh
  go generate ./...              # fetch the pinned release from jsDelivr -> ./wasm
  ```

  `go generate` runs a small Go fetcher (`internal/fetchwasm`, no shell/python deps) that
  downloads the version this library targets (`pglite.PgliteVersion`); set `PGLITE_VERSION`
  for a deliberate bump.

  Then choose how the binary gets the assets:

  | Approach | How | Trade-off |
  |----------|-----|-----------|
  | **Directory** (default) | ship `wasm/` beside the binary, or `WasmDir`/`dir=` | assets on disk at runtime |
  | **Embed** | `go generate ./... && go build -tags embed` | self-contained binary (+~16 MB) |
  | **On-demand** | `Config{Download: true}` / `download=true` | no assets shipped; fetches from jsDelivr into the user cache on first run (needs network) |
  | **Your own `fs.FS`** | `Config{WasmFS: myEmbedFS}` | embed assets you fetched in your own app |

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
| `download` | fetch wasm assets from jsDelivr if no local source (`true`) | `false` |

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
- **CGo by default; pure-Go optional.** The default build runs on wasmtime (CGo).
  A pure-Go build (`-tags wazero`, `CGO_ENABLED=0`) runs the **same** library and
  wire protocol on the [wazero](https://github.com/tetratelabs/wazero) runtime — no
  driver/feature differences — but is considerably slower (notably a ~5 min cold
  compile of `pglite.wasm`; warm starts are cached). `database/sql` support still
  needs CGo, since the driver depends on it only through the default backend.

## Layout

```
.                     pglite package — Open/Query/Exec/QueryParams (the library)
                      backend_wasmtime.go / backend_wazero.go select the runtime
pgdriver/             database/sql driver, registered as "pglite"
emscripten/           Emscripten + WASI host layer + wire hooks
                      wasmtime_port.go (default) / wazero_port.go (-tags wazero)
vfs/                  in-memory virtual filesystem
cmd/pglite/           demo using the library (builds on both backends)
internal/fetchwasm/   `go generate` fetcher: pulls the PGlite wasm assets from jsDelivr
```

## Credits

Built on [PGlite](https://github.com/electric-sql/pglite) by ElectricSQL and
[wasmtime](https://github.com/bytecodealliance/wasmtime) by the Bytecode Alliance.
