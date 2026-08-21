package emscripten

import (
	"context"
	"io"
)

// Runtime is the backend-agnostic surface the pglite library drives: one
// instantiated PGlite/PostgreSQL module wired with the host layer. Both the
// wasmtime backend (*WTRuntime, default) and the pure-Go wazero backend
// (*WZRuntime, -tags wazero) implement it, so the wire-protocol and initdb
// orchestration code is written once against this interface.
type Runtime interface {
	// ApplyDataRelocs runs __wasm_apply_data_relocs if the module exports it.
	ApplyDataRelocs(ctx context.Context) error

	// CallExport calls an exported function by name with Go-typed args
	// (int32/int64/float64) and returns the raw result.
	CallExport(name string, args ...interface{}) (interface{}, error)
	// CallExportRaw is like CallExport but reports whether the call unwound via
	// the Emscripten longjmp mechanism (so the caller can drive
	// PostgresMainLongJmp and continue) rather than failing.
	CallExportRaw(name string, args ...interface{}) (res interface{}, longjmp bool, err error)
	// CallMain calls the module's __main_argc_argv entry point with argv.
	CallMain(ctx context.Context, args []string) (int32, error)

	// RegisterRWCallbacks installs the PGlite socket read/write hooks and
	// returns the base table index (pass base+0/base+1 to pgl_set_rw_cbs).
	RegisterRWCallbacks(cb RWCallbacks) (uint32, error)
	// RegisterInitdbCallbacks installs initdb's system/popen/pclose hooks and
	// returns the base table index (pass base+0/+1/+2 to pgl_set_system_fn etc.).
	RegisterInitdbCallbacks(cb *InitdbCallbacks) (uint32, error)

	// FindStdoutFILE locates musl's static stdout FILE struct in guest memory.
	FindStdoutFILE() uint32

	SetStdout(w io.Writer)
	SetStderr(w io.Writer)
	// SetStdoutCapture redirects fd 1 into buf (nil restores normal stdout).
	SetStdoutCapture(buf *[]byte)
}

// RWCallbacks are the PGlite socket read/write hooks. Read fills dst with
// client→server bytes (non-blocking; returns the count, possibly 0). Write
// receives a copy of server→client bytes. They operate directly on guest memory.
type RWCallbacks struct {
	Read  func(dst []byte) int
	Write func(src []byte)
}
