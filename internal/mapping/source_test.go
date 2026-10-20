package mapping

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/easly1989/animeworldtv-indexer/internal/store"
)

type fixture struct {
	srv   *httptest.Server
	hits  atomic.Int32
	fail  atomic.Bool
	store *store.Store
	now   time.Time
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	body, err := os.ReadFile("testdata/anime-list-sample.json")
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{now: time.Now()}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		f.hits.Add(1)
		if f.fail.Load() {
			http.Error(w, "down", http.StatusBadGateway)
			return
		}
		_, _ = w.Write(body)
	}))
	t.Cleanup(f.srv.Close)
	f.store, err = store.Open(filepath.Join(t.TempDir(), "c.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.store.Close() })
	return f
}

func (f *fixture) source() *Source {
	s := NewSource(f.srv.URL, f.store, 24*time.Hour)
	s.now = func() time.Time { return f.now }
	return s
}

func TestSourceDownloadsOnceAndCaches(t *testing.T) {
	f := newFixture(t)
	s := f.source()
	if _, ok := s.Index(t.Context()).ByMAL(52991); !ok {
		t.Fatal("Frieren missing after download")
	}
	s.Index(t.Context())
	if f.hits.Load() != 1 {
		t.Errorf("want 1 download, got %d", f.hits.Load())
	}
	// A new process (new Source, same database) reads the cache instead of downloading.
	if _, ok := f.source().Index(t.Context()).ByMAL(52991); !ok || f.hits.Load() != 1 {
		t.Errorf("restart did not use the cache (downloads: %d)", f.hits.Load())
	}
}

func TestSourceRefreshesAfterTTL(t *testing.T) {
	f := newFixture(t)
	s := f.source()
	s.ttl = time.Nanosecond // memory and database copies both count as expired once the clock moves
	s.Index(t.Context())
	f.now = f.now.Add(time.Hour)
	s.Index(t.Context())
	if f.hits.Load() != 2 {
		t.Errorf("want a refresh download after the TTL, got %d downloads", f.hits.Load())
	}
}

func TestSourceKeepsServingStaleDataWhenRefreshFails(t *testing.T) {
	f := newFixture(t)
	s := f.source()
	s.ttl = time.Nanosecond
	s.Index(t.Context())
	f.fail.Store(true)
	f.now = f.now.Add(time.Hour)
	if _, ok := s.Index(t.Context()).ByMAL(52991); !ok {
		t.Error("stale index not served after a failed refresh")
	}
	n := f.hits.Load()
	s.Index(t.Context()) // inside the retry back-off: no new request
	if f.hits.Load() != n {
		t.Error("failed refresh retried immediately")
	}
}

func TestSourceWithoutDataIsEmptyNotNil(t *testing.T) {
	f := newFixture(t)
	f.fail.Store(true)
	idx := f.source().Index(t.Context())
	if idx == nil {
		t.Fatal("Index must never be nil")
	}
	if _, ok := idx.ByMAL(52991); ok {
		t.Error("expected an empty index")
	}
	if got := idx.Resolve(52991, 0, "Frieren 2", "/play/f.x"); got.Season != 2 || got.Mapped {
		t.Errorf("heuristic must still work on an empty index: %+v", got)
	}
}
