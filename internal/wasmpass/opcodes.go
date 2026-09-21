package wasmpass

import "fmt"

// walkInstr returns the index just past the instruction at p, plus, if the
// instruction is a call/call_indirect, its classification.
//
//	callTarget >= 0  -> direct `call`, target function index
//	indirect         -> `call_indirect`
//
// Only the opcode families present in pglite.wasm are handled precisely
// (MVP + sign-extension + memory.copy/fill + typed blocktypes); anything
// outside that set panics so it can never be silently mis-skipped.
func walkInstr(b []byte, p int) (next int, callTarget int64, indirect bool) {
	callTarget = -1
	op := b[p]
	p++
	c := &cur{b: b, p: p}
	switch {
	case op == 0x02 || op == 0x03 || op == 0x04: // block/loop/if : blocktype (s33)
		c.s64()
	case op == 0x05: // else
	case op == 0x0b: // end
	case op == 0x0c || op == 0x0d: // br / br_if
		c.u32()
	case op == 0x0e: // br_table
		n := c.u32()
		for i := uint32(0); i < n+1; i++ {
			c.u32()
		}
	case op == 0x0f: // return
	case op == 0x00 || op == 0x01: // unreachable / nop
	case op == 0x10: // call
		callTarget = int64(c.u32())
	case op == 0x11: // call_indirect
		c.u32() // typeidx
		c.u32() // tableidx
		indirect = true
	case op == 0x1a || op == 0x1b: // drop / select
	case op == 0x1c: // select t*
		n := c.u32()
		c.p += int(n)
	case op >= 0x20 && op <= 0x24: // local.get/set/tee, global.get/set
		c.u32()
	case op >= 0x25 && op <= 0x26: // table.get/set
		c.u32()
	case op >= 0x28 && op <= 0x3e: // memory load/store : memarg (align, offset)
		c.u32()
		c.u32()
	case op == 0x3f || op == 0x40: // memory.size / memory.grow : reserved memidx byte
		c.byte1()
	case op == 0x41: // i32.const
		c.s64()
	case op == 0x42: // i64.const
		c.s64()
	case op == 0x43: // f32.const
		c.p += 4
	case op == 0x44: // f64.const
		c.p += 8
	case op >= 0x45 && op <= 0xbf: // comparisons + numeric ops (no immediates)
	case op >= 0xc0 && op <= 0xc4: // sign-extension ops (i32/i64.extendN_s)
	case op == 0xd0: // ref.null t
		c.byte1()
	case op == 0xd1: // ref.is_null
	case op == 0xd2: // ref.func
		c.u32()
	case op == 0xfc: // misc prefix (bulk memory / sat trunc)
		sub := c.u32()
		switch sub {
		case 0, 1, 2, 3, 4, 5, 6, 7: // i32/i64.trunc_sat_*
		case 8: // memory.init
			c.u32()
			c.byte1()
		case 9: // data.drop
			c.u32()
		case 10: // memory.copy
			c.byte1()
			c.byte1()
		case 11: // memory.fill
			c.byte1()
		case 12: // table.init
			c.u32()
			c.u32()
		case 13: // elem.drop
			c.u32()
		case 14: // table.copy
			c.u32()
			c.u32()
		case 15, 16, 17: // table.grow/size/fill
			c.u32()
		default:
			panic(fmt.Sprintf("unhandled 0xfc sub-op %d at %d", sub, p))
		}
	default:
		panic(fmt.Sprintf("unhandled opcode 0x%02x at %d", op, p-1))
	}
	return c.p, callTarget, indirect
}

// funcCalls returns direct call targets and whether the body has any call_indirect.
func funcCalls(body []byte) (calls []int64, indirect bool) {
	p := 0
	for p < len(body) {
		var ct int64
		var ind bool
		p, ct, ind = walkInstr(body, p)
		if ct >= 0 {
			calls = append(calls, ct)
		}
		if ind {
			indirect = true
		}
	}
	return
}
