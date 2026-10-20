// Package mapping translates AnimeWorld series (identified by their MyAnimeList
// and AniList ids) into TVDB numbering: which TVDB series, which season and how
// far the episode numbers are shifted. The data comes from the Fribb/anime-lists
// project; when it has nothing for a series, season numbers are guessed from
// the title.
package mapping

import (
	"encoding/json"
	"io"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Entry is one anime in the mapping list.
type Entry struct {
	MAL      int    `json:"mal,omitempty"`
	AniList  int    `json:"anilist,omitempty"`
	TVDB     int    `json:"tvdb,omitempty"`
	Season   int    `json:"season"` // TVDB season, -1 when the list has none
	Offset   int    `json:"offset,omitempty"`
	TMDB     int    `json:"tmdb,omitempty"`
	TMDBKind string `json:"tmdb_kind,omitempty"` // "tv" or "movie"
	IMDB     string `json:"imdb,omitempty"`
	Type     string `json:"type,omitempty"` // TV, MOVIE, OVA, ...
	Slug     string `json:"slug,omitempty"` // AnimePlanet slug, a rough title for searching
}

// rawEntry is the subset of Fribb's anime-list-full.json we read.
type rawEntry struct {
	Type    string                     `json:"type"`
	MAL     int                        `json:"mal_id"`
	AniList int                        `json:"anilist_id"`
	TVDB    int                        `json:"tvdb_id"`
	Slug    string                     `json:"anime-planet_id"`
	IMDB    []string                   `json:"imdb_id"`
	TMDB    map[string]json.RawMessage `json:"themoviedb_id"` // {"tv": 1} or {"movie": [1]}
	Season  struct {
		TVDB *int `json:"tvdb"`
	} `json:"season"`
	Offset struct {
		TVDB int `json:"tvdb"`
	} `json:"episode_offset"`
}

// Parse reads anime-list-full.json, keeping entries that can be matched to an
// AnimeWorld series (they have a MAL or AniList id) and that carry a TVDB, TMDB
// or IMDb id.
func Parse(r io.Reader) ([]Entry, error) {
	var raws []rawEntry
	if err := json.NewDecoder(r).Decode(&raws); err != nil {
		return nil, err
	}
	var out []Entry
	for _, r := range raws {
		if r.MAL == 0 && r.AniList == 0 {
			continue
		}
		e := Entry{MAL: r.MAL, AniList: r.AniList, TVDB: r.TVDB, Season: -1, Offset: r.Offset.TVDB, Type: r.Type, Slug: r.Slug}
		if r.Season.TVDB != nil {
			e.Season = *r.Season.TVDB
		}
		if len(r.IMDB) > 0 {
			e.IMDB = r.IMDB[0]
		}
		for kind, raw := range r.TMDB {
			e.TMDBKind, e.TMDB = kind, firstInt(raw)
		}
		if e.TVDB == 0 && e.TMDB == 0 && e.IMDB == "" {
			continue
		}
		out = append(out, e)
	}
	return out, nil
}

// firstInt reads either a number or a list of numbers.
func firstInt(raw json.RawMessage) int {
	var n int
	if json.Unmarshal(raw, &n) == nil {
		return n
	}
	var l []int
	if json.Unmarshal(raw, &l) == nil && len(l) > 0 {
		return l[0]
	}
	return 0
}

// Index answers lookups over a parsed list. A nil *Index is valid and empty.
type Index struct {
	byMAL, byAniList map[int]Entry
	byTVDB           map[int][]Entry
}

func NewIndex(entries []Entry) *Index {
	idx := &Index{byMAL: map[int]Entry{}, byAniList: map[int]Entry{}, byTVDB: map[int][]Entry{}}
	for _, e := range entries {
		if e.MAL != 0 {
			idx.byMAL[e.MAL] = e
		}
		if e.AniList != 0 {
			idx.byAniList[e.AniList] = e
		}
		if e.TVDB != 0 {
			idx.byTVDB[e.TVDB] = append(idx.byTVDB[e.TVDB], e)
		}
	}
	for _, l := range idx.byTVDB {
		sort.SliceStable(l, func(i, j int) bool {
			if l[i].Season != l[j].Season {
				return l[i].Season < l[j].Season
			}
			return l[i].Offset < l[j].Offset
		})
	}
	return idx
}

func (i *Index) ByMAL(id int) (Entry, bool) {
	if i == nil {
		return Entry{}, false
	}
	e, ok := i.byMAL[id]
	return e, ok && id != 0
}

func (i *Index) ByAniList(id int) (Entry, bool) {
	if i == nil {
		return Entry{}, false
	}
	e, ok := i.byAniList[id]
	return e, ok && id != 0
}

// ByTVDB returns every entry of a TVDB series, ordered by season then offset.
func (i *Index) ByTVDB(id int) []Entry {
	if i == nil {
		return nil
	}
	return i.byTVDB[id]
}

// Numbering says how an AnimeWorld series maps onto TVDB: the episode it calls
// number N is TVDB episode N+Offset of Season.
type Numbering struct {
	Season, Offset int
	TVDB           int    // 0 when the series is not in the mapping
	Mapped         bool   // Season/Offset come from the mapping, not from the title
	Title          string // the series title without its season marker, as Sonarr knows the show
}

// Resolve finds the numbering of a series by its external ids, falling back to
// the season number found in its title or path.
func (i *Index) Resolve(mal, anilist int, title, path string) Numbering {
	e, ok := i.ByMAL(mal)
	if !ok {
		e, ok = i.ByAniList(anilist)
	}
	n := Numbering{Season: GuessSeason(title, path)}
	if ok && e.TVDB != 0 {
		n.Mapped, n.TVDB, n.Offset = true, e.TVDB, e.Offset
		if e.Season >= 0 {
			n.Season = e.Season
		}
	}
	n.Title = StripSeason(title, n.Season)
	return n
}

// A trailing season marker: "Season 2", "2nd Season", "II", or a bare number.
// A bare number is only trusted up to 20 so "Mob Psycho 100" is left alone.
var seasonRe = regexp.MustCompile(`(?i)^(.*?\S)[\s:–-]+(?:season\s+(\d+)|(\d+)(?:st|nd|rd|th)\s+season|(\d+)|(ii|iii|iv))$`)

var (
	romans    = map[string]int{"ii": 2, "iii": 3, "iv": 4}
	partWords = regexp.MustCompile(`(?i)\b(part|cour|vol\.?|volume|episode|ep\.?|no\.?|#)$`)
)

// splitSeason cuts a trailing season marker off s.
func splitSeason(s string) (rest string, season int) {
	s = strings.TrimSpace(s)
	m := seasonRe.FindStringSubmatch(s)
	if m == nil {
		return s, 0
	}
	switch {
	case m[2] != "":
		season, _ = strconv.Atoi(m[2])
	case m[3] != "":
		season, _ = strconv.Atoi(m[3])
	case m[4] != "":
		season, _ = strconv.Atoi(m[4])
		if partWords.MatchString(m[1]) { // "Part 2" is a cour, not a season
			return s, 0
		}
	default:
		season = romans[strings.ToLower(m[5])]
	}
	if season < 1 || season > 20 {
		return s, 0
	}
	return m[1], season
}

// A trailing "Part 2" / "Cour 2" is a split cour of the same season: Sonarr's
// show title does not carry it.
var partSuffix = regexp.MustCompile(`(?i)[\s:–-]+(?:part|cour)\s+(?:\d+|[ivx]+)$`)

var slugTail = regexp.MustCompile(`(?:-(?:sub)?ita)?\.[^./]*$`)

// GuessSeason reads the season number from a title ("Frieren 2", "Show 2nd
// Season", "Overlord III"), then from the series path slug. No marker means 1.
func GuessSeason(title, path string) int {
	if _, n := splitSeason(partSuffix.ReplaceAllString(title, "")); n > 0 {
		return n
	}
	slug := strings.TrimPrefix(path, "/play/")
	slug = strings.ReplaceAll(slugTail.ReplaceAllString(slug, ""), "-", " ")
	if _, n := splitSeason(slug); n > 0 {
		return n
	}
	return 1
}

// StripSeason removes the title's trailing season marker when it names the given
// season, so "Frieren 2" in season 2 becomes "Frieren" for Sonarr to match. A
// trailing "Part N" is dropped as well.
func StripSeason(title string, season int) string {
	title = strings.TrimSpace(partSuffix.ReplaceAllString(title, ""))
	if rest, n := splitSeason(title); n > 0 && n == season {
		return rest
	}
	return title
}
