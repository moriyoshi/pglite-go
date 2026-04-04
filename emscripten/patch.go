package emscripten

import (
	"fmt"
)

// PatchWasmImports rewrites the WASM binary's import section to redirect
// non-function imports (globals, memory, table) from "env" to a different
// module name ("env.globals"). This allows us to use wazero's HostModuleBuilder
// for the "env" module (functions only) while providing globals, memory, and
// table through a separate synthesized WASM module.
//
// The function returns the modified WASM binary along with information about
// what was redirected.
func PatchWasmImports(wasmBytes []byte) ([]byte, error) {
	if len(wasmBytes) < 8 {
		return nil, fmt.Errorf("invalid WASM: too short")
	}
	if string(wasmBytes[:4]) != "\x00asm" {
		return nil, fmt.Errorf("invalid WASM: bad magic")
	}

	result := make([]byte, 0, len(wasmBytes)+256)
	result = append(result, wasmBytes[:8]...) // copy header

	pos := 8
	for pos < len(wasmBytes) {
		sectionID := wasmBytes[pos]
		pos++
		sectionSize, n := readLEB128U(wasmBytes[pos:])
		pos += n
		sectionEnd := pos + int(sectionSize)

		if sectionID == 2 { // Import section
			newSection := rewriteImportSection(wasmBytes[pos:sectionEnd])
			result = append(result, sectionID)
			result = append(result, encodeLEB128U(uint64(len(newSection)))...)
			result = append(result, newSection...)
		} else {
			// Copy section as-is
			result = append(result, sectionID)
			result = append(result, encodeLEB128U(sectionSize)...)
			result = append(result, wasmBytes[pos:sectionEnd]...)
		}
		pos = sectionEnd
	}
	return result, nil
}

// rewriteImportSection rewrites the import section, changing the module name
// for non-function imports from "env" to "env.globals".
func rewriteImportSection(data []byte) []byte {
	pos := 0
	numImports, n := readLEB128U(data[pos:])
	pos += n

	var result []byte
	result = append(result, encodeLEB128U(numImports)...)

	for i := 0; i < int(numImports); i++ {
		// Read module name
		modNameLen, n := readLEB128U(data[pos:])
		pos += n
		modName := string(data[pos : pos+int(modNameLen)])
		pos += int(modNameLen)

		// Read field name
		fieldNameLen, n := readLEB128U(data[pos:])
		pos += n
		fieldName := string(data[pos : pos+int(fieldNameLen)])
		pos += int(fieldNameLen)

		// Read import kind
		kind := data[pos]
		pos++

		// Determine the new module name
		newModName := modName
		if modName == "env" && kind != 0 { // not a function
			newModName = "env.extras"
		}

		// Write the (potentially modified) module name
		result = append(result, encodeName(newModName)...)
		result = append(result, encodeName(fieldName)...)
		result = append(result, kind)

		// Copy the type info depending on kind
		switch kind {
		case 0: // function
			typeIdx, n := readLEB128U(data[pos:])
			pos += n
			result = append(result, encodeLEB128U(typeIdx)...)
		case 1: // table
			elemType := data[pos]
			pos++
			result = append(result, elemType)
			limitsFlag := data[pos]
			pos++
			result = append(result, limitsFlag)
			min, n := readLEB128U(data[pos:])
			pos += n
			result = append(result, encodeLEB128U(min)...)
			if limitsFlag == 1 {
				max, n := readLEB128U(data[pos:])
				pos += n
				result = append(result, encodeLEB128U(max)...)
			}
		case 2: // memory
			limitsFlag := data[pos]
			pos++
			result = append(result, limitsFlag)
			min, n := readLEB128U(data[pos:])
			pos += n
			result = append(result, encodeLEB128U(min)...)
			if limitsFlag == 1 {
				max, n := readLEB128U(data[pos:])
				pos += n
				result = append(result, encodeLEB128U(max)...)
			}
		case 3: // global
			valType := data[pos]
			pos++
			mutable := data[pos]
			pos++
			result = append(result, valType, mutable)
		}

		_ = fieldName // used for debugging if needed
	}

	return result
}

// readLEB128U reads an unsigned LEB128 value and returns the value and bytes consumed.
func readLEB128U(data []byte) (uint64, int) {
	var result uint64
	var shift uint
	for i, b := range data {
		result |= uint64(b&0x7f) << shift
		if b&0x80 == 0 {
			return result, i + 1
		}
		shift += 7
	}
	return result, len(data)
}
