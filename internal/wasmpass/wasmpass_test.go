package wasmpass

import (
	_ "embed"
	"testing"
)

// toy3 is an Emscripten-Wasm-SjLj-shaped fixture:
//
//	A(owner) --host invoke_run--> Runner --call_indirect--> C_lj --> longjmp --> _emscripten_throw_longjmp
//
// with the intermediate frames left UNINSTRUMENTED, so the pass must insert the
// guards. Its ABI: threw flag at addr 16, __stack_pointer = global 0, seed import
// "_emscripten_throw_longjmp". The end-to-end execution proof (that the guards
// produce a correct cooperative longjmp through wasm2go) lives in the migration
// docs; here we test the transform is structurally sound and stable.
//
//go:embed testdata/toy3.wasm
var toy3 []byte

func toyConfig() Config {
	return Config{
		Roots:            map[string]bool{"A": true, "Runner": true, "memory": true},
		BaseGlobals:      map[string]int32{},
		SeedImport:       "_emscripten_throw_longjmp",
		ThrewAddr:        16,
		SPGlobalOverride: 0,
	}
}

func TestRoundTripStable(t *testing.T) {
	m, err := Decode(toy3)
	if err != nil {
		t.Fatal(err)
	}
	enc := m.Encode()
	m2, err := Decode(enc)
	if err != nil {
		t.Fatalf("re-decode of encoded module failed: %v", err)
	}
	enc2 := m2.Encode()
	if len(enc) != len(enc2) {
		t.Fatalf("encode not stable: %d vs %d bytes", len(enc), len(enc2))
	}
	for i := range enc {
		if enc[i] != enc2[i] {
			t.Fatalf("encode not stable at byte %d", i)
		}
	}
}

func TestInstrumentSiteCount(t *testing.T) {
	m, err := Decode(toy3)
	if err != nil {
		t.Fatal(err)
	}
	cfg := toyConfig()
	// toy3 has 3 may-longjmp call sites: longjmp->seed, C_lj->longjmp, Runner's call_indirect.
	_, sites := m.Instrument(cfg)
	if sites != 3 {
		t.Fatalf("instrumented %d sites, want 3", sites)
	}
	// Result must re-decode cleanly (structurally valid) and re-encode stably.
	enc := m.Encode()
	if _, err := Decode(enc); err != nil {
		t.Fatalf("instrumented module does not re-decode: %v", err)
	}
}

func TestInstrumentIsSuperset(t *testing.T) {
	// Instrumenting must only grow the module (guards are additive) and must not
	// change the function or import counts.
	m0, _ := Decode(toy3)
	m1, _ := Decode(toy3)
	before := len(m0.Encode())
	m1.Instrument(toyConfig())
	after := len(m1.Encode())
	if after <= before {
		t.Fatalf("instrumented size %d not greater than original %d", after, before)
	}
	if len(m0.Codes) != len(m1.Codes) {
		t.Fatalf("function count changed: %d -> %d", len(m0.Codes), len(m1.Codes))
	}
	if len(m0.Imports) != len(m1.Imports) {
		t.Fatalf("import count changed: %d -> %d", len(m0.Imports), len(m1.Imports))
	}
}
