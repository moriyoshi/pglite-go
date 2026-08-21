package pglite

// The wasm artifacts (pglite.wasm, initdb.wasm, pglite.data, pglite.manifest.json)
// are not committed. Populate ./wasm from the jsDelivr CDN with:
//
//	go generate ./...
//
// then, for a self-contained binary, build with -tags embed (see assets_embed.go).
//
//go:generate go run ./internal/fetchwasm
