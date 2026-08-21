# pglite-go Development Journal

## Project Goal

Embed PostgreSQL in Go by running [PGlite](https://github.com/electric-sql/pglite) (PostgreSQL compiled to WASM via Emscripten) on a WebAssembly runtime, with a hand-written Emscripten + WASI host layer and an in-memory VFS.

**Now usable as a library + `database/sql` driver:**

```go
import (
    "database/sql"
    _ "github.com/moriyoshi/pglite-go/pgdriver"
)
db, _ := sql.Open("pglite", "dir=./wasm database=template1")
db.SetMaxOpenConns(1)
db.Exec("CREATE TABLE t (id int, name text)")
db.Exec("INSERT INTO t VALUES ($1,$2)", 1, "alice")
rows, _ := db.Query("SELECT id, name FROM t")
// jmoiron/sqlx works unchanged — it wraps any database/sql driver.
```

Or the lower-level package API: `pglite.Open(Config) (*DB)` → `db.Query(sql) (*Rows)` / `db.Exec`.
See "Library & database/sql driver" below for design and limits.

**Runtime: wasmtime is the primary/default path** (fastest compile + execution, only healthy Go binding); [wazero](https://github.com/tetratelabs/wazero) (pure Go, no CGo) is the fallback behind `-tags wazero`. Build/run:

```
go run ./cmd/pglite                        # wasmtime (default, needs CGo)
CGO_ENABLED=0 go run -tags wazero ./cmd/pglite-poc   # wazero (pure Go fallback)
```

Build-tag scheme: wasmtime files are `//go:build !wazero` (default-on); the wazero
command + its mmap allocator are `//go:build wazero`. The shared host layer
(`emscripten` core, `vfs`) is untagged. So the default build is CGo/wasmtime and the
`-tags wazero` build is pure Go (`CGO_ENABLED=0`). See the runtime comparison below for
why wasmtime is primary.

## Work Summary — 2026-08-19 (startup perf + runtime benchmark + wasmtime port)

Focused on startup latency, then a cross-runtime benchmark that grew into a working
port onto a second WASM runtime. Full detail in the sections below; the highlights:

### Startup latency, mitigated (~164s → ~2.3s warm restart)
1. **Persistent compilation cache.** The 8.7MB `pglite.wasm` was AOT-recompiled on
   every start (~138s) because the wazero cache was in-memory only. Switched to
   `NewCompilationCacheWithDir` (disk-backed). **Warm start compiles in 0.58s (~170×
   faster).** Cold first run still ~100s (one-time). — `cmd/pglite-poc/main.go`.
2. **VFS persistence + skip initdb.** initdb was ~9s of the ~11s warm run and only
   needs to run once per data dir, but the in-memory VFS was wiped on exit. Added
   `vfs.SaveSubtree`/`LoadSubtree` to mirror the data dir to/from the host FS; the PoC
   now loads a persisted cluster and **skips initdb entirely** → first run 8.6s,
   **subsequent runs 2.3s**. `SELECT 1+1` returns `2` on the reloaded cluster.
   - Gotcha: persist by *cluster validity* (PG_VERSION + global/pg_control present),
     not initdb's exit code (initdb exits 1 on the known collation import).
   - Gotcha: decouple host storage perms (0600/0700) from VFS modes — initdb marks
     some files unreadable (mode 0), which silently aborted reload mid-walk.

### Cross-runtime benchmark (wazero vs wasmtime vs wasmedge)
- **Compile time: wasmtime 1.43s (Cranelift) ≪ wazero ~120s < wasmedge 218s (LLVM-O2).**
  On wasmtime the one-time cold-compile cost the disk cache exists to hide largely
  disappears. Execution speed likely inverts (LLVM ≥ Cranelift ≥ wazero) — not yet
  measured. Artifacts: wazero ~34MB, wasmtime 33.5MB, wasmedge 20.6MB.
- **wasmtime instantiates in 0.002s with no binary patching** — it accepts
  global/memory/table imports natively, so `env.extras`/`patch.go` (a wazero
  function-only-host-module workaround) is unnecessary.

### wasmtime execution port — full PostgreSQL runs on wasmtime
- **End to end:** initdb builds a full cluster and `SELECT 1+1` returns `2` on
  wasmtime, matching wazero exactly. Full flow **2.93s** (1.36s compile + ~1.57s
  execution) vs wazero's **~11s** warm — **wasmtime executes ~6–7× faster** (Cranelift
  native code). Faster on both axes: ~70× cold compile and several× execution.
- Key techniques: (a) reuse the *exact* wazero Go closures by defeating wazero's
  sealed `api.Module`/`api.Memory` via interface-embedding; (b) re-implement
  `invoke_*`/`_emscripten_throw_longjmp` with wasmtime trap-and-resume; (c) initdb
  system/popen/pclose bridge via `table.Grow`+`table.Set` (simpler than wazero's
  element-segment module); (d) compile the module once, instantiate into many stores.
- Now the **default** path (`//go:build !wazero`); the wazero build is `-tags wazero`.
  Run: `go run ./cmd/pglite`.

### New tooling
`cmd/pglite` (primary wasmtime entrypoint), `cmd/pglite-poc` (wazero fallback, `-tags
wazero`), `cmd/bench-wasmtime` (compile timing), `cmd/dump-imports` (import inventory),
`cmd/probe-wasmtime` / `cmd/probe-wasmedge` (instantiation probes),
`emscripten/wasmtime_port.go` (adapters + invoke/longjmp), `vfs/persist.go` +
`vfs/persist_test.go` (data-dir persistence).

## Findings: why wazero is slow (CPU-profiled, 2026-08-19)

Profiled both axes with Go pprof (`cmd/prof-compile`, `cmd/prof-exec`). Two
distinct bottlenecks, both structural — not incidental.

### Compile bottleneck: imported-global access explosion (~66% of compile)

`wazevo` (wazero's SSA optimizing compiler) spends its time here:

| Function | cum % | what it is |
|----------|-------|------------|
| `frontend.getWasmGlobalValue` | **39%** | one SSA load + a per-BB **map assign** per global access |
| `ssa.passDeadCodeEliminationOpt` | 18% | cleaning up the resulting instruction bloat |
| `ssa.passNopInstElimination` | 9% | ditto |

Root cause: **PGlite is an Emscripten MAIN_MODULE (dynamically linked)**, so the C
stack pointer is an *imported mutable global* `__stack_pointer`, touched in every
prologue/epilogue/alloca. wazevo lowers each `global.get`/`global.set` to a memory
load/store plus `DefineVariableInCurrentBB` (a Go **map** write, `mapassign_fast32`
alone = 17%). Pervasive stack-pointer traffic → a flood of SSA loads → the DCE/nop
passes then have to delete them all. Cranelift (wasmtime) handles imported globals
without this per-access map overhead, hence ~70× faster compile.
Mitigations: file compile cache (done, amortizes it); a *statically*-linked PGlite
build would sidestep it; worth an upstream wazero issue.
(Note: clean-machine compile was ~28s here; the earlier ~100–138s figures were under
heavy concurrent load — wall time is very sensitive to background CPU pressure.)

### Execution bottleneck: full linear-memory copy on every heap grow (~65–73%)

On a compute-heavy query, 65–73% of CPU is `MemoryInstance.Grow` →
`runtime.growslice` → `runtime.memmove`. wazero's default memory is a Go `[]byte`;
`Grow` does `append(buffer, make(...))`, i.e. **reallocate + copy the entire linear
memory** whenever `newPages > Cap`. PostgreSQL grows its heap incrementally
(mmap/`sbrk` for shared buffers, sorts, temp tables), so the multi-hundred-MB buffer
is copied over and over. wasmtime reserves the max via mmap and commits pages, so it
never copies.

**First attempt (`WithMemoryCapacityFromMax(true)`) — regressed the PoC.** It sets
`Cap = Max` so `Grow` reslices instead of copying, and on the synthetic 200k-row query
it helped (0.41s → 0.30s). But on the real full initdb + query it made things *worse*
(3 runs each):

| config | time | peak RSS |
|--------|------|----------|
| baseline (copy-on-grow) | ~2.03s | ~877MB |
| `WithMemoryCapacityFromMax(true)` | ~3.5s | **3398MB** |
| **mmap allocator (applied)** | **~1.83s** | **~320MB** |

Capacity-from-max eagerly reserves *and commits* the declared 2GB max **per
subcommand** (~5 postgres processes per run); that churn outweighs the grow-copy
savings. The assumption that Go keeps the 2GB reservation lazily-uncommitted was wrong.

**Applied fix: an mmap allocator** (`cmd/pglite-poc/mmap_alloc.go`, wired via
`experimental.WithMemoryAllocator` on the base context). It backs linear memory with an
anonymous `mmap` of the declared max: `Reallocate` reslices the *same* mapping (no copy
on grow) and the OS commits pages lazily on touch (RSS tracks real usage). Result on the
full initdb + query: **~10% faster (2.03s → 1.83s) and 2.7× less memory (877MB →
320MB)**, query still returns `2`. `//go:build darwin || linux` with a no-op fallback
elsewhere. This is exactly prescription B.1 (an mmap-reserve allocator) validated on the
real workload.

### Net
wazero is slow for two independent, structural reasons tied to how this particular
module is built (dynamic linking → imported `__stack_pointer`) and how wazero manages
memory (copy-on-grow). The compile gap is inherent to wazevo + this module; the
execution gap is directly fixable with `WithMemoryCapacityFromMax` (bounded).

## Architecture

```
pglite-go/
├── emscripten/
│   ├── syscall.go      # Emscripten host functions, runtime setup
│   ├── emsyscall.go    # __syscall_* implementations (VFS-backed)
│   ├── wasi.go         # Custom WASI layer bridging VFS fds
│   ├── systemcb.go     # initdb system/popen/pclose callback bridge
│   ├── patch.go        # WASM binary patching (env → env.extras)
│   └── wasm.go         # WASM binary builder utilities
├── vfs/
│   └── vfs.go          # In-memory virtual filesystem
├── cmd/
│   ├── pglite/         # PRIMARY launcher — wasmtime (default, //go:build !wazero)
│   ├── pglite-poc/     # wazero fallback launcher (//go:build wazero, pure Go)
│   └── ...             # bench-wasmtime, dump-imports, probe-wasmtime, probe-wasmedge, prof-compile
├── emscripten/
│   └── wasmtime_port.go # wasmtime backend adapters (//go:build !wazero)
└── scripts/
    └── download-wasm.sh  # Downloads PGlite WASM from npm
```

### Module Composition

The WASM module (`pglite.wasm`) imports from several host modules:

- **`env`**: Host functions (syscalls, Emscripten runtime, invoke_*)
- **`env.extras`**: Globals (`__memory_base`, `__stack_pointer`, `__table_base`), shared memory, and shared function table
- **`GOT.mem`**: Global Offset Table for memory symbols (`__heap_base`)
- **`wasi_snapshot_preview1`**: Custom WASI implementation

wazero's `HostModuleBuilder` only supports function exports. To provide globals, memory, and a function table, we synthesize a raw WASM binary for `env.extras` and patch the guest WASM to import non-function symbols from it.

### initdb Callback Bridge

initdb calls `system()` and `popen()` to run postgres subcommands. PGlite uses `pgl_set_system_fn`, `pgl_set_popen_fn`, `pgl_set_pclose_fn` to register function-pointer callbacks. Since wazero doesn't allow modifying the function table post-instantiation, we create a small WASM bridge module that:

1. Imports Go handler functions from an `initdb_host` module
2. Imports the shared table from `env.extras`
3. Defines wrapper functions and places them in the table via element segments

## Key Findings

### 1. Emscripten uses WASI errno values, not Linux values

**Impact**: All syscall error returns were wrong, causing mysterious failures.

| Error | Linux | WASI (Emscripten) |
|-------|-------|-------------------|
| ENOENT | 2 | 44 |
| EACCES | 13 | 2 |
| EEXIST | 17 | 20 |
| EINVAL | 22 | 28 |
| EFBIG | 27 | 22 |

We were returning `-22` for EINVAL, but Emscripten interpreted it as EFBIG ("File too large"). This manifested as `realpath()` failing with "File too large" when resolving the postgres binary path.

**Source**: Emscripten's musl arch uses `__WASI_ERRNO_*` values defined in `musl/arch/emscripten/bits/errno.h`.

### 2. `__memory_base` and `__table_base` must be 0

**Impact**: Non-zero values cause broken function pointers in module-defined globals and dynamically-created structs.

Emscripten's linker generates module-defined globals (like stdout/stderr FILE struct pointers) with pre-relocation values. In the JS runtime, `relocateExports()` adds `__memory_base` to all numeric exports post-instantiation. wazero has no API to modify globals after instantiation.

Similarly, `__wasm_apply_data_relocs` only patches memory contents, not function pointer values embedded in code via `global.get __table_base + i32.const N + i32.add`. The code already computes `__table_base + N`, so `__table_base` must match the element segment offset (which is 0 when `__table_base = 0`).

Setting both to 0 avoids the need for post-instantiation relocation entirely.

### 3. Emscripten struct stat layout

**Impact**: Wrong field offsets caused stat() to return garbage, breaking file permission checks.

Emscripten's musl for wasm32 uses a simplified stat struct (96 bytes) without the `__st_dev_padding`, `__st_ino_truncated`, and `__st_rdev_padding` fields present in standard ARM/Linux musl. Key types from `bits/alltypes.h`:

- `blkcnt_t` = `int` (4 bytes, not `long long`)
- `dev_t` = `unsigned int` (4 bytes)
- `off_t` = `_Int64` (8 bytes)
- `time_t` = `_Int64` (8 bytes)

### 4. `getdents64` must track read position

**Impact**: Infinite loop (millions of repeated directory reads per second).

Our initial implementation returned all directory entries on every call. The caller (musl's `readdir`) expected subsequent calls to return new entries and a final call to return 0 (EOF). We fixed this by tracking the read offset in the VFS `OpenFile` struct.

### 5. `__wasm_call_ctors` failure and start function handling

The start function calls `__wasm_call_ctors` which runs data relocations AND C++ static constructors. For `pglite.wasm`, the C++ constructor for `std::__stdinbuf` crashes with `call_indirect` type mismatch (libc++ iostream vtable issue). For `initdb.wasm`, it succeeds.

For pglite.wasm, we let the start function run (it initializes `environ` and other C runtime state) despite the libc++ constructor crash. The crash is handled by the invoke/setjmp mechanism and doesn't prevent PostgreSQL from functioning since it doesn't use C++ iostream.

### 6. Shell command parsing must stop at operators

**Impact**: initdb's `system("postgres --check ... < /dev/null > /dev/null 2>&1")` passed shell redirections as postgres arguments.

PGlite's `getArgs()` uses a shell parser that recognizes `<`, `>`, `|`, etc. as operator tokens and stops collecting arguments. We replicate this by breaking at shell operator tokens including `>/path` (no space between `>` and path).

### 7. VFS write performance

**Impact**: 74 million fd_write calls for 953KB of bootstrap SQL took 600+ seconds.

musl's stdio flushes the FILE buffer via WASI `fd_write`. Each call crossed the WASM→Go boundary and our VFS `Write` method allocated a new byte slice for every extension (`make + copy` = O(n²)).

Fixes:
- Capacity-based growing (2x amortized allocation)
- Removed mutex from Read/Write (WASM is single-threaded)
- Used `pgl_freopen` + WASI stdout capture instead of `fopen` for pipe files

### 8. `/dev/urandom` must return real random data

**Impact**: `postgres --boot` crashed with "could not generate secret authorization token".

Our initial VFS had `/dev/urandom` as a 0-byte regular file (returning EOF on read). PostgreSQL's `pg_strong_random()` reads from it. Fixed by special-casing `/dev/urandom` reads in the WASI `fd_read` handler to return `crypto/rand` data.

### 9. popen("w") pipe mechanism

PGlite's initdb uses `popen("w")` to pipe bootstrap SQL to `postgres --boot`. The flow:

1. popen("w") returns a FILE* for initdb to write SQL to
2. initdb writes ~953KB of bootstrap SQL via fprintf/puts
3. On pclose(), postgres --boot runs with the SQL as stdin

Since fopen-created FILE structs cause an infinite write loop (74M+ fd_write calls despite correct function pointers - root cause still unclear), we use WASI stdout capture: popen("w") starts capturing stdout, initdb writes SQL via its existing stdout FILE struct (which works correctly), and pclose() saves the captured data and feeds it to postgres as stdin.

### 10. Anonymous mmap for shared memory

**Impact**: `FATAL: could not map anonymous shared memory: Out of memory` on every `postgres --check` and `postgres --boot` call.

PostgreSQL uses `mmap(MAP_ANONYMOUS | MAP_SHARED)` for shared memory segments. Implemented `_mmap_js` by calling the WASM module's exported `emscripten_builtin_memalign(65536, len)` to allocate page-aligned memory within the WASM linear memory space. Also implemented `emscripten_resize_heap` to allow WASM memory growth.

### 11. Emscripten environment variables bypass WASI

**Impact**: `postgres --boot` couldn't find PGDATA despite our WASI `environ_get` returning it.

Emscripten does NOT use WASI `environ_get`/`environ_sizes_get` for environment variables. It has its own mechanism: the JavaScript runtime sets `Module.ENV` and calls C `setenv()` during `preRun`. Our WASI environ functions are never called.

Workaround: pass `-D /tmp/pglite/data` explicitly to all postgres subcommands instead of relying on the `PGDATA` environment variable.

### 12. `-D` data directory placement matters

**Impact**: `--boot requires a value` or `invalid command-line argument: -D`.

PostgreSQL 17's `getopt_long` parsing is sensitive to option ordering. `-D /tmp/pglite/data --boot` causes `--boot` to be misinterpreted as requiring a value. For `--single` mode, `-D` must come before the database name (last positional argument).

Solution: append `-D` after `--boot` flags (no database name follows); insert `-D` before the database name for `--single`; skip for `-V`/`--version`.

## Current Status

### What Works

- **initdb**: Creates full database cluster (directories, configs, system catalogs)
- **postgres --boot**: Processes 953KB of bootstrap SQL, creates `global/pg_control`
- **postgres --single**: Starts with `backend>` prompt, performs WAL checkpoints, executes SQL
- **SQL queries**: `SELECT 1 + 1 AS result` returns `typeid=23 (int4), result column` via single-user stdin/stdout
- **VFS**: 694 files from PGlite npm package + runtime-created database files
- **30+ syscalls**: openat, stat64, lstat64, faccessat, mkdirat, readlinkat, getdents64, pipe, getcwd, chdir, fchmod, fchown, unlinkat, renameat, fcntl64, ioctl, dup, ftruncate64, fdatasync, utimensat, statfs64, fadvise64, fallocate, symlinkat
- **Custom WASI**: fd_read/write/seek/close/sync/pread/pwrite, environ, clock_time_get, random_get, proc_exit, stdin/stdout capture
- **Emscripten runtime**: invoke_*, longjmp, date_now, get_now, resize_heap, mmap_js, munmap_js, localtime_js, gmtime_js, mktime_js, tzset_js, setitimer_js

### What Remains

1. **Post-bootstrap template1 initialization**: `postgres --single` for template1 setup returns exit code 1. The `pg_import_system_collations` function fails, likely because locale/collation data from the host OS is unavailable in the WASM environment. PGlite's JS version may handle this differently via its Emscripten FS layer. This doesn't prevent basic SQL execution but means some system catalog entries are incomplete.

2. **Wire protocol bridge**: Currently queries use single-user mode stdin/stdout (text format). For production use, implement PGlite's `pgl_set_rw_cbs` mechanism which provides read/write callbacks for the PostgreSQL wire protocol (same binary protocol as libpq). This would enable proper result parsing, prepared statements, and transactions.

3. **`database/sql` driver interface**: Wrap the wire protocol in a Go `database/sql` driver to provide the standard Go database API (`db.Query()`, `db.Exec()`, etc.).

4. **fopen FILE struct write loop**: Dynamically-created FILE structs from `fopen` have correct function pointer values (verified by memory dump: write=10 → table[10] with matching type signature) but writes cause an infinite loop. The workaround (stdout capture) works for the initdb pipe but is not a general solution. Root cause investigation needed — possibly related to FILE buffer initialization or musl internal state.

5. **Emscripten environment variables**: Need to implement `setenv` calls on the WASM module to properly set environment variables (PGDATA, PATH, etc.) instead of passing `-D` flags explicitly. This requires either finding/exporting `setenv` from the WASM module or writing to the `environ` array in WASM memory directly.

6. **Compilation cache persistence**: ~~First compilation of `pglite.wasm` (8.7MB) takes ~90 seconds...~~ **DONE.** Switched the PoC from `wazero.NewCompilationCache()` (in-memory only, lost on restart) to `wazero.NewCompilationCacheWithDir()` in `~/Library/Caches/pglite-go/wazero` (via `os.UserCacheDir()`, repo-local `.wazero-cache` fallback). Measured: cold compile 100–138s → **warm start 0.58s (~170× faster)**. On-disk cache ~34MB. The cache is keyed on the patched wasm bytes + wazero version, so it self-invalidates on a wasm swap or wazero upgrade. Remaining startup cost is now dominated by initdb/bootstrap work, not compilation.

7. **Single-instance reuse**: Each postgres subcommand currently creates a new wazero runtime (compile → instantiate → run → close). PGlite's JS version reuses a single WASM instance with heap restoration (`HEAPU8.set(origHEAPU8)`) for dramatically faster repeated calls. Implementing this pattern in wazero would require snapshotting/restoring WASM linear memory.

8. ~~**VFS persistence**~~ **DONE.** The data dir is persisted to the host FS after initdb via `vfs.SaveSubtree`/`LoadSubtree`, and reloaded on startup to skip initdb (see "VFS persistence" under Startup Profile). Note this persists a *snapshot* of the data dir at initdb time; live persistence of ongoing writes (or a fully host-FS-backed VFS layer) is still future work for durability across a running session.

## Startup Profile (warm cache, 2026-08-19)

After the persistent compilation cache, a full warm run is **~11s** (cold first run ~164s). Breakdown:

| Phase | Time | Nature |
|-------|------|--------|
| Cache deserialize + pre-compile | ~0.5s | fixed |
| initdb setup + `postgres -V` probe | ~1.5s | 1 instantiation |
| `postgres --boot` (953KB bootstrap SQL) | ~2.8s | DB bootstrap work |
| `postgres --single` post-bootstrap (249KB SQL) | ~3.9s | DB bootstrap work |
| Phase 2: actual query | ~1.1s | 1 instantiation |

**~9s of the 11s is initdb**, which only needs to run once per data directory. The in-memory VFS is wiped on process exit, so the PoC re-runs initdb every start. The "open existing DB + query" path is only ~1.6s. The highest-value next startup lever is therefore VFS persistence (#8) — persist the data dir so initdb runs once, not per process.

### VFS persistence (DONE, 2026-08-19)

Implemented in `vfs/persist.go`: `SaveSubtree(vfsPath, hostDir)` mirrors a VFS
subtree to the host FS; `LoadSubtree(hostDir, vfsPath)` mirrors it back. The PoC
now saves `/tmp/pglite/data` after initdb and, on startup, if a persisted cluster
exists (`PG_VERSION` present), loads it and **skips initdb entirely**.

Result: **first run 8.6s (initdb + persist) → subsequent runs 2.3s** (load + query),
matching the predicted "open existing DB" path. `SELECT 1+1` returns `2` on the
reloaded cluster.

Two subtleties handled:
- **Persist despite initdb exit 1.** initdb exits 1 on the collation import but
  produces a valid cluster; the PoC persists based on the presence of essential
  files (`PG_VERSION`, `global/pg_control`) in the VFS, not initdb's exit code.
- **Host storage perms are decoupled from VFS modes.** initdb marks some files
  read-restricted (e.g. mode 0); writing those to the host with their VFS mode
  made them unreadable on reload, silently aborting the load partway. Host files
  are now stored 0600 / dirs 0700 and reloaded as 0700/0600 — a valid strict
  PostgreSQL data-dir permission set. Covered by `vfs/persist_test.go`.

## WASM Runtime Comparison (wasmtime vs wazero, 2026-08-19)

Investigating alternate runtimes. Only compilation and instantiation are directly
comparable without re-porting the entire host layer (VFS, 100+ syscalls, custom
WASI, Emscripten invoke/longjmp, initdb bridge) — all currently written against
wazero's Go API. Measured via `cmd/bench-wasmtime` and `cmd/probe-wasmtime`
(wasmtime-go v34, Cranelift, x86_64 Mac):

| Metric | wazero | wasmtime | wasmedge |
|--------|--------|----------|----------|
| Compiler | own (optimizing) | Cranelift | LLVM -O2 |
| Compile `pglite.wasm` (8.7MB) | ~100–138s | **1.43s** | 218s |
| AOT artifact | ~34MB | 33.5MB | **20.6MB** (.so) |
| Instantiate (all externs wired) | n/a | **0.002s** | not measured¹ |

¹ wasmedge instantiate needs host imports wired via wasmedge-go; not measured here.
wasmedge AOT breakdown: verify 1.8s, **optimize 101s**, **codegen 112s**, link 0.5s.
wasmedge also has an interpreter mode (near-zero startup, slower execution) if AOT
is skipped.

**Reading the numbers:** compile-time ranking is wasmtime (1.4s) ≪ wazero (~120s) <
wasmedge (218s), but this is *compile only* — execution speed likely inverts
(LLVM-O2 ≥ Cranelift ≥ wazero). For an embeddable Postgres where cold-start matters,
wasmtime's Cranelift is the clear compile-time winner; wasmedge's LLVM AOT only pays
off if execution throughput dominates and the one-time 218s compile is cached to the
.so. All three cache their AOT artifact to disk, amortizing compile across restarts.

Key findings:
- **wasmtime's Cranelift compiles ~70× faster than wazero's compiler** — on wasmtime
  the one-time cold-compile cost the disk cache was built to hide (item #6) largely
  disappears (1.4s is negligible; parallelized across cores, 15.9s user time).
- **No WASM binary patching needed on wasmtime.** wazero's `HostModuleBuilder` only
  exports functions, forcing us to synthesize an `env.extras` module and patch the
  guest to import globals/memory/table from it (`patch.go`). wasmtime accepts
  `Global`/`Memory`/`Table` externs directly, so the module instantiates unmodified.
- Imports to satisfy: `env`:122 funcs + memory + table + 3 globals, `GOT.mem`:1 global
  (`__heap_base`), `wasi_snapshot_preview1`:13 funcs. Of the env funcs, ~87 are
  `__syscall_*`, ~60 are `invoke_*` trampolines, the rest Emscripten runtime.

### wasmedge execution port — feasibility proven (raw CGo)

Extending the execution comparison to wasmedge hit a toolchain wall, then a path
around it:

- The maintained Go binding (`second-state/WasmEdge-go`) tops out at **v0.14.0** and
  **does not compile against the installed libwasmedge 0.17.1** — the C API changed
  `WasmEdge_Limit` (struct) into an opaque `WasmEdge_LimitContext*`. No 0.17-compatible
  binding exists.
- Downloading a matching 0.14.x lib requires WasmEdge's `curl | bash` installer, which
  the sandbox blocks (external download+execute) — needs the user.
- **Raw CGo against the installed 0.17.1 C API works.** `cmd/probe-wasmedge`
  (`//go:build wasmedge`) wires all imports — **130 func + 1 mem + 1 table + 4 global
  across 3 host modules** — by reusing each import's declared type from the parsed AST,
  then `WasmEdge_ExecutorInstantiate` succeeds. This is the wasmedge analog of
  `cmd/probe-wasmtime` and de-risks the full port on a version-consistent lib (same
  0.17.1 used for the AOT compile benchmark), no downgrade needed.

Remaining for a full wasmedge execution port (~large, CGo): a single C host-func
trampoline dispatching by index into the reused wazero Go closures; guest memory via
`WasmEdge_CallingFrameGetMemoryInstance` + `MemoryInstanceGetPointer`; invoke_/longjmp;
the initdb bridge; and calling `__main_argc_argv`. Its LLVM-AOT execution is expected to
be the fastest of the three once wired.

Build/run the probe:

	CGO_ENABLED=1 CGO_CFLAGS="-I/usr/local/include" \
	  CGO_LDFLAGS="-L/usr/local/lib -lwasmedge" DYLD_LIBRARY_PATH=/usr/local/lib \
	  go run -tags wasmedge ./cmd/probe-wasmedge wasm/pglite.wasm

**Execution port status:** PostgreSQL **runs on wasmtime** — `postgres -V` prints
"PostgreSQL 17.5" and exits 0 (see `emscripten/wasmtime_port.go`,
`cmd/pglite-wasmtime`, behind `//go:build wasmtime`; run
`go run -tags wasmtime ./cmd/pglite-wasmtime`). This exercises compile+instantiate
(1.5s), data relocs, C++ static ctors via the invoke_/longjmp trampolines (the
libc++ iostream crash the journal documents — handled correctly), argv setup, and
WASI stdout. What worked:
1. **Reuse via adapters.** wazero *seals* `api.Module`/`api.Memory`/`api.Function`
   with an unexported `wazeroOnly()` marker, so external types can't implement them.
   Workaround: embed the interface (nil) to inherit the marker, then override the
   ~6 methods actually used. This lets the wasmtime path reuse the *exact* wazero Go
   closures (syscalls, WASI, Emscripten runtime funcs) unchanged.
2. **invoke_/longjmp via trap-and-resume.** `_emscripten_throw_longjmp` returns a
   sentinel-messaged `wasmtime.Trap`; the invoke trampoline saves the stack
   (`emscripten_stack_get_current`), calls `table[index]`, and on that trap restores
   the stack + `setThrew(1,0)`. wasmtime resumes exports after a trap, so this works.
3. **No binary patching / no env.extras.** Globals, memory, and table are native
   wasmtime externs.

**Full end-to-end runs on wasmtime (DONE).** initdb (with the system/popen/pclose
bridge) builds a complete cluster and `SELECT 1+1` returns `2`, matching wazero
exactly — including the expected `pg_import_system_collations` exit-1 (`locale -a`
unimplemented in WASM). The initdb bridge is *simpler* on wasmtime: instead of
wazero's synthesized element-segment bridge module, `table.Grow` + `table.Set` place
the three Go callbacks directly, then `pgl_set_{system,popen,pclose}_fn` point initdb
at them. Also needed: an instance-backed module adapter (for Go-side orchestration),
nested runtimes per subcommand, `_mmap_js` during `--boot`, stdout-capture toggling,
and deriving memory/table types per-module (pglite vs initdb differ).

**End-to-end execution benchmark (the payoff — compile-time ranking inverts):**

| Full initdb + `SELECT 1+1` | wazero (warm disk cache) | wasmtime (compile once, reuse) |
|----------------------------|--------------------------|--------------------------------|
| Compile | ~0.6s (deserialize 34MB) | 1.36s (fresh; no cache yet) |
| initdb + query (execution) | ~10.5s | **~1.57s** |
| **Total** | **~11s** | **2.93s** |

Reusing the compiled `wasmtime.Module` across subcommands (one Module → many stores)
cut the wasmtime total from 7.46s to 2.93s. **wasmtime executes the initdb+query
~6–7× faster than wazero** (Cranelift native code vs wazero's), confirming the
prediction that execution speed inverts the compile-time ranking. Net: wasmtime is
faster on *both* axes here — ~70× faster cold compile and several× faster execution.

**wasmtime compile cache (added).** `compileCached` serializes the compiled module to
`~/Library/Caches/pglite-go/wasmtime/*.cwasm` (keyed by wasm content hash; wasmtime's
blob self-validates runtime version + host CPU, so a stale/foreign blob is rejected and
we recompile transparently). Warm runs deserialize instead of recompiling:

| wasmtime run | compile | total |
|--------------|---------|-------|
| cold (populate cache) | 1.43s | 2.90s |
| warm (load `.cwasm`) | **0.03s** | **1.49s** |

Both runtimes now cache compiled native code to disk. Warm end-to-end: **wasmtime 1.49s
vs wazero ~1.8s** (the wazero figure now includes the mmap allocator).

**VFS persistence on the primary path.** `cmd/pglite` also gained the
`SaveSubtree`/`LoadSubtree` skip-initdb logic (persist by cluster validity, not
initdb's exit code). Combined with the `.cwasm` cache, the fully-warm "open existing
DB + query" path is now:

| `cmd/pglite` (wasmtime) | total |
|-------------------------|-------|
| cold (initdb + persist) | 2.66s |
| warm, `.cwasm` but re-initdb | 1.49s |
| **fully warm (`.cwasm` + skip initdb)** | **0.20s** |

## Library & database/sql driver (2026-08-21)

Turned the demo into a reusable library plus a `database/sql` driver.

**Packages**
- `pglite` (root): `Open(Config) (*DB, error)`, `(*DB).Query/Exec/Raw/Sync/Close`.
  A `DB` is one cluster held in the in-memory VFS; Open runs initdb on first use
  and loads-and-skips-initdb after. Query execution is mutex-serialized (the WASM
  backend is single-threaded and the cluster is shared).
- `pgdriver`: registers the `"pglite"` `database/sql` driver. DSN is `key=value`
  pairs (`dir=`, `database=`, `persist=`, `ephemeral=`). `cmd/pglite` is now a thin
  consumer of the library.

**Persistent single-user session (the parity leap).** _[Historical — superseded by
the wire protocol below; `session.go` and its streaming stdin were removed.]_ Instead
of spawning a `postgres --single` backend per query, a `DB` starts **one** backend and
keeps it alive, parked between statements in a blocking, channel-fed stdin (`session.go`).
So session state — the open transaction, temp tables, `SET`, prepared statements —
survives across calls: **BEGIN in one `Query` and COMMIT in a later one form a real
transaction.** Verified: cross-call BEGIN/INSERT/INSERT → count 3, ROLLBACK → 1,
COMMIT → 2, temp table visible across calls; driver `Begin`/`Commit`/`Rollback`
tested under `-race`.

How the coordination works (no Asyncify needed): the backend runs `main()` on its
own goroutine; **`fd_read` on stdin is the authoritative "previous statement
finished" signal** (the backend only asks for input after flushing all output). So
the streaming stdin `Read` (on the backend goroutine) hands the accumulated stdout
to the caller via a channel, then blocks for the next statement. All stdout
bookkeeping is backend-goroutine-local; cross-goroutine handoff is via channels.
Gotcha that cost an hour: the guest passes **two iovecs with the first zero-length**
to `fd_read` — the initial code returned early on the zero-length first iovec,
signalling spurious EOF and making the backend shut down at startup; the fix
iterates iovecs to the first with space.

The backend is a **single connection** (as in PGlite), so access is serialized
(`DB` mutex; database/sql callers set `SetMaxOpenConns(1)`).

**Result parsing.** From the backend's `debugtup` output — a descriptor block
(column names + type OIDs) then one `\t----`-delimited block per tuple, with **NULL
attributes omitted**, so values are keyed by column index. The driver maps type
OIDs to Go types (int2/4/8→int64, float4/8→float64, bool→bool, bytea→[]byte, else
string) and interpolates `$N` params client-side. `jmoiron/sqlx` works unchanged.

## Wire protocol — full parity (2026-08-21)

Replaced the single-user backend with the **PostgreSQL v3 wire protocol** via
PGlite's own mechanism (`wire.go`), closing the remaining gaps. Reverse-engineered
from the PGlite 0.4.2 npm glue (the JS that drives *our* exact wasm):

- **Setup:** `pgl_set_rw_cbs(read, write)` → `pgl_setPGliteActive(1)` →
  `callMain(["--single","-F","-O","-j","-c","search_path=public", …, "-D", dir, db])`
  → `pgl_startPGlite()`. In PGlite mode `main()` finishes startup then "returns" via
  exit/longjmp while the backend **stays alive** — so the non-nil error from callMain
  is expected and ignored (cost an hour: I first treated it as fatal).
- **Drive (no Asyncify!):** stage a wire message; the backend's read callback is
  **non-blocking** (returns available bytes or 0), so just pump `PostgresMainLoopOnce`
  until the input is consumed and `pq_buffer_remaining_data()==0`, then
  `PostgresSendReadyForQueryIfNecessary` + `pgl_pq_flush`. The write callback captures
  the response, which is decoded. The StartupMessage (`msg[0]==0`) is handled via
  `pgl_getMyProcPort()` + `ProcessStartupPacket`.
- **Callbacks** are placed in the shared table (`RegisterRWCallbacks`, like the initdb
  bridge); longjmp during a pump is caught (`CallExportRaw`) and recovered with
  `PostgresMainLongJmp`.
- **Gotcha:** wire mode loads `pg_hba.conf` (single-user never does), whose default
  `127.0.0.1/32` CIDRs need `getaddrinfo` (stubbed out here). Fixed by writing an
  IP-free `local/host all all trust` hba before startup.

**Now decoded from the wire:** RowDescription (name + type OID), DataRow (with NULL),
CommandComplete (**command tag + RowsAffected**), ErrorResponse, ReadyForQuery (txn
status). The driver uses the **extended protocol** (Parse/Bind/Describe/Execute/Sync)
for `$N` params — real server-side prepared statements, no client-side interpolation.

**Parity with PGlite (query path): complete** — typed results, RowsAffected, command
tags, server-side prepared statements, transactions, temp tables/SET, all on one
persistent backend. Verified via `cmd/wire-probe` and the `pgdriver` tests (`-race`):
CRUD, `UPDATE`→RowsAffected=2, params, NULL, Begin/Commit/Rollback. Remaining niceties
(not blocking): COPY, LISTEN/NOTIFY.

**Binary result formats (added).** Parameterized (extended-protocol) queries request
**binary** result format for the OIDs we can decode losslessly (int2/4/8, oid, float4/8,
bool, bytea), text for the rest. Since the format must be chosen *before* the
RowDescription is known, `extendedQuery` runs two pumps: Parse+Describe(statement)+Sync
to learn the result OIDs, then Bind(per-column formats)+Execute+Sync. The wire layer now
decodes each value to a typed Go value (`int64`/`float64`/`bool`/`[]byte`/`string`/nil)
keyed by (OID, format) — so `Rows.Rows` is `[][]any` and the driver passes values
straight through (no more text→type conversion). Plain `Query` stays on the text simple
protocol (also typed-decoded). Verified by `TestBinaryResultFormats` (int2/4/8, float4/8,
bool, bytea → exact Go types). Also made the VFS manifest loader tolerant of float
offsets (a refreshed `pglite.data` manifest renders them as `5093000.0`).

**Latency vs the single-user path (ephemeral, warm, 500 iters, µs/query; same
machine/harness against commit 179c54d):**

| query | single-user (debugtup text) | wire | speedup |
|-------|-----------------------------|------|---------|
| `SELECT 1+1` | 218 | 153 | 1.4× |
| `SELECT` 10-row table | 613 | 322 | **1.9×** |
| `INSERT` one row | 165 | 144 | 1.1× |
| `SELECT count(*)` | 293 | 263 | 1.1× |

Both are the same persistent-backend model, so the delta is purely mechanism: the
single-user path does a goroutine channel handoff and parses the verbose `debugtup`
text (one line per attribute per row with typeid metadata), while wire pumps
`PostgresMainLoopOnce` directly and decodes compact binary framing. The win scales
with rows returned (1.9× on the 10-row SELECT), and is marginal for writes/scalars
where backend execution dominates. (Both are ~1000× faster than the original
fresh-`postgres --single`-per-query driver, which was ~200 ms/query.) Numbers are
µs-scale and noisy run-to-run, but the direction is consistent.

**Wire is now the sole query path** (the two single-user sections above are history).
The `postgres --single` query mechanism (feed SQL to stdin, parse `debugtup`) and its
supporting streaming-stdin plumbing (`wasiImpl.stdinReader`, the blocking `fd_read`
branch, `WTRuntime.SetStdinReader`) were removed once wire landed. What still uses
`--single`: PGlite starts the wire backend in single mode and drives it over
`pgl_set_rw_cbs` (a launch flag, not a query path); and initdb's `--boot`/`--single`
subcommands, which are fed bootstrap SQL via the surviving byte-slice `stdinData`
path. Also, initdb's own stdout/stderr are now routed to `io.Discard`, so the library
is fully quiet (an ephemeral, full-initdb run emits only the caller's output).

## Porting to PGlite 0.5.5 / PostgreSQL 18.3 (2026-08-21)

`scripts/update-wasm.sh` defaults to npm's `latest` dist-tag, so refreshing the
vendored assets jumped the build from **PGlite 0.4.2 (PostgreSQL 17.5)** to **0.5.5
(PostgreSQL 18.3)** — `strings wasm/pglite.wasm | grep PostgreSQL` reports
`PostgreSQL 18.3 (PGlite 0.5.5) on wasm32-unknown-linux-gnu`. `pglite.wasm` grew
8.7 MB → 10.1 MB. The reverse-engineered initdb/wire glue is pinned to specific
guest-memory addresses and PostgreSQL-version behaviour, so the suite went red with
`initdb produced no valid cluster (missing /global/pg_control)`. Two version-specific
fixes brought it back to green.

**1. Bootstrap-SQL capture — stdout `FILE*` address.** initdb runs
`popen("postgres --boot …", "w")` and writes ~950 KB of bootstrap SQL to that pipe.
We fake the pipe by pointing the guest's `stdout` `FILE` at a capture buffer, keyed
off the address of musl's static `__stdout_FILE`. That address moved between builds:
`29560` (0.4.2) → **`97448`** (0.5.5). With the stale address the capture returned
0 bytes, `--boot` got no stdin, and no `pg_control` was written — a silent failure
(initdb printed nothing to stderr). New value re-derived from the initdb.wasm layout;
see `cluster.go` `OnPopen`.

**2. PostgreSQL 18 file-descriptor probe.** On startup PG 18's `set_max_safe_fds()`
calls `count_usable_fds()`, which `dup()`s `stderr` (fd 2) ~58 times to measure how
many descriptors it can safely open, then closes the copies. Our stdio fds 0/1/2 are
serviced by the WASI layer and never appear in `vfs.FS.fds`, so `dup(2)` returned
`EBADF`; PG logged `duplicating stderr file descriptor failed after 0 successes` and
aborted with `insufficient file descriptors available to start server process
(System allows 0, server needs at least 58)`. PG 17.5 didn't run this probe on the
`--boot` path. Fix: `vfs.FS.Dup` now hands out placeholder descriptors for fds 0/1/2
so the probe (and the follow-up closes) succeed.

With both in place the `--boot` phase exits 0 and writes `pg_control`; the full
`pgdriver` suite (CRUD, binary result formats, transactions) and `vfs` pass, and the
`-tags wazero` fallback still builds.

**Known non-fatal issue.** initdb's post-bootstrap `postgres --single template1`
phase FATALs with `could not open collator for locale "und": U_FILE_ACCESS_ERROR` —
PG 18 opens the ICU **root** collator even though initdb runs with
`--locale-provider=libc --locale=C.UTF-8`. The phase aborts on its tail, but the
essential catalog is already in place, so every test passes. Some late system objects
may be missing; unresolved, and worth confirming before relying on ICU collations.

**Debugging aids added.** `PGLITE_DEBUG_INITDB=1` routes initdb's stderr through and
traces each `system`/`popen`/`pclose` call plus the `--boot` exit code and
`pg_control` presence — this is what localized both failures. To go back to the
older, fully-reverse-engineered build: `PGLITE_VERSION=0.4.2 ./scripts/update-wasm.sh`.

Also removed seven obsolete diagnostic commands under `cmd/` (`bench-wasmtime`,
`dump-imports`, `inspect-wasm`, `probe-wasmedge`, `probe-wasmtime`, `prof-compile`,
`wire-probe`); `cmd/pglite` (library demo) and `cmd/pglite-poc` (pure-Go wazero
fallback) remain.

## Wazero backend: full wire parity, one demo (2026-08-21)

The pure-Go wazero path used to be a separate self-contained program
(`cmd/pglite-poc`) on the old `postgres --single` query path, while the library,
wire protocol, and `database/sql` driver were wasmtime-only (`//go:build
!wazero`). Consolidated to **one command, one demo**: `cmd/pglite` now builds on
both backends from a single source file, and wazero runs the *same* library and
wire protocol as wasmtime — no feature or query-path difference.

**Architecture.** Introduced `emcompat.Runtime`, a backend-agnostic interface
(CallExport/CallExportRaw/CallMain, RegisterRWCallbacks/RegisterInitdbCallbacks,
ApplyDataRelocs, FindStdoutFILE, SetStdout/Stderr/StdoutCapture). `*WTRuntime`
(wasmtime) and the new `*WZRuntime` (wazero) both implement it, so `pglite.go`,
`wire.go`, `query.go`, and `cluster.go` dropped their build tags and are written
once against the interface. A small tagged shim (`backend_wasmtime.go` /
`backend_wazero.go`) provides the `wasmEngine`/`wasmModule` aliases and the
`newEngine`/`compileModule`/`newRuntimeFrom{Module,Wasm}` constructors. The mmap
linear-memory allocator moved into the root package (wazero-tagged).

Two things had to be re-implemented for wazero, which (unlike wasmtime-go) exposes
neither a runtime table-mutation API nor a longjmp trap:

- **Host callbacks into the function table.** wazero can't append host funcs to
  the indirect table at runtime, so `RegisterRWCallbacks`/`RegisterInitdbCallbacks`
  generate a tiny bridge wasm module that imports the shared table and the host
  functions and uses a static **element segment** to place wrapper functions at a
  reserved base index. The base sits *above* the module's own entries: the env.extras
  table is now sized from the module's **dylink `table_size`** (parsed by
  `ParseDylink`) instead of the old hardcoded `6098`, plus `reservedTableSlots`
  headroom, and callbacks go at `tableSize+1`. (The 0.4.2 constant was also just
  wrong for 0.5.5, whose `pglite.wasm` needs `table_size=7366`.)
- **longjmp out of an export.** In wire mode `PostgresMainLoopOnce` returns via
  Emscripten longjmp. wazero's built-in `_emscripten_throw_longjmp` panics with an
  error that wazero wraps (`%w`) into the value returned from `Call`, so
  `WZRuntime.CallExportRaw` detects it by the substring `_emscripten_throw_longjmp`
  — the exact analogue of the wasmtime port's trap-sentinel check. `proc_exit(N)`
  likewise surfaces as an error string containing `exit(N)`, so `runCmd`'s existing
  matching works unchanged across both backends.

**Validated end-to-end** (`go run -tags wazero ./cmd/pglite`, `CGO_ENABLED=0`):
full initdb (multiple `postgres` subcommands over wazero) + `pgl_set_rw_cbs`
handshake + `SELECT 1+1` → `2 | hello, pglite`. Cold ~315 s (dominated by the
one-time 10 MB `pglite.wasm` compile); warm (cache + skip-initdb) ~4.1 s. The
wasmtime suite (`pgdriver`, `vfs`) stayed green throughout the refactor, and both
backends build+vet clean.

**Files.** New: `emscripten/runtime.go` (the `Runtime` interface + `RWCallbacks`,
moved off the wasmtime file), `emscripten/wazero_port.go` (`WZRuntime` + the RW
bridge builder), `emscripten/dylink.go` (`ParseDylink`), `backend_wasmtime.go` /
`backend_wazero.go` (the tagged engine/module shims), `mmap_alloc.go` /
`mmap_alloc_other.go` (moved from the deleted poc). Changed: `pglite.go`,
`wire.go`, `query.go`, `cluster.go` (tags dropped, now interface-driven);
`emscripten/syscall.go` (dylink-derived table size + `reservedTableSlots`);
`emscripten/systemcb.go` (`instantiateInitdbCallbacksInto` for externally-owned
callbacks); `emscripten/wasi.go` (`WASIInstance.SetStdout/SetStderr`);
`emscripten/wasmtime_port.go` (`RWCallbacks` moved out, interface assertion).
Removed: `cmd/pglite-poc/`.

## Work Summary — 2026-08-21 (VFS refactored into an io/fs-backed overlay)

Reshaped the `vfs` package from a single in-memory node tree into an **overlay
filesystem** built on the standard `io/fs` abstraction: a read-only **lower** layer
(`fs.FS`) stacked under a writable **upper** layer (a `WritableFS`, in-memory by
default). The overlay owns the cross-cutting state — the open-fd table, cwd, path and
symlink resolution, copy-up, and whiteouts — while the layers only store bytes.

**Why.** Previously `LoadManifest` copied the whole 6.3 MB / 699-entry install bundle
into the node tree, so the install lived twice in RAM (the `pglite.data` buffer *and*
per-entry `Node.Data`). Making the install a lower `fs.FS` lets it be served
**zero-copy** — each file is a `ReadAt` over a sub-slice of the bundle blob — dropping
resident memory by roughly the install size. The writable data dir stays in the
in-memory upper layer, so hot-path read/write speed is unchanged. Each byte now lives
in exactly one layer, so there is no eager-vs-lazy materialization to schedule.

**The `File` union is the linchpin.** `type File interface { fs.File; io.ReaderAt;
io.WriterAt; io.Seeker }`. A concrete backing store provides real random access
through it — the bundle file over a `[]byte`, and (for a future write-through upper)
`*os.File`, which already satisfies the whole union. This is what defeats the "io/fs is
streaming-only, so it can't back a random-access DB workload" objection: the interface
only *guarantees* `fs.File`, but the concrete sources we use implement the full union,
so `pread`/`pwrite` map straight onto `ReadAt`/`WriteAt`.

**Design decisions (settled with the user, over several iterations):**
- Overlay with copy-up + whiteouts — *not* eager materialization, *not* a
  path-partitioned mux. Copy-up/whiteout are correct but effectively cold paths (the
  install is immutable; PG never writes under `/pglite`).
- Upper layer behind a `WritableFS` interface so an `os`-dir/write-through impl is
  possible later, but the **default upper is in-memory**, preserving today's all-in-RAM
  data dir and `SaveSubtree` persistence.
- Persisted-cluster reload **loads into the upper** (as before) via
  `LoadSubtree(os.DirFS(persistDir), dataDir)` — not mounted as a pristine lower.
- Public `Node`/`FileType`/`OpenFile` surface kept, so the syscall shim is largely
  untouched; only a few call sites changed (no back-compat shims).

**Findings / gotchas:**
- `Fstat`/`Stat` must return `*Node` even for lower-backed files, so the overlay
  **synthesizes** a transient `*Node` from `fs.FileInfo`. Size can't come from
  `len(Data)` (a lower file's bytes aren't resident), so `Node` gained a `Size()`
  method (`dataSize` field for synthesized nodes) and `writeStat64` now calls
  `node.Size()` instead of `len(node.Data)`.
- The overlay owns the per-fd offset and uses **positioned I/O exclusively**
  (`handle.ReadAt`/`WriteAt`); `fd_pread`/`fd_pwrite` dropped their save-seek-read-restore
  dance for new `FS.Pread`/`FS.Pwrite`. `memHandle`'s own offset is only used if a caller
  drives `Read`/`Seek` through the `File` interface directly.
- Go **1.25** `fs.ReadLink`/`fs.Lstat`/`fs.ReadLinkFS` make symlinks representable across
  the boundary; `os.DirFS` implements `ReadLinkFS`, so data-dir symlinks (`pg_wal`, etc.)
  round-trip. `WalkDir` reports symlinks via `DirEntry.Type()` without following them.
- `WritableFS` is defined and satisfied by `memFS`, but the overlay uses the concrete
  `*memFS` for the richer internal ops (raw `Node` access, copy-up). A genuinely
  pluggable upper would need those ops expressed on the interface — deferred until an
  `os`-backed upper is actually built.
- macOS `os.UserCacheDir` **ignores `XDG_CACHE_HOME`** and uses `~/Library/Caches`;
  isolate persistence tests with an explicit `Config.PersistDir` (a `t.TempDir()`).

**Coupling scan (for "update all consumers").** Only two files carry `vfs` *semantics* —
`emscripten/emsyscall.go` (path + fd + the `Node`-field reads in `writeStat64`) and
`emscripten/wasi.go` (fd `Read/Write/Seek`, `GetFD().Path`). The wasmtime port reuses the
same `wasiImpl`. Everything else (`syscall.go`, both `backend_*.go`, the two runtime
ports, `wire.go`, `cluster.go`) only threads `*vfs.FS` by pointer or calls `WriteFile`.
`vfs.O_*` and `Node.Name/Children/Parent`, `OpenFile.Node/Flags` are unreferenced outside
`vfs/`, so the retained surface is exactly what external code needs.

**Verification.** `go build ./...` clean; `go vet` clean except two *pre-existing* style
warnings in the untouched `wasmtime_port.go`. `go test ./...` green (vfs round-trip +
full `pgdriver` database/sql suite). End-to-end: fresh `initdb`+query, then reload
(initdb skipped); and a stricter run that created a table + row, restarted from the
persisted dir, and read `42 | overlay` back — proving bundle-lower reads, writable-upper
writes, `Pread`/`Pwrite`, and the `SaveSubtree`→`os.DirFS`→`LoadSubtree` cycle.

**Files.** New: `vfs/file.go` (the `File` union, `memHandle` with 2× append growth,
`roBufHandle` fallback, `nodeInfo` → `fs.FileInfo`), `vfs/memfs.go` (in-memory
`WritableFS` + `Node`/`FileType` + whiteout type), `bundlefs.go` (`bundleFS`: zero-copy
read-only `fs.FS` over `pglite.data` + manifest, `StatFS`/`ReadDirFS`). Rewritten:
`vfs/vfs.go` (overlay `FS`, `New(lower fs.FS)`, `Pread`/`Pwrite`), `vfs/persist.go`
(`LoadSubtree(src fs.FS, …)` via `fs.WalkDir`). Changed: `pglite.go`
(`vfs.New(loadBundleFS(...))`, `LoadSubtree(os.DirFS(...))`), `emsyscall.go`
(`node.Size()`), `wasi.go` (pread/pwrite → `Pread`/`Pwrite`), `vfs/persist_test.go`.
Removed: `vfs.LoadManifest` + `vfs.ManifestEntry` (bundle knowledge moved to
`bundlefs.go`).

## Work Summary — 2026-08-21 (asset distribution: go-generate fetcher, -tags embed, jsDelivr on-demand)

Built three complementary ways to supply the wasm artifacts (`pglite.wasm` 10 MB,
`pglite.data` 6.3 MB, `initdb.wasm` 395 KB, `pglite.manifest.json` 82 KB), all funnelled
through the `fs.FS` seam the VFS refactor introduced. Assets stay **gitignored/untracked**.

**Enabling change: asset loading is now `fs.FS`-driven.** `Config` gained `WasmFS fs.FS`
and `Download bool`; `loadBundleFS` takes an `fs.FS`; `pglite.go` reads the two `.wasm`
blobs via `fs.ReadFile(wasmFS, …)`. `resolveWasmFS` picks the source in precedence order:
`WasmFS` → `WasmDir` → embedded (`-tags embed`) → `./wasm` (if populated) → on-demand
download (if `Download`). The wasmtime `.cwasm` compile cache keys off the wasm **content
hash** under `os.UserCacheDir()`, independent of source, so warm starts survive every mode.

**1. Go fetcher (`go generate`).** Ported `scripts/update-wasm.sh` (bash + python + curl +
tar) to `internal/pgassets` + the `internal/fetchwasm` CLI, wired via
`//go:generate go run ./internal/fetchwasm`. Key simplification from the user's jsDelivr
idea: jsDelivr serves the npm package's `dist/` files **individually**, so there's no
tarball — just four HTTP GETs, no `archive/tar`/`compress/gzip` handling. The manifest is
still regex-extracted from `dist/pglite.js` (`loadPackage({files:[…],remote_package_size:N`)
and re-emitted as JSON, byte-for-byte identical to the python output (verified: all four
files `cmp`-identical to the vendored copies, 699 entries).

**2. `-tags embed`.** `assets_embed.go` (`//go:build embed`) has `//go:embed wasm` +
`defaultEmbeddedFS()`; `assets_noembed.go` returns nil. A default build never references
the embed (so `go get` consumers and clean CI still build without the assets); an
`-tags embed` build after `go generate` bakes them in. Verified: `go build -tags embed
./cmd/pglite` → 55 MB self-contained binary that runs from an empty dir.

**3. jsDelivr on-demand (`Config.Download`).** Opt-in (off by default so `Open` never does
surprise network I/O). `downloadAssets` fetches the **pinned** version into
`UserCacheDir/pglite-go/assets/<ver>/`, staged in a `.tmp` sibling and `os.Rename`d into
place so a partial fetch never looks complete; a populated cache is a fast no-op. Verified
end-to-end: from a dir with no `wasm/` and no embed, `Config{Download:true, Ephemeral:true}`
fetched from jsDelivr, ran `initdb`, and returned `SELECT 1+1 → 2`.

**Findings / decisions:**
- **Version must be pinned.** The host layer carries version-specific constants (initdb
  stdout FILE* address, PG18 fd-probe), so `pgassets.Version = "0.5.5"` (exposed as
  `pglite.PgliteVersion`) is the single source of truth. Both the fetcher default and the
  runtime download use it; `go generate` fetching "latest" would risk an incompatible bump.
- **jsDelivr compression gotcha.** A range request reported `pglite.data` as 1.67 MB
  (~3.7× smaller) while `pglite.js` said `remote_package_size:6293225`. jsDelivr serves
  these blobs **gzip-compressed on the wire**. Go's `net/http` adds `Accept-Encoding: gzip`
  itself and transparently decodes → a plain `http.Get` yields the correct 6,293,225
  uncompressed bytes. The fetcher deliberately leaves `Accept-Encoding` unset (setting it
  manually would disable auto-decode, and asking for brotli would return bytes Go can't
  inflate). Content-Length is unreliable under transport gzip, so it `io.ReadAll`s.
- **`go generate` ≠ build time.** It never runs during `go build`/`go get`/`go install`
  (Go has no build hooks). So the fetcher helps source builds/CI; it does nothing for a
  downstream `go install …@latest` consumer — which is exactly the gap `Download` (runtime
  fetch) and `WasmFS` (consumer-side embed) fill.
- `pgdriver` DSN gained `download=true`.

**Files.** New: `internal/pgassets/pgassets.go` (shared jsDelivr fetcher + manifest
extraction), `internal/fetchwasm/main.go` (CLI), `generate.go` (`//go:generate`),
`assets.go` (`resolveWasmFS`/`downloadAssets`/`PgliteVersion`), `assets_embed.go` /
`assets_noembed.go`. Changed: `pglite.go` (`Config.WasmFS`/`Download`, `fs.FS`-based load),
`bundlefs.go` (`loadBundleFS(fs.FS)`), `pgdriver/driver.go` (`download` DSN key), `README.md`.
The legacy `scripts/update-wasm.sh` was removed — `go generate` (the Go fetcher) replaces it.

## Timeline

| Milestone | Status |
|-----------|--------|
| WASM loading + compilation | Done |
| Emscripten host function layer | Done |
| In-memory VFS + preloaded data | Done |
| Custom WASI implementation | Done |
| WASM binary patching | Done |
| initdb callback bridge (system/popen/pclose) | Done |
| PostgreSQL boot (config parsing, timezone) | Done |
| initdb directory/config creation | Done |
| initdb bootstrap (postgres --boot, 953KB SQL) | Done |
| initdb post-bootstrap (postgres --single, 249KB SQL) | Done (exit 1 on collation import) |
| Anonymous mmap for shared memory | Done |
| PostgreSQL single-user mode start | Done (checkpoint works, 8 buffers, 7 sync files) |
| SQL query execution (single-user mode) | Done (`SELECT 1+1` → result) |
| Persistent (file-backed) compilation cache | Done (0.58s warm vs ~138s cold) |
| VFS persistence + skip-initdb on restart | Done (2.3s restart vs 8.6s first run) |
| wasmtime compile + instantiate benchmark | Done (1.43s compile, 0.002s instantiate) |
| wasmedge AOT compile benchmark | Done (218s LLVM-O2, 20.6MB .so) |
| wasmedge instantiation probe (raw CGo, 0.17.1) | Done (binding incompatible; C API works) |
| wasmedge full execution port | Not started (feasibility proven; large CGo effort) |
| wasmtime execution port: postgres -V runs | Done (1.5s compile+instantiate+run) |
| wasmtime full initdb + SELECT 1+1 | Done (2.93s total vs wazero ~11s) |
| Library API (pglite package) | Done |
| database/sql driver + sqlx | Done (typed rows, params, NULL) |
| Persistent session + real transactions | Done (cross-call BEGIN/COMMIT/ROLLBACK, -race clean) |
| Wire protocol (pgl_set_rw_cbs) | Done — typed results, RowsAffected, server-side prepared stmts |
| Binary result formats (int/float/bool/bytea) | Done |
| Port to PGlite 0.5.5 / PostgreSQL 18.3 | Done — FILE* addr 29560→97448, PG18 fd-probe Dup fix |
| ICU "und" collator in initdb post-bootstrap | Open (non-fatal; tests pass) |
| wazero backend: full wire parity via emcompat.Runtime | Done — one `cmd/pglite` on both backends; poc removed |
| Warm-start persistence hang on 0.5.5 user-table scans | Open (see memory note; ephemeral works) |
| VFS refactored into an io/fs-backed overlay (lower fs.FS + writable upper) | Done — zero-copy install, `File` union, `Pread`/`Pwrite`; pgdriver suite green |
| Asset distribution: go-generate fetcher + `-tags embed` + jsDelivr on-demand | Done — Go fetcher (byte-identical), 55 MB self-contained binary, `Config.Download` verified |
