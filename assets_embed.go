//go:build embed

package pglite

import (
	"embed"
	"io/fs"
)

// Built with -tags embed: the wasm/ directory is baked into the binary, so the
// artifacts must be present at build time (run `go generate ./...` first). This
// adds ~16 MB to the binary but makes it fully self-contained.
//
//go:embed wasm
var embeddedAssets embed.FS

func defaultEmbeddedFS() fs.FS {
	sub, err := fs.Sub(embeddedAssets, "wasm")
	if err != nil {
		return nil
	}
	return sub
}
