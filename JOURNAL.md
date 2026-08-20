# pglite-go Development Journal

## Project Goal

Embed PostgreSQL in Go by combining [PGlite](https://github.com/electric-sql/pglite) (PostgreSQL compiled to WASM via Emscripten) with [wazero](https://github.com/tetratelabs/wazero) (pure Go WebAssembly runtime).

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
- Behind `//go:build wasmtime`; the default CGo-free wazero build is untouched.
  Run: `go run -tags wasmtime ./cmd/pglite-wasmtime`.

### New tooling
`cmd/bench-wasmtime` (compile timing), `cmd/dump-imports` (import inventory),
`cmd/probe-wasmtime` (instantiation probe), `cmd/pglite-wasmtime` (execution port),
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
│   ├── pglite-poc/     # PoC launcher (initdb + postgres)
│   └── inspect-wasm/   # WASM import/export inspector
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
A wasmtime compile cache (not yet added) would drop its total to ~1.6s.

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
| wasmtime execution port: postgres -V runs | Done (1.5s compile+instantiate+run) |
| wasmtime full initdb + SELECT 1+1 | Done (2.93s total vs wazero ~11s) |
| Wire protocol (pgl_set_rw_cbs) | Not started |
| `database/sql` driver interface | Not started |
| VFS persistence | Not started |
