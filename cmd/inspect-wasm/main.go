//go:build ignore

package main

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/tetratelabs/wazero"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintf(os.Stderr, "usage: %s <wasm-file>\n", os.Args[0])
		os.Exit(1)
	}

	wasmPath := os.Args[1]
	wasmBytes, err := os.ReadFile(wasmPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to read %s: %v\n", wasmPath, err)
		os.Exit(1)
	}

	ctx := context.Background()
	r := wazero.NewRuntime(ctx)
	defer r.Close(ctx)

	compiled, err := r.CompileModule(ctx, wasmBytes)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to compile WASM: %v\n", err)
		os.Exit(1)
	}

	// Print imported functions grouped by module
	imports := compiled.ImportedFunctions()
	moduleImports := make(map[string][]string)
	for _, imp := range imports {
		moduleName, name, _ := imp.Import()
		paramTypes := imp.ParamTypes()
		resultTypes := imp.ResultTypes()
		sig := fmt.Sprintf("  func %s(%v) -> (%v)", name, formatTypes(paramTypes), formatTypes(resultTypes))
		moduleImports[moduleName] = append(moduleImports[moduleName], sig)
	}

	// Print imported globals
	importedGlobals := compiled.ImportedGlobals()
	for _, g := range importedGlobals {
		moduleName, name, _ := g.Import()
		sig := fmt.Sprintf("  global %s: %s (mutable=%v)", name, formatType(g.ValueTypes()[0]), g.Mutable())
		moduleImports[moduleName] = append(moduleImports[moduleName], sig)
	}

	// Print imported memories
	importedMemories := compiled.ImportedMemories()
	for _, m := range importedMemories {
		moduleName, name, _ := m.Import()
		min, max, hasMax := m.MemoryDefinition().Pages()
		if hasMax {
			sig := fmt.Sprintf("  memory %s: min=%d max=%d pages", name, min, max)
			moduleImports[moduleName] = append(moduleImports[moduleName], sig)
		} else {
			sig := fmt.Sprintf("  memory %s: min=%d pages", name, min)
			moduleImports[moduleName] = append(moduleImports[moduleName], sig)
		}
	}

	modules := make([]string, 0, len(moduleImports))
	for m := range moduleImports {
		modules = append(modules, m)
	}
	sort.Strings(modules)

	totalImports := len(imports) + len(importedGlobals) + len(importedMemories)
	fmt.Printf("=== IMPORTS (%d total) ===\n", totalImports)
	for _, mod := range modules {
		entries := moduleImports[mod]
		sort.Strings(entries)
		fmt.Printf("\nModule %q (%d entries):\n", mod, len(entries))
		for _, entry := range entries {
			fmt.Println(entry)
		}
	}

	// Print key PGlite exports only
	exports := compiled.ExportedFunctions()
	exportNames := make([]string, 0, len(exports))
	for name := range exports {
		exportNames = append(exportNames, name)
	}
	sort.Strings(exportNames)

	fmt.Printf("\n=== EXPORTS (%d functions) ===\n", len(exportNames))
	keyPrefixes := []string{"_pgl_", "_PostgresMain", "_ProcessStartup", "_main", "__main", "callMain"}
	fmt.Println("\nKey PGlite-related exports:")
	for _, name := range exportNames {
		for _, prefix := range keyPrefixes {
			if strings.HasPrefix(name, prefix) {
				def := exports[name]
				fmt.Printf("  %s(%v) -> (%v)\n", name, formatTypes(def.ParamTypes()), formatTypes(def.ResultTypes()))
				break
			}
		}
	}

	mems := compiled.ExportedMemories()
	fmt.Printf("\n=== EXPORTED MEMORIES (%d) ===\n", len(mems))
	for name := range mems {
		fmt.Printf("  %s\n", name)
	}

	globals := compiled.ExportedGlobals()
	fmt.Printf("\n=== EXPORTED GLOBALS (%d) ===\n", len(globals))
	for name, g := range globals {
		fmt.Printf("  %s: %v\n", name, g.ValueTypes())
	}
}

func formatType(t byte) string {
	switch t {
	case 0x7f:
		return "i32"
	case 0x7e:
		return "i64"
	case 0x7d:
		return "f32"
	case 0x7c:
		return "f64"
	case 0x70:
		return "funcref"
	case 0x6f:
		return "externref"
	default:
		return fmt.Sprintf("0x%02x", t)
	}
}

func formatTypes(types []byte) string {
	if len(types) == 0 {
		return ""
	}
	names := make([]string, len(types))
	for i, t := range types {
		names[i] = formatType(t)
	}
	return strings.Join(names, ", ")
}
