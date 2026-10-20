package animeworld

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/easly1989/animeworldtv-indexer/internal/solver"
)

// challengedSite answers 403 + cf-mitigated until the request carries the
// cookie and user agent the fake solver hands out.
func challengedSite(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var challenges atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if c, err := r.Cookie("cf_clearance"); err != nil || c.Value != "ok" || r.UserAgent() != "SolverUA" {
			challenges.Add(1)
			w.Header().Set("Cf-Mitigated", "challenge")
			w.WriteHeader(http.StatusForbidden)
			return
		}
		w.Write([]byte(`{"grabber":"http://files/x.mp4"}`))
	}))
	t.Cleanup(srv.Close)
	return srv, &challenges
}

func fakeSolver(t *testing.T, site *httptest.Server) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	u, _ := url.Parse(site.URL)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Write([]byte(`{"status":"ok","solution":{"status":200,"userAgent":"SolverUA",
			"cookies":[{"name":"cf_clearance","value":"ok","domain":"` + u.Hostname() + `"}]}}`))
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

func TestChallengeSolvedOnceAndCookiesReused(t *testing.T) {
	site, challenges := challengedSite(t)
	sol, solves := fakeSolver(t, site)
	c := New(site.URL, site.Client())
	c.retryDelay = time.Millisecond
	c.UseSolver(solver.New(sol.URL, site.URL, time.Hour, nil))

	for i := range 3 {
		if u, err := c.FileURL(context.Background(), "t"); err != nil || u != "http://files/x.mp4" {
			t.Fatalf("request %d: u=%q err=%v", i, u, err)
		}
	}
	if solves.Load() != 1 || challenges.Load() != 1 {
		t.Errorf("solver calls=%d challenges=%d, want 1 and 1", solves.Load(), challenges.Load())
	}
}

func TestChallengeSolverUnreachable(t *testing.T) {
	site, _ := challengedSite(t)
	dead := httptest.NewServer(http.NotFoundHandler())
	dead.Close()
	c := New(site.URL, site.Client())
	c.UseSolver(solver.New(dead.URL, site.URL, time.Hour, nil))

	_, err := c.FileURL(context.Background(), "t")
	if !errors.Is(err, ErrBlocked) {
		t.Fatalf("err=%v, want ErrBlocked", err)
	}
}

func TestChallengeWithoutSolver(t *testing.T) {
	site, _ := challengedSite(t)
	if _, err := New(site.URL, site.Client()).FileURL(context.Background(), "t"); !errors.Is(err, ErrBlocked) {
		t.Fatalf("err=%v, want ErrBlocked", err)
	}
}

func TestPlain403IsNotAChallenge(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusForbidden) }))
	defer srv.Close()
	if _, err := New(srv.URL, srv.Client()).FileURL(context.Background(), "t"); err == nil || errors.Is(err, ErrBlocked) {
		t.Fatalf("err=%v, want a plain status error", err)
	}
}
