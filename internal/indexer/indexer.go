// Package indexer turns AnimeWorld searches into per-episode releases. It owns
// caching and request politeness; the HTTP and HTML details live in animeworld.
package indexer

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/easly1989/animeworldtv-indexer/internal/animeworld"
	"github.com/easly1989/animeworldtv-indexer/internal/mapping"
)

const (
	searchTTL  = 10 * time.Minute
	seriesTTL  = time.Hour       // airing series gain episodes
	latestTTL  = 5 * time.Minute // the /updated listing; RSS polls every ~15 min
	idsTTL     = 24 * time.Hour  // a series' MAL/AniList ids are all the latest feed reads from its page
	metaTTL    = 30 * 24 * time.Hour
	defaultMiB = 300 // size guess when nothing at all could be measured
)

// Site is the part of animeworld.Client the service uses.
type Site interface {
	Search(ctx context.Context, q string) ([]animeworld.Hit, error)
	Updated(ctx context.Context) ([]animeworld.Update, error)
	Series(ctx context.Context, path string) (animeworld.Series, error)
	FileURL(ctx context.Context, token string) (string, error)
	FileMeta(ctx context.Context, fileURL string) (animeworld.FileMeta, error)
}

// Cache is the part of store.Store the service uses.
type Cache interface {
	Get(key string, maxAge time.Duration, v any) bool
	Put(key string, v any) error
}

// Mapper supplies the AnimeWorld to TVDB mapping (mapping.Source).
type Mapper interface {
	Index(ctx context.Context) *mapping.Index
}

// Options bound how hard a search may hit the site.
type Options struct {
	MaxSeries   int           // series expanded per search
	Concurrency int           // simultaneous requests to the site
	Budget      time.Duration // how long a search waits for file sizes
	Mapping     Mapper        // nil: season numbers come from titles only
	Titles      Titler        // nil: a TVDB search without a title falls back to the AnimePlanet slug
}

// Titler gives the MAL title of a series (mapping.Titles).
type Titler interface {
	Title(ctx context.Context, mal int) string
}

// TVQuery is a Torznab tvsearch request; zero fields are unset.
type TVQuery struct {
	Q      string
	TVDBID int
	Season int
	Ep     int
}

// Release is one downloadable episode.
type Release struct {
	Title      string
	Token      string // episode token, what /api/episode/info takes
	Num        string // the site's episode number
	Season     int    // TVDB numbering; both 0 when the site's number is not an integer
	Episode    int
	TVDBID     int // 0 when the series is not in the mapping
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

// Search returns the releases matching q (the latest episodes when q is empty),
// newest episode first, windowed by offset/limit. Only the window's sizes are
// resolved. A trailing "S02E03" in q selects that season and episode.
func (s *Service) Search(ctx context.Context, q string, offset, limit int) ([]Release, error) {
	return s.SearchTV(ctx, TVQuery{Q: q}, offset, limit)
}

// SearchTV is Search for a TV query. With a TVDB id only series the mapping
// ties to that id are returned (plus unmapped ones when a title was given, which
// get their season from the title); Season and Ep, when set, keep only that
// TVDB season and episode. With neither a title nor a TVDB id it is the RSS
// feed: the site's latest episodes (see latest).
func (s *Service) SearchTV(ctx context.Context, tq TVQuery, offset, limit int) ([]Release, error) {
	kw, season, ep := splitQuery(tq.Q)
	if tq.Season > 0 {
		season = tq.Season
	}
	if tq.Ep > 0 {
		ep = tq.Ep
	}
	if kw == "" && tq.TVDBID == 0 {
		return s.latest(ctx, season, ep, offset, limit)
	}
	idx := s.mapIndex(ctx)
	series, err := s.find(ctx, kw, tq.TVDBID, season, idx)
	if err != nil {
		return nil, err
	}

	var rels []Release
	for _, nb := range series {
		se := nb.series
		for i := len(se.Episodes) - 1; i >= 0; i-- {
			if r, ok := newRelease(nb, se.Episodes[i], season, ep); ok {
				rels = append(rels, r)
			}
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

// newRelease numbers and titles one episode of a series, or reports false when
// it must not be offered: before TVDB's first episode, or not the requested
// season (0: any) and episode (0: any).
func newRelease(nb numbered, e animeworld.Episode, season, ep int) (Release, bool) {
	se := nb.series
	r := Release{Token: e.Token, Num: e.Num, SeriesPath: se.Path, Dub: se.Dub,
		MALID: se.MALID, AniListID: se.AniListID, TVDBID: nb.TVDB}
	n, err := strconv.Atoi(e.Num)
	switch {
	case err == nil && n+nb.Offset >= 1:
		r.Season, r.Episode = nb.Season, n+nb.Offset
	case err == nil:
		return r, false // before TVDB's first episode: never emit E00
	case season > 0 || ep > 0:
		return r, false // no TVDB number to compare with the request
	}
	if season > 0 && r.Season != season || ep > 0 && r.Episode != ep {
		return r, false
	}
	r.Title = releaseTitle(nb.Title, r.Season, r.Episode, e.Num, se.Dub)
	return r, true
}

// latest answers an RSS sync: the episodes in the site's /updated listing
// (cached for latestTTL), numbered like a TV search, newest first. The listing
// names the episode, so only the window's series pages are read, for their
// external ids and from a long-lived cache, and sizes come from the usual
// cached lookups. A call starts at most 2*MaxSeries uncached series pages and
// as many size lookups, newest first, so a cold cache warms over a few calls
// instead of hammering the site. An episode whose series page is missing is
// left out until a later call: guessing its numbering could grab the wrong
// episode.
func (s *Service) latest(ctx context.Context, season, ep, offset, limit int) ([]Release, error) {
	ups, err := s.updates(ctx)
	if err != nil {
		return nil, err
	}
	offset = min(offset, len(ups))
	ups = ups[offset:]
	if limit > 0 && limit < len(ups) {
		ups = ups[:limit]
	}

	var hits []animeworld.Hit
	for _, u := range ups {
		if !slices.ContainsFunc(hits, func(h animeworld.Hit) bool { return h.Path == u.Path }) {
			hits = append(hits, animeworld.Hit{Path: u.Path})
		}
	}
	idx := s.mapIndex(ctx)
	byPath := map[string]numbered{}
	for _, se := range s.seriesAllTTL(ctx, hits, idsTTL, 2*s.opt.MaxSeries) {
		byPath[se.Path] = numbered{se, idx.Resolve(se.MALID, se.AniListID, se.Title, se.Path)}
	}

	var rels []Release
	for _, u := range ups {
		nb, ok := byPath[u.Path]
		if !ok {
			continue
		}
		if r, ok := newRelease(nb, animeworld.Episode{Num: u.Num, Token: u.Token}, season, ep); ok {
			r.Published = s.firstSeen(u.Token) // until the file's own date is known
			rels = append(rels, r)
		}
	}
	err = s.fillSizesN(ctx, rels, 2*s.opt.MaxSeries)
	sort.SliceStable(rels, func(i, j int) bool { return rels[i].Published.After(rels[j].Published) })
	return rels, err
}

func (s *Service) updates(ctx context.Context) ([]animeworld.Update, error) {
	var ups []animeworld.Update
	if s.cache.Get("updated", latestTTL, &ups) {
		return ups, nil
	}
	s.sem <- struct{}{}
	ups, err := s.site.Updated(ctx)
	<-s.sem
	if err != nil {
		return nil, err
	}
	s.put("updated", ups)
	return ups, nil
}

// firstSeen is when this service first listed the episode: a stand-in for the
// publish time when the file's Last-Modified is not (yet) known.
func (s *Service) firstSeen(token string) time.Time {
	var t time.Time
	if s.cache.Get("seen:"+token, metaTTL, &t) {
		return t
	}
	t = time.Now().UTC().Truncate(time.Second)
	s.put("seen:"+token, t)
	return t
}

// numbered is a series together with how it maps onto TVDB.
type numbered struct {
	series animeworld.Series
	mapping.Numbering
}

// findTimeout bounds one whole find: several keywords, each a search plus its
// series pages, must still answer before Prowlarr gives up.
var findTimeout = 60 * time.Second

// find returns the series answering a query. Without a TVDB id that is the
// site's own search for kw. With one, kw is tried first (Prowlarr does not
// always send a title), then the wanted season's MAL title and AnimePlanet slug,
// from the full name down to the first word, until every wanted season has
// turned up. Series are accepted when mapped to tvdb, or, from kw only, unmapped.
func (s *Service) find(ctx context.Context, kw string, tvdb, season int, idx *mapping.Index) ([]numbered, error) {
	ctx, cancel := context.WithTimeout(ctx, findTimeout)
	defer cancel()
	var want []mapping.Entry
	var keywords []string
	if kw != "" || tvdb == 0 {
		keywords = append(keywords, kw)
	}
	if tvdb != 0 {
		all := idx.ByTVDB(tvdb)
		for _, e := range all {
			if season > 0 && e.Season == season || season == 0 && e.Season >= 1 {
				want = append(want, e)
			}
		}
		// The title comes from a wanted entry, else the first real season; the
		// group's first entry is often a season 0 special or a movie.
		src, ok := mapping.Entry{}, false
		if len(want) > 0 {
			src, ok = want[0], true
		} else if i := slices.IndexFunc(all, func(e mapping.Entry) bool { return e.Season >= 1 }); i >= 0 {
			src, ok = all[i], true
		}
		if ok && kw == "" && src.MAL != 0 && s.opt.Titles != nil {
			keywords = appendUnique(keywords, titleKeywords(s.opt.Titles.Title(ctx, src.MAL))...)
		}
		if ok {
			keywords = appendUnique(keywords, titleKeywords(strings.ReplaceAll(src.Slug, "-", " "))...)
		}
	}

	var out []numbered
	seen := map[string]bool{}
	for _, k := range keywords {
		hits, err := s.hits(ctx, k)
		if err != nil {
			if len(out) == 0 {
				return nil, err
			}
			slog.Warn("fallback search failed, keeping what was found", "q", k, "err", err)
			break
		}
		if len(hits) > s.opt.MaxSeries {
			hits = hits[:s.opt.MaxSeries]
		}
		for _, se := range s.seriesAll(ctx, hits) {
			nb := numbered{series: se, Numbering: idx.Resolve(se.MALID, se.AniListID, se.Title, se.Path)}
			// Mapped to another show: never ours. Unmapped: only on the caller's own title.
			if seen[se.Path] || tvdb != 0 && nb.TVDB != tvdb && (nb.TVDB != 0 || k != kw) {
				continue
			}
			seen[se.Path] = true
			out = append(out, nb)
		}
		if covered(want, out) {
			break
		}
	}
	return out, nil
}

// covered reports whether every wanted entry has a series among found.
func covered(want []mapping.Entry, found []numbered) bool {
	if len(want) == 0 {
		return true
	}
	for _, e := range want {
		ok := false
		for _, f := range found {
			if e.MAL != 0 && e.MAL == f.series.MALID || e.AniList != 0 && e.AniList == f.series.AniListID {
				ok = true
				break
			}
		}
		if !ok {
			return false
		}
	}
	return true
}

// titleKeywords turns a title or slug into search terms, longest first: the
// title without its season marker, its part before a colon, then its first
// three, two and one words. The site's search trips on punctuation and on
// seasonal suffixes, so the short forms are the safety net.
func titleKeywords(t string) []string {
	t = mapping.StripSeason(strings.TrimSpace(t), mapping.GuessSeason(t, ""))
	if t == "" {
		return nil
	}
	out := []string{t}
	if i := strings.Index(t, ":"); i >= 4 {
		out = appendUnique(out, t[:i])
	}
	w := strings.Fields(t)
	for _, n := range []int{3, 2, 1} {
		if n < len(w) {
			out = appendUnique(out, strings.TrimRight(strings.Join(w[:n], " "), ":-,;"))
		}
	}
	return out
}

func appendUnique(l []string, items ...string) []string {
	for _, it := range items {
		if !slices.Contains(l, it) {
			l = append(l, it)
		}
	}
	return l
}

// trailingEp matches a trailing "S02E03" in a query.
var trailingEp = regexp.MustCompile(`(?i)^(.*\S)\s+S(\d{1,2})E(\d{1,3})$`)

// splitQuery separates the "S02E03" Sonarr appends to a title search from the
// title. Other trailing numbers ("Kaiju No. 8", "Mob Psycho 100") may belong to
// the title and reach the site untouched.
func splitQuery(q string) (kw string, season, ep int) {
	q = strings.TrimSpace(q)
	m := trailingEp.FindStringSubmatch(q)
	if m == nil {
		return q, 0, 0
	}
	season, _ = strconv.Atoi(m[2])
	ep, _ = strconv.Atoi(m[3])
	return m[1], season, ep
}

func (s *Service) mapIndex(ctx context.Context) *mapping.Index {
	if s.opt.Mapping == nil {
		return nil
	}
	return s.opt.Mapping.Index(ctx)
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
	return s.seriesAllTTL(ctx, hits, seriesTTL, 0)
}

// seriesAllTTL is seriesAll accepting cached pages up to ttl old and fetching
// at most maxFetch uncached ones (0: no limit), in hit order; the rest are
// left out.
func (s *Service) seriesAllTTL(ctx context.Context, hits []animeworld.Hit, ttl time.Duration, maxFetch int) []animeworld.Series {
	out := make([]*animeworld.Series, len(hits))
	var wg sync.WaitGroup
	fetching := 0
	for i, h := range hits {
		key := "series:" + h.Path
		var se animeworld.Series
		if s.cache.Get(key, ttl, &se) {
			out[i] = &se
			continue
		}
		if maxFetch > 0 && fetching == maxFetch {
			continue
		}
		fetching++
		wg.Add(1)
		go func() {
			defer wg.Done()
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
	return s.fillSizesN(ctx, rels, 0)
}

// fillSizesN is fillSizes starting at most maxFetch lookups (0: no limit),
// in release order; the others are estimated and measured by a later call.
func (s *Service) fillSizesN(ctx context.Context, rels []Release, maxFetch int) error {
	calls := make([]*call, len(rels))
	fetching := 0
	for i, r := range rels {
		var m animeworld.FileMeta
		if s.cache.Get("meta:"+r.Token, metaTTL, &m) {
			rels[i].setMeta(m)
			continue
		}
		if maxFetch > 0 && fetching == maxFetch {
			continue
		}
		fetching++
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
				rels[i].setMeta(c.meta)
			}
		default: // still running: estimated below
		}
	}
	estimate(rels)
	return nil
}

// setMeta takes the measured size, and the file's date when it has one.
func (r *Release) setMeta(m animeworld.FileMeta) {
	r.Size = m.Size
	if !m.Modified.IsZero() {
		r.Published = m.Modified
	}
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

// releaseTitle is shaped like "Title S02E03 [SUB ITA]", which Sonarr parses
// for standard and Anime-type series alike. An episode with no TVDB number (a
// non-integer one such as 12.5) keeps the site's number: "Title - 12.5 [SUB ITA]".
func releaseTitle(title string, season, ep int, num string, dub bool) string {
	lang := "SUB ITA"
	if dub {
		lang = "ITA"
	}
	if season == 0 && ep == 0 {
		if n, err := strconv.Atoi(num); err == nil {
			num = fmt.Sprintf("%02d", n)
		}
		return fmt.Sprintf("%s - %s [%s]", title, num, lang)
	}
	return fmt.Sprintf("%s S%02dE%02d [%s]", title, season, ep, lang)
}
