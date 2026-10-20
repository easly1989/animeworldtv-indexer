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
	empty atomic.Bool
	gate  chan struct{} // when set, downloads wait for it to close
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
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.hits.Add(1)
		if f.gate != nil {
			select {
			case <-f.gate:
			case <-r.Context().Done():
				return
			}
		}
		switch {
		case f.fail.Load():
			http.Error(w, "down", http.StatusBadGateway)
		case f.empty.Load():
			_, _ = w.Write([]byte("[]"))
		default:
			_, _ = w.Write(body)
		}
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

func has(idx *Index) bool { _, ok := idx.ByMAL(52991); return ok }

func TestSourceNeverBlocksOnTheDownload(t *testing.T) {
	f := newFixture(t)
	f.gate = make(chan struct{})
	s := f.source()
	start := time.Now()
	idx := s.Index(t.Context())
	if idx == nil || has(idx) || time.Since(start) > time.Second {
		t.Fatalf("cold start must return an empty index at once (took %v)", time.Since(start))
	}
	close(f.gate)
	s.wait()
	if !has(s.Index(t.Context())) {
		t.Error("index not swapped in after the background download")
	}
}

func TestSourceDownloadsOnceAndCaches(t *testing.T) {
	f := newFixture(t)
	s := f.source()
	s.Index(t.Context())
	s.wait()
	s.Index(t.Context())
	s.wait()
	if !has(s.Index(t.Context())) || f.hits.Load() != 1 {
		t.Errorf("want Frieren after exactly 1 download, got %d downloads", f.hits.Load())
	}
	// A new process (new Source, same database) reads the cache instead of downloading.
	s2 := f.source()
	if !has(s2.Index(t.Context())) {
		t.Error("restart did not serve the cached list at once")
	}
	s2.wait()
	if f.hits.Load() != 1 {
		t.Errorf("restart downloaded again (%d)", f.hits.Load())
	}
}

func TestSourceRefreshesInTheBackgroundAfterTTL(t *testing.T) {
	f := newFixture(t)
	s := f.source()
	s.ttl = time.Nanosecond // memory and database copies both count as expired once the clock moves
	s.Index(t.Context())
	s.wait()
	f.now = f.now.Add(time.Hour)
	f.gate = make(chan struct{})
	if !has(s.Index(t.Context())) { // the old copy is served while the refresh runs
		t.Error("expired index not served during the refresh")
	}
	close(f.gate)
	s.wait()
	if f.hits.Load() != 2 {
		t.Errorf("want a refresh download after the TTL, got %d downloads", f.hits.Load())
	}
}

func TestSourceKeepsServingStaleDataWhenRefreshFails(t *testing.T) {
	f := newFixture(t)
	s := f.source()
	s.ttl = time.Nanosecond
	s.Index(t.Context())
	s.wait()
	f.fail.Store(true)
	f.now = f.now.Add(time.Hour)
	s.Index(t.Context())
	s.wait()
	if !has(s.Index(t.Context())) {
		t.Error("stale index not served after a failed refresh")
	}
	n := f.hits.Load()
	s.Index(t.Context()) // inside the retry back-off: no new request
	s.wait()
	if f.hits.Load() != n {
		t.Error("failed refresh retried immediately")
	}
}

func TestSourceWithoutDataIsEmptyNotNil(t *testing.T) {
	f := newFixture(t)
	f.fail.Store(true)
	s := f.source()
	idx := s.Index(t.Context())
	s.wait()
	idx = s.Index(t.Context())
	if idx == nil {
		t.Fatal("Index must never be nil")
	}
	if has(idx) {
		t.Error("expected an empty index")
	}
	if got := idx.Resolve(52991, 0, "Frieren 2", "/play/f.x"); got.Season != 2 || got.Mapped {
		t.Errorf("heuristic must still work on an empty index: %+v", got)
	}
}

func TestSourceDoesNotCacheAnEmptyList(t *testing.T) {
	f := newFixture(t)
	f.empty.Store(true)
	s := f.source()
	s.Index(t.Context())
	s.wait()
	var entries []Entry
	if f.store.Get(cacheKey, time.Hour, &entries) {
		t.Error("an empty list was stored as valid")
	}
	f.empty.Store(false)
	f.now = f.now.Add(retryAfter + time.Second)
	s.Index(t.Context())
	s.wait()
	if !has(s.Index(t.Context())) {
		t.Error("a later good download was not used")
	}
}
