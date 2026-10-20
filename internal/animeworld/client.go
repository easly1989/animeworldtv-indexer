package animeworld

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// A browser-like agent: the site sits behind Cloudflare.
const userAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0 Safari/537.36"

// Client talks to AnimeWorld over plain HTTP.
type Client struct {
	base       string
	http       *http.Client
	retryDelay time.Duration
}

// New returns a client for the site at baseURL (the .tv/.so domains redirect to .ac).
func New(baseURL string, hc *http.Client) *Client {
	if hc == nil {
		hc = &http.Client{Timeout: 45 * time.Second}
	}
	return &Client{base: strings.TrimRight(baseURL, "/"), http: hc, retryDelay: time.Second}
}

// Search lists series matching q. An empty q returns the latest updates,
// because the site redirects an empty keyword away.
func (c *Client) Search(ctx context.Context, q string) ([]Hit, error) {
	path := "/updated"
	if q != "" {
		path = "/search?keyword=" + url.QueryEscape(q)
	}
	var hits []Hit
	err := c.do(ctx, http.MethodGet, c.base+path, func(r *http.Response) (err error) {
		hits, err = ParseSearch(r.Body)
		return
	})
	return hits, err
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
func (c *Client) do(ctx context.Context, method, rawURL string, handle func(*http.Response) error) error {
	var err error
	for attempt := 0; attempt < 2; attempt++ {
		if attempt > 0 {
			select {
			case <-time.After(c.retryDelay):
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		var retry bool
		if retry, err = c.once(ctx, method, rawURL, handle); !retry {
			return err
		}
	}
	return err
}

func (c *Client) once(ctx context.Context, method, rawURL string, handle func(*http.Response) error) (retry bool, err error) {
	req, err := http.NewRequestWithContext(ctx, method, rawURL, nil)
	if err != nil {
		return false, err
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := c.http.Do(req)
	if err != nil {
		return true, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return resp.StatusCode >= 500, fmt.Errorf("%s %s: %s", method, rawURL, resp.Status)
	}
	return false, handle(resp)
}
