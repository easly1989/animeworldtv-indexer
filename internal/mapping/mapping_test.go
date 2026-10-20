package mapping

import (
	"os"
	"reflect"
	"testing"
)

func sampleIndex(t *testing.T) *Index {
	t.Helper()
	f, err := os.Open("testdata/anime-list-sample.json")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	entries, err := Parse(f)
	if err != nil {
		t.Fatal(err)
	}
	return NewIndex(entries)
}

func TestParseKeepsOnlyUsefulFields(t *testing.T) {
	idx := sampleIndex(t)
	frieren2, ok := idx.ByMAL(59978)
	want := Entry{MAL: 59978, AniList: 182255, TVDB: 424536, Season: 2, Offset: 0,
		TMDB: 209867, TMDBKind: "tv", IMDB: "tt22248376", Type: "TV", Slug: "frieren-beyond-journeys-end-season-2"}
	if !ok || frieren2 != want {
		t.Errorf("got %+v (%v), want %+v", frieren2, ok, want)
	}
	movie, _ := idx.ByMAL(164)
	if movie.TMDB != 128 || movie.TMDBKind != "movie" || movie.IMDB != "tt0119698" || movie.TVDB != 0 {
		t.Errorf("movie ids wrong: %+v", movie)
	}
	if e, _ := idx.ByAniList(100049); e.MAL != 0 || e.TVDB != 305089 || e.Season != 0 {
		t.Errorf("entry known only by AniList id: %+v", e)
	}
	if e, _ := idx.ByMAL(197); e.Season != -1 {
		t.Errorf("a missing TVDB season must be -1, got %d", e.Season)
	}
	if e, _ := idx.ByMAL(20381); e.Offset != -1 {
		t.Errorf("negative offset lost: %+v", e)
	}
}

func TestFilmsAreFoundByIMDbAndTMDBId(t *testing.T) {
	idx := sampleIndex(t)
	for _, l := range [][]Entry{idx.ByIMDB("tt0119698"), idx.ByTMDB(128)} {
		if len(l) != 1 || l[0].MAL != 164 {
			t.Errorf("got %+v", l)
		}
	}
	// Frieren 2 is a TV show: its ids answer neither lookup (a TMDB id is only unique per kind).
	if l := idx.ByIMDB("tt22248376"); len(l) != 0 {
		t.Errorf("a TV show's IMDb id returned %+v", l)
	}
	if l := idx.ByTMDB(209867); len(l) != 0 {
		t.Errorf("a TV show's TMDB id returned %+v", l)
	}
	var nilIdx *Index
	if nilIdx.ByIMDB("tt0119698") != nil || nilIdx.ByTMDB(128) != nil {
		t.Error("a nil index must be empty")
	}
}

func TestByTVDBIsOrderedBySeasonThenOffset(t *testing.T) {
	var got [][2]int
	for _, e := range sampleIndex(t).ByTVDB(305089) {
		got = append(got, [2]int{e.Season, e.Offset})
	}
	want := [][2]int{{0, 0}, {1, 0}, {2, 0}, {2, 13}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
	if n := len(sampleIndex(t).ByTVDB(1)); n != 0 {
		t.Errorf("unknown tvdb id returned %d entries", n)
	}
}

func TestResolve(t *testing.T) {
	idx := sampleIndex(t)
	tests := []struct {
		name         string
		mal, anilist int
		title, path  string
		want         Numbering
	}{
		{"mapped season 1", 52991, 0, "Frieren: Beyond Journey's End", "/play/sousou-no-frieren.kbcPv",
			Numbering{Season: 1, Offset: 0, TVDB: 424536, Mapped: true, Title: "Frieren: Beyond Journey's End"}},
		{"mapped later season, marker stripped from the title", 59978, 0, "Frieren: Beyond Journey's End 2", "/play/frieren-beyond-journeys-end-2.JvOQx",
			Numbering{Season: 2, TVDB: 424536, Mapped: true, Title: "Frieren: Beyond Journey's End"}},
		{"second cour carries an episode offset", 42203, 0, "Re:Zero 2nd Season Part 2", "/play/rezero-2-part-2.x",
			Numbering{Season: 2, Offset: 13, TVDB: 305089, Mapped: true, Title: "Re:Zero"}},
		{"found through the AniList id", 0, 182255, "Frieren 2", "/play/f.x",
			Numbering{Season: 2, TVDB: 424536, Mapped: true, Title: "Frieren"}},
		{"mapped entry without a season falls back to the heuristic", 197, 0, "Rizelmine 3", "/play/r.x",
			Numbering{Season: 3, TVDB: 83692, Mapped: true, Title: "Rizelmine"}},
		{"unmapped, heuristic from the title", 999999, 0, "Some Show Season 4", "/play/some-show.x",
			Numbering{Season: 4, Title: "Some Show"}},
		{"unmapped, heuristic from the slug", 0, 0, "Some Show", "/play/some-show-3-ita.x",
			Numbering{Season: 3, Title: "Some Show"}},
		{"unmapped, no marker means season 1", 0, 0, "Some Show", "/play/some-show.x",
			Numbering{Season: 1, Title: "Some Show"}},
		{"a mapped number that is not the season stays in the title", 52991, 0, "Mob Psycho 100", "/play/mob-psycho-100.x",
			Numbering{Season: 1, TVDB: 424536, Mapped: true, Title: "Mob Psycho 100"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := idx.Resolve(tc.mal, tc.anilist, tc.title, tc.path); got != tc.want {
				t.Errorf("got %+v\nwant %+v", got, tc.want)
			}
		})
	}
}

func TestNilIndexResolvesByHeuristic(t *testing.T) {
	var idx *Index
	got := idx.Resolve(52991, 0, "Frieren 2", "/play/frieren-2.x")
	if got.Mapped || got.Season != 2 || got.Title != "Frieren" {
		t.Errorf("got %+v", got)
	}
	if len(idx.ByTVDB(1)) != 0 {
		t.Error("nil index must be empty")
	}
}

func TestGuessSeason(t *testing.T) {
	tests := []struct {
		title, path string
		want        int
	}{
		{"Frieren 2", "", 2},
		{"Frieren: Beyond Journey's End 3", "", 3},
		{"Show 2nd Season", "", 2},
		{"Show 3rd Season", "", 3},
		{"Show 4th Season", "", 4},
		{"Show Second Season", "", 2},
		{"Show third season", "", 3},
		{"Show Fourth Season", "", 4},
		{"Show Season 2", "", 2},
		{"Show season 5", "", 5},
		{"Overlord II", "", 2},
		{"Overlord IV", "", 4},
		{"Re:Zero 2nd Season Part 2", "", 2},
		{"Show Part 2", "", 1},
		{"Show", "", 1},
		{"Mob Psycho 100", "", 1},
		{"Gintama 2015", "", 1},
		{"Show 1", "", 1},
		{"Konosuba", "/play/konosuba-2.AbC", 2},
		{"Konosuba", "/play/konosuba-3-ita.AbC", 3},
		{"Konosuba", "/play/konosuba-season-2.AbC", 2},
		{"Konosuba", "/play/konosuba-2nd-season-subita.AbC", 2},
		{"Konosuba", "/play/konosuba-100.AbC", 1},
		{"Show 2", "/play/show.AbC", 2}, // the title wins over the slug
		{"", "", 1},
	}
	for _, tc := range tests {
		if got := GuessSeason(tc.title, tc.path); got != tc.want {
			t.Errorf("GuessSeason(%q, %q) = %d, want %d", tc.title, tc.path, got, tc.want)
		}
	}
}

func TestStripSeason(t *testing.T) {
	tests := []struct {
		title  string
		season int
		want   string
	}{
		{"Frieren 2", 2, "Frieren"},
		{"Frieren 2", 3, "Frieren 2"}, // the marker is not this season's
		{"Show: 2nd Season", 2, "Show"},
		{"Show - Season 3", 3, "Show"},
		{"Overlord III", 3, "Overlord"},
		{"Show Second Season", 2, "Show"},
		{"Show Part 2", 1, "Show"},
		{"Show Season 2 - Cour 2", 2, "Show"},
		{"Show", 2, "Show"},
		{"Mob Psycho 100", 1, "Mob Psycho 100"},
	}
	for _, tc := range tests {
		if got := StripSeason(tc.title, tc.season); got != tc.want {
			t.Errorf("StripSeason(%q, %d) = %q, want %q", tc.title, tc.season, got, tc.want)
		}
	}
}
