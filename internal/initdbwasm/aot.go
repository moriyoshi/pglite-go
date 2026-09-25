//go:build aot

// Package initdbwasm is the AOT (wasm2go-transpiled) initdb module. The
// transpiled code is a build artifact produced by `go generate -tags aot`
// (the same wasmpass + wasm2go + genaot pipeline as internal/pgwasm) and is
// not committed.
//
// This file is a hand-written placeholder so backend_aot.go links before the
// initdb module is transpiled. NewAOT returns a stub whose Call panics; the
// initdb path (cluster.go) is not yet functional under -tags aot.
package initdbwasm

import (
	emcompat "github.com/moriyoshi/pglite-go/emscripten"
	"github.com/moriyoshi/pglite-go/vfs"
)

type stub struct{ mem []byte }

func (s *stub) Memory() []byte   { return s.mem }
func (s *stub) ApplyDataRelocs() {}
func (s *stub) CallCtors()       {}
func (s *stub) Call(name string, args []uint64) uint64 {
	panic("initdbwasm: AOT initdb not generated (run go generate -tags aot)")
}
func (s *stub) RegisterRW(read func(dst []byte) int, write func(src []byte)) uint32 { return 0 }
func (s *stub) RegisterInitdb(cb *emcompat.InitdbCallbacks) uint32                  { return 0 }
func (s *stub) SP() int32                                                           { return 0 }
func (s *stub) SetSP(int32)                                                         {}

// NewAOT returns a placeholder AOTModule. Replace by transpiling initdb.wasm.
func NewAOT(fs *vfs.FS, stdin []byte, capture *[]byte) emcompat.AOTModule {
	return &stub{mem: make([]byte, 1<<16)}
}
