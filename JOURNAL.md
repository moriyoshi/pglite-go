# pglite-go Development Journal

## Project Goal

Embed PostgreSQL in Go by combining [PGlite](https://github.com/electric-sql/pglite) (PostgreSQL compiled to WASM via Emscripten) with [wazero](https://github.com/tetratelabs/wazero) (pure Go WebAssembly runtime).

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

6. **Compilation cache persistence**: First compilation of `pglite.wasm` (8.7MB) takes ~90 seconds (AOT compilation to native code). `wazero.CompilationCache` helps with subsequent instantiations within the same process, but the cache is lost on restart. Consider using file-based cache persistence.

7. **Single-instance reuse**: Each postgres subcommand currently creates a new wazero runtime (compile → instantiate → run → close). PGlite's JS version reuses a single WASM instance with heap restoration (`HEAPU8.set(origHEAPU8)`) for dramatically faster repeated calls. Implementing this pattern in wazero would require snapshotting/restoring WASM linear memory.

8. **VFS persistence**: The in-memory VFS loses all data when the process exits. For a production embeddable database, need to persist the VFS to disk (or implement a VFS layer backed by the host filesystem).

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
| Wire protocol (pgl_set_rw_cbs) | Not started |
| `database/sql` driver interface | Not started |
| VFS persistence | Not started |
