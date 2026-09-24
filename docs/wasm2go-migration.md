# wasm2go migration — standalone-wasm pipeline

This documents the experimental path to running PGlite as **pure Go** via
[goccy/wasm2go](https://github.com/goccy/wasm2go) (an AOT wasm→Go transpiler),
instead of the wasmtime/wazero runtimes. It is not the active runtime; it is a
feasibility track with a working toolchain.

## Why a pre-pass is needed

`pglite.wasm` is an Emscripten **MAIN_MODULE** (dynamic-linked) that implements
`setjmp`/`longjmp` via a host-thrown unwind (`SUPPORT_LONGJMP=emscripten`). Two
things block a direct wasm2go translation:

1. **Dylink imports.** The module imports its memory, table, and the base globals
   `__memory_base` / `__stack_pointer` / `__table_base` / `__heap_base`. wasm2go
   wants a standalone module.
2. **Host-thrown longjmp.** `longjmp` calls the host import
   `_emscripten_throw_longjmp`, which in a real engine unwinds the native stack to
   the nearest `invoke_*`. wasm2go has no engine and no unwind hook other than Go
   `panic`/`recover`; generated `call`s are plain Go calls with no post-call
   check, and only setjmp *owners* read `__THREW__`.

`internal/wasmpass` rewrites the module to fix both **without** forking wasm2go or
rebuilding PGlite, and **without** panic/recover:

- **Strip** — keep only the ~16 exports pglite-go actually calls (avoids wasm2go
  export-name mangle collisions; lets DCE prune to the reachable subset).
- **Prelink** — internalize memory/table/base-globals to the constants the
  existing backends use (`emscripten/syscall.go`: memory_base=0,
  stack_pointer=heap_base=10937088, table_base=0), and fold elem/data segment
  offsets from `global.get` to literal `i32.const` (wasm2go rejects `global.get`
  in a const-expr).
- **Instrument** — after every call that can transitively reach
  `_emscripten_throw_longjmp` (plus every `call_indirect`), insert a stack-neutral
  guard:

  ```wat
  i32.const <__THREW__ addr> ; i32.load ; if (void) { <restore SP> <typed zeros> return } end
  ```

  This turns the host-thrown unwind into **cooperative returns**: the host
  `_emscripten_throw_longjmp` becomes a no-op that returns, each intermediate
  frame early-returns while `__THREW__` is set, and the setjmp owner (which is
  still live and whose `invoke_*` call site is a host import, so it is *not*
  instrumented) runs its existing `__wasm_setjmp_test` dispatch unchanged. Only
  unwind is needed, never rewind — far lighter than asyncify.

## Pipeline

```sh
go generate ./...                                   # 1. fetch pglite.wasm into ./wasm
go run ./cmd/wasmpass -i wasm/pglite.wasm \
    -o wasm/pglite.standalone.wasm                  # 2. strip + prelink + instrument  (~1s)
go run github.com/goccy/wasm2go/cmd/wasm2go@latest \
    -i wasm/pglite.standalone.wasm \
    -import github.com/moriyoshi/pglite-go/internal/pgwasm \
    -pkg pgwasm -pure -out-dir internal/pgwasm      # 3. transpile to Go (~90s, ~7GB RAM)
```

The host `env` interface wasm2go emits is then implemented by the pglite-go host
layer: the 129 function imports (`invoke_*` as plain indirect-call passthroughs,
`_emscripten_throw_longjmp` as a no-op return, the syscalls/WASI) plus the socket
and initdb callbacks.

## Measured cost (PGlite 0.5.5, `-pure`, macOS amd64)

| stage | baseline (strip+prelink) | + longjmp instrumentation |
|---|---|---|
| wasm size | 10.10 MB | 12.03–12.34 MB (+19–22%) |
| coverage | — | 11,057 may-longjmp funcs; 136,835 sites (80.1%) |
| wasm2go generate | 80s / 7 GB | 90s |
| generated Go | 10.9M LOC / 199 MB | 14.1M LOC / 221 MB (+29% LOC) |
| `go build ./...` | 155s / 2.8 GB | 174s / 2.5 GB (+12%) |

Headline: wasm2go'd PostgreSQL **compiles** (~11M LOC, ~2.5 min, ~3 GB), and the
longjmp instrumentation is cheap (+12% compile, +19% wasm). The `wasmpass` step
itself runs in ~1s.

## Known wasm2go gotchas (pre-1.0, worked around)

- Export-name mangle collisions (e.g. `relation_close`/`RelationClose`) → Strip.
- Multi-package top wrapper emits all three constructors named `New` → rename
  `NewWithMemory`/`NewFromSnapshot` (upstream bug).
- `global.get` in elem/data const-exprs unsupported → Prelink folds offsets.

## Three backends, one interface

The AOT path is a third `emscripten.Runtime` implementation, selected by build tag
alongside the existing engines — the orchestration in `wire.go`/`cluster.go`/
`pglite.go` is unchanged:

| tag | backend | properties |
|-----|---------|------------|
| *(default)* `!wazero && !aot` | wasmtime (`backend_wasmtime.go`) | CGo, fast |
| `wazero && !aot` | wazero (`backend_wazero.go`) | pure-Go, portable, slower |
| `aot` | wasm2go (`backend_aot.go` + `emscripten/aot_port.go`) | pure-Go, fast, no engine/asset |

`A2GRuntime` (in `emscripten/aot_port.go`) drives the transpiled module through a
small `AOTModule` interface, so it is type-agnostic and shared by the pglite and
initdb transpiled packages. The host layer is **not** rewritten: the syscall/WASI
handlers already funnel through wazero's `api.GoModuleFunc` ABI (the wasmtime
backend adapts it too), so the generated env/WASI adapter bridges the module's
`EnvImports` to those same handlers via an `api.Memory` shim over `m.Memory`.
Only `backend_aot.go`, `aot_port.go`, and the generated shim+adapter are
AOT-specific; everything else is shared. Cooperative longjmp maps to
`CallExportRaw` reading (and clearing) `__THREW__` after the top-level call — no
Go panic/recover.

## Keeping the distributed codebase thin: the companion module

The transpiled Go is ~14M LOC / ~200 MB — far too much to carry in this module. It
lives in a **separate companion module, `github.com/moriyoshi/pglite-go-aot`**, so
this core module stays thin (the default wasmtime/wazero backends need no generated
Go at all). AOT is enabled with a `database/sql`-style **driver blank-import**:

```go
import (
    "github.com/moriyoshi/pglite-go"
    _ "github.com/moriyoshi/pglite-go-aot" // registers the AOT backend via init()
)
// build with -tags aot
```

The companion imports this module's `emscripten`/`vfs` packages and calls
`pglite.RegisterAOT(pgwasm.NewAOT, initdbwasm.NewAOT)` from an `init()`. This module
never imports the companion — so there is **no module cycle**, and `-tags aot`
compiles here with no generated code present (the constructors are nil until a
companion registers them; `newRuntimeFrom*` returns a clear error otherwise). The
companion is regenerated and re-tagged per PGlite release (the `internal/wasmpass`
+ `internal/genaot` tools live here).

Other levers, if the companion itself needs to be smaller: keep `-pure` (the asm
backend doubles output); keep tight DCE roots; and have `internal/wasmpass` share
one unwind landing per function instead of per-site (attacks the +29% LOC). The
10 MB `pglite.wasm` remains the single source of truth for all three backends —
runtime backends load it, AOT transpiles it.

## Generated glue (`internal/genaot`)

`go generate -tags aot` runs, after wasm2go, the `internal/genaot` emitter, which
reads the transpiled package's `base.go` and writes `aot_generated.go` into it:

- **`NewAOT(fs, stdin, capture)`** — builds the env host adapter and instantiates
  the module (`New`, using the built-in `DefaultWASI`), returning an
  `emcompat.AOTModule`.
- **`envHost`** implementing `base.EnvImports` (all 116 methods): `invoke_*` →
  indirect call through the exported `m.T0`; `_emscripten_throw_longjmp` → no-op
  (the cooperative-unwind driver); the ~60 syscalls → stubs today
  (`TODO(aot)`: bridge to the shared `api.GoModuleFunc` handlers via an
  `api.Memory` shim over `m.Memory`).
- **`aotModule`** implementing `emcompat.AOTModule`: `Memory`/`ApplyDataRelocs`/
  `CallCtors`, a `Call` name-dispatch over the export roots, and `RegisterRW`/
  `RegisterInitdb` that append typed closures to `m.T0` (trivial in AOT — no
  wazero-style bridge module needed).

**Verified:** with `internal/pgwasm` generated, `go test -tags aot` drives
`backend_aot.go → NewAOT → New → A2GRuntime → ApplyDataRelocs` (data relocs +
`__wasm_call_ctors`) — the AOT backend links and PostgreSQL init runs, no panic.
`internal/initdbwasm/aot.go` is a committed placeholder so the build links before
initdb is transpiled (its `Call` panics; the initdb path is not yet functional).

## Host bridge (`emscripten/aot_membridge.go`)

The shared syscall/WASI/systemcb handlers are `api.GoModuleFunc` —
`func(ctx, api.Module, stack)` — reading guest memory via `mod.Memory()`. To reuse
them from the AOT backend (where memory is a plain `[]byte`), the natural move is
to fabricate an `api.Module`/`api.Memory` over the slice. **That is impossible:
wazero seals those interfaces with an unexported `wazeroOnly()` method.**

The fix is project-defined interfaces the handlers can target instead:

- `Mem` — the subset of `api.Memory` the handlers actually use (`Read`/`Write`/
  `ReadByte`/`WriteByte`/`Read|WriteUint32Le`/`Read|WriteUint64Le`/`Size`/`Grow`).
- `HostFn` — the `Call` subset of `api.Function`.
- `Host` — `Memory() Mem` + `ExportedFunction(name) HostFn`.

`sliceMemory` (over a `func() []byte` accessor, so it tracks memory growth) and
`hostModule`/`hostFunc` implement these directly for AOT. Crucially, **`api.Memory`
structurally satisfies `Mem`** (verified), so the wazero and wasmtime backends
adapt by wrapping their `api.Module` in a tiny `Host` with the memory passed
through unchanged.

**Resolved — no handler refactor needed.** wazero's own trick works: the wasmtime
backend (`wtModule`) *embeds* the sealed `api.Module`/`api.Memory` (nil) so the
`wazeroOnly()` marker is promoted, then overrides only `Memory()`/`ExportedFunction()`.
`aot_membridge.go` does the same over a `[]byte` (`aotMemory`/`aotAPIModule`/
`aotFunction` + `NewAOTModuleAdapter`), so the shared handlers run unchanged.
`aot_dispatch.go` (`AOTDispatch`) assembles the syscall map (`NewSyscallHandler(fs).Register()`)
and a VFS-backed WASI map, and `Invoke(name, mod, stack, params, results)` resolves
(with the `createHostFunc`/`makeStub` fallbacks) and runs the handler. `genaot`
emits, per non-invoke import, the stack marshaling + `disp.Invoke` by reverse-mangled
name. `CallMain` marshals argv onto the shadow stack (via `SP()`/`SetSP()` = global
`G1`) — no malloc needed.

WASI is also VFS-backed: `genaot` emits the 13 `Wasi_snapshot_preview1Imports`
methods onto the same `host` struct (no name overlap with `EnvImports`) routing
through the same dispatch, and `NewAOT` uses `NewWithWASI(h, h)` instead of `New`
so file reads/writes go through the VFS, not wasm2go's host-backed `DefaultWASI`.

**Verified end to end:** `go test -tags aot -run TestAOTCallMain` drives postgres
`main()` through the AOT backend against a VFS holding only `/pglite/bin/postgres`.
It runs full early init (memory contexts, GUC — the code that SIGSEGV'd in
`palloc0` without `CallMain`), resolves its executable from the VFS, checks the
data directory through the bridged `__syscall_openat`/`stat`, logs via the
VFS-backed WASI `fd_write`, and exits gracefully:

```
postgres: could not access directory "/pgdata": No such file or directory
Run initdb or pg_basebackup to initialize a PostgreSQL data directory.
```

That is the canonical state a fresh postgres reaches with no cluster.

**A LIVE QUERY RUNS.** `go test -tags aot -run TestAOTQuery` warm-loads a
pre-initialized cluster (`Open` with a `PersistDir` → `isPersistedCluster` →
`LoadSubtree`, skipping the not-yet-transpiled initdb) and runs `SELECT 42`
end to end through the AOT backend — `Open` → `startWire` (`RegisterRWCallbacks`
→ `T0`, `CallMain`, `pgl_startPGlite`, the `PostgresMainLoopOnce` wire loop with
cooperative longjmp) → `Query` → `rows=[[42]]` → clean shutdown → `Close`. Real
PostgreSQL 18.3 executing as pure Go: no CGo, no wasm engine.

Two fixes were needed to get there:
- **Heap growth.** postgres grows past the initial 128 MB via
  `emscripten_resize_heap`; genaot special-cases it to reslice `m.Memory` within a
  pre-reserved cap (`NewWithWASIReserve(h, h, 768<<20)`), so the backing array —
  and every caller's cached `m.M` base pointer — stays valid (a realloc mid-call
  would corrupt them).
- **exit().** In PGlite mode `main()` (and shutdown) exit via `exit()`/`proc_exit()`,
  which `panic("exit(N)")` up through the generated frames. `CallMain`/`CallExport`/
  `CallExportRaw` recover it and report a non-fatal error, matching the engine
  backends (this is a genuine termination, distinct from the panic-free cooperative
  longjmp).

## Cold init (creating a cluster)

initdb is transpiled too, so the AOT backend can create a cluster from scratch —
not just warm-load one. `go generate -tags aot` also runs:

```sh
go run ./cmd/wasmpass -config initdb -i wasm/initdb.wasm -o wasm/initdb.standalone.wasm
go run github.com/goccy/wasm2go/cmd/wasm2go@latest -i wasm/initdb.standalone.wasm \
    -import github.com/moriyoshi/pglite-go/internal/initdbwasm -pkg initdbwasm -pure \
    -o internal/initdbwasm/initdbwasm.go
go run ./internal/genaot -dir internal/initdbwasm -pkg initdbwasm -kind initdb -sp 0
```

initdb is small, so wasm2go emits it **single-file** (methods on `*Module`,
unexported fields, no `base` subpackage) rather than the multi-package split
pglite gets — `genaot` auto-detects and handles both. Two initdb-specific
constants: `__stack_pointer` is import global 0 (pglite: 1), and `__THREW__` is at
a different address (derive it from `setThrew`'s `global.get` per module — the
global *index* differs between modules). The cold path also needs the module's
allocator/FILE exports kept (`malloc`, `emscripten_builtin_memalign`, `fopen`,
`fclose`, `fflush`, `_emscripten_stack_alloc`) because the shared `_mmap_js` and
initdb-callback handlers call back into them.

**Verified:** `go test -tags aot -run TestAOTColdInit` opens against an *empty*
persist dir, so Open runs initdb — which spawns `postgres --boot` in fresh AOT
pgwasm instances over the shared VFS, captures ~1 MB of bootstrap SQL, and writes
a valid cluster (`pg_control present=true`) — then runs `SELECT 42` -> `42`. Fully
self-contained pure-Go PostgreSQL: no pre-existing cluster, no CGo, no wasm engine.
(The `--single` post-bootstrap phase FATALs on the PG18 ICU root-collator issue,
a pre-existing PGlite quirk unrelated to AOT; the essential catalog still lands.)

## Status / open items

- Productionize: share one unwind landing per function (shave the +29% LOC).
- Alternative (not chosen — no-panic constraint): confined `panic`/`recover` in
  the ~56 host `invoke_*` trampolines only (~2 lines, zero wasm instrumentation).

The mechanism is validated end-to-end on an ABI-faithful toy
(`internal/wasmpass/testdata/toy3.wasm`): a longjmp round-trips to the correct
value with the intermediate frame's post-call side effect correctly skipped, no
panic/recover, under `-race`.
