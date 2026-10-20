package animeworld

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"github.com/easly1989/animeworldtv-indexer/internal/solver"
)

// ErrBlocked means Cloudflare refused the request and could not be got past
// (no solver configured, or it failed). Callers treat it as "no results".
var ErrBlocked = errors.New("blocked by Cloudflare")

var errChallenge = fmt.Errorf("%w: challenge page", ErrBlocked)

// A browser-like agent: the site sits behind Cloudflare.
const userAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0 Safari/537.36"

// Client talks to AnimeWorld over plain HTTP.
type Client struct {
	base       string
	host       string // base's host, to tell the site from the file servers
	http       *http.Client
	retryDelay time.Duration
	solver     *solver.Solver // nil: challenges are not worked around
	failing    atomic.Bool    // the last request to the site failed (transport error, 5xx or challenge)
}

// Reachable reports whether the last request to the site went through. It is
// true before any request, and ignores requests cancelled by the caller.
func (c *Client) Reachable() bool { return !c.failing.Load() }

// UseSolver makes the client answer Cloudflare challenges on the site through s.
func (c *Client) UseSolver(s *solver.Solver) { c.solver = s }

// New returns a client for the site at baseURL (the .tv/.so domains redirect to .ac).
func New(baseURL string, hc *http.Client) *Client {
	if hc == nil {
		hc = &http.Client{Timeout: 45 * time.Second}
	}
	u, _ := url.Parse(baseURL)
	host := ""
	if u != nil {
		host = u.Host
	}
	return &Client{base: strings.TrimRight(baseURL, "/"), host: host, http: hc, retryDelay: time.Second}
}

// Search lists series matching q. q must not be empty: the site redirects an
// empty keyword away (see Updated for the latest episodes).
func (c *Client) Search(ctx context.Context, q string) ([]Hit, error) {
	var hits []Hit
	err := c.do(ctx, http.MethodGet, c.base+"/search?keyword="+url.QueryEscape(q), func(r *http.Response) (err error) {
		hits, err = ParseSearch(r.Body)
		return
	})
	return hits, err
}

// Updated lists the latest episodes, newest first (the first page of /updated).
func (c *Client) Updated(ctx context.Context) ([]Update, error) {
	var ups []Update
	err := c.do(ctx, http.MethodGet, c.base+"/updated", func(r *http.Response) (err error) {
		ups, err = ParseUpdated(r.Body)
		return
	})
	return ups, err
}

// Series fetches a series page by path; the site redirects it to the first episode.
func (c *Client) Series(ctx context.Context, path string) (Series, error) {
	var s Series
	err := c.do(ctx, http.MethodGet, c.base+path, func(r *http.Response) (err error) {
		s, err = ParseSeries(r.Body)
		return
	})
	s.Path = path
	s.Dub = IsDub(path)
	return s, err
}

// FileURL resolves an episode token to its direct MP4 URL.
func (c *Client) FileURL(ctx context.Context, token string) (string, error) {
	var u string
	err := c.do(ctx, http.MethodGet, c.base+"/api/episode/info?id="+url.QueryEscape(token), func(r *http.Response) (err error) {
		u, err = ParseEpisodeInfo(r.Body)
		return
	})
	return u, err
}

// FileMeta is what a HEAD on the MP4 tells us.
type FileMeta struct {
	Size     int64     `json:"size"`
	Modified time.Time `json:"modified"`
}

// FileMeta issues a HEAD on a direct file URL.
func (c *Client) FileMeta(ctx context.Context, fileURL string) (FileMeta, error) {
	var m FileMeta
	err := c.do(ctx, http.MethodHead, fileURL, func(r *http.Response) error {
		if r.ContentLength <= 0 {
			return fmt.Errorf("HEAD %s: no Content-Length", fileURL)
		}
		m.Size = r.ContentLength
		m.Modified, _ = http.ParseTime(r.Header.Get("Last-Modified"))
		return nil
	})
	return m, err
}

// do runs a request and hands a 200 response to handle. A transport error or
// 5xx is retried once: the site is slow and occasionally drops connections.
//
// A Cloudflare challenge on the site is solved once (see package solver) and
// the request repeated with the solved cookies.
func (c *Client) do(ctx context.Context, method, rawURL string, handle func(*http.Response) error) error {
	var err error
	solved := false
	for attempt := 0; attempt < 2; attempt++ {
		if attempt > 0 {
			select {
			case <-time.After(c.retryDelay):
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		var retry bool
		var gen uint64
		if retry, gen, err = c.once(ctx, method, rawURL, handle); errors.Is(err, errChallenge) && c.solver != nil && !solved {
			solved = true
			if serr := c.solver.Refresh(ctx, gen); serr != nil {
				return fmt.Errorf("%w: %w", ErrBlocked, serr)
			}
			retry, _, err = c.once(ctx, method, rawURL, handle)
		}
		if !retry {
			return err
		}
	}
	return err
}

// once also reports the solver generation whose cookies the request carried.
func (c *Client) once(ctx context.Context, method, rawURL string, handle func(*http.Response) error) (retry bool, gen uint64, err error) {
	req, err := http.NewRequestWithContext(ctx, method, rawURL, nil)
	if err != nil {
		return false, 0, err
	}
	req.Header.Set("User-Agent", userAgent)
	onSite := req.URL.Host == c.host // solved cookies are for the site, not the file servers
	if c.solver != nil && onSite {
		gen = c.solver.Apply(req)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		if onSite && ctx.Err() == nil {
			c.failing.Store(true)
		}
		return true, gen, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		if onSite && isChallenge(resp) {
			c.failing.Store(true)
			return false, gen, fmt.Errorf("%s %s: %s: %w", method, rawURL, resp.Status, errChallenge)
		}
		if onSite {
			c.failing.Store(resp.StatusCode >= 500)
		}
		return resp.StatusCode >= 500, gen, fmt.Errorf("%s %s: %s", method, rawURL, resp.Status)
	}
	if onSite {
		c.failing.Store(false)
	}
	return false, gen, handle(resp)
}

// isChallenge recognises a Cloudflare interstitial: a 403 or 503 carrying a
// cf-mitigated header or the "Just a moment" page.
func isChallenge(resp *http.Response) bool {
	if resp.StatusCode != http.StatusForbidden && resp.StatusCode != http.StatusServiceUnavailable {
		return false
	}
	if resp.Header.Get("Cf-Mitigated") == "challenge" {
		return true
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	page := strings.ToLower(string(body))
	return strings.Contains(page, "just a moment") || strings.Contains(page, "challenge-platform") || strings.Contains(page, "cf-browser-verification")
}
