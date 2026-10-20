package indexer

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/easly1989/animeworldtv-indexer/internal/animeworld"
	"github.com/easly1989/animeworldtv-indexer/internal/store"
)

func manyEpisodes(path string) animeworld.Series {
	s := animeworld.Series{Path: path, Title: "Show", Dub: animeworld.IsDub(path), MALID: 1}
	for i := 1; i <= 30; i++ {
		s.Episodes = append(s.Episodes, animeworld.Episode{Num: itoa(i), Token: path + "-" + itoa(i)})
	}
	return s
}

func itoa(i int) string { return strconv.Itoa(i) }

func countEstimated(rels []Release) (n int) {
	for _, r := range rels {
		if r.Estimated {
			n++
		}
	}
	return n
}

func TestSizeLookupsAreCappedPerResponse(t *testing.T) {
	site := &fakeSite{seriesFn: manyEpisodes}
	svc := newService(t, site, 5*time.Second)
	svc.opt.SizeLookups = 10

	rels, err := svc.Search(context.Background(), "show", 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(rels) != 60 {
		t.Fatalf("want 60 releases, got %d", len(rels))
	}
	if site.count("fileurl") != 10 || site.count("meta") != 10 {
		t.Errorf("origin file-info lookups=%d, HEADs=%d, want 10 each for 60 releases", site.count("fileurl"), site.count("meta"))
	}
	if n := countEstimated(rels); n != 50 {
		t.Errorf("%d estimated, want 50", n)
	}
	for i, r := range rels {
		if r.Size <= 0 || r.Estimated != (i >= 10) {
			t.Fatalf("release %d: %+v (the first 10 are measured, the rest estimated)", i, r)
		}
	}

	// A repeat costs no new lookups for what is cached and takes the next 10.
	if _, err := svc.Search(context.Background(), "show", 0, 100); err != nil {
		t.Fatal(err)
	}
	if site.count("fileurl") != 20 {
		t.Errorf("second search: %d lookups in all, want 20", site.count("fileurl"))
	}
}

func TestNoSizeLookupsMeansAllEstimated(t *testing.T) {
	site := &fakeSite{}
	svc := newService(t, site, 5*time.Second)
	svc.opt.SizeLookups = 0
	rels, err := svc.Search(context.Background(), "show", 0, 100)
	if err != nil || len(rels) != 6 {
		t.Fatalf("err=%v rels=%d", err, len(rels))
	}
	if site.count("fileurl") != 0 || site.count("meta") != 0 || countEstimated(rels) != 6 {
		t.Errorf("fileurl=%d meta=%d estimated=%d", site.count("fileurl"), site.count("meta"), countEstimated(rels))
	}
	for _, r := range rels {
		if r.Size != defaultMiB<<20 {
			t.Errorf("size %d, want the default guess", r.Size)
		}
	}
}

// The whole stack against a counting fake origin: a cold keyword search used to
// cost one /api/episode/info per episode (140 here); now it is bounded.
func TestColdSearchOriginRequestsAreBounded(t *testing.T) {
	data := func(name string) string { return filepath.Join("..", "animeworld", "testdata", name) }
	for _, f := range []string{"search_frieren.html", "series_frieren.html"} {
		if _, err := os.Stat(data(f)); err != nil {
			t.Fatal(err)
		}
	}
	var originHits, infoHits, headHits atomic.Int32
	files := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		headHits.Add(1)
		w.Header().Set("Content-Length", "400000000")
		w.Header().Set("Last-Modified", "Sun, 31 Mar 2024 00:40:06 GMT")
	}))
	defer files.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		originHits.Add(1)
		switch {
		case strings.HasPrefix(r.URL.Path, "/search"):
			http.ServeFile(w, r, data("search_frieren.html"))
		case r.URL.Path == "/api/episode/info":
			infoHits.Add(1)
			w.Write([]byte(`{"grabber":"` + files.URL + `/` + r.URL.Query().Get("id") + `.mp4"}`))
		case strings.Count(r.URL.Path, "/") == 2: // /play/<slug>: the site redirects to the first episode
			http.Redirect(w, r, r.URL.Path+"/vlhlNp", http.StatusFound)
		default:
			http.ServeFile(w, r, data("series_frieren.html"))
		}
	}))
	defer origin.Close()

	site := animeworld.New(origin.URL, origin.Client())
	st, err := store.Open(filepath.Join(t.TempDir(), "c.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	svc := New(site, st, Options{MaxSeries: 8, Concurrency: 4, Budget: 5 * time.Second, SizeLookups: 10})

	rels, err := svc.Search(context.Background(), "frieren", 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	// 1 search + 5 series pages (2 requests each: redirect + page) + 10 file-info lookups.
	if got := int(originHits.Load()); got > 1+5*2+10 {
		t.Errorf("a cold search made %d origin requests, want at most 21", got)
	}
	if infoHits.Load() != 10 || headHits.Load() != 10 {
		t.Errorf("file-info lookups=%d HEADs=%d, want 10 each", infoHits.Load(), headHits.Load())
	}
	if len(rels) != 5*28 || countEstimated(rels) != len(rels)-10 {
		t.Errorf("%d releases, %d estimated", len(rels), countEstimated(rels))
	}

	before := originHits.Load()
	if _, err := svc.Search(context.Background(), "frieren", 0, 1000); err != nil {
		t.Fatal(err)
	}
	if got := originHits.Load() - before; got > 10 {
		t.Errorf("a repeat search made %d origin requests, want only the next size lookups (<= 10)", got)
	}
}
