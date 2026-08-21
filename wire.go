package pglite

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"os"
	"strconv"
	"strings"

	emcompat "github.com/moriyoshi/pglite-go/emscripten"
)

// wireConn drives PGlite's PostgreSQL backend over the v3 wire protocol
// (pgl_set_rw_cbs + PostgresMainLoopOnce), the same mechanism PGlite's own
// JS runtime uses. One persistent backend serves every query, so it yields
// typed results (RowDescription OIDs), command tags/affected rows, and full
// session/transaction state.
//
// Driving model (no Asyncify): a wire message is staged in `out`; the backend's
// non-blocking read callback drains it while PostgresMainLoopOnce is pumped
// until the input is consumed and no buffered data remains; the backend's writes
// are captured in `in` and decoded.
type wireConn struct {
	rt  emcompat.Runtime
	out []byte // client->server staging
	off int
	in  []byte // server->client capture
	tx  byte   // last ReadyForQuery status: 'I' idle, 'T' in tx, 'E' failed tx
}

var defaultStartParams = []string{
	"--single", "-F", "-O", "-j",
	"-c", "search_path=public",
	"-c", "exit_on_error=false",
	"-c", "log_checkpoints=false",
	// PG18 defaults io_method=worker: seq scans dispatch async buffer reads and
	// wait on an I/O worker to complete them. This single-process wasm embedding
	// has no I/O workers, so that wait never returns — any scan that reads a page
	// from disk (e.g. a user table after a warm restart) hangs forever. Force
	// synchronous reads. (PG17 and earlier had no async I/O, hence no hang there.)
	"-c", "io_method=sync",
}

func (w *wireConn) read(dst []byte) int { n := copy(dst, w.out[w.off:]); w.off += n; return n }
func (w *wireConn) write(src []byte)    { w.in = append(w.in, src...) }

func (db *DB) startWire() (*wireConn, error) {
	ctx := context.Background()
	rt, err := newRuntimeFromModule(ctx, db.engine, db.postgresMod, db.fs, nil, nil)
	if err != nil {
		return nil, err
	}
	if err := rt.ApplyDataRelocs(ctx); err != nil {
		return nil, err
	}
	// The wire backend talks to us over the rw callbacks; its fd 1/2 chatter
	// (banner, server log) is noise for a library — discard it.
	rt.SetStdout(io.Discard)
	if os.Getenv("PGLITE_DEBUG_INITDB") != "" {
		rt.SetStderr(os.Stderr)
	} else {
		rt.SetStderr(io.Discard)
	}
	w := &wireConn{rt: rt}

	// Wire mode loads pg_hba.conf (single-user never does). The default file's
	// 127.0.0.1/32 CIDRs need getaddrinfo (stubbed out here), so use IP-free
	// "all" trust entries — fine for an embedded DB with no real network.
	db.fs.WriteFile(dataDir+"/pg_hba.conf", []byte("local all all trust\nhost all all all trust\n"), 0o600)

	base, err := rt.RegisterRWCallbacks(emcompat.RWCallbacks{Read: w.read, Write: w.write})
	if err != nil {
		return nil, err
	}
	if _, err := rt.CallExport("pgl_set_rw_cbs", int32(base), int32(base+1)); err != nil {
		return nil, err
	}
	if _, err := rt.CallExport("pgl_setPGliteActive", int32(1)); err != nil {
		return nil, err
	}
	args := append([]string{"/pglite/bin/postgres"}, defaultStartParams...)
	args = append(args, "-D", dataDir, db.database)
	// In PGlite mode main() completes startup then "returns" via exit/longjmp
	// while the backend stays alive — so a non-nil error here is expected, not
	// fatal. If the backend were actually dead, pgl_startPGlite and the handshake
	// below would fail.
	rt.CallMain(ctx, args)
	if _, err := rt.CallExport("pgl_startPGlite"); err != nil {
		return nil, fmt.Errorf("pgl_startPGlite: %w", err)
	}
	// Wire-connection startup handshake.
	if _, err := w.exec(startupMessage("postgres", db.database)); err != nil {
		return nil, fmt.Errorf("startup handshake: %w", err)
	}
	return w, nil
}

// exec feeds one wire message and pumps the backend until it is consumed,
// returning the captured backend output.
func (w *wireConn) exec(msg []byte) ([]byte, error) {
	w.out = msg
	w.off = 0
	w.in = nil
	if len(msg) > 0 && msg[0] == 0 { // StartupMessage has no type byte
		port, err := w.rt.CallExport("pgl_getMyProcPort")
		if err != nil {
			return nil, err
		}
		if _, err := w.rt.CallExport("ProcessStartupPacket", asI32(port), int32(1), int32(1)); err != nil {
			return nil, err
		}
		return w.in, nil
	}
	for {
		if w.off >= len(msg) {
			rem, err := w.rt.CallExport("pq_buffer_remaining_data")
			if err != nil {
				return nil, err
			}
			if asI32(rem) == 0 {
				break
			}
		}
		_, longjmp, err := w.rt.CallExportRaw("PostgresMainLoopOnce")
		if longjmp {
			if _, err := w.rt.CallExport("PostgresMainLongJmp"); err != nil {
				return nil, err
			}
			continue
		}
		if err != nil {
			return nil, err
		}
	}
	w.rt.CallExport("PostgresSendReadyForQueryIfNecessary")
	w.rt.CallExport("pgl_pq_flush")
	return w.in, nil
}

// simpleQuery runs sql via the simple query protocol ('Q').
func (w *wireConn) simpleQuery(sql string) (*Rows, error) {
	out, err := w.exec(queryMessage(sql))
	if err != nil {
		return nil, err
	}
	return w.parse(out)
}

// extendedQuery runs sql with bind parameters via the extended protocol
// (Parse/Bind/Describe/Execute/Sync), giving real server-side prepared
// statements. Params are sent in text format; the server infers their types.
func (w *wireConn) extendedQuery(sql string, params []string, isNull []bool) (*Rows, error) {
	// Pump 1: Parse + Describe the statement to learn its result column OIDs, so
	// we can request binary format for the columns we can decode. The unnamed
	// statement survives the Sync and is reused by the Bind below.
	var m1 []byte
	m1 = append(m1, parseMessage(sql)...)
	m1 = append(m1, describeStatementMessage()...)
	m1 = append(m1, syncMessage()...)
	d1, err := w.exec(m1)
	if err != nil {
		return nil, err
	}
	oids, errText := describeResultOIDs(d1)
	if errText != "" {
		return nil, fmt.Errorf("%s", errText)
	}

	// Pump 2: Bind (requesting per-column result formats) + Execute.
	var m2 []byte
	m2 = append(m2, bindMessage(params, isNull, resultFormats(oids))...)
	m2 = append(m2, describePortalMessage()...)
	m2 = append(m2, executeMessage()...)
	m2 = append(m2, syncMessage()...)
	d2, err := w.exec(m2)
	if err != nil {
		return nil, err
	}
	return w.parse(d2)
}

func (w *wireConn) close() {
	// Best-effort graceful shutdown: deactivate + Terminate.
	w.rt.CallExport("pgl_setPGliteActive", int32(0))
	w.exec([]byte{'X', 0, 0, 0, 4})
}

// --- message builders ---

func startupMessage(user, database string) []byte {
	var body []byte
	body = binary.BigEndian.AppendUint32(body, 196608) // protocol 3.0
	body = append(body, "user\x00"...)
	body = append(body, append([]byte(user), 0)...)
	body = append(body, "database\x00"...)
	body = append(body, append([]byte(database), 0)...)
	body = append(body, 0)
	return append(binary.BigEndian.AppendUint32(nil, uint32(len(body)+4)), body...)
}

func queryMessage(sql string) []byte { return frame('Q', append([]byte(sql), 0)) }

func parseMessage(sql string) []byte {
	var b []byte
	b = append(b, 0) // unnamed statement
	b = append(b, append([]byte(sql), 0)...)
	b = binary.BigEndian.AppendUint16(b, 0) // 0 parameter type OIDs (infer)
	return frame('P', b)
}

func bindMessage(params []string, isNull []bool, formats []int16) []byte {
	var b []byte
	b = append(b, 0)                        // unnamed portal
	b = append(b, 0)                        // unnamed statement
	b = binary.BigEndian.AppendUint16(b, 0) // 0 param format codes => all text
	b = binary.BigEndian.AppendUint16(b, uint16(len(params)))
	for i, p := range params {
		if i < len(isNull) && isNull[i] {
			b = binary.BigEndian.AppendUint32(b, 0xFFFFFFFF) // NULL
			continue
		}
		b = binary.BigEndian.AppendUint32(b, uint32(len(p)))
		b = append(b, p...)
	}
	// Per-column result format codes (0 text, 1 binary).
	b = binary.BigEndian.AppendUint16(b, uint16(len(formats)))
	for _, f := range formats {
		b = binary.BigEndian.AppendUint16(b, uint16(f))
	}
	return frame('B', b)
}

// binaryOID lists the type OIDs we request in binary format and can decode
// losslessly. Everything else stays text (and is returned as a string unless
// decodeText knows a typed form).
var binaryOID = map[uint32]bool{
	16:  true, // bool
	17:  true, // bytea
	20:  true, // int8
	21:  true, // int2
	23:  true, // int4
	26:  true, // oid
	700: true, // float4
	701: true, // float8
}

func resultFormats(oids []uint32) []int16 {
	f := make([]int16, len(oids))
	for i, oid := range oids {
		if binaryOID[oid] {
			f[i] = 1
		}
	}
	return f
}

func describeStatementMessage() []byte { return frame('D', append([]byte{'S'}, 0)) }
func describePortalMessage() []byte    { return frame('D', append([]byte{'P'}, 0)) }

// describeResultOIDs extracts result column OIDs from a Parse+Describe response.
func describeResultOIDs(buf []byte) (oids []uint32, errText string) {
	for len(buf) >= 5 {
		typ := buf[0]
		n := binary.BigEndian.Uint32(buf[1:5])
		if int(n)+1 > len(buf) {
			break
		}
		payload := buf[5 : 1+n]
		switch typ {
		case 'T':
			_, oids, _ = decodeRowDesc(payload)
		case 'E':
			errText = decodeError(payload)
		}
		buf = buf[1+n:]
	}
	return
}
func executeMessage() []byte { return frame('E', append([]byte{0}, 0, 0, 0, 0)) } // portal="", maxRows=0
func syncMessage() []byte    { return frame('S', nil) }

// frame prefixes a payload with a message type byte and int32 length.
func frame(typ byte, payload []byte) []byte {
	msg := []byte{typ}
	msg = binary.BigEndian.AppendUint32(msg, uint32(len(payload)+4))
	return append(msg, payload...)
}

// --- response decoding ---

func (w *wireConn) parse(buf []byte) (*Rows, error) {
	r := &Rows{}
	var perr string
	var oids []uint32
	var formats []int16
	for len(buf) >= 5 {
		typ := buf[0]
		n := binary.BigEndian.Uint32(buf[1:5])
		if int(n)+1 > len(buf) {
			break
		}
		payload := buf[5 : 1+n]
		switch typ {
		case 'T': // RowDescription
			r.Columns, oids, formats = decodeRowDesc(payload)
			r.TypeOIDs = oids
		case 'D': // DataRow
			r.Rows = append(r.Rows, decodeDataRow(payload, oids, formats))
		case 'C': // CommandComplete
			tag := strings.TrimRight(string(payload), "\x00")
			r.Command = tag
			r.AffectedRows = affectedFromTag(tag)
		case 'E': // ErrorResponse
			perr = decodeError(payload)
		case 'Z': // ReadyForQuery
			if len(payload) > 0 {
				w.tx = payload[0]
			}
		}
		buf = buf[1+n:]
	}
	if perr != "" {
		return nil, fmt.Errorf("%s", perr)
	}
	return r, nil
}

func decodeRowDesc(p []byte) (names []string, oids []uint32, formats []int16) {
	num := int(binary.BigEndian.Uint16(p))
	p = p[2:]
	for i := 0; i < num; i++ {
		z := indexZero(p)
		names = append(names, string(p[:z]))
		p = p[z+1:]
		// tableOID(4) colNo(2) typeOID(4) typLen(2) typMod(4) format(2)
		oids = append(oids, binary.BigEndian.Uint32(p[6:10]))
		formats = append(formats, int16(binary.BigEndian.Uint16(p[16:18])))
		p = p[18:]
	}
	return
}

func decodeDataRow(p []byte, oids []uint32, formats []int16) []any {
	num := int(binary.BigEndian.Uint16(p))
	p = p[2:]
	row := make([]any, num)
	for i := 0; i < num; i++ {
		l := int32(binary.BigEndian.Uint32(p))
		p = p[4:]
		if l < 0 {
			row[i] = nil // NULL
			continue
		}
		raw := p[:l]
		p = p[l:]
		var oid uint32
		if i < len(oids) {
			oid = oids[i]
		}
		if i < len(formats) && formats[i] == 1 {
			row[i] = decodeBinary(oid, raw)
		} else {
			row[i] = decodeText(oid, raw)
		}
	}
	return row
}

// decodeText decodes a text-format value to a typed Go value where the OID is
// known, else returns it as a string.
func decodeText(oid uint32, raw []byte) any {
	s := string(raw)
	switch oid {
	case 16: // bool
		return s == "t" || s == "true"
	case 20, 21, 23, 26: // int8/int2/int4/oid
		if v, err := strconv.ParseInt(s, 10, 64); err == nil {
			return v
		}
	case 700, 701: // float4/float8
		if v, err := strconv.ParseFloat(s, 64); err == nil {
			return v
		}
	case 17: // bytea, rendered as \x hex
		if b, ok := decodeByteaText(s); ok {
			return b
		}
	}
	return s
}

// decodeBinary decodes a binary-format value (only OIDs in binaryOID reach here).
func decodeBinary(oid uint32, raw []byte) any {
	switch oid {
	case 16: // bool
		return len(raw) > 0 && raw[0] != 0
	case 21: // int2
		if len(raw) == 2 {
			return int64(int16(binary.BigEndian.Uint16(raw)))
		}
	case 23: // int4
		if len(raw) == 4 {
			return int64(int32(binary.BigEndian.Uint32(raw)))
		}
	case 20: // int8
		if len(raw) == 8 {
			return int64(binary.BigEndian.Uint64(raw))
		}
	case 26: // oid (unsigned 32)
		if len(raw) == 4 {
			return int64(binary.BigEndian.Uint32(raw))
		}
	case 700: // float4
		if len(raw) == 4 {
			return float64(math.Float32frombits(binary.BigEndian.Uint32(raw)))
		}
	case 701: // float8
		if len(raw) == 8 {
			return math.Float64frombits(binary.BigEndian.Uint64(raw))
		}
	case 17: // bytea
		b := make([]byte, len(raw))
		copy(b, raw)
		return b
	}
	return string(raw)
}

func decodeByteaText(s string) ([]byte, bool) {
	if !strings.HasPrefix(s, `\x`) {
		return nil, false
	}
	h := s[2:]
	if len(h)%2 != 0 {
		return nil, false
	}
	b := make([]byte, len(h)/2)
	for i := range b {
		hi, ok1 := hexVal(h[2*i])
		lo, ok2 := hexVal(h[2*i+1])
		if !ok1 || !ok2 {
			return nil, false
		}
		b[i] = hi<<4 | lo
	}
	return b, true
}

func hexVal(c byte) (byte, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, true
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, true
	}
	return 0, false
}

func decodeError(p []byte) string {
	var sev, msg string
	for len(p) > 0 && p[0] != 0 {
		code := p[0]
		p = p[1:]
		z := indexZero(p)
		val := string(p[:z])
		p = p[z+1:]
		switch code {
		case 'S':
			sev = val
		case 'M':
			msg = val
		}
	}
	if sev != "" {
		return sev + ": " + msg
	}
	return msg
}

// affectedFromTag extracts the row count from a CommandComplete tag such as
// "INSERT 0 3", "UPDATE 5", "DELETE 2", or "SELECT 3".
func affectedFromTag(tag string) int64 {
	fields := strings.Fields(tag)
	if len(fields) == 0 {
		return 0
	}
	if n, err := strconv.ParseInt(fields[len(fields)-1], 10, 64); err == nil {
		return n
	}
	return 0
}

func indexZero(p []byte) int {
	for i, b := range p {
		if b == 0 {
			return i
		}
	}
	return len(p)
}

func asI32(v interface{}) int32 {
	if i, ok := v.(int32); ok {
		return i
	}
	return 0
}
