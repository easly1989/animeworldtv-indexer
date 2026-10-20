package indexer

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"

	"github.com/easly1989/animeworldtv-indexer/internal/animeworld"
	"github.com/easly1989/animeworldtv-indexer/internal/mapping"
)

// MovieQuery is a Torznab movie request; zero fields are unset.
type MovieQuery struct {
	Q      string
	IMDBID string // "tt5311514" or just "5311514"
	TMDBID int
}

// SearchMovie returns the AnimeWorld films answering a movie query, one release
// per sub or dub entry. With an IMDb or TMDB id only the films the mapping ties
// to it are returned (when the mapping does not know the id: the unmapped films
// found through the caller's own title); without one, the films among the
// keyword's search results.
func (s *Service) SearchMovie(ctx context.Context, mq MovieQuery, offset, limit int) ([]Release, error) {
	idx := s.mapIndex(ctx)
	imdb := normalizeIMDB(mq.IMDBID)
	byID := imdb != "" || mq.TMDBID != 0
	var want []mapping.Entry
	for _, l := range [][]mapping.Entry{idx.ByIMDB(imdb), idx.ByTMDB(mq.TMDBID)} {
		for _, e := range l {
			if !coversEntry(want, e) {
				want = append(want, e)
			}
		}
	}
	kw := strings.TrimSpace(mq.Q)
	if kw == "" && !byID { // Radarr's RSS: the latest listing has no films, so do not ask the site
		return nil, nil
	}
	films, err := s.findMovies(ctx, kw, byID, want, idx)
	if err != nil {
		return nil, err
	}

	var rels []Release
	for _, nb := range films {
		se := nb.series
		if len(se.Episodes) == 0 {
			continue
		}
		e := lookupEntry(idx, se)
		// shortcut: a film is its first episode; a multi-part film is not split, upgrade if one shows up.
		ep := se.Episodes[0]
		r := Release{Title: movieTitle(se.Title, se.Year, se.Dub), Token: ep.Token, Num: ep.Num, SeriesPath: se.Path,
			Dub: se.Dub, MALID: se.MALID, AniListID: se.AniListID, Movie: true, Year: se.Year}
		if e.IsMovie() { // a "Movie" the list files under a TV show has the show's ids: not the film's
			r.IMDB = e.IMDB
			if e.TMDBKind == "movie" {
				r.TMDBID = e.TMDB
			}
		}
		rels = append(rels, r)
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

// findMovies is find for films. kw is tried first (Prowlarr does not always
// send a title), then, for an id search, the film's MAL title and AnimePlanet
// slug, until every wanted film has turned up. Without an id only the
// listing's "Movie" results are looked at. With one, a series is accepted when
// it is a wanted film or, when the id is not in the mapping at all, an unmapped
// film found through kw.
func (s *Service) findMovies(ctx context.Context, kw string, byID bool, want []mapping.Entry, idx *mapping.Index) ([]numbered, error) {
	ctx, cancel := context.WithTimeout(ctx, findTimeout)
	defer cancel()
	var keywords []string
	if kw != "" || !byID {
		keywords = append(keywords, kw)
	}
	if len(want) > 0 {
		if kw == "" && want[0].MAL != 0 && s.opt.Titles != nil {
			keywords = appendUnique(keywords, titleKeywords(s.opt.Titles.Title(ctx, want[0].MAL))...)
		}
		keywords = appendUnique(keywords, titleKeywords(strings.ReplaceAll(want[0].Slug, "-", " "))...)
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
		var cand []animeworld.Hit
		for _, h := range hits {
			if h.Movie || byID { // the mapping, not the badge, decides an id search
				cand = append(cand, h)
			}
		}
		if len(cand) > s.opt.MaxSeries {
			cand = cand[:s.opt.MaxSeries]
		}
		for _, se := range s.seriesAll(ctx, cand) {
			if seen[se.Path] {
				continue
			}
			if byID {
				wanted := coversSeries(want, se)
				_, mapped := idx.ByMAL(se.MALID)
				if !mapped {
					_, mapped = idx.ByAniList(se.AniListID)
				}
				if !wanted && (len(want) > 0 || mapped || k != kw || se.Category != "Movie") {
					continue
				}
			}
			seen[se.Path] = true
			out = append(out, numbered{series: se})
		}
		if byID && covered(want, out) {
			break
		}
	}
	return out, nil
}

func lookupEntry(idx *mapping.Index, se animeworld.Series) mapping.Entry {
	if e, ok := idx.ByMAL(se.MALID); ok {
		return e
	}
	e, _ := idx.ByAniList(se.AniListID)
	return e
}

func coversEntry(l []mapping.Entry, e mapping.Entry) bool {
	for _, x := range l {
		if x.MAL == e.MAL && x.AniList == e.AniList {
			return true
		}
	}
	return false
}

// coversSeries reports whether a series is one of the wanted entries.
func coversSeries(want []mapping.Entry, se animeworld.Series) bool {
	for _, e := range want {
		if e.MAL != 0 && e.MAL == se.MALID || e.AniList != 0 && e.AniList == se.AniListID {
			return true
		}
	}
	return false
}

// normalizeIMDB turns "5311514", "tt5311514" or "TT5311514" into "tt5311514",
// and anything that is not an id into "".
func normalizeIMDB(s string) string {
	n, err := strconv.Atoi(strings.TrimPrefix(strings.ToLower(strings.TrimSpace(s)), "tt"))
	if err != nil || n <= 0 {
		return ""
	}
	return fmt.Sprintf("tt%07d", n)
}

// movieTitle is shaped like "Title 2016 [SUB ITA]", which Radarr parses into a
// title and a year. A film with no known year drops it.
func movieTitle(title string, year int, dub bool) string {
	lang := "SUB ITA"
	if dub {
		lang = "ITA"
	}
	if year == 0 {
		return fmt.Sprintf("%s [%s]", title, lang)
	}
	return fmt.Sprintf("%s %d [%s]", title, year, lang)
}
