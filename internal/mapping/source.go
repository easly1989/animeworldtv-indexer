package mapping

import (
	"context"
	"errors"
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
// downloaded again once it is older than ttl. Index never waits for the network:
// it serves what it has (an expired copy, or an empty index on a cold start with
// no cache) and refreshes in one background goroutine. A failed refresh keeps the
// last copy and tries again in ten minutes, so the service works offline.
type Source struct {
	url   string
	cache Cache
	ttl   time.Duration
	hc    *http.Client
	now   func() time.Time

	mu         sync.Mutex
	idx        *Index
	expires    time.Time
	refreshing bool
	wg         sync.WaitGroup
}

func NewSource(url string, cache Cache, ttl time.Duration) *Source {
	return &Source{url: url, cache: cache, ttl: ttl, hc: &http.Client{Timeout: 30 * time.Second}, now: time.Now}
}

// Index returns the current mapping. It is never nil: with no data at all it is
// empty and series fall back to the title heuristic.
func (s *Source) Index(context.Context) *Index {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.idx == nil {
		var entries []Entry
		switch {
		case s.cache.Get(cacheKey, s.ttl, &entries) && len(entries) > 0:
			s.expires = s.now().Add(s.ttl)
		case s.cache.Get(cacheKey, math.MaxInt64, &entries) && len(entries) > 0: // stale beats nothing
			s.expires = s.now()
		default:
			entries, s.expires = nil, s.now()
		}
		s.idx = NewIndex(entries)
	}
	if !s.refreshing && !s.now().Before(s.expires) {
		s.refreshing = true
		s.wg.Add(1)
		go s.refresh()
	}
	return s.idx
}

func (s *Source) refresh() {
	defer s.wg.Done()
	entries, err := s.download(context.Background())
	if err == nil && len(entries) == 0 {
		err = errors.New("the list is empty")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refreshing = false
	if err != nil {
		slog.Warn("mapping download failed, keeping the previous copy", "err", err)
		s.expires = s.now().Add(retryAfter)
		return
	}
	if err := s.cache.Put(cacheKey, entries); err != nil {
		slog.Warn("mapping cache write failed", "err", err)
	}
	s.idx, s.expires = NewIndex(entries), s.now().Add(s.ttl)
	slog.Info("mapping list refreshed", "entries", len(entries))
}

// wait blocks until a running refresh is done (tests).
func (s *Source) wait() { s.wg.Wait() }

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
