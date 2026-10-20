package animeworld

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/easly1989/animeworldtv-indexer/internal/solver"
)

func TestLimiterBurstThenRate(t *testing.T) {
	t0 := time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC)
	l := newLimiter(2, 3, t0)
	for i := range 3 {
		if d := l.reserve(t0); d != 0 {
			t.Fatalf("burst token %d waits %v", i, d)
		}
	}
	if d := l.reserve(t0); d != 500*time.Millisecond {
		t.Errorf("4th waits %v, want 500ms", d)
	}
	if d := l.reserve(t0); d != time.Second {
		t.Errorf("5th waits %v, want 1s (queued behind the 4th)", d)
	}
	// After a long idle spell only the burst is saved, not the whole idle time.
	later := t0.Add(time.Hour)
	for i := range 3 {
		if d := l.reserve(later); d != 0 {
			t.Fatalf("refilled token %d waits %v", i, d)
		}
	}
	if d := l.reserve(later); d == 0 {
		t.Error("a 4th token right after the refill must wait")
	}
}

func TestLimiterWaitHonoursContext(t *testing.T) {
	l := newLimiter(0.5, 1, time.Now())
	if err := l.wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := l.wait(ctx); !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > time.Second {
		t.Fatalf("err=%v after %v, want a prompt deadline error", err, time.Since(start))
	}
}

// stamps is a fake origin that records when each request arrived.
type stamps struct {
	mu sync.Mutex
	ts []time.Time
}

func (s *stamps) handler(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	s.ts = append(s.ts, time.Now())
	s.mu.Unlock()
	w.Write([]byte(`{"grabber":"x"}`))
}

// Concurrent callers (searches, RSS, grabs all share one client) must not
// push the origin past burst + rate: in every run of requests, n requests need
// at least (n-burst)/rate seconds. Timing is checked with a generous margin.
func TestRateNeverExceededUnderConcurrentCallers(t *testing.T) {
	const rps, burst, callers = 20, 2, 14
	st := &stamps{}
	srv := httptest.NewServer(http.HandlerFunc(st.handler))
	defer srv.Close()
	c := New(srv.URL, srv.Client())
	c.SetRate(rps, burst)

	start := time.Now()
	var wg sync.WaitGroup
	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := c.FileURL(context.Background(), "t"); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()

	if len(st.ts) != callers {
		t.Fatalf("origin saw %d requests, want %d", len(st.ts), callers)
	}
	if min := time.Duration(float64(callers-burst) / rps * 0.6 * float64(time.Second)); time.Since(start) < min {
		t.Errorf("%d requests took %v, rate limit would need at least %v", callers, time.Since(start), min)
	}
	slices.SortFunc(st.ts, func(a, b time.Time) int { return a.Compare(b) })
	for i := range st.ts {
		for j := i + burst; j < len(st.ts); j++ {
			n := j - i + 1
			if need := time.Duration(float64(n-burst) / rps * 0.6 * float64(time.Second)); st.ts[j].Sub(st.ts[i]) < need {
				t.Fatalf("requests %d..%d (%d of them) arrived within %v, want >= %v", i, j, n, st.ts[j].Sub(st.ts[i]), need)
			}
		}
	}
}

// A series path 302s to the episode page: both hops are origin requests.
func TestRedirectHopsCountAgainstTheLimit(t *testing.T) {
	c, srv := serve(t, map[string]string{"search": "testdata/search_frieren.html", "series": "testdata/series_frieren.html"})
	c = New(srv.URL, srv.Client())
	c.SetRate(20, 1)
	start := time.Now()
	if _, err := c.Series(context.Background(), "/play/sousou-no-frieren.kbcPv"); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) < 40*time.Millisecond { // the second hop waits 1/20 s for a token
		t.Errorf("a redirected series fetch took %v: the hop was not rate limited", time.Since(start))
	}
}

// The MP4 servers are not the origin: a slow origin limit must not slow them.
func TestFileServersAreNotRateLimited(t *testing.T) {
	var heads atomic.Int32
	files := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		heads.Add(1)
		w.Header().Set("Content-Length", "1000")
	}))
	defer files.Close()
	origin := httptest.NewServer(http.NotFoundHandler())
	defer origin.Close()
	c := New(origin.URL, files.Client())
	c.SetRate(0.1, 1)

	start := time.Now()
	for range 6 {
		if _, err := c.FileMeta(context.Background(), files.URL+"/x.mp4"); err != nil {
			t.Fatal(err)
		}
	}
	if heads.Load() != 6 || time.Since(start) > time.Second {
		t.Errorf("heads=%d in %v", heads.Load(), time.Since(start))
	}
}

// blockedOrigin answers like the real one did on 2026-10-21: Cloudflare's edge
// passing an nginx 403 page (saved in testdata) that embeds the challenge-platform script.
type blockedOrigin struct {
	*httptest.Server
	hits  atomic.Int32
	block atomic.Bool
}

func newBlockedOrigin(t *testing.T) *blockedOrigin {
	t.Helper()
	page, err := os.ReadFile("testdata/origin_403_nginx.html")
	if err != nil {
		t.Fatal(err)
	}
	o := &blockedOrigin{}
	o.block.Store(true)
	o.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		o.hits.Add(1)
		if o.block.Load() {
			w.Header().Set("Server", "cloudflare")
			w.Header().Set("Content-Type", "text/html")
			w.WriteHeader(http.StatusForbidden)
			w.Write(page)
			return
		}
		w.Write([]byte(`{"grabber":"x"}`))
	}))
	t.Cleanup(o.Close)
	return o
}

// clock is a settable time for the block pause.
type clock struct{ ns atomic.Int64 }

func (k *clock) now() time.Time          { return time.Unix(0, k.ns.Load()) }
func (k *clock) advance(d time.Duration) { k.ns.Add(int64(d)) }

func TestNginx403IsABlockNotAChallenge(t *testing.T) {
	o := newBlockedOrigin(t)
	sol, solves := fakeSolver(t, o.Server)
	c := New(o.URL, o.Client())
	c.retryDelay = time.Millisecond
	c.UseSolver(solver.New(sol.URL, o.URL, time.Hour, nil))
	k := &clock{}
	k.advance(1000 * time.Hour)
	c.now = k.now

	_, err := c.FileURL(context.Background(), "t")
	if !errors.Is(err, ErrOriginBlocked) || !errors.Is(err, ErrBlocked) {
		t.Fatalf("err=%v, want ErrOriginBlocked (and so ErrBlocked)", err)
	}
	if solves.Load() != 0 {
		t.Errorf("the solver was called %d times for an IP ban", solves.Load())
	}
	if o.hits.Load() != 1 {
		t.Errorf("a block must not be retried: origin hits=%d", o.hits.Load())
	}
	if c.Blocked() == "" || c.Reachable() {
		t.Errorf("blocked=%q reachable=%v, want a reason and unreachable", c.Blocked(), c.Reachable())
	}

	// Paused: every kind of origin request fails fast without touching the origin.
	for _, f := range []func() error{
		func() error { _, err := c.FileURL(context.Background(), "t"); return err },
		func() error { _, err := c.Search(context.Background(), "frieren"); return err },
		func() error { _, err := c.Series(context.Background(), "/play/x.y"); return err },
	} {
		if err := f(); !errors.Is(err, ErrOriginBlocked) {
			t.Errorf("during the pause: err=%v", err)
		}
	}
	if o.hits.Load() != 1 || solves.Load() != 0 {
		t.Errorf("origin hits=%d solver calls=%d during the pause, want 1 and 0", o.hits.Load(), solves.Load())
	}
}

func TestBlockBacksOffExponentiallyAndResets(t *testing.T) {
	o := newBlockedOrigin(t)
	c := New(o.URL, o.Client())
	k := &clock{}
	k.advance(1000 * time.Hour)
	c.now = k.now
	try := func() error { _, err := c.FileURL(context.Background(), "t"); return err }

	// Pauses: 5m, 10m, 20m, 40m, then capped at 1h.
	for i, pause := range []time.Duration{5 * time.Minute, 10 * time.Minute, 20 * time.Minute, 40 * time.Minute, time.Hour, time.Hour} {
		hits := o.hits.Load()
		if err := try(); !errors.Is(err, ErrOriginBlocked) || o.hits.Load() != hits+1 {
			t.Fatalf("probe %d: err=%v, origin hits %d -> %d", i, err, hits, o.hits.Load())
		}
		k.advance(pause - time.Second)
		if err := try(); !errors.Is(err, ErrOriginBlocked) || o.hits.Load() != hits+1 {
			t.Fatalf("probe %d: still inside the %v pause but the origin was hit (hits %d)", i, pause, o.hits.Load())
		}
		if c.Blocked() == "" {
			t.Fatalf("probe %d: no reason while paused", i)
		}
		k.advance(2 * time.Second)
		if c.Blocked() != "" {
			t.Fatalf("probe %d: still reported blocked after the %v pause", i, pause)
		}
	}

	// The origin recovers: success clears the history, the next ban is 5 minutes again.
	o.block.Store(false)
	if err := try(); err != nil || !c.Reachable() {
		t.Fatalf("recovered origin: err=%v reachable=%v", err, c.Reachable())
	}
	o.block.Store(true)
	hits := o.hits.Load()
	_ = try()
	k.advance(5*time.Minute + time.Second)
	_ = try()
	if o.hits.Load() != hits+2 {
		t.Errorf("after a success the pause must restart at 5 minutes: hits %d -> %d", hits, o.hits.Load())
	}
}

// Every 429 and an HTML 403 are bans; a 403 about one resource (JSON or empty)
// is not, and must not pause the whole site.
func TestOtherRefusals(t *testing.T) {
	for name, tc := range map[string]struct {
		status int
		ctype  string
		body   string
		ban    bool
	}{
		"429":       {429, "", "slow down", true},
		"429 nginx": {429, "", "<html><center><h1>429 Too Many Requests</h1></center><hr><center>nginx</center></html>", true},
		"403 no cf": {403, "", "<h1>403 Forbidden</h1>", true},
		"empty 403": {403, "", "", false},
		"json 403":  {403, "application/json", `{"error":"forbidden"}`, false},
	} {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if tc.ctype != "" {
					w.Header().Set("Content-Type", tc.ctype)
				}
				w.WriteHeader(tc.status)
				w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			c := New(srv.URL, srv.Client())
			_, err := c.FileURL(context.Background(), "t")
			if errors.Is(err, ErrOriginBlocked) != tc.ban || err == nil {
				t.Fatalf("err=%v, want ban=%v", err, tc.ban)
			}
			if (c.Blocked() != "") != tc.ban {
				t.Errorf("Blocked() = %q, want ban=%v", c.Blocked(), tc.ban)
			}
		})
	}
}

// A 5xx is not a ban: it is retried once and never pauses the client.
func TestServerErrorsDoNotPause(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	c := New(srv.URL, srv.Client())
	c.retryDelay = time.Millisecond
	for range 2 {
		if _, err := c.FileURL(context.Background(), "t"); err == nil || errors.Is(err, ErrBlocked) {
			t.Fatalf("err=%v, want a plain status error", err)
		}
	}
	if hits.Load() != 4 || c.Blocked() != "" {
		t.Errorf("hits=%d blocked=%q", hits.Load(), c.Blocked())
	}
}

// A ban seen only by the solver's browser (the site challenges cookie-less
// clients first) must pause the client too: otherwise every request after the
// solver's failure backoff would start another full browser load of the site.
func TestBanSeenByTheSolverPauses(t *testing.T) {
	var direct, solves atomic.Int32
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		direct.Add(1)
		w.Header().Set("Cf-Mitigated", "challenge")
		w.WriteHeader(http.StatusForbidden)
	}))
	defer site.Close()
	sol := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		solves.Add(1)
		w.Write([]byte(`{"status":"ok","solution":{"status":403,"userAgent":"x","cookies":[]}}`))
	}))
	defer sol.Close()
	c := New(site.URL, site.Client())
	c.UseSolver(solver.New(sol.URL, site.URL, time.Hour, nil))
	if _, err := c.FileURL(context.Background(), "t"); !errors.Is(err, ErrOriginBlocked) || c.Blocked() == "" {
		t.Fatalf("err=%v blocked=%q, want a pause", err, c.Blocked())
	}
	if _, err := c.FileURL(context.Background(), "t"); !errors.Is(err, ErrOriginBlocked) {
		t.Fatalf("second call: %v", err)
	}
	if direct.Load() != 1 || solves.Load() != 1 {
		t.Errorf("site hit %d times, solver %d times; want 1 and 1", direct.Load(), solves.Load())
	}
}

// A 200 that was already in flight when the ban arrived must not lift the
// pause, or the backoff would restart at its minimum instead of growing.
func TestInFlightSuccessKeepsThePause(t *testing.T) {
	release, started := make(chan struct{}), make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/slow" {
			close(started)
			<-release
			w.Write([]byte(`{"grabber":"x"}`))
			return
		}
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte("<html><h1>403 Forbidden</h1></html>"))
	}))
	defer srv.Close()
	c := New(srv.URL, srv.Client())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = c.do(context.Background(), http.MethodGet, srv.URL+"/slow", func(*http.Response) error { return nil })
	}()
	<-started
	if err := c.do(context.Background(), http.MethodGet, srv.URL+"/ban", func(*http.Response) error { return nil }); !errors.Is(err, ErrOriginBlocked) {
		t.Fatalf("setup: %v", err)
	}
	close(release)
	<-done
	if c.Blocked() == "" {
		t.Error("the pause was lifted by a request that started before the ban")
	}
}

// One forbidden episode must not stop requests for the others.
func TestForbiddenResourceDoesNotPauseTheSite(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("id") == "gone" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			w.Write([]byte(`{"error":"forbidden"}`))
			return
		}
		w.Write([]byte(`{"grabber":"x"}`))
	}))
	defer srv.Close()
	c := New(srv.URL, srv.Client())
	_, _ = c.FileURL(context.Background(), "gone")
	if _, err := c.FileURL(context.Background(), "ok"); err != nil {
		t.Errorf("a 403 on one episode paused the client: %v", err)
	}
}
