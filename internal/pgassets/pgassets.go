// Package pgassets fetches the PGlite wasm artifacts (pglite.wasm, initdb.wasm,
// pglite.data, and the derived pglite.manifest.json) from the jsDelivr CDN mirror
// of the @electric-sql/pglite npm package.
//
// It backs three things: the `go generate` fetcher (cmd internal/fetchwasm), the
// optional runtime on-demand download, and, indirectly, the -tags embed build
// (which embeds a directory previously populated by the fetcher).
package pgassets

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"time"
)

// Version is the PGlite release this library targets. It is pinned because the
// host layer carries version-specific constants (e.g. the initdb stdout FILE*
// address and PG18 fd-probe handling), so the artifacts must match the code.
// Bumping it is a deliberate, code-coordinated change.
const Version = "0.5.5"

// cdnBase is the jsDelivr npm mirror. Files live under <base><version>/dist/.
const cdnBase = "https://cdn.jsdelivr.net/npm/@electric-sql/pglite@"

// blobFiles are copied verbatim from dist/.
var blobFiles = []string{"pglite.wasm", "initdb.wasm", "pglite.data"}

// manifestRe extracts the Emscripten preloaded-file table from the JS glue:
//
//	loadPackage({files:[{filename:"..",start:N,end:N}, ..],remote_package_size:N
var manifestRe = regexp.MustCompile(`(?s)loadPackage\(\{files:(\[.*?\]),remote_package_size:(\d+)`)

// bareKeyRe quotes the bare object keys so the JS array literal parses as JSON.
var bareKeyRe = regexp.MustCompile(`([{,])(filename|start|end):`)

type manifestEntry struct {
	Filename string `json:"filename"`
	Start    int64  `json:"start"`
	End      int64  `json:"end"`
}

// Fetch downloads the artifacts for version into destDir, writing pglite.wasm,
// initdb.wasm, pglite.data and a derived pglite.manifest.json. destDir is created
// if absent; existing files are overwritten. logf, if non-nil, receives progress
// lines. The download is validated: the manifest's remote_package_size and last
// entry offset must equal the actual pglite.data size.
func Fetch(ctx context.Context, version, destDir string, logf func(string, ...any)) error {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	if version == "" {
		version = Version
	}
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", destDir, err)
	}

	base := cdnBase + version + "/dist/"

	var dataLen int64
	for _, name := range blobFiles {
		logf("  -> %s", name)
		body, err := get(ctx, base+name)
		if err != nil {
			return fmt.Errorf("download %s@%s: %w", name, version, err)
		}
		if name == "pglite.data" {
			dataLen = int64(len(body))
		}
		if err := os.WriteFile(filepath.Join(destDir, name), body, 0o644); err != nil {
			return fmt.Errorf("write %s: %w", name, err)
		}
	}

	logf("  -> pglite.manifest.json (extracted from dist/pglite.js)")
	glue, err := get(ctx, base+"pglite.js")
	if err != nil {
		return fmt.Errorf("download pglite.js@%s: %w", version, err)
	}
	manifest, count, err := extractManifest(glue, dataLen)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(destDir, "pglite.manifest.json"), manifest, 0o644); err != nil {
		return fmt.Errorf("write manifest: %w", err)
	}
	logf("     %d entries, %d bytes", count, dataLen)
	return nil
}

// extractManifest parses the loadPackage(...) metadata out of the JS glue and
// re-emits it as indented JSON, validating it against the actual data size.
func extractManifest(glue []byte, dataLen int64) ([]byte, int, error) {
	m := manifestRe.FindSubmatch(glue)
	if m == nil {
		return nil, 0, fmt.Errorf("could not locate loadPackage(...) metadata in pglite.js")
	}
	arr := bareKeyRe.ReplaceAll(m[1], []byte(`$1"$2":`))
	var entries []manifestEntry
	if err := json.Unmarshal(arr, &entries); err != nil {
		return nil, 0, fmt.Errorf("parse manifest array: %w", err)
	}
	remoteSize, err := strconv.ParseInt(string(m[2]), 10, 64)
	if err != nil {
		return nil, 0, fmt.Errorf("parse remote_package_size: %w", err)
	}
	if remoteSize != dataLen {
		return nil, 0, fmt.Errorf("remote_package_size %d != pglite.data size %d", remoteSize, dataLen)
	}
	if len(entries) > 0 && entries[len(entries)-1].End != dataLen {
		return nil, 0, fmt.Errorf("manifest end %d != pglite.data size %d", entries[len(entries)-1].End, dataLen)
	}
	out, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return nil, 0, err
	}
	return append(out, '\n'), len(entries), nil
}

// get fetches a URL and returns the fully-read (transparently gunzipped) body.
func get(ctx context.Context, url string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	// Leave Accept-Encoding unset so net/http negotiates gzip and transparently
	// decodes it — jsDelivr serves these blobs gzip-compressed on the wire.
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	return io.ReadAll(resp.Body)
}
