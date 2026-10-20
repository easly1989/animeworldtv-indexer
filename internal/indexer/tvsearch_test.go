package indexer

import (
	"context"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/easly1989/animeworldtv-indexer/internal/animeworld"
	"github.com/easly1989/animeworldtv-indexer/internal/mapping"
)

type staticMapper struct{ idx *mapping.Index }

func (m staticMapper) Index(context.Context) *mapping.Index { return m.idx }

type fakeSeries struct {
	title string
	mal   int
	eps   int
	dub   bool
}

var world = map[string]fakeSeries{
	"/play/sousou-no-frieren.a":                 {"Frieren: Beyond Journey's End", 52991, 28, false},
	"/play/frieren-beyond-journeys-end-ita.b":   {"Frieren: Beyond Journey's End", 52991, 28, true},
	"/play/frieren-beyond-journeys-end-2.c":     {"Frieren: Beyond Journey's End 2", 59978, 4, false},
	"/play/frieren-beyond-journeys-end-2-ita.d": {"Frieren: Beyond Journey's End 2", 59978, 4, true},
	"/play/black-clover.e":                      {"Black Clover", 34572, 3, false},
	"/play/rezero-2.f":                          {"Re:Zero 2nd Season", 39587, 13, false},
	"/play/rezero-2-part-2.g":                   {"Re:Zero 2nd Season Part 2", 42203, 12, false},
	"/play/foo-2.h":                             {"Foo 2", 0, 5, false},
}

func worldSite(onlyFor ...string) *fakeSite {
	return &fakeSite{
		searchFn: func(q string) []animeworld.Hit {
			for _, ok := range onlyFor {
				if q == ok {
					return worldHits(q)
				}
			}
			return nil
		},
		seriesFn: func(path string) animeworld.Series {
			w := world[path]
			s := animeworld.Series{Path: path, Title: w.title, Dub: w.dub, MALID: w.mal}
			for i := 1; i <= w.eps; i++ {
				s.Episodes = append(s.Episodes, animeworld.Episode{Num: fmt.Sprint(i), Token: fmt.Sprintf("%s-%d", path, i)})
			}
			return s
		},
	}
}

func worldHits(q string) []animeworld.Hit {
	var paths []string
	switch q {
	case "frieren":
		paths = []string{"/play/sousou-no-frieren.a", "/play/frieren-beyond-journeys-end-ita.b",
			"/play/frieren-beyond-journeys-end-2.c", "/play/frieren-beyond-journeys-end-2-ita.d", "/play/black-clover.e"}
	case "rezero":
		paths = []string{"/play/rezero-2.f", "/play/rezero-2-part-2.g"}
	case "foo":
		paths = []string{"/play/foo-2.h", "/play/sousou-no-frieren.a"}
	}
	var hits []animeworld.Hit
	for _, p := range paths {
		hits = append(hits, animeworld.Hit{Path: p, Title: world[p].title, Dub: world[p].dub})
	}
	return hits
}

func testMapping() *mapping.Index {
	return mapping.NewIndex([]mapping.Entry{
		{MAL: 52991, TVDB: 424536, Season: 1, Slug: "frieren-beyond-journeys-end"},
		{MAL: 59978, TVDB: 424536, Season: 2, Slug: "frieren-beyond-journeys-end-season-2"},
		{MAL: 39587, TVDB: 305089, Season: 2},
		{MAL: 42203, TVDB: 305089, Season: 2, Offset: 13},
		{MAL: 34572, TVDB: 328724, Season: 1},
	})
}

func newTVService(t *testing.T, site Site) *Service {
	t.Helper()
	svc := newService(t, site, 5*time.Second)
	svc.opt.Mapping = staticMapper{testMapping()}
	return svc
}

func titles(rels []Release) []string {
	var out []string
	for _, r := range rels {
		out = append(out, r.Title)
	}
	return out
}

func TestSearchTVMapsSeasonsAndEpisodes(t *testing.T) {
	tests := []struct {
		name string
		q    TVQuery
		want []string
	}{
		{"later season", TVQuery{Q: "frieren", TVDBID: 424536, Season: 2, Ep: 3},
			[]string{"Frieren: Beyond Journey's End S02E03 [SUB ITA]", "Frieren: Beyond Journey's End S02E03 [ITA]"}},
		{"first season", TVQuery{Q: "frieren", TVDBID: 424536, Season: 1, Ep: 3},
			[]string{"Frieren: Beyond Journey's End S01E03 [SUB ITA]", "Frieren: Beyond Journey's End S01E03 [ITA]"}},
		{"episode beyond the season", TVQuery{Q: "frieren", TVDBID: 424536, Season: 2, Ep: 9}, nil},
		{"second cour continues the season numbering", TVQuery{Q: "rezero", TVDBID: 305089, Season: 2, Ep: 14},
			[]string{"Re:Zero S02E14 [SUB ITA]"}},
		{"first cour keeps its own numbers", TVQuery{Q: "rezero", TVDBID: 305089, Season: 2, Ep: 3},
			[]string{"Re:Zero S02E03 [SUB ITA]"}},
		{"unknown tvdb id with no title finds nothing", TVQuery{TVDBID: 1, Season: 1, Ep: 1}, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			svc := newTVService(t, worldSite("frieren", "rezero"))
			rels, err := svc.SearchTV(context.Background(), tc.q, 0, 100)
			if err != nil {
				t.Fatal(err)
			}
			if got := titles(rels); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("got  %q\nwant %q", got, tc.want)
			}
		})
	}
}

func TestSearchTVWithoutSeasonReturnsEveryMappedSeasonOnly(t *testing.T) {
	svc := newTVService(t, worldSite("frieren"))
	rels, err := svc.SearchTV(context.Background(), TVQuery{Q: "frieren", TVDBID: 424536}, 0, 500)
	if err != nil {
		t.Fatal(err)
	}
	if len(rels) != 2*28+2*4 {
		t.Fatalf("want 64 releases (two seasons, sub and dub), got %d", len(rels))
	}
	for _, r := range rels {
		if r.MALID == 34572 || r.TVDBID != 424536 {
			t.Fatalf("foreign series leaked: %+v", r)
		}
	}
}

func TestSearchTVFindsTheShowFromTheMappingWhenNoTitleIsGiven(t *testing.T) {
	site := worldSite("frieren")
	svc := newTVService(t, site)
	rels, err := svc.SearchTV(context.Background(), TVQuery{TVDBID: 424536, Season: 1, Ep: 1}, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"frieren beyond journeys end", "frieren beyond", "frieren"}
	if !reflect.DeepEqual(site.queries, want) {
		t.Errorf("queries %q, want %q (full slug first, then shorter)", site.queries, want)
	}
	if len(rels) != 2 || rels[0].Season != 1 || rels[0].Episode != 1 {
		t.Errorf("got %+v", rels)
	}
}

type fakeTitles map[int]string

func (f fakeTitles) Title(_ context.Context, mal int) string { return f[mal] }

func TestSearchTVWithoutTitleAsksForTheMALTitleFirst(t *testing.T) {
	site := worldSite("rezero")
	svc := newTVService(t, site)
	svc.opt.Titles = fakeTitles{39587: "rezero"}
	rels, err := svc.SearchTV(context.Background(), TVQuery{TVDBID: 305089, Season: 2, Ep: 3}, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(site.queries, []string{"rezero"}) {
		t.Errorf("queries %q", site.queries)
	}
	if got, want := titles(rels), []string{"Re:Zero S02E03 [SUB ITA]"}; !reflect.DeepEqual(got, want) {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestSearchTVStopsSearchingOnceEverySeasonIsFound(t *testing.T) {
	site := worldSite("frieren")
	svc := newTVService(t, site)
	if _, err := svc.SearchTV(context.Background(), TVQuery{Q: "frieren", TVDBID: 424536, Season: 2}, 0, 100); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(site.queries, []string{"frieren"}) {
		t.Errorf("queries %q: the given title already found season 2", site.queries)
	}
}

func TestSearchTVFallsBackToTheHeuristicWhenTheMappingLacksTheShow(t *testing.T) {
	svc := newTVService(t, worldSite("foo"))
	// tvdb 555 is not in the mapping; "foo" also returns Frieren, which belongs to another TVDB series.
	rels, err := svc.SearchTV(context.Background(), TVQuery{Q: "foo", TVDBID: 555, Season: 2, Ep: 3}, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := titles(rels), []string{"Foo S02E03 [SUB ITA]"}; !reflect.DeepEqual(got, want) {
		t.Errorf("got %q, want %q", got, want)
	}
	if len(rels) == 1 && rels[0].TVDBID != 0 {
		t.Errorf("a heuristic match must not claim a TVDB id: %+v", rels[0])
	}
}

func TestSearchTVWithoutMapperUsesTheHeuristic(t *testing.T) {
	svc := newService(t, worldSite("foo"), time.Second) // no Mapping configured
	rels, err := svc.SearchTV(context.Background(), TVQuery{Q: "foo", Season: 2}, 0, 100)
	if err != nil || len(rels) != 5 || rels[0].Title != "Foo S02E05 [SUB ITA]" {
		t.Fatalf("err=%v titles=%q", err, titles(rels))
	}
}

func TestPlainSearchParsesSeasonAndEpisodeOutOfTheQuery(t *testing.T) {
	tests := []struct {
		q    string
		want []string
	}{
		{"frieren S02E03", []string{"Frieren: Beyond Journey's End S02E03 [SUB ITA]", "Frieren: Beyond Journey's End S02E03 [ITA]"}},
		{"frieren s01e28", []string{"Frieren: Beyond Journey's End S01E28 [SUB ITA]", "Frieren: Beyond Journey's End S01E28 [ITA]"}},
	}
	for _, tc := range tests {
		site := worldSite("frieren")
		rels, err := newTVService(t, site).Search(context.Background(), tc.q, 0, 100)
		if err != nil {
			t.Fatal(err)
		}
		if got := titles(rels); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%q: got %q, want %q", tc.q, got, tc.want)
		}
		if !reflect.DeepEqual(site.queries, []string{"frieren"}) {
			t.Errorf("%q: site was asked %q", tc.q, site.queries)
		}
	}
}

func TestSplitQuery(t *testing.T) {
	tests := []struct {
		q          string
		kw         string
		season, ep int
	}{
		{"Frieren", "Frieren", 0, 0},
		{"", "", 0, 0},
		{"Frieren S02E03", "Frieren", 2, 3},
		{"Sousou no Frieren s2e13", "Sousou no Frieren", 2, 13},
		{"Frieren 28", "Frieren", 0, 0}, // absolute numbers cannot filter without a TVDB order
		{"Frieren 2nd Season", "Frieren 2nd Season", 0, 0},
		{"86", "86", 0, 0},
		{"Mob Psycho 100", "Mob Psycho", 0, 0},
	}
	for _, tc := range tests {
		kw, s, e := splitQuery(tc.q)
		if kw != tc.kw || s != tc.season || e != tc.ep {
			t.Errorf("splitQuery(%q) = %q,%d,%d want %q,%d,%d", tc.q, kw, s, e, tc.kw, tc.season, tc.ep)
		}
	}
}

func TestReleaseTitle(t *testing.T) {
	tests := []struct {
		title      string
		season, ep int
		num        string
		dub        bool
		want       string
	}{
		{"Sousou no Frieren", 2, 3, "3", false, "Sousou no Frieren S02E03 [SUB ITA]"},
		{"Sousou no Frieren", 1, 112, "112", true, "Sousou no Frieren S01E112 [ITA]"},
		{"Show", 0, 0, "7", false, "Show - 07 [SUB ITA]"},
		{"Show", 0, 0, "12.5", true, "Show - 12.5 [ITA]"},
	}
	for _, tc := range tests {
		if got := releaseTitle(tc.title, tc.season, tc.ep, tc.num, tc.dub); got != tc.want {
			t.Errorf("got %q, want %q", got, tc.want)
		}
	}
}
