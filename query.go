//go:build !wazero

package pglite

// Rows is the structured result of a query, decoded from the PostgreSQL wire
// protocol.
type Rows struct {
	Columns  []string // column names, in order
	TypeOIDs []uint32 // PostgreSQL type OID per column
	// Rows is row-major, one []any per row. Each value is a decoded Go value —
	// int64, float64, bool, []byte, string, or nil (SQL NULL) — chosen by the
	// column's type OID. Common numeric/bool/bytea types are received in binary
	// format (extended-protocol queries); everything else arrives as text and is
	// returned as a string.
	Rows         [][]any
	Command      string // CommandComplete tag, e.g. "INSERT 0 1", "SELECT 3"
	AffectedRows int64  // rows affected/returned, parsed from Command
}

// Query runs sql via the simple query protocol on the persistent backend and
// returns its rows. Session state (open transaction, temp tables, SET) persists
// across calls, so BEGIN in one Query and COMMIT in a later one form a single
// transaction.
func (db *DB) Query(sql string) (*Rows, error) {
	db.mu.Lock()
	defer db.mu.Unlock()
	return db.wire.simpleQuery(sql)
}

// QueryParams runs sql with bind parameters via the extended query protocol
// (a real server-side prepared statement). Values are given as their text
// encodings; a true entry in isNull sends SQL NULL for that position.
func (db *DB) QueryParams(sql string, params []string, isNull []bool) (*Rows, error) {
	db.mu.Lock()
	defer db.mu.Unlock()
	return db.wire.extendedQuery(sql, params, isNull)
}

// Exec runs sql for its side effects and returns the command tag (e.g.
// "INSERT 0 1"). Use the Rows.AffectedRows from Query for a numeric count.
func (db *DB) Exec(sql string) (string, error) {
	r, err := db.Query(sql)
	if err != nil {
		return "", err
	}
	return r.Command, nil
}
