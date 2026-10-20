// Package indexer turns AnimeWorld searches into per-episode releases. It owns
// caching and request politeness; the HTTP and HTML details live in animeworld.
package indexer

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/easly1989/animeworldtv-indexer/internal/animeworld"
)

const (
	searchTTL  = 10 * time.Minute
	seriesTTL  = time.Hour // airing series gain episodes
	metaTTL    = 30 * 24 * time.Hour
	defaultMiB = 300 // size guess when nothing at all could be measured
)

// Site is the part of animeworld.Client the service uses.
type Site interface {
	Search(ctx context.Context, q string) ([]animeworld.Hit, error)
	Series(ctx context.Context, path string) (animeworld.Series, error)
	FileURL(ctx context.Context, token string) (string, error)
	FileMeta(ctx context.Context, fileURL string) (animeworld.FileMeta, error)
}

// Cache is the part of store.Store the service uses.
type Cache interface {
	Get(key string, maxAge time.Duration, v any) bool
	Put(key string, v any) error
}

// Options bound how hard a search may hit the site.
type Options struct {
	MaxSeries   int           // series expanded per search
	Concurrency int           // simultaneous requests to the site
	Budget      time.Duration // how long a search waits for file sizes
}

// Release is one downloadable episode.
type Release struct {
	Title      string
	Token      string // episode token, what /api/episode/info takes
	Num        string
	SeriesPath string
	Dub        bool
	MALID      int
	AniListID  int
	Size       int64
	Estimated  bool // Size is a guess; the real one is still being fetched
	Published  time.Time
}

type call struct {
	done chan struct{}
	meta animeworld.FileMeta
	err  error
}

type Service struct {
	site  Site
	cache Cache
	opt   Options
	sem   chan struct{}

	mu       sync.Mutex
	inflight map[string]*call
}

func New(site Site, cache Cache, opt Options) *Service {
	if opt.Concurrency < 1 {
		opt.Concurrency = 1
	}
	return &Service{site: site, cache: cache, opt: opt, sem: make(chan struct{}, opt.Concurrency), inflight: map[string]*call{}}
}

// FileURL resolves an episode token to its direct MP4 URL. Never cached: the
// file servers may rotate.
func (s *Service) FileURL(ctx context.Context, token string) (string, error) {
	s.sem <- struct{}{}
	defer func() { <-s.sem }()
	return s.site.FileURL(ctx, token)
}

// Search returns the releases matching q (latest updates when q is empty),
// newest episode first, windowed by offset/limit. Only the window's sizes are
// resolved.
func (s *Service) Search(ctx context.Context, q string, offset, limit int) ([]Release, error) {
	hits, err := s.hits(ctx, q)
	if err != nil {
		return nil, err
	}
	if len(hits) > s.opt.MaxSeries {
		hits = hits[:s.opt.MaxSeries]
	}
	series := s.seriesAll(ctx, hits)

	var rels []Release
	for _, se := range series {
		for i := len(se.Episodes) - 1; i >= 0; i-- {
			e := se.Episodes[i]
			rels = append(rels, Release{
				Title: releaseTitle(se.Title, e.Num, se.Dub), Token: e.Token, Num: e.Num,
				SeriesPath: se.Path, Dub: se.Dub, MALID: se.MALID, AniListID: se.AniListID,
			})
		}
	}
	if offset > len(rels) {
		offset = len(rels)
	}
	rels = rels[offset:]
	if limit > 0 && limit < len(rels) {
		rels = rels[:limit]
	}
	return rels, s.fillSizes(ctx, rels)
}

func (s *Service) hits(ctx context.Context, q string) ([]animeworld.Hit, error) {
	key := "search:" + strings.ToLower(strings.TrimSpace(q))
	var hits []animeworld.Hit
	if s.cache.Get(key, searchTTL, &hits) {
		return hits, nil
	}
	s.sem <- struct{}{}
	hits, err := s.site.Search(ctx, q)
	<-s.sem
	if err != nil {
		return nil, err
	}
	s.put(key, hits)
	return hits, nil
}

// seriesAll fetches the hits' series pages concurrently and keeps hit order.
// A series that fails is logged and skipped so one bad page does not sink a search.
func (s *Service) seriesAll(ctx context.Context, hits []animeworld.Hit) []animeworld.Series {
	out := make([]*animeworld.Series, len(hits))
	var wg sync.WaitGroup
	for i, h := range hits {
		wg.Add(1)
		go func() {
			defer wg.Done()
			key := "series:" + h.Path
			var se animeworld.Series
			if s.cache.Get(key, seriesTTL, &se) {
				out[i] = &se
				return
			}
			s.sem <- struct{}{}
			se, err := s.site.Series(ctx, h.Path)
			<-s.sem
			if err != nil {
				slog.Warn("series fetch failed", "path", h.Path, "err", err)
				return
			}
			s.put(key, se)
			out[i] = &se
		}()
	}
	wg.Wait()
	var res []animeworld.Series
	for _, se := range out {
		if se != nil {
			res = append(res, *se)
		}
	}
	return res
}

// fillSizes sets Size and Published on every release. Sizes not cached are
// fetched concurrently; whatever is still missing when the budget runs out (or
// fails) gets an estimate, while the fetches keep going in the background so
// the next search finds them cached.
func (s *Service) fillSizes(ctx context.Context, rels []Release) error {
	calls := make([]*call, len(rels))
	for i, r := range rels {
		var m animeworld.FileMeta
		if s.cache.Get("meta:"+r.Token, metaTTL, &m) {
			rels[i].Size, rels[i].Published = m.Size, m.Modified
			continue
		}
		calls[i] = s.fetchMeta(ctx, r.Token)
	}

	timer := time.NewTimer(s.opt.Budget)
	defer timer.Stop()
wait:
	for _, c := range calls {
		if c == nil {
			continue
		}
		select {
		case <-c.done:
		case <-timer.C:
			break wait
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	for i, c := range calls {
		if c == nil {
			continue
		}
		select {
		case <-c.done:
			if c.err == nil {
				rels[i].Size, rels[i].Published = c.meta.Size, c.meta.Modified
			}
		default: // still running: estimated below
		}
	}
	estimate(rels)
	return nil
}

// estimate gives every release without a measured size the median of its
// series, else of the whole result, else a fixed guess.
func estimate(rels []Release) {
	bySeries := map[string][]int64{}
	var all []int64
	for _, r := range rels {
		if r.Size > 0 {
			bySeries[r.SeriesPath] = append(bySeries[r.SeriesPath], r.Size)
			all = append(all, r.Size)
		}
	}
	for i := range rels {
		if rels[i].Size > 0 {
			continue
		}
		rels[i].Estimated = true
		switch {
		case len(bySeries[rels[i].SeriesPath]) > 0:
			rels[i].Size = median(bySeries[rels[i].SeriesPath])
		case len(all) > 0:
			rels[i].Size = median(all)
		default:
			rels[i].Size = defaultMiB << 20
		}
		if rels[i].Published.IsZero() {
			rels[i].Published = time.Now()
		}
	}
}

func median(v []int64) int64 {
	c := append([]int64(nil), v...)
	sort.Slice(c, func(i, j int) bool { return c[i] < c[j] })
	return c[len(c)/2]
}

// fetchMeta starts (or joins) the lookup of one episode's file size. It runs
// detached from the request so an abandoned search still warms the cache.
func (s *Service) fetchMeta(ctx context.Context, token string) *call {
	s.mu.Lock()
	defer s.mu.Unlock()
	if c := s.inflight[token]; c != nil {
		return c
	}
	c := &call{done: make(chan struct{})}
	s.inflight[token] = c
	bg := context.WithoutCancel(ctx)
	go func() {
		s.sem <- struct{}{}
		c.meta, c.err = s.loadMeta(bg, token)
		<-s.sem
		if c.err != nil {
			slog.Warn("file size lookup failed", "episode", token, "err", c.err)
		}
		s.mu.Lock()
		delete(s.inflight, token)
		s.mu.Unlock()
		close(c.done)
	}()
	return c
}

func (s *Service) loadMeta(ctx context.Context, token string) (animeworld.FileMeta, error) {
	u, err := s.site.FileURL(ctx, token)
	if err != nil {
		return animeworld.FileMeta{}, err
	}
	m, err := s.site.FileMeta(ctx, u)
	if err != nil {
		return m, err
	}
	s.put("meta:"+token, m)
	return m, nil
}

func (s *Service) put(key string, v any) {
	if err := s.cache.Put(key, v); err != nil {
		slog.Warn("cache write failed", "key", key, "err", err)
	}
}

// releaseTitle is shaped like "Title - 01 [SUB ITA]": the absolute-number
// pattern Sonarr already parses for anime.
func releaseTitle(title, num string, dub bool) string {
	lang := "SUB ITA"
	if dub {
		lang = "ITA"
	}
	if n, err := strconv.Atoi(num); err == nil {
		num = fmt.Sprintf("%02d", n)
	}
	return fmt.Sprintf("%s - %s [%s]", title, num, lang)
}
