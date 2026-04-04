package emscripten

// WASM binary encoding utilities.

// encodeLEB128U encodes an unsigned integer as LEB128.
func encodeLEB128U(v uint64) []byte {
	if v == 0 {
		return []byte{0}
	}
	var buf []byte
	for v > 0 {
		b := byte(v & 0x7f)
		v >>= 7
		if v > 0 {
			b |= 0x80
		}
		buf = append(buf, b)
	}
	return buf
}

// encodeLEB128S encodes a signed integer as LEB128.
func encodeLEB128S(v int64) []byte {
	var buf []byte
	more := true
	for more {
		b := byte(v & 0x7f)
		v >>= 7
		if (v == 0 && b&0x40 == 0) || (v == -1 && b&0x40 != 0) {
			more = false
		} else {
			b |= 0x80
		}
		buf = append(buf, b)
	}
	return buf
}

// encodeSection creates a WASM section with the given id and payload.
func encodeSection(id byte, payload []byte) []byte {
	result := []byte{id}
	result = append(result, encodeLEB128U(uint64(len(payload)))...)
	result = append(result, payload...)
	return result
}

// encodeName encodes a WASM name (length-prefixed UTF-8 string).
func encodeName(name string) []byte {
	result := encodeLEB128U(uint64(len(name)))
	result = append(result, []byte(name)...)
	return result
}

// funcSig describes a WASM function signature.
type funcSig struct {
	params  []byte
	results []byte
}

// globalDef describes a global to define and export.
type globalDef struct {
	name    string
	valType byte // 0x7f=i32, 0x7e=i64, etc.
	mutable bool
	initI32 int32
}

// buildGOTMemModule builds a WASM binary for the "GOT.mem" module
// that exports mutable i32 globals.
func buildGOTMemModule(globals map[string]int32) []byte {
	var buf []byte
	buf = append(buf, 0x00, 0x61, 0x73, 0x6d)
	buf = append(buf, 0x01, 0x00, 0x00, 0x00)

	names := make([]string, 0, len(globals))
	for name := range globals {
		names = append(names, name)
	}

	// Global section
	var globalPayload []byte
	globalPayload = append(globalPayload, encodeLEB128U(uint64(len(globals)))...)
	for _, name := range names {
		val := globals[name]
		globalPayload = append(globalPayload, 0x7f) // i32
		globalPayload = append(globalPayload, 0x01) // mutable
		globalPayload = append(globalPayload, 0x41) // i32.const
		globalPayload = append(globalPayload, encodeLEB128S(int64(val))...)
		globalPayload = append(globalPayload, 0x0b) // end
	}
	buf = append(buf, encodeSection(6, globalPayload)...)

	// Export section
	var exportPayload []byte
	exportPayload = append(exportPayload, encodeLEB128U(uint64(len(globals)))...)
	for i, name := range names {
		exportPayload = append(exportPayload, encodeName(name)...)
		exportPayload = append(exportPayload, 0x03) // global
		exportPayload = append(exportPayload, encodeLEB128U(uint64(i))...)
	}
	buf = append(buf, encodeSection(7, exportPayload)...)

	return buf
}
