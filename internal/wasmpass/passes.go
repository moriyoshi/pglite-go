// Package wasmpass rewrites an Emscripten (dynamic-linked, longjmp-via-host)
// wasm module into a standalone module that goccy/wasm2go can translate into a
// self-contained, panic-free Go package.
//
// Three transforms, applied in order by [Transform]:
//
//   - Strip:     drop all exports except the caller-supplied roots, so wasm2go's
//     export-name mangling cannot collide and DCE can prune to the
//     reachable subset.
//   - Prelink:   internalize the Emscripten dynamic-linking imports (memory,
//     table, and the base globals __memory_base/__stack_pointer/
//     __table_base/__heap_base) to constants, and fold elem/data
//     segment offset const-exprs from `global.get` to literals.
//   - Instrument: after every call that can transitively reach the longjmp throw
//     primitive (plus every call_indirect), insert a stack-neutral
//     guard that early-returns when a longjmp is in flight — turning
//     Emscripten's host-thrown unwind into cooperative returns, so no
//     Go panic/recover is needed in the generated code.
//
// See docs/wasm2go-migration.md for the full rationale.
package wasmpass

import "fmt"

// Config carries the module-specific constants the transforms need. Use
// [DefaultConfig] for the PGlite build this library targets.
type Config struct {
	// Roots are the export names to keep (all others are stripped).
	Roots map[string]bool
	// BaseGlobals maps each dynamic-linking base global's import name to the
	// constant value it should be internalized to.
	BaseGlobals map[string]int32
	// SeedImport is the import name of the host longjmp throw primitive.
	SeedImport string
	// ThrewAddr is the linear-memory address of the __THREW__ flag word.
	ThrewAddr int32
	// SPGlobalOverride, when >= 0, forces the __stack_pointer global index
	// (used for test modules whose SP global is not an import).
	SPGlobalOverride int
}

// DefaultConfig returns the configuration for the PGlite wasm this library
// targets (PostgreSQL 18 / PGlite 0.5.x, Emscripten SUPPORT_LONGJMP=emscripten).
func DefaultConfig() Config {
	return Config{
		Roots: map[string]bool{
			"PostgresMainLongJmp": true, "PostgresMainLoopOnce": true,
			"PostgresSendReadyForQueryIfNecessary": true, "ProcessStartupPacket": true,
			"pgl_getMyProcPort": true, "pgl_pq_flush": true, "pgl_setPGliteActive": true,
			"pgl_set_rw_cbs": true, "pgl_startPGlite": true, "pq_buffer_remaining_data": true,
			"pgl_set_pclose_fn": true, "pgl_set_popen_fn": true, "pgl_set_system_fn": true,
			"__main_argc_argv": true, "__wasm_call_ctors": true, "__wasm_apply_data_relocs": true,
			"memory": true,
			// allocators the shared _mmap_js/_munmap_js handlers call back into
			"emscripten_builtin_memalign": true, "malloc": true,
		},
		BaseGlobals: map[string]int32{
			"__memory_base": 0, "__stack_pointer": 10937088, "__table_base": 0, "__heap_base": 10937088,
		},
		SeedImport:       "_emscripten_throw_longjmp",
		ThrewAddr:        2980948,
		SPGlobalOverride: -1,
	}
}

// DefaultInitdbConfig returns the configuration for initdb.wasm. It differs from
// pglite in the export roots and the __THREW__ address; the base-global values are
// the same host-provided constants, and __stack_pointer is import global 0 (vs 1
// in pglite) — wasmpass derives that automatically for the instrumentation.
func DefaultInitdbConfig() Config {
	return Config{
		Roots: map[string]bool{
			"__main_argc_argv": true, "__wasm_call_ctors": true, "__wasm_apply_data_relocs": true,
			"pgl_set_system_fn": true, "pgl_set_popen_fn": true, "pgl_set_pclose_fn": true,
			"fopen": true, "fclose": true, "fflush": true, "_emscripten_stack_alloc": true,
			"emscripten_builtin_memalign": true, "malloc": true,
			"memory": true,
		},
		BaseGlobals: map[string]int32{
			"__memory_base": 0, "__stack_pointer": 10937088, "__table_base": 0, "__heap_base": 10937088,
		},
		SeedImport:       "_emscripten_throw_longjmp",
		ThrewAddr:        139240, // value of the global setThrew loads (initdb global 39; derive per module)
		SPGlobalOverride: -1,
	}
}

// Stats reports what each transform did.
type Stats struct {
	ExportsStripped   int
	MayLongjmpFuncs   int
	InstrumentedSites int
}

// Transform runs Strip, Prelink, and Instrument over the module bytes in and
// returns the rewritten module.
func Transform(in []byte, cfg Config) (out []byte, st Stats, err error) {
	m, err := Decode(in)
	if err != nil {
		return nil, st, err
	}
	st.ExportsStripped = m.StripExports(cfg)
	m.Prelink(cfg)
	st.MayLongjmpFuncs, st.InstrumentedSites = m.Instrument(cfg)
	return m.Encode(), st, nil
}

// StripExports drops every export whose name is not in cfg.Roots and returns the
// number removed.
func (m *Module) StripExports(cfg Config) int {
	kept := m.Exports[:0]
	n := 0
	for _, e := range m.Exports {
		if cfg.Roots[e.Name] {
			kept = append(kept, e)
		} else {
			n++
		}
	}
	m.Exports = kept
	return n
}

// Prelink internalizes the dynamic-linking imports (memory, table, base globals)
// to constants and folds elem/data segment offsets to literals.
func (m *Module) Prelink(cfg Config) {
	m.spGlobal = m.globalIndexOfImport("__stack_pointer")
	m.spGlobalSet = true

	var keep []Import
	var newGlobals []byte
	newGlobalN := uint32(0)
	for _, imp := range m.Imports {
		switch imp.Kind {
		case kMem:
			m.NewMems = encodeMemSection(imp.MLimMin, imp.MLimMax, imp.MHasMax, imp.MShared)
		case kTable:
			m.NewTables = encodeTableSection(imp.TElem, imp.TLimMin, imp.TLimMax, imp.THasMax)
		case kGlobal:
			if v, ok := cfg.BaseGlobals[imp.Name]; ok {
				newGlobals = append(newGlobals, imp.GType)
				if imp.GMutable {
					newGlobals = append(newGlobals, 1)
				} else {
					newGlobals = append(newGlobals, 0)
				}
				newGlobals = append(newGlobals, 0x41)
				newGlobals = putS32(newGlobals, v)
				newGlobals = append(newGlobals, 0x0b)
				newGlobalN++
			} else {
				keep = append(keep, imp)
			}
		default:
			keep = append(keep, imp)
		}
	}
	m.Imports = keep
	// Prepend so the internalized globals keep their original indices (0..3).
	m.Globals = append(newGlobals, m.Globals...)
	m.GlobalN += newGlobalN

	for i := range m.Elems {
		m.Elems[i].Offset = foldOffset(m.Elems[i].Offset)
	}
	for i := range m.Datas {
		m.Datas[i].Offset = foldOffset(m.Datas[i].Offset)
	}
}

// foldOffset rewrites `global.get N; end` into `i32.const 0; end` (both
// __memory_base and __table_base are 0). Literal offsets pass through.
func foldOffset(expr []byte) []byte {
	c := &cur{b: expr}
	if c.byte1() == 0x23 { // global.get
		out := []byte{0x41}
		out = putS32(out, 0)
		return append(out, 0x0b)
	}
	return expr
}

func encodeMemSection(min, max uint32, hasMax, shared bool) []byte {
	e := putU32(nil, 1)
	return encodeLimits(e, min, max, hasMax, shared)
}
func encodeTableSection(elem byte, min, max uint32, hasMax bool) []byte {
	e := putU32(nil, 1)
	e = append(e, elem)
	return encodeLimits(e, min, max, hasMax, false)
}

// mayLongjmpSet returns the set of function indices (in the overall index space)
// that can transitively reach the longjmp throw seed, plus the seed's index.
func (m *Module) mayLongjmpSet(cfg Config) (map[int]bool, int) {
	nImp := m.numImportFuncs()
	seed, fi := -1, 0
	for _, imp := range m.Imports {
		if imp.Kind == kFunc {
			if imp.Name == cfg.SeedImport {
				seed = fi
			}
			fi++
		}
	}
	if seed < 0 {
		panic(fmt.Sprintf("wasmpass: no %q import", cfg.SeedImport))
	}
	callers := map[int64][]int{}
	set := map[int]bool{}
	for i := range m.Codes {
		calls, ind := funcCalls(m.Codes[i].Body)
		overall := nImp + i
		if ind {
			set[overall] = true
		}
		for _, cal := range calls {
			callers[cal] = append(callers[cal], overall)
			if int(cal) == seed {
				set[overall] = true
			}
		}
	}
	work := make([]int, 0, len(set))
	for k := range set {
		work = append(work, k)
	}
	for len(work) > 0 {
		x := work[len(work)-1]
		work = work[:len(work)-1]
		for _, caller := range callers[int64(x)] {
			if !set[caller] {
				set[caller] = true
				work = append(work, caller)
			}
		}
	}
	return set, seed
}

// Instrument inserts the cooperative-unwind guard after every may-longjmp call
// site. It returns the may-longjmp function count and the number of sites
// instrumented. Prelink must run first.
func (m *Module) Instrument(cfg Config) (mayLongjmp, sites int) {
	set, seed := m.mayLongjmpSet(cfg)
	mayLongjmp = len(set)

	spIdx := m.spGlobal
	if !m.spGlobalSet {
		spIdx = m.globalIndexOfImport("__stack_pointer")
	}
	if cfg.SPGlobalOverride >= 0 {
		spIdx = cfg.SPGlobalOverride
	}
	if spIdx < 0 {
		panic("wasmpass: no __stack_pointer global")
	}

	for i := range m.Codes {
		results := m.Types[m.Funcs[i]].Results
		body := m.Codes[i].Body

		var after []int
		for p := 0; p < len(body); {
			np, ct, ind := walkInstr(body, p)
			if ind || (ct >= 0 && (int(ct) == seed || set[int(ct)])) {
				after = append(after, np)
			}
			p = np
		}
		if len(after) == 0 {
			continue
		}

		writesSP := bodyWritesGlobal(body, uint32(spIdx))
		spSaveLocal := 0
		if writesSP {
			spSaveLocal = m.addLocal(&m.Codes[i], 0x7f)
		}
		guard := buildGuard(cfg, results, writesSP, spSaveLocal, spIdx)

		nb := body
		for j := len(after) - 1; j >= 0; j-- {
			at := after[j]
			out := make([]byte, 0, len(nb)+len(guard))
			out = append(out, nb[:at]...)
			out = append(out, guard...)
			out = append(out, nb[at:]...)
			nb = out
			sites++
		}
		if writesSP {
			pre := []byte{0x23}
			pre = putU32(pre, uint32(spIdx))
			pre = append(pre, 0x21)
			pre = putU32(pre, uint32(spSaveLocal))
			nb = append(pre, nb...)
		}
		m.Codes[i].Body = nb
	}
	return mayLongjmp, sites
}

func buildGuard(cfg Config, results []byte, writesSP bool, spSaveLocal, spIdx int) []byte {
	g := []byte{0x41}
	g = putS32(g, cfg.ThrewAddr) // i32.const ThrewAddr
	g = append(g, 0x28)          // i32.load
	g = putU32(g, 2)             // align=2
	g = putU32(g, 0)             // offset=0
	g = append(g, 0x04, 0x40)    // if (void)
	if writesSP {
		g = append(g, 0x20)
		g = putU32(g, uint32(spSaveLocal)) // local.get spSave
		g = append(g, 0x24)
		g = putU32(g, uint32(spIdx)) // global.set sp
	}
	for _, vt := range results {
		g = append(g, zeroConst(vt)...)
	}
	g = append(g, 0x0f) // return
	g = append(g, 0x0b) // end
	return g
}

func zeroConst(vt byte) []byte {
	switch vt {
	case 0x7f:
		return []byte{0x41, 0x00}
	case 0x7e:
		return []byte{0x42, 0x00}
	case 0x7d:
		return []byte{0x43, 0x00, 0x00, 0x00, 0x00}
	case 0x7c:
		return []byte{0x44, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00}
	default:
		panic(fmt.Sprintf("wasmpass: no zero for valtype 0x%02x", vt))
	}
}

func (m *Module) addLocal(code *Code, vt byte) int {
	n := 0
	for _, c := range code.LocalCounts {
		n += int(c)
	}
	idx := -1
	for i := range m.Codes {
		if &m.Codes[i] == code {
			idx = i
			break
		}
	}
	params := len(m.Types[m.Funcs[idx]].Params)
	newIndex := params + n
	if len(code.LocalTypes) > 0 && code.LocalTypes[len(code.LocalTypes)-1] == vt {
		code.LocalCounts[len(code.LocalCounts)-1]++
	} else {
		code.LocalCounts = append(code.LocalCounts, 1)
		code.LocalTypes = append(code.LocalTypes, vt)
	}
	return newIndex
}

func bodyWritesGlobal(body []byte, g uint32) bool {
	for p := 0; p < len(body); {
		if body[p] == 0x24 { // global.set
			c := &cur{b: body, p: p + 1}
			if c.u32() == g {
				return true
			}
		}
		p, _, _ = walkInstr(body, p)
	}
	return false
}
