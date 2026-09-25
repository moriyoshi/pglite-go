// Command genaot emits the AOT glue for a goccy/wasm2go-transpiled PGlite
// package: the AOTModule shim (NewAOT + the emscripten.AOTModule methods) and
// the env/WASI host adapter. It reads the transpiled package's interfaces and
// writes aot_generated.go into the package directory.
//
// It handles both wasm2go output shapes:
//   - multi-package (large modules, e.g. pglite): a base subpackage holds the
//     Module (exported fields Memory/T0/GN), exports are package funcs
//     WasmCallCtors(m *base.Module), interfaces live in base/base.go.
//   - single-file (small modules, e.g. initdb): everything in one package,
//     unexported fields (memory/t0/gN), exports are methods m.WasmCallCtors(),
//     interfaces in the top .go file.
//
// It is run by `go generate -tags aot` after wasm2go; see docs/wasm2go-migration.md.
package main

import (
	"flag"
	"fmt"
	"go/format"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

const (
	emscriptenImport = "github.com/moriyoshi/pglite-go/emscripten"
	vfsImport        = "github.com/moriyoshi/pglite-go/vfs"
)

// exportEntry maps a wasm export name to its generated Go name. argc counts i32
// args after the receiver; ret is true if it returns an i32.
type exportEntry struct {
	goFunc string
	argc   int
	ret    bool
}

// pgliteExports maps the roots pglite-go calls (see wasmpass.DefaultConfig).
var pgliteExports = map[string]exportEntry{
	"__wasm_call_ctors":                    {"WasmCallCtors", 0, false},
	"__wasm_apply_data_relocs":             {"WasmApplyDataRelocs", 0, false},
	"__main_argc_argv":                     {"MainArgcArgv", 2, true},
	"pgl_startPGlite":                      {"PglStartPGlite", 0, false},
	"pgl_pq_flush":                         {"PglPqFlush", 0, false},
	"pgl_getMyProcPort":                    {"PglGetMyProcPort", 0, true},
	"pgl_setPGliteActive":                  {"PglSetPGliteActive", 1, true},
	"pgl_set_rw_cbs":                       {"PglSetRwCbs", 2, false},
	"pgl_set_system_fn":                    {"PglSetSystemFn", 1, false},
	"pgl_set_popen_fn":                     {"PglSetPopenFn", 1, false},
	"pgl_set_pclose_fn":                    {"PglSetPcloseFn", 1, false},
	"pq_buffer_remaining_data":             {"PqBufferRemainingData", 0, true},
	"ProcessStartupPacket":                 {"ProcessStartupPacket", 3, true},
	"PostgresMainLoopOnce":                 {"PostgresMainLoopOnce", 0, false},
	"PostgresMainLongJmp":                  {"PostgresMainLongJmp", 0, false},
	"PostgresSendReadyForQueryIfNecessary": {"PostgresSendReadyForQueryIfNecessary", 0, false},
	// Allocators the shared _mmap_js/_munmap_js handlers call back into.
	"emscripten_builtin_memalign": {"EmscriptenBuiltinMemalign", 2, true},
	"malloc":                      {"Malloc", 1, true},
}

// initdbExports maps the roots runInitdb + InitdbCallbacks call.
var initdbExports = map[string]exportEntry{
	"__wasm_call_ctors":           {"WasmCallCtors", 0, false},
	"__wasm_apply_data_relocs":    {"WasmApplyDataRelocs", 0, false},
	"__main_argc_argv":            {"MainArgcArgv", 2, true},
	"pgl_set_system_fn":           {"PglSetSystemFn", 1, false},
	"pgl_set_popen_fn":            {"PglSetPopenFn", 1, false},
	"pgl_set_pclose_fn":           {"PglSetPcloseFn", 1, false},
	"fopen":                       {"Fopen", 2, true},
	"fclose":                      {"Fclose", 1, true},
	"fflush":                      {"Fflush", 1, true},
	"_emscripten_stack_alloc":     {"EmscriptenStackAlloc", 1, true}, // export method mangle strips leading _
	"emscripten_builtin_memalign": {"EmscriptenBuiltinMemalign", 2, true},
	"malloc":                      {"Malloc", 1, true},
}

// layout captures the differences between the two wasm2go output shapes.
type layout struct {
	mod     string // Module type name: "base.Module" or "Module"
	mem     string // memory field: "Memory" or "memory"
	t0      string // table field: "T0" or "t0"
	gpre    string // global-field prefix: "G" or "g"
	memSize string // memSize field: "MemSize" or "memSize"
	method  bool   // exports are methods on *Module (single-file) vs package funcs (multi)
	useBase bool   // import the base subpackage
	ifaceGo string // file to parse interfaces from
}

func (L layout) modRef() string { return "*" + L.mod }

// entryCall renders a call to a generated export. recv is the module expr
// ("m" or "a.m"). For method layout it is recv.Name(args); for func layout it is
// Name(recv, args).
func (L layout) entryCall(recv, name string, args ...string) string {
	if L.method {
		return recv + "." + name + "(" + strings.Join(args, ", ") + ")"
	}
	return name + "(" + strings.Join(append([]string{recv}, args...), ", ") + ")"
}

// baseType rewrites the interface's "*Module" to the layout's module type.
func (L layout) baseType(t string) string { return strings.ReplaceAll(t, "*Module", "*"+L.mod) }

type method struct {
	name   string
	params []param // excludes the leading m *Module
	ret    string  // "" if void
}
type param struct{ name, typ string }

var methodRe = regexp.MustCompile(`^\s*(\w+)\((.*?)\)\s*(\S+)?\s*$`)

func parseInterface(goFile, iface string) []method {
	src, err := os.ReadFile(goFile)
	must(err)
	s := string(src)
	i := strings.Index(s, "type "+iface+" interface {")
	if i < 0 {
		return nil // interface may be absent (e.g. no WASI imports)
	}
	body := s[i:]
	body = body[strings.Index(body, "{")+1:]
	body = body[:strings.Index(body, "\n}")]
	var out []method
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || !strings.Contains(line, "(") {
			continue
		}
		mm := methodRe.FindStringSubmatch(line)
		if mm == nil {
			continue
		}
		var ps []param
		for _, p := range splitParams(mm[2]) {
			f := strings.Fields(p)
			ps = append(ps, param{f[0], strings.Join(f[1:], " ")})
		}
		if len(ps) > 0 {
			ps = ps[1:] // drop the leading "m *Module"
		}
		out = append(out, method{mm[1], ps, mm[3]})
	}
	return out
}

func splitParams(s string) []string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	return strings.Split(s, ",")
}

// detectLayout inspects the generated package directory.
func detectLayout(dir, baseImport string) layout {
	if _, err := os.Stat(filepath.Join(dir, "base", "base.go")); err == nil {
		return layout{
			mod: "base.Module", mem: "Memory", t0: "T0", gpre: "G", memSize: "MemSize",
			method: false, useBase: true,
			ifaceGo: filepath.Join(dir, "base", "base.go"),
		}
	}
	// single-file: find the .go with the interfaces
	iface := ""
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".go") {
			continue
		}
		b, _ := os.ReadFile(filepath.Join(dir, e.Name()))
		if strings.Contains(string(b), "type EnvImports interface {") {
			iface = filepath.Join(dir, e.Name())
			break
		}
	}
	if iface == "" {
		fatal("could not find EnvImports interface in " + dir)
	}
	return layout{
		mod: "Module", mem: "memory", t0: "t0", gpre: "g", memSize: "memSize",
		method: true, useBase: false,
		ifaceGo: iface,
	}
}

func main() {
	dir := flag.String("dir", "", "generated top-package directory")
	pkg := flag.String("pkg", "", "top package name")
	baseImport := flag.String("baseimport", "", "import path of the base subpackage (multi-package layout)")
	kind := flag.String("kind", "pglite", "pglite|initdb")
	sp := flag.Int("sp", 1, "__stack_pointer global index (pglite=1, initdb=0)")
	flag.Parse()
	if *dir == "" || *pkg == "" {
		flag.Usage()
		os.Exit(2)
	}

	L := detectLayout(*dir, *baseImport)
	methods := parseInterface(L.ifaceGo, "EnvImports")
	// The same host struct also implements WASI, so file I/O routes through the
	// VFS-backed handlers instead of wasm2go's host-backed DefaultWASI.
	methods = append(methods, parseInterface(L.ifaceGo, "Wasi_snapshot_preview1Imports")...)

	table := pgliteExports
	if *kind == "initdb" {
		table = initdbExports
	}

	var b strings.Builder
	p := func(f string, a ...any) { fmt.Fprintf(&b, f, a...); b.WriteByte('\n') }

	p("// Code generated by internal/genaot; DO NOT EDIT.")
	p("//go:build aot")
	p("")
	p("package %s", *pkg)
	p("")
	p("import (")
	p("\t%q", "context")
	p("\t%q", "io")
	if L.useBase {
		p("\tbase %q", *baseImport)
	}
	p("\temcompat %q", emscriptenImport)
	p("\t%q", vfsImport)
	p("\tapi %q", "github.com/tetratelabs/wazero/api")
	p(")")
	p("")

	// --- host: implements EnvImports + Wasi_snapshot_preview1Imports ---
	p("// host implements the env + WASI import interfaces: invoke_* trampolines")
	p("// call through the table; _emscripten_throw_longjmp is the no-op cooperative")
	p("// unwind driver; everything else routes through the shared emscripten")
	p("// handlers via disp.Invoke + the api.Module adapter.")
	p("type host struct {")
	p("\tfs      *vfs.FS")
	p("\tstdin   []byte")
	p("\tcapture *[]byte")
	p("\tdisp    *emcompat.AOTDispatch")
	p("}")
	p("")
	p("func (h *host) mod(m %s) api.Module {", L.modRef())
	p("\treturn emcompat.NewAOTModuleAdapter(func() []byte { return m.%s }, func(n string, a []uint64) uint64 { return aotCall(m, n, a) })", L.mem)
	p("}")
	p("")
	for _, m := range methods {
		emitMethod(p, L, m)
	}

	// --- aotModule: implements emcompat.AOTModule ---
	p("type aotModule struct {")
	p("\tm    %s", L.modRef())
	p("\tdisp *emcompat.AOTDispatch")
	p("}")
	p("")
	p("func (a *aotModule) Memory() []byte { return a.m.%s }", L.mem)
	p("func (a *aotModule) ApplyDataRelocs() { %s }", L.entryCall("a.m", "WasmApplyDataRelocs"))
	p("func (a *aotModule) CallCtors() { %s }", L.entryCall("a.m", "WasmCallCtors"))
	p("func (a *aotModule) SP() int32 { return a.m.%s%d }", L.gpre, *sp)
	p("func (a *aotModule) SetSP(v int32) { a.m.%s%d = v }", L.gpre, *sp)
	p("func (a *aotModule) SetStdout(w io.Writer) { a.disp.SetStdout(w) }")
	p("func (a *aotModule) SetStderr(w io.Writer) { a.disp.SetStderr(w) }")
	p("func (a *aotModule) SetStdoutCapture(bb *[]byte) { a.disp.SetStdoutCapture(bb) }")
	p("")
	emitCall(p, L, table)
	emitRegisterRW(p, L)
	emitRegisterInitdb(p, L)

	p("// NewAOT wires the host adapter, instantiates the transpiled module with")
	p("// headroom for emscripten_resize_heap to grow by reslicing (no realloc,")
	p("// so cached m.M stays valid), and returns it as an emcompat.AOTModule.")
	p("func NewAOT(fs *vfs.FS, stdin []byte, capture *[]byte) emcompat.AOTModule {")
	p("\th := &host{fs: fs, stdin: stdin, capture: capture, disp: emcompat.NewAOTDispatch(fs, stdin, capture)}")
	p("\treturn &aotModule{m: NewWithWASIReserve(h, h, 768<<20), disp: h.disp}")
	p("}")
	p("")
	p("func aotI32(v uint64) int32 { return int32(uint32(v)) }")
	p("")
	p("var _ = context.Background")

	out := filepath.Join(*dir, "aot_generated.go")
	formatted, err := format.Source([]byte(b.String()))
	if err != nil {
		_ = os.WriteFile(out, []byte(b.String()), 0o644)
		fatal(fmt.Sprintf("format: %v (wrote unformatted to %s)", err, out))
	}
	must(os.WriteFile(out, formatted, 0o644))
	fmt.Fprintf(os.Stderr, "genaot: wrote %s (%d import methods, layout method=%v)\n", out, len(methods), L.method)
}

func emitMethod(p func(string, ...any), L layout, m method) {
	var sig []string
	for _, pr := range m.params {
		sig = append(sig, pr.name+" "+L.baseType(pr.typ))
	}
	params := "m " + L.modRef()
	if len(sig) > 0 {
		params += ", " + strings.Join(sig, ", ")
	}
	retClause := ""
	if m.ret != "" {
		retClause = " " + m.ret
	}
	p("func (h *host) %s(%s)%s {", m.name, params, retClause)
	switch {
	case m.name == "X_emscripten_throw_longjmp":
		p("\t// cooperative unwind: __THREW__ already set; just return")
	case m.name == "Emscripten_resize_heap":
		p("\tnewLen := uint64(uint32(%s))", m.params[0].name)
		p("\tif newLen <= m.%s.Load() {", L.memSize)
		p("\t\treturn 1")
		p("\t}")
		p("\tif newLen > uint64(cap(m.%s)) {", L.mem)
		p("\t\treturn 0")
		p("\t}")
		p("\tm.%s = m.%s[:newLen]", L.mem, L.mem)
		p("\tm.%s.Store(newLen)", L.memSize)
		p("\treturn 1")
	case strings.HasPrefix(m.name, "Invoke_"):
		idx := m.params[0].name
		callArgs := m.params[1:]
		var argTypes, argVals []string
		for _, a := range callArgs {
			argTypes = append(argTypes, L.baseType(a.typ))
			argVals = append(argVals, a.name)
		}
		fnsig := "func(" + L.modRef()
		if len(argTypes) > 0 {
			fnsig += ", " + strings.Join(argTypes, ", ")
		}
		fnsig += ")"
		if m.ret != "" {
			fnsig += " " + m.ret
		}
		call := fmt.Sprintf("m.%s[%s].(%s)(m%s)", L.t0, idx, fnsig, prefixComma(argVals))
		if m.ret != "" {
			p("\treturn %s", call)
		} else {
			p("\t%s", call)
		}
	default:
		np := len(m.params)
		nr := 0
		if m.ret != "" {
			nr = 1
		}
		stackSize := np
		if nr > stackSize {
			stackSize = nr
		}
		p("\tstack := make([]uint64, %d)", stackSize)
		var vts []string
		for i, pr := range m.params {
			p("\tstack[%d] = %s", i, encFor(pr.typ, pr.name))
			vts = append(vts, vtByte(pr.typ))
		}
		resVT := ""
		if m.ret != "" {
			resVT = vtByte(m.ret)
		}
		p("\th.disp.Invoke(%q, h.mod(m), stack, []byte{%s}, []byte{%s})", origName(m.name), strings.Join(vts, ", "), resVT)
		if m.ret != "" {
			p("\treturn %s", decFor(m.ret))
		}
	}
	p("}")
	p("")
}

func prefixComma(vals []string) string {
	if len(vals) == 0 {
		return ""
	}
	return ", " + strings.Join(vals, ", ")
}

func origName(m string) string {
	if strings.HasPrefix(m, "X_") {
		return m[1:] // X__syscall_openat -> __syscall_openat ; X_abort_js -> _abort_js
	}
	return strings.ToLower(m[:1]) + m[1:] // Emscripten_resize_heap -> emscripten_resize_heap
}
func encFor(typ, name string) string {
	switch typ {
	case "int64":
		return "api.EncodeI64(" + name + ")"
	case "float64":
		return "api.EncodeF64(" + name + ")"
	case "float32":
		return "api.EncodeF32(" + name + ")"
	}
	return "api.EncodeI32(" + name + ")"
}
func decFor(typ string) string {
	switch typ {
	case "int64":
		return "int64(stack[0])" // api has no DecodeI64; the lane is the raw uint64
	case "float64":
		return "api.DecodeF64(stack[0])"
	case "float32":
		return "api.DecodeF32(stack[0])"
	}
	return "api.DecodeI32(stack[0])"
}
func vtByte(typ string) string {
	switch typ {
	case "int64":
		return "0x7e"
	case "float64":
		return "0x7c"
	case "float32":
		return "0x7d"
	}
	return "0x7f"
}

func emitCall(p func(string, ...any), L layout, table map[string]exportEntry) {
	p("func (a *aotModule) Call(name string, args []uint64) uint64 { return aotCall(a.m, name, args) }")
	p("")
	p("// aotCall dispatches an exported function by name (wasm value lanes).")
	p("func aotCall(m %s, name string, args []uint64) uint64 {", L.modRef())
	p("\tswitch name {")
	for name, e := range table {
		var callArgs []string
		for i := 0; i < e.argc; i++ {
			callArgs = append(callArgs, fmt.Sprintf("aotI32(args[%d])", i))
		}
		invoke := L.entryCall("m", e.goFunc, callArgs...)
		p("\tcase %q:", name)
		if e.ret {
			p("\t\treturn uint64(uint32(%s))", invoke)
		} else {
			p("\t\t%s", invoke)
			p("\t\treturn 0")
		}
	}
	p("\tdefault:")
	p("\t\tpanic(\"aot: unknown export \" + name)")
	p("\t}")
	p("}")
	p("")
}

func emitRegisterRW(p func(string, ...any), L layout) {
	p("// RegisterRW appends the socket read/write callbacks (ptr i32, len i32 -> i32,")
	p("// matching the wazero backend) and returns the base index.")
	p("func (a *aotModule) RegisterRW(read func(dst []byte) int, write func(src []byte)) uint32 {")
	p("\tidx := uint32(len(a.m.%s))", L.t0)
	p("\treadFn := func(m %s, ptr, max int32) int32 {", L.modRef())
	p("\t\ttmp := make([]byte, max)")
	p("\t\tn := read(tmp)")
	p("\t\tif n > 0 {")
	p("\t\t\tcopy(m.%s[ptr:ptr+int32(n)], tmp[:n])", L.mem)
	p("\t\t}")
	p("\t\treturn int32(n)")
	p("\t}")
	p("\twriteFn := func(m %s, ptr, length int32) int32 {", L.modRef())
	p("\t\twrite(m.%s[ptr : ptr+length])", L.mem)
	p("\t\treturn length")
	p("\t}")
	p("\ta.m.%s = append(a.m.%s, readFn, writeFn)", L.t0, L.t0)
	p("\treturn idx")
	p("}")
	p("")
}

func emitRegisterInitdb(p func(string, ...any), L layout) {
	p("// RegisterInitdb gives the callbacks module access (readCString, Fopen via")
	p("// exports) and appends the system/popen/pclose table entries.")
	p("func (a *aotModule) RegisterInitdb(cb *emcompat.InitdbCallbacks) uint32 {")
	p("\tcb.SetModule(emcompat.NewAOTModuleAdapter(func() []byte { return a.m.%s }, func(n string, ar []uint64) uint64 { return aotCall(a.m, n, ar) }))", L.mem)
	p("\tidx := uint32(len(a.m.%s))", L.t0)
	p("\tsystemFn := func(m %s, cmd int32) int32 {", L.modRef())
	p("\t\treturn cb.OnSystem(context.Background(), aotCStr(m.%s, cmd))", L.mem)
	p("\t}")
	p("\tpopenFn := func(m %s, cmd, mode int32) int32 {", L.modRef())
	p("\t\treturn cb.OnPopen(context.Background(), aotCStr(m.%s, cmd), aotCStr(m.%s, mode))", L.mem, L.mem)
	p("\t}")
	p("\tpcloseFn := func(m %s, stream int32) int32 {", L.modRef())
	p("\t\treturn cb.OnPclose(context.Background(), stream)")
	p("\t}")
	p("\ta.m.%s = append(a.m.%s, systemFn, popenFn, pcloseFn)", L.t0, L.t0)
	p("\treturn idx")
	p("}")
	p("")
	p("func aotCStr(mem []byte, ptr int32) string {")
	p("\tif ptr == 0 {")
	p("\t\treturn \"\"")
	p("\t}")
	p("\tend := int(ptr)")
	p("\tfor end < len(mem) && mem[end] != 0 {")
	p("\t\tend++")
	p("\t}")
	p("\treturn string(mem[ptr:end])")
	p("}")
	p("")
}

func must(err error) {
	if err != nil {
		fatal(err.Error())
	}
}
func fatal(s string) {
	fmt.Fprintln(os.Stderr, "genaot:", s)
	os.Exit(1)
}
