// Command fetchwasm downloads the pinned PGlite wasm artifacts into ./wasm.
//
// It is the target of the module's `go generate` directive:
//
//	go generate ./...
//
// or run directly, optionally with a destination dir and a pinned version:
//
//	go run ./internal/fetchwasm [destDir]
//	PGLITE_VERSION=0.5.5 go run ./internal/fetchwasm
//
// The default version is the one this library targets (pgassets.Version); set
// PGLITE_VERSION only for a deliberate, code-coordinated bump.
package main

import (
	"context"
	"fmt"
	"log"
	"os"

	"github.com/moriyoshi/pglite-go/internal/pgassets"
)

func main() {
	log.SetFlags(0)

	version := os.Getenv("PGLITE_VERSION")
	if version == "" {
		version = pgassets.Version
	}
	dest := "wasm"
	if len(os.Args) > 1 {
		dest = os.Args[1]
	}

	log.Printf("Fetching @electric-sql/pglite@%s from jsDelivr -> %s", version, dest)
	if err := pgassets.Fetch(context.Background(), version, dest, log.Printf); err != nil {
		fmt.Fprintln(os.Stderr, "fetchwasm:", err)
		os.Exit(1)
	}
	log.Printf("Done.")
}
