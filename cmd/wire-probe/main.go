//go:build !wazero

// Command wire-probe prototypes the PGlite wire-protocol path (pgl_set_rw_cbs +
// PostgresMainLoopOnce) end to end: startup handshake, then a couple of queries,
// printing the decoded backend messages. Requires a persisted cluster (run
// ./cmd/pglite once first).
package main

import (
	"context"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/bytecodealliance/wasmtime-go/v34"

	emcompat "github.com/moriyoshi/pglite-go/emscripten"
	"github.com/moriyoshi/pglite-go/vfs"
)

const dataDir = "/tmp/pglite/data"

var defaultStartParams = []string{
	"--single", "-F", "-O", "-j",
	"-c", "search_path=public",
	"-c", "exit_on_error=false",
	"-c", "log_checkpoints=false",
}

type wireIO struct {
	out []byte // client->server (message being fed)
	off int
	in  []byte // server->client (captured)
}

func (w *wireIO) Read(dst []byte) int { n := copy(dst, w.out[w.off:]); w.off += n; return n }
func (w *wireIO) Write(src []byte)    { w.in = append(w.in, src...) }

func main() {
	wasmDir := "wasm"
	if len(os.Args) > 1 {
		wasmDir = os.Args[1]
	}
	ctx := context.Background()

	fs := vfs.New()
	if err := fs.LoadManifest(wasmDir+"/pglite.manifest.json", wasmDir+"/pglite.data"); err != nil {
		panic(err)
	}
	fs.MkdirAll(dataDir, 0o700)
	fs.MkdirAll("/dev", 0o755)
	fs.WriteFile("/dev/null", nil, 0o666)
	fs.WriteFile("/dev/urandom", nil, 0o666)
	fs.MkdirAll("/home/web_user", 0o755)

	cache, _ := os.UserCacheDir()
	persist := filepath.Join(cache, "pglite-go", "data")
	if _, err := os.Stat(persist + "/PG_VERSION"); err != nil {
		fmt.Fprintln(os.Stderr, "no persisted cluster; run ./cmd/pglite first")
		os.Exit(1)
	}
	if err := fs.LoadSubtree(persist, dataDir); err != nil {
		panic(err)
	}
	// Wire mode loads pg_hba.conf (single-user never does). The default file has
	// 127.0.0.1/32 CIDR entries whose parsing needs getaddrinfo (stubbed out in
	// our host layer). Replace with IP-free "all" entries — fine for an embedded
	// DB with no real network.
	fs.WriteFile(dataDir+"/pg_hba.conf", []byte("local all all trust\nhost all all all trust\n"), 0o600)

	wasm, _ := os.ReadFile(wasmDir + "/pglite.wasm")
	engine := wasmtime.NewEngine()
	mod, err := wasmtime.NewModule(engine, wasm)
	if err != nil {
		panic(err)
	}
	rt, err := emcompat.NewWTRuntimeFromModule(ctx, engine, mod, fs, nil, nil)
	if err != nil {
		panic(err)
	}
	if err := rt.ApplyDataRelocs(ctx); err != nil {
		panic(err)
	}

	io := &wireIO{}
	base, err := rt.RegisterRWCallbacks(emcompat.RWCallbacks{Read: io.Read, Write: io.Write})
	if err != nil {
		panic(err)
	}
	must := func(name string, args ...interface{}) {
		if _, err := rt.CallExport(name, args...); err != nil {
			panic(fmt.Errorf("%s: %w", name, err))
		}
	}
	must("pgl_set_rw_cbs", int32(base), int32(base+1))
	must("pgl_setPGliteActive", int32(1))
	if _, err := rt.CallMain(ctx, append([]string{"/pglite/bin/postgres"}, append(append([]string{}, defaultStartParams...), "-D", dataDir, "template1")...)); err != nil {
		fmt.Fprintln(os.Stderr, "callMain:", err)
	}
	must("pgl_startPGlite")
	fmt.Println("backend started in wire mode")

	// Startup handshake.
	resp := execProtocol(rt, io, startupMessage("postgres", "template1"))
	fmt.Println("== startup ==")
	printMessages(resp)

	for _, q := range []string{
		"SELECT 1 + 1 AS result, 'hi'::text AS greeting, NULL::int8 AS n;",
		"CREATE TEMP TABLE t (id int);",
		"INSERT INTO t VALUES (1),(2),(3);",
		"SELECT count(*) FROM t;",
	} {
		fmt.Printf("\n== %s ==\n", q)
		printMessages(execProtocol(rt, io, queryMessage(q)))
	}
}

// execProtocol feeds one wire message and pumps the backend until it's consumed.
func execProtocol(rt *emcompat.WTRuntime, io *wireIO, msg []byte) []byte {
	io.out = msg
	io.off = 0
	io.in = nil
	if len(msg) > 0 && msg[0] == 0 { // StartupMessage: no type byte
		port, _ := rt.CallExport("pgl_getMyProcPort")
		rt.CallExport("ProcessStartupPacket", asI32(port), int32(1), int32(1))
		return io.in
	}
	for {
		remaining := int32(0)
		if io.off < len(msg) {
			// still have input to deliver
		} else {
			r, _ := rt.CallExport("pq_buffer_remaining_data")
			remaining = asI32(r)
			if remaining == 0 {
				break
			}
		}
		_, longjmp, err := rt.CallExportRaw("PostgresMainLoopOnce")
		if longjmp {
			rt.CallExport("PostgresMainLongJmp")
		} else if err != nil {
			fmt.Fprintln(os.Stderr, "MainLoopOnce:", err)
			break
		}
	}
	rt.CallExport("PostgresSendReadyForQueryIfNecessary")
	rt.CallExport("pgl_pq_flush")
	return io.in
}

func asI32(v interface{}) int32 {
	if i, ok := v.(int32); ok {
		return i
	}
	return 0
}

// --- wire message builders ---

func startupMessage(user, database string) []byte {
	var body []byte
	body = binary.BigEndian.AppendUint32(body, 196608) // protocol 3.0
	body = append(body, "user\x00"...)
	body = append(body, append([]byte(user), 0)...)
	body = append(body, "database\x00"...)
	body = append(body, append([]byte(database), 0)...)
	body = append(body, 0) // terminator
	msg := binary.BigEndian.AppendUint32(nil, uint32(len(body)+4))
	return append(msg, body...)
}

func queryMessage(sql string) []byte {
	body := append([]byte(sql), 0)
	msg := []byte{'Q'}
	msg = binary.BigEndian.AppendUint32(msg, uint32(len(body)+4))
	return append(msg, body...)
}

// --- response decoding ---

func printMessages(buf []byte) {
	for len(buf) >= 5 {
		typ := buf[0]
		n := binary.BigEndian.Uint32(buf[1:5])
		if int(n)+1 > len(buf) {
			break
		}
		payload := buf[5 : 1+n]
		switch typ {
		case 'T':
			cols := decodeRowDesc(payload)
			names := make([]string, len(cols))
			for i, c := range cols {
				names[i] = fmt.Sprintf("%s:oid%d", c.name, c.oid)
			}
			fmt.Println("  RowDescription:", strings.Join(names, ", "))
		case 'D':
			fmt.Println("  DataRow:", decodeDataRow(payload))
		case 'C':
			fmt.Printf("  CommandComplete: %q\n", strings.TrimRight(string(payload), "\x00"))
		case 'E':
			fmt.Printf("  ErrorResponse: %s\n", decodeError(payload))
		case 'Z':
			fmt.Printf("  ReadyForQuery(%c)\n", payload[0])
		case 'I':
			fmt.Println("  EmptyQueryResponse")
		case 'R', 'S', 'K', 'N':
			// auth/paramstatus/backendkey/notice — ignore in the probe
		default:
			fmt.Printf("  msg %c (len %d)\n", typ, n)
		}
		buf = buf[1+n:]
	}
}

type col struct {
	name string
	oid  uint32
}

func decodeRowDesc(p []byte) []col {
	num := int(binary.BigEndian.Uint16(p))
	p = p[2:]
	cols := make([]col, 0, num)
	for i := 0; i < num; i++ {
		z := 0
		for p[z] != 0 {
			z++
		}
		name := string(p[:z])
		p = p[z+1:]
		oid := binary.BigEndian.Uint32(p[6:10]) // skip tableOID(4)+colno(2)
		p = p[18:]                              // tableOID4 colno2 typeOID4 typlen2 typmod4 format2
		cols = append(cols, col{name, oid})
	}
	return cols
}

func decodeDataRow(p []byte) []string {
	num := int(binary.BigEndian.Uint16(p))
	p = p[2:]
	out := make([]string, 0, num)
	for i := 0; i < num; i++ {
		l := int32(binary.BigEndian.Uint32(p))
		p = p[4:]
		if l < 0 {
			out = append(out, "NULL")
			continue
		}
		out = append(out, string(p[:l]))
		p = p[l:]
	}
	return out
}

func decodeError(p []byte) string {
	parts := []string{}
	for len(p) > 0 && p[0] != 0 {
		code := p[0]
		p = p[1:]
		z := 0
		for p[z] != 0 {
			z++
		}
		if code == 'M' || code == 'S' || code == 'C' {
			parts = append(parts, string(p[:z]))
		}
		p = p[z+1:]
	}
	return strings.Join(parts, " ")
}
