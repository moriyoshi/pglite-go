//go:build !embed

package pglite

import "io/fs"

// Default build: no artifacts are embedded. Assets come from a directory, an
// explicit fs.FS, or an on-demand download. Build with -tags embed to bake them
// into the binary instead.
func defaultEmbeddedFS() fs.FS { return nil }
