//go:build aot

package emscripten

import (
	"context"
	"testing"

	"github.com/tetratelabs/wazero/api"
)

var (
	_ api.Memory   = (*aotMemory)(nil)
	_ api.Module   = (*aotAPIModule)(nil)
	_ api.Function = (*aotFunction)(nil)
)

// TestAOTModuleAdapter proves a real api.GoModuleFunc handler (the exact shape of
// every shared syscall/WASI handler) runs through the AOT adapter: it reads args
// from the stack, touches guest memory via mod.Memory(), calls an export via
// mod.ExportedFunction(), and writes a result back — all over a []byte, with no
// wazero engine.
func TestAOTModuleAdapter(t *testing.T) {
	buf := make([]byte, 128)
	mod := NewAOTModuleAdapter(
		func() []byte { return buf },
		func(name string, args []uint64) uint64 {
			if name == "triple" {
				return args[0] * 3
			}
			return 0
		},
	)

	// A handler in the api.GoModuleFunc shape used everywhere in syscall.go/wasi.go.
	var handler api.GoModuleFunc = func(_ context.Context, m api.Module, stack []uint64) {
		ptr := api.DecodeU32(stack[0])
		v, ok := m.Memory().ReadUint32Le(ptr)
		if !ok {
			t.Fatal("memory read failed")
		}
		res, _ := m.ExportedFunction("triple").Call(context.Background(), uint64(v))
		m.Memory().WriteUint32Le(ptr, uint32(res[0]))
		stack[0] = api.EncodeI32(0) // success
	}

	buf2 := mod.Memory()
	buf2.WriteUint32Le(16, 14)
	stack := []uint64{api.EncodeU32(16)}
	handler(context.Background(), mod, stack)

	if got, _ := mod.Memory().ReadUint32Le(16); got != 42 {
		t.Fatalf("handler via adapter: mem[16]=%d, want 42", got)
	}
	if api.DecodeI32(stack[0]) != 0 {
		t.Fatalf("handler result = %d, want 0", api.DecodeI32(stack[0]))
	}
	t.Log("shared api.GoModuleFunc handler runs through the AOT []byte adapter — no handler changes needed")
}
