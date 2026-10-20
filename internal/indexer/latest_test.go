package indexer

import (
	"context"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/easly1989/animeworldtv-indexer/internal/animeworld"
)

func day(d int) time.Time { return time.Date(2024, 3, d, 12, 0, 0, 0, time.UTC) }

// latestSite lists four episodes, as /updated would (newest first by the site's
// own order), whose files are dated so that the listing order is not the date order.
func latestSite() *fakeSite {
	site := worldSite()
	site.updates = []animeworld.Update{
		{Path: "/play/frieren-beyond-journeys-end-2.c", Num: "4", Token: "u1"},
		{Path: "/play/frieren-beyond-journeys-end-2-ita.d", Num: "4", Token: "u2", Dub: true},
		{Path: "/play/rezero-2-part-2.g", Num: "12", Token: "u3"},
		{Path: "/play/foo-2.h", Num: "5", Token: "u4"},
	}
	site.modified = map[string]time.Time{"u1": day(3), "u2": day(2), "u3": day(4), "u4": day(1)}
	return site
}

func TestEmptyTVSearchReturnsTheLatestEpisodesNewestFirst(t *testing.T) {
	site := latestSite()
	svc := newTVService(t, site)
	rels, err := svc.SearchTV(context.Background(), TVQuery{}, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"Re:Zero S02E25 [SUB ITA]", // AnimeWorld episode 12 of the second cour: TVDB 12+13
		"Frieren: Beyond Journey's End S02E04 [SUB ITA]",
		"Frieren: Beyond Journey's End S02E04 [ITA]",
		"Foo S02E05 [SUB ITA]", // not in the mapping: season from the title
	}
	if got := titles(rels); !reflect.DeepEqual(got, want) {
		t.Fatalf("got  %q\nwant %q", got, want)
	}
	for i, d := range []int{4, 3, 2, 1} {
		if !rels[i].Published.Equal(day(d)) || rels[i].Size != 500<<20 {
			t.Errorf("%s: published %v size %d, want the file's date %v", rels[i].Title, rels[i].Published, rels[i].Size, day(d))
		}
	}
	if rels[0].TVDBID != 305089 || rels[0].Season != 2 || rels[0].Episode != 25 || rels[3].TVDBID != 0 {
		t.Errorf("numbering attrs: %+v / %+v", rels[0], rels[3])
	}

	// The plain search without a query is the same feed.
	plain, err := svc.Search(context.Background(), "", 0, 100)
	if err != nil || !reflect.DeepEqual(titles(plain), want) {
		t.Errorf("t=search without q: err=%v got %q", err, titles(plain))
	}
}

func TestLatestIsBoundedAndCached(t *testing.T) {
	site := latestSite()
	svc := newTVService(t, site)
	if _, err := svc.SearchTV(context.Background(), TVQuery{}, 0, 100); err != nil {
		t.Fatal(err)
	}
	// One listing, one page per series and one HEAD per listed episode: not the series' whole episode list.
	if got := map[string]int{"updated": site.count("updated"), "series": site.count("series"), "meta": site.count("meta")}; !reflect.DeepEqual(got, map[string]int{"updated": 1, "series": 4, "meta": 4}) {
		t.Errorf("first call: %v", got)
	}
	if _, err := svc.SearchTV(context.Background(), TVQuery{}, 0, 100); err != nil {
		t.Fatal(err)
	}
	if got := map[string]int{"updated": site.count("updated"), "series": site.count("series"), "meta": site.count("meta")}; !reflect.DeepEqual(got, map[string]int{"updated": 1, "series": 4, "meta": 4}) {
		t.Errorf("second call must come from the cache: %v", got)
	}
}

func TestLatestWindowLimitsSeriesPagesAndHeads(t *testing.T) {
	site := latestSite()
	rels, err := newTVService(t, site).SearchTV(context.Background(), TVQuery{}, 1, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(rels) != 2 || site.count("series") != 2 || site.count("meta") != 2 {
		t.Errorf("releases=%d series=%d meta=%d, want 2 each", len(rels), site.count("series"), site.count("meta"))
	}
}

func TestLatestFetchesAtMostTwiceMaxSeriesPagesAndSizesPerCall(t *testing.T) {
	site := &fakeSite{}
	for i := 0; i < 20; i++ {
		site.updates = append(site.updates, animeworld.Update{Path: fmt.Sprintf("/play/s%d.x", i), Num: "1", Token: fmt.Sprintf("t%d", i)})
	}
	svc := newTVService(t, site) // MaxSeries 8: 16 per call
	rels, err := svc.SearchTV(context.Background(), TVQuery{}, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(rels) != 16 || site.count("series") != 16 || site.count("meta") != 16 {
		t.Fatalf("first call: releases=%d series=%d meta=%d, want 16 each", len(rels), site.count("series"), site.count("meta"))
	}
	for i, r := range rels { // the listing's head is served first
		if want := fmt.Sprintf("t%d", i); r.Token != want {
			t.Errorf("release %d is %s, want %s", i, r.Token, want)
		}
	}
	rels, err = svc.SearchTV(context.Background(), TVQuery{}, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(rels) != 20 || site.count("series") != 20 || site.count("meta") != 20 {
		t.Errorf("second call: releases=%d series=%d meta=%d, want 20 each", len(rels), site.count("series"), site.count("meta"))
	}
}

func TestLatestFiltersBySeasonAndEpisodeWhenAsked(t *testing.T) {
	rels, err := newTVService(t, latestSite()).SearchTV(context.Background(), TVQuery{Season: 2, Ep: 25}, 0, 100)
	if err != nil || len(rels) != 1 || rels[0].Title != "Re:Zero S02E25 [SUB ITA]" {
		t.Fatalf("err=%v got %q", err, titles(rels))
	}
}

func TestLatestFallsBackToFirstSeenWhenTheFileDateIsUnknown(t *testing.T) {
	site := latestSite()
	site.metaFail = true
	svc := newTVService(t, site)
	if err := svc.cache.Put("seen:u1", day(20)); err != nil {
		t.Fatal(err)
	}
	rels, err := svc.SearchTV(context.Background(), TVQuery{}, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rels {
		if !r.Estimated {
			t.Fatalf("sizes should be estimates here: %+v", r)
		}
		if r.Token == "u1" && !r.Published.Equal(day(20)) {
			t.Errorf("published %v, want the recorded first sighting", r.Published)
		}
		if r.Published.IsZero() {
			t.Errorf("no publish time: %+v", r)
		}
	}
	if last := rels[len(rels)-1]; last.Token != "u1" { // the others were first seen just now
		t.Errorf("newest first: %q", titles(rels))
	}
}
