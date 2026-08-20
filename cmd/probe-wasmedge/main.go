//go:build wasmedge

// Command probe-wasmedge is the wasmedge analog of cmd/probe-wasmtime: it wires
// the memory/table/global/function imports pglite.wasm needs (funcs as no-op
// stubs) via the wasmedge C API and instantiates the module, proving the
// raw-CGo route is viable on the installed libwasmedge.
//
// The maintained Go binding (second-state/WasmEdge-go, ≤ v0.14.0) does NOT
// compile against libwasmedge 0.17.x (the WasmEdge_Limit type became an opaque
// WasmEdge_LimitContext*), so this talks to the C API directly.
//
// Build/run (with wasmedge installed via brew):
//
//	CGO_ENABLED=1 CGO_CFLAGS="-I/usr/local/include" \
//	  CGO_LDFLAGS="-L/usr/local/lib -lwasmedge" DYLD_LIBRARY_PATH=/usr/local/lib \
//	  go run -tags wasmedge ./cmd/probe-wasmedge wasm/pglite.wasm
package main

/*
#cgo CFLAGS: -I/usr/local/include
#cgo LDFLAGS: -L/usr/local/lib -lwasmedge
#include <wasmedge/wasmedge.h>
#include <stdlib.h>

// No-op host-function trampoline. Instantiation never calls host functions
// (the module's start does not run ctors), so a stub suffices for the probe.
static WasmEdge_Result probeHostFunc(void *data, const WasmEdge_CallingFrameContext *frame,
                                     const WasmEdge_Value *params, WasmEdge_Value *returns) {
	return WasmEdge_Result_Success;
}
static WasmEdge_HostFunc_t get_probe_fn() { return probeHostFunc; }
*/
import "C"

import (
	"fmt"
	"os"
	"unsafe"
)

func gostr(s C.WasmEdge_String) string { return C.GoStringN(s.Buf, C.int(s.Length)) }

func main() {
	path := "wasm/pglite.wasm"
	if len(os.Args) > 1 {
		path = os.Args[1]
	}
	wasm, err := os.ReadFile(path)
	if err != nil {
		panic(err)
	}

	conf := C.WasmEdge_ConfigureCreate()
	loader := C.WasmEdge_LoaderCreate(conf)
	var ast *C.WasmEdge_ASTModuleContext
	if !C.WasmEdge_ResultOK(C.WasmEdge_LoaderParseFromBuffer(loader, &ast,
		(*C.uint8_t)(unsafe.Pointer(&wasm[0])), C.uint32_t(len(wasm)))) {
		fmt.Println("parse failed")
		os.Exit(1)
	}
	if !C.WasmEdge_ResultOK(C.WasmEdge_ValidatorValidate(C.WasmEdge_ValidatorCreate(conf), ast)) {
		fmt.Println("validate failed")
		os.Exit(1)
	}

	n := C.WasmEdge_ASTModuleListImportsLength(ast)
	imports := make([]*C.WasmEdge_ImportTypeContext, int(n))
	C.WasmEdge_ASTModuleListImports(ast,
		(**C.WasmEdge_ImportTypeContext)(unsafe.Pointer(&imports[0])), n)

	// One host ModuleInstance per distinct import module name (env, GOT.mem,
	// wasi_snapshot_preview1). Each import reuses its own declared type from the
	// AST, so the provided externs match by construction.
	mods := map[string]*C.WasmEdge_ModuleInstanceContext{}
	getMod := func(name C.WasmEdge_String) *C.WasmEdge_ModuleInstanceContext {
		k := gostr(name)
		if m, ok := mods[k]; ok {
			return m
		}
		m := C.WasmEdge_ModuleInstanceCreate(name)
		mods[k] = m
		return m
	}

	nf, nm, nt, ng := 0, 0, 0, 0
	for i := 0; i < int(n); i++ {
		imp := imports[i]
		mod := getMod(C.WasmEdge_ImportTypeGetModuleName(imp))
		fname := C.WasmEdge_ImportTypeGetExternalName(imp)
		switch C.WasmEdge_ImportTypeGetExternalType(imp) {
		case C.WasmEdge_ExternalType_Function:
			fn := C.WasmEdge_FunctionInstanceCreate(
				C.WasmEdge_ImportTypeGetFunctionType(ast, imp), C.get_probe_fn(), nil, 0)
			C.WasmEdge_ModuleInstanceAddFunction(mod, fname, fn)
			nf++
		case C.WasmEdge_ExternalType_Memory:
			mi := C.WasmEdge_MemoryInstanceCreate(C.WasmEdge_ImportTypeGetMemoryType(ast, imp))
			C.WasmEdge_ModuleInstanceAddMemory(mod, fname, mi)
			nm++
		case C.WasmEdge_ExternalType_Table:
			ti := C.WasmEdge_TableInstanceCreate(C.WasmEdge_ImportTypeGetTableType(ast, imp))
			C.WasmEdge_ModuleInstanceAddTable(mod, fname, ti)
			nt++
		case C.WasmEdge_ExternalType_Global:
			gt := C.WasmEdge_ImportTypeGetGlobalType(ast, imp)
			v := C.WasmEdge_ValueGenI32(0)
			if k := gostr(fname); k == "__stack_pointer" || k == "__heap_base" {
				v = C.WasmEdge_ValueGenI32(10937088)
			}
			gi := C.WasmEdge_GlobalInstanceCreate(gt, v)
			C.WasmEdge_ModuleInstanceAddGlobal(mod, fname, gi)
			ng++
		}
	}
	fmt.Printf("imports wired: %d func, %d mem, %d table, %d global (modules: %d)\n", nf, nm, nt, ng, len(mods))

	executor := C.WasmEdge_ExecutorCreate(conf, nil)
	store := C.WasmEdge_StoreCreate()
	for _, m := range mods {
		C.WasmEdge_ExecutorRegisterImport(executor, store, m)
	}
	var inst *C.WasmEdge_ModuleInstanceContext
	if !C.WasmEdge_ResultOK(C.WasmEdge_ExecutorInstantiate(executor, &inst, store, ast)) {
		fmt.Println("INSTANTIATE FAILED")
		os.Exit(1)
	}
	fmt.Println("INSTANTIATED pglite.wasm on wasmedge (raw CGo, libwasmedge 0.17.1)")
}
