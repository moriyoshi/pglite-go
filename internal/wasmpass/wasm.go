package wasmpass

import (
	"encoding/binary"
	"fmt"
)

// ---- LEB128 ----

type cur struct {
	b []byte
	p int
}

func (c *cur) u32() uint32 {
	var r uint32
	var s uint
	for {
		x := c.b[c.p]
		c.p++
		r |= uint32(x&0x7f) << s
		if x&0x80 == 0 {
			return r
		}
		s += 7
	}
}
func (c *cur) u64() uint64 {
	var r uint64
	var s uint
	for {
		x := c.b[c.p]
		c.p++
		r |= uint64(x&0x7f) << s
		if x&0x80 == 0 {
			return r
		}
		s += 7
	}
}
func (c *cur) s64() int64 {
	var r int64
	var s uint
	var x byte
	for {
		x = c.b[c.p]
		c.p++
		r |= int64(x&0x7f) << s
		s += 7
		if x&0x80 == 0 {
			break
		}
	}
	if s < 64 && x&0x40 != 0 {
		r |= -1 << s
	}
	return r
}
func (c *cur) byte1() byte { b := c.b[c.p]; c.p++; return b }
func (c *cur) bytes(n int) []byte {
	r := c.b[c.p : c.p+n]
	c.p += n
	return r
}
func (c *cur) name() string {
	n := int(c.u32())
	return string(c.bytes(n))
}
func (c *cur) eof() bool { return c.p >= len(c.b) }

func putU32(dst []byte, v uint32) []byte {
	for {
		x := byte(v & 0x7f)
		v >>= 7
		if v != 0 {
			dst = append(dst, x|0x80)
		} else {
			dst = append(dst, x)
			return dst
		}
	}
}
func putS32(dst []byte, v int32) []byte {
	for {
		x := byte(v & 0x7f)
		v >>= 7
		done := (v == 0 && x&0x40 == 0) || (v == -1 && x&0x40 != 0)
		if done {
			return append(dst, x)
		}
		dst = append(dst, x|0x80)
	}
}
func putName(dst []byte, s string) []byte {
	dst = putU32(dst, uint32(len(s)))
	return append(dst, s...)
}

// ---- Model (hybrid: typed for sections we transform, raw for the rest) ----

const (
	kFunc   = 0
	kTable  = 1
	kMem    = 2
	kGlobal = 3
)

type Import struct {
	Module, Name string
	Kind         byte
	TypeIdx      uint32 // kFunc
	// kTable
	TElem   byte
	TLimMin uint32
	TLimMax uint32
	THasMax bool
	// kMem
	MLimMin uint32
	MLimMax uint32
	MHasMax bool
	MShared bool
	// kGlobal
	GType    byte
	GMutable bool
}

type Export struct {
	Name  string
	Kind  byte
	Index uint32
}

type FuncType struct {
	Params, Results []byte // valtypes
}

type Code struct {
	// locals: run-length encoded
	LocalCounts []uint32
	LocalTypes  []byte
	Body        []byte // instruction bytes incl. terminating 0x0B
}

type rawSection struct {
	id      byte
	payload []byte
}

type Module struct {
	// raw, kept verbatim (re-emitted as-is)
	rawTypes   []byte // payload of section 1 (incl count)
	rawFunc    []byte // section 3
	rawStart   []byte // section 8 (payload)
	rawDataCnt []byte // section 12 payload
	hasStart   bool
	hasDataCnt bool

	// parsed / transformed
	Types     []FuncType // parsed from rawTypes (for result arities)
	Imports   []Import
	Exports   []Export
	Funcs     []uint32 // defined-func type indices (parsed from rawFunc)
	Globals   []byte   // raw entries (after count); we prepend new ones
	GlobalN   uint32   // count
	NewTables []byte   // synthesized table section payload (prelink), or nil
	NewMems   []byte   // synthesized memory section payload (prelink), or nil
	Elems     []ElemSeg
	Datas     []DataSeg
	Codes     []Code

	spGlobal    int // index of __stack_pointer global (preserved across prelink); -1 if unknown
	spGlobalSet bool
}

type ElemSeg struct {
	Flag   uint32
	Offset []byte // const expr incl 0x0B (for active flag 0)
	Funcs  []uint32
	Raw    []byte // for flags we don't specialize
}
type DataSeg struct {
	Flag   uint32
	Offset []byte
	Data   []byte
	Raw    []byte
}

func Decode(b []byte) (*Module, error) {
	if len(b) < 8 || string(b[:4]) != "\x00asm" {
		return nil, fmt.Errorf("not a wasm module")
	}
	m := &Module{}
	c := &cur{b: b, p: 8}
	for !c.eof() {
		id := c.byte1()
		size := int(c.u32())
		payload := c.bytes(size)
		switch id {
		case 0: // custom — dropped
		case 1:
			m.rawTypes = payload
			m.parseTypes(payload)
		case 2:
			m.parseImports(payload)
		case 3:
			m.rawFunc = payload
			m.parseFuncs(payload)
		case 4: // table (defined) — none in input, but keep if present
			m.NewTables = payload
		case 5:
			m.NewMems = payload
		case 6:
			pc := &cur{b: payload}
			m.GlobalN = pc.u32()
			m.Globals = append([]byte(nil), payload[pc.p:]...)
		case 7:
			m.parseExports(payload)
		case 8:
			m.rawStart = payload
			m.hasStart = true
		case 9:
			m.parseElems(payload)
		case 10:
			m.parseCode(payload)
		case 11:
			m.parseData(payload)
		case 12:
			m.rawDataCnt = payload
			m.hasDataCnt = true
		default:
			return nil, fmt.Errorf("unknown section id %d", id)
		}
	}
	return m, nil
}

func (m *Module) parseTypes(p []byte) {
	c := &cur{b: p}
	n := c.u32()
	for i := uint32(0); i < n; i++ {
		form := c.byte1() // 0x60
		_ = form
		np := int(c.u32())
		params := append([]byte(nil), c.bytes(np)...)
		nr := int(c.u32())
		results := append([]byte(nil), c.bytes(nr)...)
		m.Types = append(m.Types, FuncType{params, results})
	}
}

func (m *Module) parseFuncs(p []byte) {
	c := &cur{b: p}
	n := c.u32()
	for i := uint32(0); i < n; i++ {
		m.Funcs = append(m.Funcs, c.u32())
	}
}

func (m *Module) parseImports(p []byte) {
	c := &cur{b: p}
	n := c.u32()
	for i := uint32(0); i < n; i++ {
		var imp Import
		imp.Module = c.name()
		imp.Name = c.name()
		imp.Kind = c.byte1()
		switch imp.Kind {
		case kFunc:
			imp.TypeIdx = c.u32()
		case kTable:
			imp.TElem = c.byte1()
			imp.readLimits(c, kTable)
		case kMem:
			imp.readLimits(c, kMem)
		case kGlobal:
			imp.GType = c.byte1()
			imp.GMutable = c.byte1() == 1
		}
		m.Imports = append(m.Imports, imp)
	}
}

func (imp *Import) readLimits(c *cur, kind byte) {
	flag := c.byte1()
	min := c.u32()
	var max uint32
	hasMax := false
	shared := false
	switch flag {
	case 0x00:
	case 0x01:
		max = c.u32()
		hasMax = true
	case 0x03: // shared with max
		max = c.u32()
		hasMax = true
		shared = true
	case 0x02: // shared no max
		shared = true
	}
	if kind == kTable {
		imp.TLimMin, imp.TLimMax, imp.THasMax = min, max, hasMax
	} else {
		imp.MLimMin, imp.MLimMax, imp.MHasMax, imp.MShared = min, max, hasMax, shared
	}
}

func (m *Module) parseExports(p []byte) {
	c := &cur{b: p}
	n := c.u32()
	for i := uint32(0); i < n; i++ {
		var e Export
		e.Name = c.name()
		e.Kind = c.byte1()
		e.Index = c.u32()
		m.Exports = append(m.Exports, e)
	}
}

func constExpr(c *cur) []byte {
	start := c.p
	for {
		op := c.byte1()
		switch op {
		case 0x0b: // end
			return append([]byte(nil), c.b[start:c.p]...)
		case 0x23, 0x41: // global.get / i32.const : one leb operand
			c.u32()
		case 0x42:
			c.u64()
		case 0x43:
			c.p += 4
		case 0x44:
			c.p += 8
		default:
			// other const-expr ops unexpected here
		}
	}
}

func (m *Module) parseElems(p []byte) {
	c := &cur{b: p}
	n := c.u32()
	for i := uint32(0); i < n; i++ {
		var e ElemSeg
		e.Flag = c.u32()
		if e.Flag == 0 {
			e.Offset = constExpr(c)
			cnt := c.u32()
			for j := uint32(0); j < cnt; j++ {
				e.Funcs = append(e.Funcs, c.u32())
			}
		} else {
			panic(fmt.Sprintf("elem flag %d not supported", e.Flag))
		}
		m.Elems = append(m.Elems, e)
	}
}

func (m *Module) parseData(p []byte) {
	c := &cur{b: p}
	n := c.u32()
	for i := uint32(0); i < n; i++ {
		var d DataSeg
		d.Flag = c.u32()
		if d.Flag == 0 {
			d.Offset = constExpr(c)
			ln := int(c.u32())
			d.Data = append([]byte(nil), c.bytes(ln)...)
		} else {
			panic(fmt.Sprintf("data flag %d not supported", d.Flag))
		}
		m.Datas = append(m.Datas, d)
	}
}

func (m *Module) parseCode(p []byte) {
	c := &cur{b: p}
	n := c.u32()
	for i := uint32(0); i < n; i++ {
		size := int(c.u32())
		end := c.p + size
		var code Code
		nl := c.u32()
		for j := uint32(0); j < nl; j++ {
			code.LocalCounts = append(code.LocalCounts, c.u32())
			code.LocalTypes = append(code.LocalTypes, c.byte1())
		}
		code.Body = append([]byte(nil), c.b[c.p:end]...)
		c.p = end
		m.Codes = append(m.Codes, code)
	}
}

// numImportFuncs / numImportGlobals helpers
func (m *Module) numImportFuncs() int {
	n := 0
	for _, imp := range m.Imports {
		if imp.Kind == kFunc {
			n++
		}
	}
	return n
}
func (m *Module) globalIndexOfImport(name string) int {
	gi := 0
	for _, imp := range m.Imports {
		if imp.Kind == kGlobal {
			if imp.Name == name {
				return gi
			}
			gi++
		}
	}
	return -1
}

// ---- Encode ----

func section(id byte, payload []byte) []byte {
	out := []byte{id}
	out = putU32(out, uint32(len(payload)))
	return append(out, payload...)
}

func (m *Module) Encode() []byte {
	out := []byte("\x00asm")
	out = append(out, 1, 0, 0, 0)

	// 1 types
	out = append(out, section(1, m.rawTypes)...)
	// 2 imports
	out = append(out, section(2, m.encodeImports())...)
	// 3 functions
	out = append(out, section(3, m.rawFunc)...)
	// 4 tables
	if len(m.NewTables) > 0 {
		out = append(out, section(4, m.NewTables)...)
	}
	// 5 memories
	if len(m.NewMems) > 0 {
		out = append(out, section(5, m.NewMems)...)
	}
	// 6 globals
	out = append(out, section(6, m.encodeGlobals())...)
	// 7 exports
	out = append(out, section(7, m.encodeExports())...)
	// 8 start
	if m.hasStart {
		out = append(out, section(8, m.rawStart)...)
	}
	// 9 elems
	out = append(out, section(9, m.encodeElems())...)
	// 12 datacount
	if m.hasDataCnt {
		out = append(out, section(12, m.rawDataCnt)...)
	}
	// 10 code
	out = append(out, section(10, m.encodeCode())...)
	// 11 data
	out = append(out, section(11, m.encodeData())...)
	return out
}

func (m *Module) encodeImports() []byte {
	var e []byte
	e = putU32(e, uint32(len(m.Imports)))
	for _, imp := range m.Imports {
		e = putName(e, imp.Module)
		e = putName(e, imp.Name)
		e = append(e, imp.Kind)
		switch imp.Kind {
		case kFunc:
			e = putU32(e, imp.TypeIdx)
		case kTable:
			e = append(e, imp.TElem)
			e = encodeLimits(e, imp.TLimMin, imp.TLimMax, imp.THasMax, false)
		case kMem:
			e = encodeLimits(e, imp.MLimMin, imp.MLimMax, imp.MHasMax, imp.MShared)
		case kGlobal:
			e = append(e, imp.GType)
			if imp.GMutable {
				e = append(e, 1)
			} else {
				e = append(e, 0)
			}
		}
	}
	return e
}

func encodeLimits(e []byte, min, max uint32, hasMax, shared bool) []byte {
	var flag byte
	switch {
	case shared && hasMax:
		flag = 0x03
	case shared:
		flag = 0x02
	case hasMax:
		flag = 0x01
	default:
		flag = 0x00
	}
	e = append(e, flag)
	e = putU32(e, min)
	if hasMax {
		e = putU32(e, max)
	}
	return e
}

func (m *Module) encodeGlobals() []byte {
	var e []byte
	e = putU32(e, m.GlobalN)
	e = append(e, m.Globals...)
	return e
}

func (m *Module) encodeExports() []byte {
	var e []byte
	e = putU32(e, uint32(len(m.Exports)))
	for _, x := range m.Exports {
		e = putName(e, x.Name)
		e = append(e, x.Kind)
		e = putU32(e, x.Index)
	}
	return e
}

func (m *Module) encodeElems() []byte {
	var e []byte
	e = putU32(e, uint32(len(m.Elems)))
	for _, s := range m.Elems {
		e = putU32(e, s.Flag)
		e = append(e, s.Offset...)
		e = putU32(e, uint32(len(s.Funcs)))
		for _, f := range s.Funcs {
			e = putU32(e, f)
		}
	}
	return e
}

func (m *Module) encodeData() []byte {
	var e []byte
	e = putU32(e, uint32(len(m.Datas)))
	for _, s := range m.Datas {
		e = putU32(e, s.Flag)
		e = append(e, s.Offset...)
		e = putU32(e, uint32(len(s.Data)))
		e = append(e, s.Data...)
	}
	return e
}

func (m *Module) encodeCode() []byte {
	var e []byte
	e = putU32(e, uint32(len(m.Codes)))
	for _, code := range m.Codes {
		var body []byte
		body = putU32(body, uint32(len(code.LocalCounts)))
		for i := range code.LocalCounts {
			body = putU32(body, code.LocalCounts[i])
			body = append(body, code.LocalTypes[i])
		}
		body = append(body, code.Body...)
		e = putU32(e, uint32(len(body)))
		e = append(e, body...)
	}
	return e
}

var _ = binary.LittleEndian
