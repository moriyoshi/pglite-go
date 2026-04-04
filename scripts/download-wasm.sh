#!/usr/bin/env bash
set -euo pipefail

PGLITE_VERSION="${PGLITE_VERSION:-0.4.2}"
TARBALL_URL="https://registry.npmjs.org/@electric-sql/pglite/-/pglite-${PGLITE_VERSION}.tgz"

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PROJECT_DIR="$(dirname "$SCRIPT_DIR")"
WASM_DIR="${PROJECT_DIR}/wasm"

mkdir -p "$WASM_DIR"

echo "Downloading @electric-sql/pglite@${PGLITE_VERSION}..."
TMPDIR=$(mktemp -d)
trap 'rm -rf "$TMPDIR"' EXIT

curl -sL "$TARBALL_URL" -o "$TMPDIR/pglite.tgz"
echo "Extracting WASM artifacts..."
tar -xzf "$TMPDIR/pglite.tgz" -C "$TMPDIR"

# Find and copy the WASM-related files
find "$TMPDIR/package/dist" -name '*.wasm' -o -name '*.data' | while read -r f; do
    base=$(basename "$f")
    echo "  -> $base"
    cp "$f" "$WASM_DIR/$base"
done

# Also copy the JS glue to inspect imports/exports
find "$TMPDIR/package/dist" -name 'postgres.js' -o -name 'postgres.cjs' | while read -r f; do
    base=$(basename "$f")
    echo "  -> $base (JS glue for reference)"
    cp "$f" "$WASM_DIR/$base"
done

echo "Done. WASM artifacts are in ${WASM_DIR}/"
ls -lh "$WASM_DIR/"
