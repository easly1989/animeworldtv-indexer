package animeworld

import (
	"os"
	"strings"
	"testing"
)

func fixture(t *testing.T, name string) *os.File {
	t.Helper()
	f, err := os.Open("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}

func TestParseSearch(t *testing.T) {
	hits, err := ParseSearch(fixture(t, "search_frieren.html"))
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 5 {
		t.Fatalf("want 5 hits, got %d: %+v", len(hits), hits)
	}
	first := hits[0]
	if first.Path != "/play/sousou-no-frieren.kbcPv" || first.Title != "Frieren: Beyond Journey's End" ||
		first.JTitle != "Sousou no Frieren" || first.Dub {
		t.Errorf("first hit wrong: %+v", first)
	}
	if !hits[1].Dub || hits[1].Path != "/play/frieren-beyond-journeys-end-ita.v26Gn" {
		t.Errorf("dubbed hit wrong: %+v", hits[1])
	}
}

func TestParseSearchEpisodeLinksCollapseToSeries(t *testing.T) {
	// The /updated listing links straight to episodes of the same series.
	html := `<div class="film-list">
	<div class="item"><a href="/play/a.x1/tok1" class="name" data-jtitle="A">A</a></div>
	<div class="item"><a href="/play/a.x1/tok2" class="name" data-jtitle="A">A</a></div>
	<div class="item"><a href="/play/b-ita.y2/tok3" class="name" data-jtitle="B">B (ITA)</a></div></div>`
	hits, err := ParseSearch(strings.NewReader(html))
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 2 || hits[0].Path != "/play/a.x1" || hits[1].Path != "/play/b-ita.y2" || !hits[1].Dub {
		t.Fatalf("unexpected hits: %+v", hits)
	}
}

func TestParseSearchNoList(t *testing.T) {
	if _, err := ParseSearch(strings.NewReader("<html><body>blocked</body></html>")); err == nil {
		t.Fatal("expected an error when the film list is missing")
	}
}

func TestParseUpdated(t *testing.T) {
	ups, err := ParseUpdated(fixture(t, "updated.html"))
	if err != nil {
		t.Fatal(err)
	}
	if len(ups) != 40 {
		t.Fatalf("want 40 updates, got %d", len(ups))
	}
	if want := (Update{Path: "/play/battle-spirits-re-zekkai-no-kuu.G1u8d", Title: "Battle Spirits [Re] Zekkai No Ku", Num: "3", Token: "2PPEH7"}); ups[0] != want {
		t.Errorf("first: got %+v, want %+v", ups[0], want)
	}
	// A dubbed and a subbed entry of the same series and episode are two updates.
	if want := (Update{Path: "/play/cyberpunk-edgerunners-2-ita.Dg7-P", Title: "Cyberpunk: Edgerunners 2", Dub: true, Num: "10", Token: "fIiRSa"}); ups[2] != want {
		t.Errorf("dub: got %+v, want %+v", ups[2], want)
	}
	if ups[3].Dub || ups[3].Path != "/play/cyberpunk-edgerunners-2.O8RTe" || ups[3].Token != "-6p-gY" {
		t.Errorf("sub: got %+v", ups[3])
	}
	dubs := 0
	for _, u := range ups {
		if u.Num == "" || u.Token == "" || u.Path == "" {
			t.Errorf("incomplete update: %+v", u)
		}
		if u.Dub {
			dubs++
		}
	}
	if dubs != 5 {
		t.Errorf("want 5 dubbed updates, got %d", dubs)
	}
}

func TestParseUpdatedSkipsEntriesWithoutANumberedEpisode(t *testing.T) {
	html := `<div class="film-list">
	<div class="item"><a href="/play/a.x1/t1" class="name">A</a><div class="status"><div class="ep"> Ep 12.5 </div></div></div>
	<div class="item"><a href="/play/m.x2/t2" class="name">Movie</a><div class="status"><div class="ona">ONA</div></div></div>
	<div class="item"><a href="/play/s.x3/t3" class="name">Special</a><div class="status"><div class="ep"> Ep Speciale </div></div></div>
	<div class="item"><a href="/play/a.x1/t1" class="name">A</a><div class="status"><div class="ep"> Ep 12.5 </div></div></div>
	<div class="item"><a href="/play/b.x4" class="name">B</a><div class="status"><div class="ep"> Ep 1 </div></div></div></div>`
	ups, err := ParseUpdated(strings.NewReader(html))
	if err != nil {
		t.Fatal(err)
	}
	if len(ups) != 1 || ups[0].Num != "12.5" || ups[0].Token != "t1" {
		t.Fatalf("unexpected updates: %+v", ups)
	}
	if _, err := ParseUpdated(strings.NewReader("<html>blocked</html>")); err == nil {
		t.Fatal("expected an error when the film list is missing")
	}
}

func TestParseSeries(t *testing.T) {
	s, err := ParseSeries(fixture(t, "series_frieren.html"))
	if err != nil {
		t.Fatal(err)
	}
	if s.Title != "Frieren: Beyond Journey's End" || s.JTitle != "Sousou no Frieren" {
		t.Errorf("titles wrong: %q / %q", s.Title, s.JTitle)
	}
	if s.MALID != 52991 || s.AniListID != 154587 {
		t.Errorf("ids wrong: mal=%d anilist=%d", s.MALID, s.AniListID)
	}
	if len(s.Episodes) != 28 {
		t.Fatalf("want 28 episodes, got %d", len(s.Episodes))
	}
	if e := s.Episodes[0]; e.Num != "1" || e.Token != "vlhlNp" {
		t.Errorf("episode 1 wrong: %+v", e)
	}
	if e := s.Episodes[11]; e.Num != "12" || e.Token != "NFYNE-" {
		t.Errorf("episode 12 wrong: %+v", e)
	}
}

func TestParseSearchMarksMovies(t *testing.T) {
	hits, err := ParseSearch(fixture(t, "search_yourname.html"))
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 3 || !hits[0].Movie || !hits[1].Movie || !hits[1].Dub || hits[2].Movie {
		t.Fatalf("movie badge not read: %+v", hits)
	}
}

func TestParseSeriesReadsCategoryAndYear(t *testing.T) {
	s, err := ParseSeries(fixture(t, "series_yourname.html"))
	if err != nil {
		t.Fatal(err)
	}
	if s.Category != "Movie" || s.Year != 2016 || s.MALID != 32281 || len(s.Episodes) != 1 {
		t.Errorf("got %+v", s)
	}
}

func TestParseSeriesWithoutTitleFails(t *testing.T) {
	if _, err := ParseSeries(strings.NewReader("<html></html>")); err == nil {
		t.Fatal("expected an error for a page without a title")
	}
}

func TestParseEpisodeInfo(t *testing.T) {
	u, err := ParseEpisodeInfo(fixture(t, "episode_info.json"))
	if err != nil {
		t.Fatal(err)
	}
	if want := "https://srv17-scupiddu.sweetpixel.org/DDL/ANIME/SousouNoFrieren/SousouNoFrieren_Ep_01_SUB_ITA.mp4"; u != want {
		t.Errorf("got %q", u)
	}
	if _, err := ParseEpisodeInfo(strings.NewReader(`{"name":"x"}`)); err == nil {
		t.Error("expected an error without grabber")
	}
}

func TestCleanTitle(t *testing.T) {
	for in, want := range map[string]string{
		"Frieren: Beyond Journey's End (ITA)": "Frieren: Beyond Journey's End",
		"Frieren 2":                           "Frieren 2",
	} {
		if got := CleanTitle(in); got != want {
			t.Errorf("CleanTitle(%q)=%q want %q", in, got, want)
		}
	}
}
