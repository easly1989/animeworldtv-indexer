package indexer

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/easly1989/animeworldtv-indexer/internal/animeworld"
	"github.com/easly1989/animeworldtv-indexer/internal/store"
)

type fakeSite struct {
	mu        sync.Mutex
	calls     map[string]int
	queries   []string
	metaDelay time.Duration
	metaFail  bool
	// Optional overrides; the defaults serve one show with a sub and a dub entry.
	searchErrFor string // a query the site fails on
	searchFn     func(q string) []animeworld.Hit
	updates      []animeworld.Update // what /updated lists
	seriesFn     func(path string) animeworld.Series
}

func (f *fakeSite) count(k string) int { f.mu.Lock(); defer f.mu.Unlock(); return f.calls[k] }
func (f *fakeSite) hit(k string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.calls == nil {
		f.calls = map[string]int{}
	}
	f.calls[k]++
}

func (f *fakeSite) Search(_ context.Context, q string) ([]animeworld.Hit, error) {
	f.hit("search")
	f.mu.Lock()
	f.queries = append(f.queries, q)
	f.mu.Unlock()
	if f.searchErrFor != "" && q == f.searchErrFor {
		return nil, errors.New("site down")
	}
	if f.searchFn != nil {
		return f.searchFn(q), nil
	}
	return []animeworld.Hit{
		{Path: "/play/show.aaa", Title: "Show"},
		{Path: "/play/show-ita.bbb", Title: "Show", Dub: true},
	}, nil
}

func (f *fakeSite) Updated(context.Context) ([]animeworld.Update, error) {
	f.hit("updated")
	return f.updates, nil
}

func (f *fakeSite) Series(_ context.Context, path string) (animeworld.Series, error) {
	f.hit("series")
	if f.seriesFn != nil {
		return f.seriesFn(path), nil
	}
	s := animeworld.Series{Path: path, Title: "Show", Dub: animeworld.IsDub(path), MALID: 1}
	for i := 1; i <= 3; i++ {
		s.Episodes = append(s.Episodes, animeworld.Episode{Num: fmt.Sprint(i), Token: fmt.Sprintf("%s-%d", path, i)})
	}
	return s, nil
}

func (f *fakeSite) FileURL(_ context.Context, token string) (string, error) {
	f.hit("fileurl")
	return "http://files/" + token, nil
}

func (f *fakeSite) FileMeta(ctx context.Context, _ string) (animeworld.FileMeta, error) {
	f.hit("meta")
	select {
	case <-time.After(f.metaDelay):
	case <-ctx.Done():
	}
	if f.metaFail {
		return animeworld.FileMeta{}, errors.New("boom")
	}
	return animeworld.FileMeta{Size: 500 << 20, Modified: time.Date(2024, 3, 31, 0, 0, 0, 0, time.UTC)}, nil
}

func newService(t *testing.T, site Site, budget time.Duration) *Service {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "c.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return New(site, st, Options{MaxSeries: 8, Concurrency: 4, Budget: budget})
}

func TestSearchBuildsTaggedReleasesWithSizes(t *testing.T) {
	site := &fakeSite{}
	svc := newService(t, site, 5*time.Second)
	rels, err := svc.Search(context.Background(), "show", 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(rels) != 6 {
		t.Fatalf("want 6 releases, got %d", len(rels))
	}
	first, dubbed := rels[0], rels[3]
	if first.Title != "Show S01E03 [SUB ITA]" || dubbed.Title != "Show S01E03 [ITA]" {
		t.Errorf("titles: %q / %q (latest episode first, series in hit order)", first.Title, dubbed.Title)
	}
	for _, r := range rels {
		if r.Size != 500<<20 || r.Estimated || r.Published.Year() != 2024 {
			t.Errorf("bad size/date: %+v", r)
		}
	}
}

func TestSecondSearchIsServedFromCache(t *testing.T) {
	site := &fakeSite{}
	svc := newService(t, site, 5*time.Second)
	if _, err := svc.Search(context.Background(), "show", 0, 100); err != nil {
		t.Fatal(err)
	}
	before := map[string]int{"search": site.count("search"), "series": site.count("series"), "meta": site.count("meta")}
	if _, err := svc.Search(context.Background(), "Show", 0, 100); err != nil { // case-insensitive key
		t.Fatal(err)
	}
	for k, n := range before {
		if site.count(k) != n {
			t.Errorf("%s requests went %d -> %d on a repeated search", k, n, site.count(k))
		}
	}
}

func TestWindowOnlyResolvesRequestedSizes(t *testing.T) {
	site := &fakeSite{}
	svc := newService(t, site, 5*time.Second)
	rels, err := svc.Search(context.Background(), "show", 1, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(rels) != 2 || rels[0].Title != "Show S01E02 [SUB ITA]" {
		t.Fatalf("window wrong: %+v", rels)
	}
	if n := site.count("meta"); n != 2 {
		t.Errorf("want 2 HEADs for a window of 2, got %d", n)
	}
}

func TestSlowSizesFallBackToEstimate(t *testing.T) {
	site := &fakeSite{metaDelay: 2 * time.Second}
	svc := newService(t, site, 50*time.Millisecond)
	start := time.Now()
	rels, err := svc.Search(context.Background(), "show", 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > time.Second {
		t.Error("search did not honour its time budget")
	}
	for _, r := range rels {
		if r.Size <= 0 || !r.Estimated {
			t.Errorf("want a non-zero estimated size, got %+v", r)
		}
	}
}

func TestFailedSizesStillYieldNonZeroEstimate(t *testing.T) {
	svc := newService(t, &fakeSite{metaFail: true}, time.Second)
	rels, err := svc.Search(context.Background(), "show", 0, 100)
	if err != nil || len(rels) == 0 {
		t.Fatalf("err=%v rels=%d", err, len(rels))
	}
	if rels[0].Size <= 0 || !rels[0].Estimated {
		t.Errorf("got %+v", rels[0])
	}
}

func TestNumberedTitleKeepsNonIntegerEpisodes(t *testing.T) {
	if got := releaseTitle("Show", 0, 0, "12.5", false); got != "Show - 12.5 [SUB ITA]" {
		t.Errorf("got %q", got)
	}
}
