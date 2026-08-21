// Command pglite is a small demo of the embedded PostgreSQL library: it opens a
// cluster (initdb on first run, load-and-skip-initdb after) and runs a query.
// It builds on both backends — wasmtime by default, or pure-Go wazero with
// -tags wazero. For the standard Go database API, use the "pglite" database/sql
// driver — see package github.com/moriyoshi/pglite-go/pgdriver.
//
//	go run ./cmd/pglite [wasmDir]
//	go run -tags wazero ./cmd/pglite [wasmDir]
package main

import (
	"fmt"
	"os"
	"strings"
	"time"

	pglite "github.com/moriyoshi/pglite-go"
)

func main() {
	cfg := pglite.Config{}
	if len(os.Args) > 1 {
		cfg.WasmDir = os.Args[1]
	}

	t0 := time.Now()
	db, err := pglite.Open(cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "open:", err)
		os.Exit(1)
	}
	defer db.Close()
	fmt.Printf("cluster ready in %.2fs\n", time.Since(t0).Seconds())

	rows, err := db.Query("SELECT 1 + 1 AS result, 'hello, ' || 'pglite' AS greeting")
	if err != nil {
		fmt.Fprintln(os.Stderr, "query:", err)
		os.Exit(1)
	}
	fmt.Println(strings.Join(rows.Columns, " | "))
	for _, row := range rows.Rows {
		cells := make([]string, len(row))
		for i, v := range row {
			if v == nil {
				cells[i] = "NULL"
			} else {
				cells[i] = fmt.Sprint(v)
			}
		}
		fmt.Println(strings.Join(cells, " | "))
	}
	fmt.Printf("total %.2fs\n", time.Since(t0).Seconds())
}
