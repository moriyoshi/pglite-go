//go:build !wazero

package pglite

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"sync"

	emcompat "github.com/moriyoshi/pglite-go/emscripten"
)

// session is a single, persistent `postgres --single` backend parked between
// statements in a blocking, channel-fed stdin. Because one backend process
// stays alive, session state — the current transaction, temp tables, SET,
// prepared statements — survives across calls, unlike spawning a backend per
// query.
//
// Coordination: the backend runs main() on its own goroutine (G1). fd_read on
// stdin is the authoritative "previous statement finished" signal — the backend
// only asks for input once it has flushed all output and printed its prompt. So
// Read (on G1) delivers the accumulated stdout to the caller (G2) and then
// blocks for the next statement. All stdout/stderr bookkeeping is G1-local;
// cross-goroutine handoff is via the channels, which establish happens-before.
type session struct {
	rt    *emcompat.WTRuntime
	inCh  chan []byte
	outCh chan []byte
	done  chan struct{}

	mainErr error

	// G1-local:
	pending   []byte // stdout accumulated since the last flush
	remainder []byte // stdin bytes not yet delivered to the backend

	errMu  sync.Mutex
	errBuf bytes.Buffer // stderr; guarded because Close may race the final write
}

func (db *DB) startSession() (*session, error) {
	ctx := context.Background()
	rt, err := emcompat.NewWTRuntimeFromModule(ctx, db.engine, db.postgresMod, db.fs, nil, nil)
	if err != nil {
		return nil, err
	}
	if err := rt.ApplyDataRelocs(ctx); err != nil {
		return nil, err
	}
	s := &session{
		rt:    rt,
		inCh:  make(chan []byte),
		outCh: make(chan []byte),
		done:  make(chan struct{}),
	}
	rt.SetStdinReader(s)
	rt.SetStdout(s)
	rt.SetStderr(&lockedWriter{s: s})

	go func() {
		args := []string{"/pglite/bin/postgres", "--single", "-D", dataDir, db.database}
		_, s.mainErr = rt.CallMain(ctx, args)
		close(s.done)
		close(s.outCh) // unblock any exec waiting on output
	}()

	// Drain the startup banner (the first flush).
	select {
	case <-s.outCh:
	case <-s.done:
		return nil, fmt.Errorf("backend exited during startup: %v", s.mainErr)
	}
	return s, nil
}

// Read implements the streaming stdin (runs on G1). See the type doc.
func (s *session) Read(p []byte) (int, error) {
	if len(s.remainder) == 0 {
		out := s.pending
		s.pending = nil
		s.outCh <- out // signal: previous statement's output is complete
		in, ok := <-s.inCh
		if !ok {
			return 0, io.EOF // Close was called
		}
		s.remainder = in
	}
	n := copy(p, s.remainder)
	s.remainder = s.remainder[n:]
	return n, nil
}

// Write implements the streaming stdout (runs on G1).
func (s *session) Write(p []byte) (int, error) {
	s.pending = append(s.pending, p...)
	return len(p), nil
}

type lockedWriter struct{ s *session }

func (w *lockedWriter) Write(p []byte) (int, error) {
	w.s.errMu.Lock()
	defer w.s.errMu.Unlock()
	return w.s.errBuf.Write(p)
}

// exec sends one SQL string (which may contain multiple ;-separated statements)
// to the backend and returns its combined stdout plus any stderr emitted.
func (s *session) exec(sql string) (stdout []byte, stderr string, err error) {
	if !strings.HasSuffix(sql, "\n") {
		sql += "\n"
	}
	s.errMu.Lock()
	errBefore := s.errBuf.Len()
	s.errMu.Unlock()

	select {
	case s.inCh <- []byte(sql):
	case <-s.done:
		return nil, "", fmt.Errorf("pglite: backend exited: %v", s.mainErr)
	}
	out, ok := <-s.outCh
	if !ok {
		return nil, "", fmt.Errorf("pglite: backend exited: %v", s.mainErr)
	}

	s.errMu.Lock()
	newErr := string(s.errBuf.Bytes()[errBefore:])
	s.errMu.Unlock()
	return out, newErr, nil
}

// close ends the session: EOF on stdin makes the backend exit.
func (s *session) close() error {
	close(s.inCh)
	<-s.done
	return nil
}
