package animeworld

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"
)

// ErrOriginBlocked means the origin itself refused us (nginx "403 Forbidden" or
// a 429), which is a ban on our IP: the solver cannot help, so requests to the
// site are paused for a while. It is an ErrBlocked, so callers that already
// answer "no results" to a block keep doing so.
var ErrOriginBlocked = fmt.Errorf("%w: the origin refuses requests from this IP", ErrBlocked)

// Pause after an origin block: it doubles on every consecutive block, up to the cap.
const (
	blockPauseMin = 5 * time.Minute
	blockPauseMax = time.Hour
)

// limiter is a token bucket: rate tokens a second, up to burst saved. A caller
// takes a token right away and sleeps until it is valid, so waiters are served
// in arrival order.
type limiter struct {
	mu     sync.Mutex
	rate   float64
	burst  float64
	tokens float64 // negative while callers are queued
	last   time.Time
}

func newLimiter(rps float64, burst int, now time.Time) *limiter {
	return &limiter{rate: rps, burst: float64(burst), tokens: float64(burst), last: now}
}

// reserve takes one token and returns how long to wait before using it.
func (l *limiter) reserve(now time.Time) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.tokens = min(l.burst, l.tokens+now.Sub(l.last).Seconds()*l.rate)
	l.last = now
	l.tokens--
	if l.tokens >= 0 {
		return 0
	}
	return time.Duration(-l.tokens / l.rate * float64(time.Second))
}

// wait blocks until the caller may send a request. A nil limiter never waits.
func (l *limiter) wait(ctx context.Context) error {
	if l == nil {
		return nil
	}
	d := l.reserve(time.Now())
	if d == 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done(): // the token is not given back: a later caller may already be timed against it
		return ctx.Err()
	}
}

// SetRate limits requests to the site (not to its file servers) to rps a
// second, with bursts of up to burst. rps <= 0 removes the limit.
func (c *Client) SetRate(rps float64, burst int) {
	if rps <= 0 {
		c.limit = nil
		return
	}
	c.limit = newLimiter(rps, max(burst, 1), time.Now())
}

// politeTransport applies the rate limit and the block pause to every HTTP
// round trip to the site, redirect hops and retries included.
type politeTransport struct {
	base http.RoundTripper
	c    *Client
}

func (p politeTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Host != p.c.host {
		return p.base.RoundTrip(req)
	}
	if err := p.c.pausedErr(); err != nil {
		return nil, err
	}
	if err := p.c.limit.wait(req.Context()); err != nil {
		return nil, err
	}
	if err := p.c.pausedErr(); err != nil { // a request ahead of us may have been refused meanwhile
		return nil, err
	}
	return p.base.RoundTrip(req)
}

// pausedErr is non-nil while requests to the site are paused after a block.
func (c *Client) pausedErr() error {
	c.bmu.Lock()
	defer c.bmu.Unlock()
	if c.now().Before(c.until) {
		return fmt.Errorf("%w (paused until %s)", ErrOriginBlocked, c.until.UTC().Format(time.TimeOnly+"Z"))
	}
	return nil
}

// markBlocked opens a pause window, longer each time the origin refuses us
// again. Requests already in flight when it opens do not extend it, and the
// window is logged once.
func (c *Client) markBlocked(status int) {
	c.bmu.Lock()
	defer c.bmu.Unlock()
	now := c.now()
	if now.Before(c.until) {
		return
	}
	c.pause = min(max(c.pause*2, c.pauseMin), c.pauseMax)
	c.until = now.Add(c.pause)
	c.reason = fmt.Sprintf("AnimeWorld refused this IP (HTTP %d); requests are paused until %s", status, c.until.UTC().Format(time.RFC3339))
	slog.Error("AnimeWorld refused this IP, pausing all requests to the site", "status", status, "pause", c.pause.String(), "until", c.until.UTC().Format(time.RFC3339))
}

// clearBlock forgets the pause history after a good response. Not while a
// pause is open: that 200 was in flight before the ban (or a cached page), and
// clearing would reset the backoff instead of letting it grow.
func (c *Client) clearBlock() {
	c.bmu.Lock()
	if !c.now().Before(c.until) {
		c.pause, c.until, c.reason = 0, time.Time{}, ""
	}
	c.bmu.Unlock()
}

// Blocked says why requests to the site are paused, or "" when they are not.
func (c *Client) Blocked() string {
	c.bmu.Lock()
	defer c.bmu.Unlock()
	if c.now().Before(c.until) {
		return c.reason
	}
	return ""
}

type pageKind int

const (
	pageOther     pageKind = iota
	pageChallenge          // Cloudflare interstitial: the solver can pass it
	pageBlocked            // the origin's own refusal: it cannot be passed
)

// classify tells a Cloudflare challenge from an origin block in a 403, 429 or
// 503. Only the challenge header and the "Just a moment" page count as a
// challenge: the origin's nginx 403 page also embeds Cloudflare's
// challenge-platform script, so that marker proves nothing. It reads the body.
func classify(resp *http.Response) pageKind {
	switch resp.StatusCode {
	case http.StatusForbidden, http.StatusTooManyRequests, http.StatusServiceUnavailable:
	default:
		return pageOther
	}
	if resp.Header.Get("Cf-Mitigated") == "challenge" {
		return pageChallenge
	}
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	page := strings.ToLower(string(b))
	if strings.Contains(page, "just a moment") || strings.Contains(page, "cf-browser-verification") {
		return pageChallenge
	}
	switch {
	case resp.StatusCode == http.StatusTooManyRequests:
		return pageBlocked
	// The ban is an HTML error page; a 403 in JSON (or empty) is about one
	// resource, such as a removed episode, and must not pause the whole site.
	case resp.StatusCode == http.StatusForbidden && strings.HasPrefix(strings.ToLower(resp.Header.Get("Content-Type")), "text/html"):
		return pageBlocked
	}
	return pageOther
}
