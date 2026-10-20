package store

import (
	"path/filepath"
	"testing"
	"time"
)

func TestPutGetAndExpiry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "cache.db") // the data dir may not exist yet
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	type v struct{ A int }
	if err := s.Put("k", v{7}); err != nil {
		t.Fatal(err)
	}
	var got v
	if !s.Get("k", time.Hour, &got) || got.A != 7 {
		t.Fatalf("fresh entry not returned: %+v", got)
	}
	if s.Get("k", -time.Second, &got) {
		t.Error("expired entry returned")
	}
	if s.Get("missing", time.Hour, &got) {
		t.Error("missing key returned")
	}
	// Survives a reopen: the point of using SQLite over a map.
	s.Close()
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if !s.Get("k", time.Hour, &got) {
		t.Error("entry lost after reopen")
	}
}
