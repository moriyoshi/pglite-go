//go:build !wazero

// Command dump-imports lists every import of a wasm module with its extern
// kind, to plan the wasmtime host layer.
package main

import (
	"fmt"
	"os"
	"sort"

	"github.com/bytecodealliance/wasmtime-go/v34"
)

func main() {
	path := "wasm/pglite.wasm"
	if len(os.Args) > 1 {
		path = os.Args[1]
	}
	wasm, err := os.ReadFile(path)
	if err != nil {
		panic(err)
	}
	engine := wasmtime.NewEngine()
	mod, err := wasmtime.NewModule(engine, wasm)
	if err != nil {
		panic(err)
	}

	type row struct{ module, name, kind, detail string }
	var rows []row
	for _, imp := range mod.Imports() {
		name := ""
		if imp.Name() != nil {
			name = *imp.Name()
		}
		ty := imp.Type()
		kind, detail := "?", ""
		switch {
		case ty.FuncType() != nil:
			ft := ty.FuncType()
			kind = "func"
			detail = fmt.Sprintf("(%d)->(%d)", len(ft.Params()), len(ft.Results()))
		case ty.GlobalType() != nil:
			gt := ty.GlobalType()
			kind = "global"
			detail = fmt.Sprintf("%v mutable=%v", gt.Content().Kind(), gt.Mutable())
		case ty.MemoryType() != nil:
			mt := ty.MemoryType()
			kind = "memory"
			max, ok := mt.Maximum()
			detail = fmt.Sprintf("min=%d max=%d(%v)", mt.Minimum(), max, ok)
		case ty.TableType() != nil:
			tt := ty.TableType()
			kind = "table"
			max, ok := tt.Maximum()
			detail = fmt.Sprintf("%v min=%d max=%d(%v)", tt.Element().Kind(), tt.Minimum(), max, ok)
		}
		rows = append(rows, row{imp.Module(), name, kind, detail})
	}
	// Non-func imports first (the tricky ones), then a func summary.
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].kind != rows[j].kind {
			return rows[i].kind < rows[j].kind
		}
		if rows[i].module != rows[j].module {
			return rows[i].module < rows[j].module
		}
		return rows[i].name < rows[j].name
	})
	fmt.Println("=== non-func imports (memory/table/global) ===")
	for _, r := range rows {
		if r.kind != "func" {
			fmt.Printf("  %-8s %-12s %-24s %s\n", r.kind, r.module, r.name, r.detail)
		}
	}
	fmt.Println("=== func imports (sample of first 40) ===")
	n := 0
	for _, r := range rows {
		if r.kind == "func" {
			if n < 40 {
				fmt.Printf("  %-12s %-30s %s\n", r.module, r.name, r.detail)
			}
			n++
		}
	}
	fmt.Printf("... %d func imports total\n", n)
}
