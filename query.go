//go:build !wazero

package pglite

import (
	"bufio"
	"bytes"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Rows is the structured result of a query.
type Rows struct {
	Columns  []string  // column names, in order
	TypeOIDs []uint32  // PostgreSQL type OID per column
	Rows     [][]*string // row-major values; nil element = SQL NULL
	Command  string    // last command tag if reported (e.g. "INSERT 0 1")
}

// Raw sends sql to the persistent backend and returns its raw stdout. Exposed
// for debugging/calibration of the result parser.
func (db *DB) Raw(sql string) ([]byte, error) {
	db.mu.Lock()
	defer db.mu.Unlock()
	out, _, err := db.sess.exec(sql)
	return out, err
}

// Query runs sql on the persistent backend and returns its rows. Session state
// (open transactions, temp tables, SET) persists across calls, so BEGIN in one
// Query and COMMIT in a later one form a single transaction.
func (db *DB) Query(sql string) (*Rows, error) {
	db.mu.Lock()
	defer db.mu.Unlock()
	return db.queryLocked(sql)
}

func (db *DB) queryLocked(sql string) (*Rows, error) {
	out, errText, err := db.sess.exec(sql)
	if err != nil {
		return nil, err
	}
	// PostgreSQL errors go to stderr in single-user mode; some also echo on
	// stdout. Check both.
	if perr := parseError([]byte(errText)); perr != "" {
		return nil, fmt.Errorf("%s", perr)
	}
	if perr := parseError(out); perr != "" {
		return nil, fmt.Errorf("%s", perr)
	}
	return parseSingleUser(out), nil
}

// Exec runs sql for its side effects and returns the command tag if the backend
// reported one (empty otherwise).
func (db *DB) Exec(sql string) (string, error) {
	r, err := db.Query(sql)
	if err != nil {
		return "", err
	}
	return r.Command, nil
}

// errRe matches a PostgreSQL ERROR/FATAL/PANIC line the backend emits.
var errRe = regexp.MustCompile(`(?m)^(?:\d{4}-\d\d-\d\d[^\n]*?\b(ERROR|FATAL|PANIC)\b:|(ERROR|FATAL|PANIC):)\s*(.*)$`)

func parseError(out []byte) string {
	if m := errRe.FindSubmatch(out); m != nil {
		return strings.TrimSpace(string(m[len(m)-1]))
	}
	return ""
}

type attr struct {
	idx   int
	oid   uint32
	name  string // set on descriptor lines
	value string // set on value lines
	isVal bool
}

// parseSingleUser parses postgres --single "debugtup" output into rows.
//
// The backend prints attribute lines "\t<idx>: <name>\t(typeid = <oid>, ...)"
// for the descriptor, then "\t<idx>:  = <value>\t(typeid = <oid>, ...)" per
// tuple, with blocks separated by "\t----". NULL attributes are omitted from a
// tuple entirely, so values are keyed by <idx> within each block.
func parseSingleUser(out []byte) *Rows {
	r := &Rows{}
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 0, 64*1024), 64*1024*1024)

	var blocks [][]attr
	var cur []attr
	for sc.Scan() {
		line := strings.TrimPrefix(sc.Text(), "backend> ")
		if strings.TrimSpace(line) == "----" {
			blocks = append(blocks, cur)
			cur = nil
			continue
		}
		if a, ok := parseAttrLine(line); ok {
			cur = append(cur, a)
		}
	}
	if len(blocks) == 0 {
		return r
	}
	// Block 0 is the descriptor (column names + OIDs).
	ncol := 0
	for _, a := range blocks[0] {
		if a.idx > ncol {
			ncol = a.idx
		}
	}
	r.Columns = make([]string, ncol)
	r.TypeOIDs = make([]uint32, ncol)
	for _, a := range blocks[0] {
		if a.idx >= 1 && a.idx <= ncol {
			r.Columns[a.idx-1] = a.name
			r.TypeOIDs[a.idx-1] = a.oid
		}
	}
	// Remaining blocks are tuples; missing indices are NULL (nil).
	for _, blk := range blocks[1:] {
		row := make([]*string, ncol)
		for _, a := range blk {
			if a.isVal && a.idx >= 1 && a.idx <= ncol {
				v := a.value
				row[a.idx-1] = &v
			}
		}
		r.Rows = append(r.Rows, row)
	}
	return r
}

var oidRe = regexp.MustCompile(`typeid = (\d+)`)

// parseAttrLine parses one "\t<idx>: ...\t(typeid = <oid>, ...)" line.
func parseAttrLine(line string) (attr, bool) {
	fields := strings.Split(line, "\t")
	metaIdx := -1
	for i, f := range fields {
		if strings.HasPrefix(strings.TrimSpace(f), "(typeid") {
			metaIdx = i
		}
	}
	if metaIdx < 2 {
		return attr{}, false
	}
	head := strings.Join(fields[1:metaIdx], "\t") // value may contain tabs
	colon := strings.Index(head, ":")
	if colon < 0 {
		return attr{}, false
	}
	idx, err := strconv.Atoi(strings.TrimSpace(head[:colon]))
	if err != nil {
		return attr{}, false
	}
	a := attr{idx: idx}
	if m := oidRe.FindStringSubmatch(fields[metaIdx]); m != nil {
		if v, e := strconv.ParseUint(m[1], 10, 32); e == nil {
			a.oid = uint32(v)
		}
	}
	rest := head[colon+1:]
	if eq := strings.Index(rest, "="); eq >= 0 {
		a.isVal = true
		a.value = decodeValue(strings.TrimSpace(rest[eq+1:]))
	} else {
		a.name = strings.TrimSpace(rest)
	}
	return a, true
}

// decodeValue strips the surrounding quotes debugtup adds around values.
func decodeValue(tok string) string {
	if len(tok) >= 2 && tok[0] == '"' && tok[len(tok)-1] == '"' {
		return tok[1 : len(tok)-1]
	}
	return tok
}
