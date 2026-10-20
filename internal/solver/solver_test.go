package solver

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type memCache struct {
	mu sync.Mutex
	m  map[string]string
}

func (c *memCache) Get(key string, _ time.Duration, v any) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	raw, ok := c.m[key]
	return ok && json.Unmarshal([]byte(raw), v) == nil
}

func (c *memCache) Put(key string, v any) error {
	raw, _ := json.Marshal(v)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = map[string]string{}
	}
	c.m[key] = string(raw)
	return nil
}

// fakeSolver answers like Byparr and counts the calls.
func fakeSolver(t *testing.T, reply string) (*httptest.Server, *atomic.Int32, *string) {
	t.Helper()
	var calls atomic.Int32
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		time.Sleep(20 * time.Millisecond) // let concurrent callers pile up
		w.Write([]byte(reply))
	}))
	t.Cleanup(srv.Close)
	return srv, &calls, &gotBody
}

const okReply = `{"status":"ok","solution":{"status":200,"userAgent":"SolverUA",
"cookies":[{"name":"cf_clearance","value":"abc","domain":".animeworld.ac"},{"name":"other","value":"x","domain":"example.com"}]}}`

func apply(s *Solver, host string) (*http.Request, uint64) {
	req, _ := http.NewRequest(http.MethodGet, "https://"+host+"/", nil)
	return req, s.Apply(req)
}

func TestRefreshStoresApplyAndPersist(t *testing.T) {
	srv, calls, body := fakeSolver(t, okReply)
	cache := &memCache{}
	s := New(srv.URL, "https://www.animeworld.ac", time.Hour, cache)

	if st := s.Status(); !st.Configured || st.CookiesValid {
		t.Fatalf("fresh status: %+v", st)
	}
	_, gen := apply(s, "www.animeworld.ac")
	if err := s.Refresh(context.Background(), gen); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(*body, `"cmd":"request.get"`) || !strings.Contains(*body, `"url":"https://www.animeworld.ac"`) {
		t.Errorf("solver request: %s", *body)
	}
	req, _ := apply(s, "www.animeworld.ac")
	if c, err := req.Cookie("cf_clearance"); err != nil || c.Value != "abc" || req.UserAgent() != "SolverUA" {
		t.Errorf("cookie=%v err=%v ua=%q", c, err, req.UserAgent())
	}
	if _, err := req.Cookie("other"); err == nil {
		t.Error("cookie of another domain must not be sent")
	}
	if st := s.Status(); !st.CookiesValid || st.ExpiresAt == nil {
		t.Errorf("status after solve: %+v", st)
	}

	// A restart picks the session up from the cache, without calling the solver.
	s2 := New(srv.URL, "https://www.animeworld.ac", time.Hour, cache)
	if !s2.Status().CookiesValid || calls.Load() != 1 {
		t.Errorf("restored=%v calls=%d", s2.Status().CookiesValid, calls.Load())
	}
	// ...unless it is older than the TTL.
	time.Sleep(30 * time.Millisecond)
	if New(srv.URL, "https://www.animeworld.ac", 10*time.Millisecond, cache).Status().CookiesValid {
		t.Error("expired session was restored")
	}
}

func TestConcurrentRefreshSolvesOnce(t *testing.T) {
	srv, calls, _ := fakeSolver(t, okReply)
	s := New(srv.URL, "https://www.animeworld.ac", time.Hour, nil)
	_, gen := apply(s, "www.animeworld.ac")
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := s.Refresh(context.Background(), gen); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if calls.Load() != 1 {
		t.Errorf("solver called %d times, want 1", calls.Load())
	}
}

func TestRefreshFailures(t *testing.T) {
	for name, reply := range map[string]string{
		"error status":  `{"status":"error","message":"timed out"}`,
		"site said 403": `{"status":"ok","solution":{"status":403,"cookies":[]}}`,
		"not json":      `<html>Bad gateway</html>`,
	} {
		srv, _, _ := fakeSolver(t, reply)
		s := New(srv.URL, "https://www.animeworld.ac", time.Hour, nil)
		if err := s.Refresh(context.Background(), 0); err == nil || s.Status().CookiesValid {
			t.Errorf("%s: err=%v valid=%v", name, err, s.Status().CookiesValid)
		}
	}
}

func TestUnreachableSolverFailsFastAfterFirstTry(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close() // nothing listens there any more
	s := New(url, "https://www.animeworld.ac", time.Hour, nil)
	err := s.Refresh(context.Background(), 0)
	if err == nil || !strings.Contains(err.Error(), "unreachable") {
		t.Fatalf("err=%v", err)
	}
	if err2 := s.Refresh(context.Background(), 0); err2 == nil || err2.Error() != err.Error() {
		t.Errorf("second call: %v", err2)
	}
}

func TestNilSolverStatus(t *testing.T) {
	var s *Solver
	if st := s.Status(); st.Configured || st.CookiesValid {
		t.Errorf("%+v", st)
	}
}
