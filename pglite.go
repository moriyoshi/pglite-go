//go:build !wazero

// Package pglite embeds PostgreSQL in Go by running PGlite (PostgreSQL compiled
// to WebAssembly) on the wasmtime runtime, with a hand-written Emscripten/WASI
// host layer and an in-memory VFS.
//
// A DB is a cluster: Open initializes (or loads a persisted) data directory,
// keeps it in an in-memory filesystem, and starts one persistent PostgreSQL
// backend driven over the v3 wire protocol (PGlite's pgl_set_rw_cbs +
// PostgresMainLoopOnce). Query/Exec feed protocol messages to that live backend,
// so results carry column type OIDs and command tags (RowsAffected), and session
// state — the current transaction, temp tables, SET, prepared statements —
// persists across calls: BEGIN in one Query and COMMIT in a later one form a
// single transaction. QueryParams runs a real server-side prepared statement
// (extended protocol). For the standard Go database API, use the registered
// "pglite" database/sql driver (package pgdriver).
//
// The backend is a single connection (as in PGlite itself), so concurrent use
// must be serialized — the DB does this internally, and database/sql callers
// should set db.SetMaxOpenConns(1).
package pglite

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/bytecodealliance/wasmtime-go/v34"

	emcompat "github.com/moriyoshi/pglite-go/emscripten"
	"github.com/moriyoshi/pglite-go/vfs"
)

// Config configures a DB. The zero value is usable: it looks for wasm assets in
// "./wasm", persists the cluster under the user cache dir, and uses template1.
type Config struct {
	// WasmDir holds pglite.wasm, initdb.wasm, pglite.manifest.json, pglite.data.
	// Defaults to "wasm".
	WasmDir string
	// PersistDir is the host directory the initialized cluster is saved to and
	// loaded from. Empty uses the user cache dir; set Ephemeral to disable.
	PersistDir string
	// Ephemeral, if true, keeps the cluster only in memory (no host persistence).
	Ephemeral bool
	// Database to connect to. Defaults to "template1".
	Database string
}

const dataDir = "/tmp/pglite/data"

// DB is an embedded PostgreSQL cluster backed by an in-memory filesystem.
//
// A DB is safe for concurrent use: query execution is serialized (the WASM
// backend is single-threaded and the in-memory cluster is shared), so callers
// (including database/sql's pool) block rather than race.
type DB struct {
	cfg         Config
	engine      *wasmtime.Engine
	postgresMod *wasmtime.Module
	fs          *vfs.FS
	persistDir  string
	database    string
	wire        *wireConn  // persistent wire-protocol backend
	mu          sync.Mutex // serializes backend invocations
}

// Open initializes or loads a cluster and returns a ready DB.
func Open(cfg Config) (*DB, error) {
	if cfg.WasmDir == "" {
		cfg.WasmDir = "wasm"
	}
	if cfg.Database == "" {
		cfg.Database = "template1"
	}
	db := &DB{cfg: cfg, database: cfg.Database}
	if !cfg.Ephemeral {
		db.persistDir = cfg.PersistDir
		if db.persistDir == "" {
			if d, err := os.UserCacheDir(); err == nil {
				db.persistDir = filepath.Join(d, "pglite-go", "data")
			} else {
				db.persistDir = ".pglite-data"
			}
		}
	}

	db.fs = vfs.New()
	if err := db.fs.LoadManifest(cfg.WasmDir+"/pglite.manifest.json", cfg.WasmDir+"/pglite.data"); err != nil {
		return nil, fmt.Errorf("load manifest: %w", err)
	}
	db.fs.MkdirAll(dataDir, 0o700)
	db.fs.MkdirAll("/dev", 0o755)
	db.fs.WriteFile("/dev/null", nil, 0o666)
	db.fs.WriteFile("/dev/urandom", nil, 0o666)
	db.fs.MkdirAll("/home/web_user", 0o755)

	postgresWasm, err := os.ReadFile(cfg.WasmDir + "/pglite.wasm")
	if err != nil {
		return nil, fmt.Errorf("read pglite.wasm: %w", err)
	}
	db.engine = wasmtime.NewEngine()
	mod, _, err := compileCached(db.engine, postgresWasm, "pglite")
	if err != nil {
		return nil, fmt.Errorf("compile pglite.wasm: %w", err)
	}
	db.postgresMod = mod

	// Ensure a cluster exists: load a persisted one, else initdb (+persist).
	ctx := context.Background()
	if db.persistDir != "" && isPersistedCluster(db.persistDir) {
		if err := db.fs.LoadSubtree(db.persistDir, dataDir); err != nil {
			return nil, fmt.Errorf("load persisted cluster: %w", err)
		}
	} else {
		initdbWasm, err := os.ReadFile(cfg.WasmDir + "/initdb.wasm")
		if err != nil {
			return nil, fmt.Errorf("read initdb.wasm: %w", err)
		}
		if err := db.runInitdb(ctx, initdbWasm, postgresWasm); err != nil {
			return nil, fmt.Errorf("initdb: %w", err)
		}
		for _, f := range []string{"/PG_VERSION", "/global/pg_control"} {
			if _, err := db.fs.Stat(dataDir + f); err != nil {
				return nil, fmt.Errorf("initdb produced no valid cluster (missing %s)", f)
			}
		}
		if err := db.Sync(); err != nil {
			return nil, err
		}
	}

	// Start the persistent wire-protocol backend so session state (transactions,
	// temp tables, SET, prepared statements) survives across queries.
	wire, err := db.startWire()
	if err != nil {
		return nil, fmt.Errorf("start backend: %w", err)
	}
	db.wire = wire
	return db, nil
}

// Sync persists the in-memory cluster to the host (no-op when Ephemeral).
func (db *DB) Sync() error {
	db.mu.Lock()
	defer db.mu.Unlock()
	return db.syncLocked()
}

func (db *DB) syncLocked() error {
	if db.persistDir == "" {
		return nil
	}
	return db.fs.SaveSubtree(dataDir, db.persistDir)
}

// Close ends the backend session, syncs the cluster to the host, and releases
// resources.
func (db *DB) Close() error {
	db.mu.Lock()
	defer db.mu.Unlock()
	if db.wire != nil {
		// Flush committed data to the VFS heap files before persisting.
		db.wire.simpleQuery("CHECKPOINT")
		db.wire.close()
		db.wire = nil
	}
	err := db.syncLocked()
	if db.engine != nil {
		db.engine.Close()
	}
	return err
}

// runCmd runs a postgres subcommand in a fresh wasmtime instance against the
// shared VFS, reusing the once-compiled module.
func (db *DB) runCmd(ctx context.Context, args []string, stdin []byte, stdoutCapture *[]byte) (int32, error) {
	rt, err := emcompat.NewWTRuntimeFromModule(ctx, db.engine, db.postgresMod, db.fs, stdin, stdoutCapture)
	if err != nil {
		return -1, err
	}
	// Keep the library quiet: initdb subcommands are chatty on stdout/stderr.
	if stdoutCapture == nil {
		rt.SetStdout(io.Discard)
	}
	rt.SetStderr(io.Discard)
	if err := rt.ApplyDataRelocs(ctx); err != nil {
		return -1, err
	}
	code, err := rt.CallMain(ctx, args)
	if err != nil {
		msg := err.Error()
		if strings.Contains(msg, "exit(0)") {
			return 0, nil
		}
		if strings.Contains(msg, "exit(") {
			return 1, nil
		}
		return -1, err
	}
	return code, nil
}

func compileCached(engine *wasmtime.Engine, wasm []byte, name string) (*wasmtime.Module, bool, error) {
	dir := ""
	if d, err := os.UserCacheDir(); err == nil {
		dir = filepath.Join(d, "pglite-go", "wasmtime")
	}
	sum := sha256.Sum256(wasm)
	path := ""
	if dir != "" {
		path = filepath.Join(dir, fmt.Sprintf("%s-%s.cwasm", name, hex.EncodeToString(sum[:8])))
		if mod, err := wasmtime.NewModuleDeserializeFile(engine, path); err == nil {
			return mod, true, nil
		}
	}
	mod, err := wasmtime.NewModule(engine, wasm)
	if err != nil {
		return nil, false, err
	}
	if path != "" {
		if blob, serr := mod.Serialize(); serr == nil {
			_ = os.MkdirAll(dir, 0o755)
			tmp := path + ".tmp"
			if os.WriteFile(tmp, blob, 0o644) == nil {
				_ = os.Rename(tmp, path)
			}
		}
	}
	return mod, false, nil
}

func isPersistedCluster(persistDir string) bool {
	_, err := os.Stat(persistDir + "/PG_VERSION")
	return err == nil
}
