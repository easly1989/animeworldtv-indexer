package mapping

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"sync"
	"time"
)

// The source file is Fribb/anime-lists' anime-list-full.json: of its lists only
// this one carries the TVDB season and episode offset.
const (
	cacheKey      = "mapping:fribb"
	retryAfter    = 10 * time.Minute
	downloadLimit = 128 << 20
)

// Cache is the part of store.Store the source uses.
type Cache interface {
	Get(key string, maxAge time.Duration, v any) bool
	Put(key string, v any) error
}

// Source keeps the mapping list fresh: in memory, in the SQLite cache, and
// downloaded again once it is older than ttl. A failed refresh keeps serving the
// last copy and tries again in ten minutes, so the service works offline.
type Source struct {
	url   string
	cache Cache
	ttl   time.Duration
	hc    *http.Client
	now   func() time.Time

	mu      sync.Mutex
	idx     *Index
	expires time.Time
}

func NewSource(url string, cache Cache, ttl time.Duration) *Source {
	return &Source{url: url, cache: cache, ttl: ttl, hc: &http.Client{Timeout: 2 * time.Minute}, now: time.Now}
}

// Index returns the current mapping. It is never nil: with no data at all it is
// empty and series fall back to the title heuristic.
func (s *Source) Index(ctx context.Context) *Index {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.idx != nil && s.now().Before(s.expires) {
		return s.idx
	}
	var entries []Entry
	switch {
	case s.cache.Get(cacheKey, s.ttl, &entries):
		s.expires = s.now().Add(s.ttl)
	default:
		var err error
		// Detached from the request: a cancelled search must not leave the next one without data.
		if entries, err = s.download(context.WithoutCancel(ctx)); err == nil {
			if err = s.cache.Put(cacheKey, entries); err != nil {
				slog.Warn("mapping cache write failed", "err", err)
			}
			s.expires = s.now().Add(s.ttl)
			slog.Info("mapping list refreshed", "entries", len(entries))
		} else {
			slog.Warn("mapping download failed, keeping the previous copy", "err", err)
			s.expires = s.now().Add(retryAfter)
			if s.idx != nil {
				return s.idx
			}
			s.cache.Get(cacheKey, math.MaxInt64, &entries) // stale beats nothing
		}
	}
	s.idx = NewIndex(entries)
	return s.idx
}

func (s *Source) download(ctx context.Context) ([]Entry, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := s.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("mapping download: %s", resp.Status)
	}
	return Parse(io.LimitReader(resp.Body, downloadLimit))
}
