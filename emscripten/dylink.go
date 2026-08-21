package emscripten

import "fmt"

// ParseDylink reads the Emscripten "dylink.0" custom section that leads a
// SIDE_MODULE/MAIN_MODULE wasm, returning the module's required static memory
// size and indirect-function-table size. The wazero backend uses tableSize to
// size the shared function table (and to place host callbacks safely above the
// module's own entries) instead of hardcoding a version-specific constant.
//
// Layout: the first section is a custom section named "dylink.0" whose first
// subsection (id 1, WASM_DYLINK_MEM_INFO) holds four ULEB128 values:
// memsize, memalign, tablesize, tablealign.
func ParseDylink(wasm []byte) (memSize, tableSize uint32, err error) {
	if len(wasm) < 8 || string(wasm[:4]) != "\x00asm" {
		return 0, 0, fmt.Errorf("not a wasm module")
	}
	p := 8
	uleb := func() (uint32, bool) {
		var r uint32
		var s uint
		for {
			if p >= len(wasm) {
				return 0, false
			}
			b := wasm[p]
			p++
			r |= uint32(b&0x7f) << s
			if b&0x80 == 0 {
				return r, true
			}
			s += 7
		}
	}
	if wasm[p] != 0 { // section id 0 = custom
		return 0, 0, fmt.Errorf("first section is not custom (no dylink)")
	}
	p++
	if _, ok := uleb(); !ok { // section size
		return 0, 0, fmt.Errorf("truncated section size")
	}
	nameLen, ok := uleb()
	if !ok || p+int(nameLen) > len(wasm) {
		return 0, 0, fmt.Errorf("truncated section name")
	}
	if string(wasm[p:p+int(nameLen)]) != "dylink.0" {
		return 0, 0, fmt.Errorf("custom section is %q, not dylink.0", wasm[p:p+int(nameLen)])
	}
	p += int(nameLen)
	if p >= len(wasm) || wasm[p] != 1 { // subsection id 1 = WASM_DYLINK_MEM_INFO
		return 0, 0, fmt.Errorf("dylink.0 missing MEM_INFO subsection")
	}
	p++
	if _, ok := uleb(); !ok { // subsection size
		return 0, 0, fmt.Errorf("truncated subsection size")
	}
	memSize, ok = uleb()
	if !ok {
		return 0, 0, fmt.Errorf("truncated memsize")
	}
	if _, ok = uleb(); !ok { // memalign
		return 0, 0, fmt.Errorf("truncated memalign")
	}
	tableSize, ok = uleb()
	if !ok {
		return 0, 0, fmt.Errorf("truncated tablesize")
	}
	return memSize, tableSize, nil
}
