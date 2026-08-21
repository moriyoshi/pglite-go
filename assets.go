package pglite

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/moriyoshi/pglite-go/internal/pgassets"
)

// PgliteVersion is the pinned PGlite release the artifacts must match.
const PgliteVersion = pgassets.Version

// assetNames are the files an asset source must provide.
var assetNames = []string{"pglite.wasm", "initdb.wasm", "pglite.data", "pglite.manifest.json"}

// resolveWasmFS selects where the wasm artifacts come from, in precedence order:
//
//  1. cfg.WasmFS               — an explicit fs.FS (e.g. the caller's embed.FS)
//  2. cfg.WasmDir              — an explicit host directory
//  3. embedded assets          — present only in -tags embed builds
//  4. ./wasm                   — the default working-dir location, if populated
//  5. on-demand jsDelivr fetch — only when cfg.Download is set
func (cfg Config) resolveWasmFS() (fs.FS, error) {
	if cfg.WasmFS != nil {
		return cfg.WasmFS, nil
	}
	if cfg.WasmDir != "" {
		return os.DirFS(cfg.WasmDir), nil
	}
	if e := defaultEmbeddedFS(); e != nil {
		return e, nil
	}
	if assetsComplete("wasm") {
		return os.DirFS("wasm"), nil
	}
	if cfg.Download {
		dir, err := downloadAssets(context.Background())
		if err != nil {
			return nil, err
		}
		return os.DirFS(dir), nil
	}
	return nil, fmt.Errorf("no pglite wasm assets found: set Config.WasmDir or Config.WasmFS, "+
		"build with -tags embed, run `go generate ./...`, or set Config.Download=true (PGlite %s)", PgliteVersion)
}

// assetsComplete reports whether dir holds all required artifacts.
func assetsComplete(dir string) bool {
	for _, name := range assetNames {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			return false
		}
	}
	return true
}

// downloadAssets ensures the pinned artifacts are cached under the user cache dir
// and returns that directory. The download is staged in a sibling temp dir and
// renamed into place so a partial fetch never looks complete.
func downloadAssets(ctx context.Context) (string, error) {
	base, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("locate cache dir: %w", err)
	}
	dir := filepath.Join(base, "pglite-go", "assets", pgassets.Version)
	if assetsComplete(dir) {
		return dir, nil
	}
	staging := dir + ".tmp"
	if err := os.RemoveAll(staging); err != nil {
		return "", err
	}
	if err := pgassets.Fetch(ctx, pgassets.Version, staging, nil); err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return "", err
	}
	_ = os.RemoveAll(dir)
	if err := os.Rename(staging, dir); err != nil {
		return "", err
	}
	return dir, nil
}
