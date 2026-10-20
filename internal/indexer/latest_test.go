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

// latestService lists four episodes, as /updated would (newest first by the
// site's own order). Two have a measured file in the cache, dated so that the
// listing order is not the date order.
func latestService(t *testing.T) (*Service, *fakeSite) {
	t.Helper()
	site := worldSite()
	site.updates = []animeworld.Update{
		{Path: "/play/frieren-beyond-journeys-end-2.c", Num: "4", Token: "u1"},
		{Path: "/play/frieren-beyond-journeys-end-2-ita.d", Num: "4", Token: "u2", Dub: true},
		{Path: "/play/rezero-2-part-2.g", Num: "12", Token: "u3"},
		{Path: "/play/foo-2.h", Num: "5", Token: "u4"},
	}
	svc := newTVService(t, site)
	for tok, d := range map[string]int{"u1": 3, "u3": 4} {
		if err := svc.cache.Put("meta:"+tok, animeworld.FileMeta{Size: 700 << 20, Modified: day(d)}); err != nil {
			t.Fatal(err)
		}
	}
	for tok, d := range map[string]int{"u2": 10, "u4": 9} { // when the others were first listed
		if err := svc.cache.Put("seen:"+tok, day(d)); err != nil {
			t.Fatal(err)
		}
	}
	return svc, site
}

func TestEmptyTVSearchReturnsTheLatestEpisodesNewestFirst(t *testing.T) {
	svc, _ := latestService(t)
	rels, err := svc.SearchTV(context.Background(), TVQuery{}, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	// u3 and u1 have a file date (Mar 4, Mar 3); u2 and u4 were first listed on Mar 10 and 9.
	want := []string{
		"Frieren: Beyond Journey's End S02E04 [ITA]",
		"Foo S02E05 [SUB ITA]",     // not in the mapping: season from the title
		"Re:Zero S02E25 [SUB ITA]", // AnimeWorld episode 12 of the second cour: TVDB 12+13
		"Frieren: Beyond Journey's End S02E04 [SUB ITA]",
	}
	if got := titles(rels); !reflect.DeepEqual(got, want) {
		t.Fatalf("got  %q\nwant %q", got, want)
	}
	if !rels[2].Published.Equal(day(4)) || !rels[3].Published.Equal(day(3)) || rels[2].Size != 700<<20 || rels[2].Estimated {
		t.Errorf("a measured file keeps its date and size: %+v / %+v", rels[2], rels[3])
	}
	if !rels[0].Estimated || rels[0].Size <= 0 || !rels[0].Published.Equal(day(10)) {
		t.Errorf("an unmeasured episode gets an estimated size and the time it was first seen: %+v", rels[0])
	}
	if rels[2].TVDBID != 305089 || rels[2].Season != 2 || rels[2].Episode != 25 || rels[1].TVDBID != 0 {
		t.Errorf("numbering attrs: %+v / %+v", rels[2], rels[1])
	}

	// The plain search without a query is the same feed.
	plain, err := svc.Search(context.Background(), "", 0, 100)
	if err != nil || !reflect.DeepEqual(titles(plain), want) {
		t.Errorf("t=search without q: err=%v got %q", err, titles(plain))
	}
}

func TestLatestIsCheapAndCached(t *testing.T) {
	svc, site := latestService(t)
	for call := 1; call <= 2; call++ {
		if _, err := svc.SearchTV(context.Background(), TVQuery{}, 0, 100); err != nil {
			t.Fatal(err)
		}
		// One listing, one page per series, no file lookups at all (two requests per episode).
		got := map[string]int{"updated": site.count("updated"), "series": site.count("series"), "fileurl": site.count("fileurl"), "meta": site.count("meta")}
		if want := map[string]int{"updated": 1, "series": 4, "fileurl": 0, "meta": 0}; !reflect.DeepEqual(got, want) {
			t.Errorf("after call %d: %v, want %v", call, got, want)
		}
	}
}

func TestLatestWindowLimitsSeriesPages(t *testing.T) {
	svc, site := latestService(t)
	rels, err := svc.SearchTV(context.Background(), TVQuery{}, 1, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(rels) != 2 || site.count("series") != 2 {
		t.Errorf("releases=%d series=%d, want 2 each", len(rels), site.count("series"))
	}
}

func TestLatestFetchesAtMostMaxSeriesPagesPerCall(t *testing.T) {
	site := &fakeSite{}
	for i := 0; i < 20; i++ {
		site.updates = append(site.updates, animeworld.Update{Path: fmt.Sprintf("/play/s%d.x", i), Num: "1", Token: fmt.Sprintf("t%d", i)})
	}
	svc := newTVService(t, site) // MaxSeries 8
	for call, want := range []int{8, 16, 20} {
		rels, err := svc.SearchTV(context.Background(), TVQuery{}, 0, 100)
		if err != nil {
			t.Fatal(err)
		}
		if len(rels) != want || site.count("series") != want {
			t.Fatalf("call %d: releases=%d series pages=%d, want %d each", call+1, len(rels), site.count("series"), want)
		}
		for i, r := range rels { // the listing's head is served first
			if tok := fmt.Sprintf("t%d", i); r.Token != tok {
				t.Errorf("call %d: release %d is %s, want %s", call+1, i, r.Token, tok)
			}
		}
	}
}

func TestLatestFiltersBySeasonAndEpisodeWhenAsked(t *testing.T) {
	svc, _ := latestService(t)
	rels, err := svc.SearchTV(context.Background(), TVQuery{Season: 2, Ep: 25}, 0, 100)
	if err != nil || len(rels) != 1 || rels[0].Title != "Re:Zero S02E25 [SUB ITA]" {
		t.Fatalf("err=%v got %q", err, titles(rels))
	}
}

func TestLatestKeepsTheFirstSightingAsPublishTime(t *testing.T) {
	svc, _ := latestService(t)
	if err := svc.cache.Put("seen:u4", day(20)); err != nil {
		t.Fatal(err)
	}
	rels, err := svc.SearchTV(context.Background(), TVQuery{}, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rels {
		if r.Token == "u4" && !r.Published.Equal(day(20)) {
			t.Errorf("published %v, want the recorded first sighting", r.Published)
		}
	}
}
