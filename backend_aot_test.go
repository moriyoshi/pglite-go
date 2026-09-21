//go:build aot

package pglite

import (
	"context"
	"testing"
)

// TestAOTInstantiateAndInit drives the full -tags aot backend path:
// backend_aot.go -> pgwasm.NewAOT (generated shim) -> New (transpiled) ->
// A2GRuntime -> ApplyDataRelocs (data relocs + C++ static ctors). It proves the
// AOT backend links and that real PostgreSQL init runs through the instrumented
// pure-Go build with no panic. Requires `go generate -tags aot` to have produced
// internal/pgwasm.
func TestAOTInstantiateAndInit(t *testing.T) {
	e := newEngine()
	defer closeEngine(e)

	mod, err := compileModule(e, nil, "pglite")
	if err != nil {
		t.Fatal(err)
	}
	rt, err := newRuntimeFromModule(context.Background(), e, mod, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := rt.ApplyDataRelocs(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Log("AOT: instantiated + data relocs + __wasm_call_ctors ran; no panic")
}
