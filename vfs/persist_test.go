package vfs

import (
	"bytes"
	"testing"
)

// TestSaveLoadRoundTrip builds a VFS subtree (including a file whose VFS mode
// has no read bit, as initdb produces), persists it with SaveSubtree, reloads
// it into a fresh VFS with LoadSubtree, and verifies every entry round-trips
// with its data intact. This guards against the bug where host files inherited
// a non-readable VFS mode and could not be read back.
func TestSaveLoadRoundTrip(t *testing.T) {
	src := New()
	src.MkdirAll("/data/base/1", 0o700)
	src.MkdirAll("/data/global", 0o750)
	src.MkdirAll("/data/pg_replslot", 0o700) // empty dir must survive
	src.WriteFile("/data/PG_VERSION", []byte("17\n"), 0o440)
	src.WriteFile("/data/postgresql.conf", []byte("# conf\n"), 0o600)
	src.WriteFile("/data/global/pg_control", []byte("ctrl"), 0o600)
	src.WriteFile("/data/base/1/2619", []byte("rel"), 0o600)
	// A node with NO permission bits — the exact case that broke reload.
	src.WriteFile("/data/base/1/noread", []byte("secret"), 0o000)

	host := t.TempDir() + "/persisted"
	if err := src.SaveSubtree("/data", host); err != nil {
		t.Fatalf("SaveSubtree: %v", err)
	}

	dst := New()
	dst.MkdirAll("/data", 0o700)
	if err := dst.LoadSubtree(host, "/data"); err != nil {
		t.Fatalf("LoadSubtree: %v", err)
	}

	want := map[string][]byte{
		"/data/PG_VERSION":        []byte("17\n"),
		"/data/postgresql.conf":   []byte("# conf\n"),
		"/data/global/pg_control": []byte("ctrl"),
		"/data/base/1/2619":       []byte("rel"),
		"/data/base/1/noread":     []byte("secret"),
	}
	for p, data := range want {
		n, err := dst.Stat(p)
		if err != nil {
			t.Errorf("after reload, missing %s: %v", p, err)
			continue
		}
		if !bytes.Equal(n.Data, data) {
			t.Errorf("%s: data = %q, want %q", p, n.Data, data)
		}
	}
	if _, err := dst.Stat("/data/pg_replslot"); err != nil {
		t.Errorf("empty dir /data/pg_replslot did not survive: %v", err)
	}
}
