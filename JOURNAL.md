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

1. Imports Go handler functions from a `initdb_host` module
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

For pglite.wasm, we skip the start function (`WithStartFunctions()`) and manually call `__wasm_apply_data_relocs`. This is sufficient because PostgreSQL doesn't use C++ iostream.

### 6. Shell command parsing must stop at operators

**Impact**: initdb's `system("postgres --check ... < /dev/null > /dev/null 2>&1")` passed shell redirections as postgres arguments.

PGlite's `getArgs()` uses a shell parser that recognizes `<`, `>`, `|`, etc. as operator tokens and stops collecting arguments. We replicate this by breaking at shell operator tokens including `>/path` (no space between `>` and path).

### 7. VFS write performance

**Impact**: 74 million fd_write calls for 953KB of bootstrap SQL took 600+ seconds.

musl's stdio flushes the FILE buffer via WASI `fd_write`. Each call crossed the WASM→Go boundary and our VFS `Write` method allocated a new byte slice for every extension (`make + copy` = O(n^2)).

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

Since fopen-created FILE structs had broken write function pointers (the exact cause is still unclear - possibly related to musl's `__stdio_write` function index), we use WASI stdout capture: popen("w") starts capturing stdout, initdb writes SQL via its existing stdout FILE struct (which works), and pclose() saves the captured data and feeds it to postgres as stdin.

## Current Status

### What Works

- **initdb**: Creates full database cluster (directories, configs, system catalogs)
- **postgres --boot**: Processes 953KB of bootstrap SQL, creates `global/pg_control`
- **postgres --single**: Starts with `backend>` prompt, performs WAL checkpoints
- **VFS**: 694 files from PGlite npm package + runtime-created files
- **30+ syscalls**: openat, stat64, lstat64, faccessat, mkdirat, readlinkat, getdents64, pipe, getcwd, chdir, fchmod, fchown, unlinkat, renameat, fcntl64, ioctl, dup, ftruncate64, fdatasync, utimensat, statfs64, fadvise64, fallocate, symlinkat
- **Custom WASI**: fd_read/write/seek/close/sync/pread/pwrite, environ, clock_time_get, random_get, proc_exit
- **Emscripten runtime**: invoke_*, longjmp, date_now, get_now, resize_heap, localtime_js, gmtime_js, mktime_js, tzset_js, setitimer_js

### What Remains

1. **Post-bootstrap initialization**: `postgres --single` for template1 setup returns exit code 1 (some init SQL fails). Likely related to missing `mmap` support for shared memory or missing socket syscalls.

2. **Wire protocol bridge**: To execute SQL queries, need to implement PGlite's `pgl_set_rw_cbs` mechanism which provides read/write callbacks for the PostgreSQL wire protocol (same protocol as libpq).

3. **Shared memory (`mmap`)**: Implemented via `emscripten_builtin_memalign`. Anonymous mmap allocates aligned memory in the WASM linear memory. PostgreSQL's shared memory subsystem now works.

4. **fopen FILE struct issue**: Dynamically-created FILE structs from `fopen` have correct function pointer values (verified by memory dump) but writes cause an infinite loop (74M+ fd_write calls). The root cause is unclear - the workaround uses `pgl_freopen` + WASI stdout capture instead of `fopen` for pipe files.

5. **Emscripten environment variables**: Emscripten doesn't use WASI `environ_get`. It has its own env mechanism via `Module.ENV` in JavaScript. Our WASI environ functions are never called. Workaround: pass `-D /tmp/pglite/data` explicitly to all postgres subcommands.

6. **Compilation cache**: `wazero.CompilationCache` shares compiled code across runtimes. First compilation takes ~90 seconds, subsequent instantiations are fast (~1-2s each).

7. **Performance**: Each postgres subcommand creates a new wazero runtime. `CompilationCache` helps but instantiation overhead remains. PGlite's JS version reuses a single module instance with heap restoration.

## Timeline

| Milestone | Status |
|-----------|--------|
| WASM loading + compilation | Done |
| Emscripten host function layer | Done |
| In-memory VFS + preloaded data | Done |
| Custom WASI implementation | Done |
| PostgreSQL boot (config parsing) | Done |
| initdb directory/config creation | Done |
| initdb bootstrap (postgres --boot) | Done |
| initdb post-bootstrap (postgres --single) | Done (exit 1 on collation import) |
| PostgreSQL single-user mode start | Done (checkpoint works, 8 buffers) |
| Wire protocol for SQL queries | Not started |
| `database/sql` driver interface | Not started |
