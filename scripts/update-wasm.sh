#!/usr/bin/env bash
#
# Refresh the vendored PGlite artifacts under wasm/ from the upstream npm
# package. Writes all four files the driver/library loads:
#
#   wasm/pglite.wasm            PostgreSQL 17.x compiled to wasm
#   wasm/initdb.wasm            initdb entrypoint
#   wasm/pglite.data            Emscripten preloaded-file bundle (the data dir)
#   wasm/pglite.manifest.json   filename/start/end index into pglite.data
#
# The manifest is not shipped as a standalone file upstream; it is the
# `loadPackage({files:[...]})` metadata embedded in dist/pglite.js. We extract
# that array and emit it as JSON (see vfs.LoadManifest / vfs.ManifestEntry).
#
# Usage:   ./scripts/update-wasm.sh                  # latest published release
#          PGLITE_VERSION=0.4.2 ./scripts/update-wasm.sh   # pin a version
set -euo pipefail

REGISTRY="https://registry.npmjs.org/@electric-sql/pglite"

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PROJECT_DIR="$(dirname "$SCRIPT_DIR")"
WASM_DIR="${PROJECT_DIR}/wasm"

for tool in curl tar python3; do
    if ! command -v "$tool" >/dev/null 2>&1; then
        echo "error: '$tool' is required but not found in PATH" >&2
        exit 1
    fi
done

# Default to the latest published release (npm's "latest" dist-tag) unless pinned.
if [[ -z "${PGLITE_VERSION:-}" ]]; then
    echo "Resolving latest @electric-sql/pglite release..."
    PGLITE_VERSION=$(curl -fsSL "${REGISTRY}/latest" \
        | python3 -c 'import json,sys; print(json.load(sys.stdin)["version"])')
    if [[ -z "$PGLITE_VERSION" ]]; then
        echo "error: could not resolve latest version from npm registry" >&2
        exit 1
    fi
    echo "  latest is ${PGLITE_VERSION}"
fi

TARBALL_URL="${REGISTRY}/-/pglite-${PGLITE_VERSION}.tgz"

mkdir -p "$WASM_DIR"

TMPDIR=$(mktemp -d)
trap 'rm -rf "$TMPDIR"' EXIT

echo "Downloading @electric-sql/pglite@${PGLITE_VERSION}..."
curl -fsSL "$TARBALL_URL" -o "$TMPDIR/pglite.tgz"

echo "Extracting..."
tar -xzf "$TMPDIR/pglite.tgz" -C "$TMPDIR"
DIST="$TMPDIR/package/dist"

# 1. Copy the wasm + data blobs verbatim.
for f in pglite.wasm initdb.wasm pglite.data; do
    if [[ ! -f "$DIST/$f" ]]; then
        echo "error: expected $f not found in package/dist (upstream layout changed?)" >&2
        exit 1
    fi
    echo "  -> $f"
    cp "$DIST/$f" "$WASM_DIR/$f"
done

# 2. Extract the preloaded-file manifest from the JS glue and emit it as JSON.
echo "  -> pglite.manifest.json (extracted from dist/pglite.js)"
python3 - "$DIST/pglite.js" "$WASM_DIR/pglite.manifest.json" "$WASM_DIR/pglite.data" <<'PY'
import json, os, re, sys

glue_path, out_path, data_path = sys.argv[1], sys.argv[2], sys.argv[3]
text = open(glue_path).read()

# loadPackage({files:[{filename:"...",start:N,end:N}, ...], remote_package_size:N})
m = re.search(r'loadPackage\(\{files:(\[.*?\]),remote_package_size:(\d+)', text, re.DOTALL)
if not m:
    sys.exit("error: could not locate loadPackage(...) metadata in pglite.js")

# Quote the bare object keys so the JS array literal parses as JSON.
arr = re.sub(r'([{,])(filename|start|end):', r'\1"\2":', m.group(1))
entries = json.loads(arr)

remote_size = int(m.group(2))
data_size = os.path.getsize(data_path)
if remote_size != data_size:
    sys.exit(f"error: remote_package_size {remote_size} != pglite.data size {data_size}")
if entries and entries[-1]["end"] != data_size:
    sys.exit(f"error: manifest end {entries[-1]['end']} != pglite.data size {data_size}")

with open(out_path, "w") as f:
    json.dump(entries, f, indent=2)
    f.write("\n")

print(f"     {len(entries)} entries, {data_size} bytes")
PY

echo "Done. wasm/ now holds @electric-sql/pglite@${PGLITE_VERSION}:"
ls -lh "$WASM_DIR/"
