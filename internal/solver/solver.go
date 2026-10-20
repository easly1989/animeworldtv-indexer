// Package solver gets past Cloudflare challenges through a FlareSolverr
// compatible service (Byparr in the arr stack). A solve returns the cookies and
// user agent of a real browser; they are reused for plain requests until they
// stop working, and cached on disk so a restart does not pay for a new solve.
package solver

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	cacheKey = "solver:session"
	// Solves take 40-130 s on the NAS, so the solver gets two minutes.
	solveTimeout = 120 * time.Second
	// After a failed solve, further callers get the same error for this long
	// instead of each waiting on a solver that is down.
	failureBackoff = 30 * time.Second
)

// Cache is the part of store.Store the solver uses.
type Cache interface {
	Get(key string, maxAge time.Duration, v any) bool
	Put(key string, v any) error
}

type Cookie struct {
	Name   string `json:"name"`
	Value  string `json:"value"`
	Domain string `json:"domain"`
}

type session struct {
	Cookies   []Cookie  `json:"cookies"`
	UserAgent string    `json:"user_agent"`
	SolvedAt  time.Time `json:"solved_at"`
}

// Solver holds the current solved session. The zero value is not usable; a nil
// *Solver reports itself as not configured.
type Solver struct {
	url   string
	site  string // page the solver opens: the site's front page
	ttl   time.Duration
	http  *http.Client
	cache Cache

	sem chan struct{} // held for the duration of a solve: one at a time

	mu       sync.RWMutex
	sess     session
	gen      uint64 // bumped by every successful solve
	failedAt time.Time
	failErr  error
}

// New returns a solver for the service at url. site is the page to solve and
// ttl how long a solve stays valid (mircrewrr uses 12 h). Any session cached in
// cache and still within ttl is picked up.
func New(url, site string, ttl time.Duration, cache Cache) *Solver {
	s := &Solver{
		url: strings.TrimRight(url, "/"), site: site, ttl: ttl, cache: cache,
		http: &http.Client{Timeout: solveTimeout + 30*time.Second},
		sem:  make(chan struct{}, 1),
	}
	var sess session
	if cache != nil && cache.Get(cacheKey, ttl, &sess) && s.fresh(sess) {
		s.sess = sess
		slog.Info("solver session restored from cache", "cookies", len(sess.Cookies), "solved_at", sess.SolvedAt)
	}
	return s
}

func (s *Solver) fresh(sess session) bool {
	return len(sess.Cookies) > 0 && time.Since(sess.SolvedAt) < s.ttl
}

// Apply adds the solved cookies and user agent to req, if there are valid ones.
// The returned generation identifies that state: pass it to Refresh when the
// request still got challenged.
func (s *Solver) Apply(req *http.Request) uint64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.fresh(s.sess) {
		req.Header.Set("User-Agent", s.sess.UserAgent)
		host := req.URL.Hostname()
		for _, c := range s.sess.Cookies {
			if d := strings.TrimPrefix(c.Domain, "."); d == "" || host == d || strings.HasSuffix(host, "."+d) {
				req.AddCookie(&http.Cookie{Name: c.Name, Value: c.Value})
			}
		}
	}
	return s.gen
}

// Refresh solves the challenge again, unless another caller already did since
// generation gen was handed out. Concurrent callers wait for the same solve.
func (s *Solver) Refresh(ctx context.Context, gen uint64) error {
	select {
	case s.sem <- struct{}{}:
		defer func() { <-s.sem }()
	case <-ctx.Done():
		return ctx.Err()
	}
	s.mu.RLock()
	solved, failedAt, failErr := s.gen != gen, s.failedAt, s.failErr
	s.mu.RUnlock()
	if solved {
		return nil
	}
	if failErr != nil && time.Since(failedAt) < failureBackoff {
		return failErr
	}

	sess, err := s.solve(ctx)
	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil {
		slog.Error("Cloudflare solve failed", "solver", s.url, "err", err)
		s.failedAt, s.failErr = time.Now(), err
		return err
	}
	s.sess, s.gen, s.failErr = sess, s.gen+1, nil
	if s.cache != nil {
		if err := s.cache.Put(cacheKey, sess); err != nil {
			slog.Warn("cache write failed", "key", cacheKey, "err", err)
		}
	}
	slog.Info("Cloudflare solved", "cookies", len(sess.Cookies))
	return nil
}

// SiteRefused is a solve whose browser reached the site and got a 403 or 429:
// a ban on the IP, which no solve can pass.
type SiteRefused struct{ Status int }

func (e SiteRefused) Error() string {
	return fmt.Sprintf("solver got HTTP %d from the site: it refuses this IP", e.Status)
}

type solveResponse struct {
	Status   string `json:"status"`
	Message  string `json:"message"`
	Solution struct {
		Status    int      `json:"status"`
		Cookies   []Cookie `json:"cookies"`
		UserAgent string   `json:"userAgent"`
	} `json:"solution"`
}

func (s *Solver) solve(ctx context.Context) (session, error) {
	slog.Info("solving Cloudflare challenge", "solver", s.url, "site", s.site)
	body, _ := json.Marshal(map[string]any{
		"cmd": "request.get", "url": s.site,
		"maxTimeout":  solveTimeout.Milliseconds(), // FlareSolverr
		"max_timeout": int(solveTimeout.Seconds()), // Byparr
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.url+"/v1", bytes.NewReader(body))
	if err != nil {
		return session{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.http.Do(req)
	if err != nil {
		return session{}, fmt.Errorf("solver at %s unreachable: %w", s.url, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))

	var r solveResponse
	if err := json.Unmarshal(raw, &r); err != nil {
		return session{}, fmt.Errorf("solver returned HTTP %d with a non-JSON body: %.200q", resp.StatusCode, raw)
	}
	if r.Status != "ok" {
		return session{}, fmt.Errorf("solver error (HTTP %d): %s", resp.StatusCode, cmp.Or(r.Message, "unknown"))
	}
	switch r.Solution.Status {
	case http.StatusOK:
	case http.StatusForbidden, http.StatusTooManyRequests:
		return session{}, SiteRefused{r.Solution.Status}
	default:
		return session{}, fmt.Errorf("solver got HTTP %d from the site: challenge not passed", r.Solution.Status)
	}
	return session{Cookies: r.Solution.Cookies, UserAgent: r.Solution.UserAgent, SolvedAt: time.Now()}, nil
}

// Status is what /health reports.
type Status struct {
	Configured   bool       `json:"configured"`
	URL          string     `json:"url,omitempty"`
	CookiesValid bool       `json:"cookies_valid"`
	ExpiresAt    *time.Time `json:"expires_at,omitempty"`
}

// Status reports the solver and whether valid cookies are cached. Safe on nil.
func (s *Solver) Status() Status {
	if s == nil {
		return Status{}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	st := Status{Configured: true, URL: s.url, CookiesValid: s.fresh(s.sess)}
	if st.CookiesValid {
		exp := s.sess.SolvedAt.Add(s.ttl)
		st.ExpiresAt = &exp
	}
	return st
}
