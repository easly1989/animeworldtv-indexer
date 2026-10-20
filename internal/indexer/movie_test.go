package indexer

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/easly1989/animeworldtv-indexer/internal/animeworld"
	"github.com/easly1989/animeworldtv-indexer/internal/mapping"
)

type fakeFilm struct {
	title, category string
	mal, year       int
	dub             bool
}

var films = map[string]fakeFilm{
	"/play/your-name.a":                  {"Your Name.", "Movie", 32281, 2016, false},
	"/play/your-name-ita.b":              {"Your Name.", "Movie", 32281, 2016, true},
	"/play/your-name-the-series.c":       {"Your Name: The Series", "TV", 7, 2020, false},
	"/play/a-silent-voice.d":             {"A Silent Voice", "Movie", 28851, 2016, false},
	"/play/unmapped-film.e":              {"Unmapped Film", "Movie", 0, 2021, false},
	"/play/your-name-unmapped-special.f": {"Your Name Special", "OVA", 0, 2017, false},
}

func filmHits(q string) []animeworld.Hit {
	var paths []string
	switch q {
	case "your name":
		paths = []string{"/play/your-name.a", "/play/your-name-ita.b", "/play/your-name-the-series.c", "/play/a-silent-voice.d",
			"/play/unmapped-film.e", "/play/your-name-unmapped-special.f"}
	case "Kimi no Na wa.":
		paths = []string{"/play/your-name.a", "/play/your-name-ita.b"}
	}
	var hits []animeworld.Hit
	for _, p := range paths {
		f := films[p]
		hits = append(hits, animeworld.Hit{Path: p, Title: f.title, Dub: f.dub, Movie: f.category == "Movie"})
	}
	return hits
}

func filmSite() *fakeSite {
	return &fakeSite{
		searchFn: filmHits,
		seriesFn: func(path string) animeworld.Series {
			f := films[path]
			return animeworld.Series{Path: path, Title: f.title, Dub: f.dub, MALID: f.mal, Category: f.category, Year: f.year,
				Episodes: []animeworld.Episode{{Num: "1", Token: "tok-" + path}}}
		},
	}
}

func movieMapping() *mapping.Index {
	return mapping.NewIndex([]mapping.Entry{
		{MAL: 32281, AniList: 21519, IMDB: "tt5311514", TMDB: 372058, TMDBKind: "movie", Type: "MOVIE", Slug: "your-name"},
		{MAL: 28851, IMDB: "tt5323662", TMDB: 378064, TMDBKind: "movie", Type: "MOVIE", Slug: "a-silent-voice"},
		// A TV show whose TMDB id equals the film's number must never answer a movie search.
		{MAL: 7, TVDB: 1, TMDB: 372058, TMDBKind: "tv", IMDB: "tt9999999", Type: "TV", Season: 1},
	})
}

func newMovieService(t *testing.T, site Site) *Service {
	t.Helper()
	svc := newService(t, site, 5*time.Second)
	svc.opt.Mapping = staticMapper{movieMapping()}
	svc.opt.Titles = fakeTitles{32281: "Kimi no Na wa."}
	return svc
}

func TestSearchMovieByIMDbAndTMDBId(t *testing.T) {
	want := []string{"Your Name. 2016 [SUB ITA]", "Your Name. 2016 [ITA]"}
	tests := []struct {
		name string
		q    MovieQuery
	}{
		{"imdb with tt and title", MovieQuery{Q: "your name", IMDBID: "tt5311514"}},
		{"imdb without tt", MovieQuery{Q: "your name", IMDBID: "5311514"}},
		{"tmdb", MovieQuery{Q: "your name", TMDBID: 372058}},
		{"imdb, no title: the MAL title is looked up", MovieQuery{IMDBID: "tt5311514"}},
		{"tmdb, no title", MovieQuery{TMDBID: 372058}},
		{"padded and shouting imdb", MovieQuery{IMDBID: " TT05311514 "}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rels, err := newMovieService(t, filmSite()).SearchMovie(context.Background(), tc.q, 0, 100)
			if err != nil {
				t.Fatal(err)
			}
			if got := titles(rels); !reflect.DeepEqual(got, want) {
				t.Fatalf("got %q, want %q", got, want)
			}
			for _, r := range rels {
				if !r.Movie || r.Year != 2016 || r.IMDB != "tt5311514" || r.TMDBID != 372058 || r.Season != 0 || r.TVDBID != 0 {
					t.Errorf("release wrong: %+v", r)
				}
			}
			if rels[0].Dub || !rels[1].Dub {
				t.Errorf("sub first, dub second: %+v", rels)
			}
		})
	}
}

func TestSearchMovieWithoutTitleSearchesTheMALTitleOnly(t *testing.T) {
	site := filmSite()
	if _, err := newMovieService(t, site).SearchMovie(context.Background(), MovieQuery{IMDBID: "tt5311514"}, 0, 100); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(site.queries, []string{"Kimi no Na wa."}) {
		t.Errorf("queries %q: the first keyword already found the film", site.queries)
	}
}

func TestSearchMovieNeverReturnsAFilmMappedToAnotherId(t *testing.T) {
	// "your name" also lists A Silent Voice and a TV series; neither is tt5311514 / tmdb 372058.
	for _, q := range []MovieQuery{{Q: "your name", IMDBID: "tt5311514"}, {Q: "your name", TMDBID: 372058}} {
		rels, err := newMovieService(t, filmSite()).SearchMovie(context.Background(), q, 0, 100)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range rels {
			if r.MALID != 32281 {
				t.Errorf("%+v leaked into %+v", r, q)
			}
		}
	}
}

func TestSearchMovieUnknownIDFallsBackToTheCallersTitle(t *testing.T) {
	// The id is not in the mapping: unmapped films found through q are accepted, mapped or non-film ones are not.
	rels, err := newMovieService(t, filmSite()).SearchMovie(context.Background(), MovieQuery{Q: "your name", IMDBID: "tt0000001"}, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := titles(rels), []string{"Unmapped Film 2021 [SUB ITA]"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q, want %q", got, want)
	}
	if rels[0].IMDB != "" || rels[0].TMDBID != 0 {
		t.Errorf("a guess must not claim ids: %+v", rels[0])
	}
}

func TestSearchMovieUnknownIDWithoutTitleFindsNothing(t *testing.T) {
	site := filmSite()
	rels, err := newMovieService(t, site).SearchMovie(context.Background(), MovieQuery{IMDBID: "tt0000001"}, 0, 100)
	if err != nil || len(rels) != 0 || len(site.queries) != 0 {
		t.Errorf("err=%v rels=%q queries=%q", err, titles(rels), site.queries)
	}
}

func TestSearchMovieKeywordOnlyListsFilms(t *testing.T) {
	site := filmSite()
	rels, err := newMovieService(t, site).SearchMovie(context.Background(), MovieQuery{Q: "your name"}, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"Your Name. 2016 [SUB ITA]", "Your Name. 2016 [ITA]", "A Silent Voice 2016 [SUB ITA]", "Unmapped Film 2021 [SUB ITA]"}
	if got := titles(rels); !reflect.DeepEqual(got, want) {
		t.Errorf("got %q, want %q", got, want)
	}
	if site.count("series") != 4 {
		t.Errorf("series pages of non-films must not be fetched: %d fetches", site.count("series"))
	}
	if rels[2].IMDB != "tt5323662" || rels[2].TMDBID != 378064 || rels[3].IMDB != "" {
		t.Errorf("ids come from the mapping only: %+v / %+v", rels[2], rels[3])
	}
}

func TestSearchMovieOmitsTheYearItDoesNotKnow(t *testing.T) {
	if got := movieTitle("Foo", 0, true); got != "Foo [ITA]" {
		t.Errorf("got %q", got)
	}
	if got := movieTitle("Foo", 1999, false); got != "Foo 1999 [SUB ITA]" {
		t.Errorf("got %q", got)
	}
}

func TestSearchMovieWindow(t *testing.T) {
	rels, err := newMovieService(t, filmSite()).SearchMovie(context.Background(), MovieQuery{Q: "your name"}, 1, 2)
	if err != nil || len(rels) != 2 || rels[0].Dub != true {
		t.Fatalf("err=%v rels=%q", err, titles(rels))
	}
}

func TestNormalizeIMDB(t *testing.T) {
	for in, want := range map[string]string{
		"tt5311514": "tt5311514", "5311514": "tt5311514", "TT5311514": "tt5311514", "tt123": "tt0000123",
		"": "", "tt": "", "abc": "", "0": "", "-5": "",
	} {
		if got := normalizeIMDB(in); got != want {
			t.Errorf("normalizeIMDB(%q) = %q, want %q", in, got, want)
		}
	}
}
