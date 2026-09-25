//go:build aot

package emscripten

import (
	"context"
	"encoding/binary"

	"github.com/tetratelabs/wazero/api"
)

// The AOT (wasm2go) world exposes linear memory as a plain Go []byte and exports
// callable by name. The shared host handlers (syscall.go, wasi.go, systemcb.go)
// are api.GoModuleFunc — func(ctx, api.Module, stack) — which read guest memory
// via mod.Memory(). wazero seals api.Module/api.Memory/api.Function with an
// unexported wazeroOnly() marker, so they cannot be implemented directly. The
// wasmtime backend works around this by EMBEDDING the sealed interface (nil) so
// the marker method is promoted, then overriding only the methods it uses
// (see wtModule/wtMemory). The AOT backend does the same over a []byte, which
// lets it reuse every shared handler with no changes to the handlers or the
// other backends.

// aotMemory adapts a live []byte to api.Memory. get is called on every access so
// the view tracks the module's memory after a growth reallocates the backing.
type aotMemory struct {
	api.Memory // nil embed: promotes the sealing marker + unused methods
	get        func() []byte
}

func (m *aotMemory) buf() []byte  { return m.get() }
func (m *aotMemory) Size() uint32 { return uint32(len(m.buf())) }
func (m *aotMemory) Grow(uint32) (uint32, bool) {
	return uint32(len(m.buf()) / 65536), true // the transpiled module owns growth
}
func (m *aotMemory) has(off, n uint32) ([]byte, bool) {
	b := m.buf()
	if uint64(off)+uint64(n) > uint64(len(b)) {
		return nil, false
	}
	return b[off : off+n], true
}
func (m *aotMemory) Read(off, n uint32) ([]byte, bool) { return m.has(off, n) }
func (m *aotMemory) Write(off uint32, v []byte) bool {
	if s, ok := m.has(off, uint32(len(v))); ok {
		copy(s, v)
		return true
	}
	return false
}
func (m *aotMemory) WriteString(off uint32, v string) bool { return m.Write(off, []byte(v)) }
func (m *aotMemory) ReadByte(off uint32) (byte, bool) {
	if s, ok := m.has(off, 1); ok {
		return s[0], true
	}
	return 0, false
}
func (m *aotMemory) WriteByte(off uint32, v byte) bool {
	if s, ok := m.has(off, 1); ok {
		s[0] = v
		return true
	}
	return false
}
func (m *aotMemory) ReadUint16Le(off uint32) (uint16, bool) {
	if s, ok := m.has(off, 2); ok {
		return binary.LittleEndian.Uint16(s), true
	}
	return 0, false
}
func (m *aotMemory) WriteUint16Le(off uint32, v uint16) bool {
	if s, ok := m.has(off, 2); ok {
		binary.LittleEndian.PutUint16(s, v)
		return true
	}
	return false
}
func (m *aotMemory) ReadUint32Le(off uint32) (uint32, bool) {
	if s, ok := m.has(off, 4); ok {
		return binary.LittleEndian.Uint32(s), true
	}
	return 0, false
}
func (m *aotMemory) WriteUint32Le(off, v uint32) bool {
	if s, ok := m.has(off, 4); ok {
		binary.LittleEndian.PutUint32(s, v)
		return true
	}
	return false
}
func (m *aotMemory) ReadUint64Le(off uint32) (uint64, bool) {
	if s, ok := m.has(off, 8); ok {
		return binary.LittleEndian.Uint64(s), true
	}
	return 0, false
}
func (m *aotMemory) WriteUint64Le(off uint32, v uint64) bool {
	if s, ok := m.has(off, 8); ok {
		binary.LittleEndian.PutUint64(s, v)
		return true
	}
	return false
}

// aotAPIModule adapts a transpiled module to api.Module. It overrides only
// Memory() and ExportedFunction(); the shared handlers use nothing else.
type aotAPIModule struct {
	api.Module // nil embed: promotes the sealing marker
	mem        *aotMemory
	call       func(name string, args []uint64) uint64
}

func (m *aotAPIModule) Memory() api.Memory { return m.mem }
func (m *aotAPIModule) Name() string       { return "pglite" }
func (m *aotAPIModule) ExportedFunction(name string) api.Function {
	return &aotFunction{name: name, call: m.call}
}

type aotFunction struct {
	api.Function // nil embed: promotes the sealing marker
	name         string
	call         func(name string, args []uint64) uint64
}

func (f *aotFunction) Call(_ context.Context, params ...uint64) ([]uint64, error) {
	return []uint64{f.call(f.name, params)}, nil
}
func (f *aotFunction) CallWithStack(_ context.Context, stack []uint64) error {
	r := f.call(f.name, stack)
	if len(stack) > 0 {
		stack[0] = r
	}
	return nil
}

// NewAOTModuleAdapter returns an api.Module over the transpiled module's live
// memory accessor and name-dispatched Call, for passing to the shared
// api.GoModuleFunc handlers.
func NewAOTModuleAdapter(get func() []byte, call func(name string, args []uint64) uint64) api.Module {
	return &aotAPIModule{mem: &aotMemory{get: get}, call: call}
}
