package torznab

import (
	"context"
	"encoding/xml"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/easly1989/animeworldtv-indexer/internal/downloader"
	"github.com/easly1989/animeworldtv-indexer/internal/indexer"
)

type fakeBackend struct {
	q             string
	offset, limit int
	err           error
	tv            *indexer.TVQuery // set when SearchTV was called
	movie         *indexer.MovieQuery
}

func (f *fakeBackend) SearchMovie(_ context.Context, mq indexer.MovieQuery, _, _ int) ([]indexer.Release, error) {
	f.movie = &mq
	return []indexer.Release{{Title: "Your Name. 2016 [SUB ITA]", Token: "t6nzso", SeriesPath: "/play/your-name.QA3xM", Size: 1 << 30,
		Movie: true, Year: 2016, IMDB: "tt5311514", TMDBID: 372058}}, nil
}

func (f *fakeBackend) SearchTV(ctx context.Context, tq indexer.TVQuery, offset, limit int) ([]indexer.Release, error) {
	f.tv = &tq
	rels, err := f.Search(ctx, tq.Q, offset, limit)
	if err == nil {
		rels[0].Season, rels[0].Episode, rels[0].TVDBID = 2, 3, 424536
	}
	return rels, err
}

func (f *fakeBackend) Search(_ context.Context, q string, offset, limit int) ([]indexer.Release, error) {
	f.q, f.offset, f.limit = q, offset, limit
	if f.err != nil {
		return nil, f.err
	}
	return []indexer.Release{
		{Title: "Frieren & Co - 01 [SUB ITA]", Token: "vlhlNp", SeriesPath: "/play/sousou-no-frieren.kbcPv",
			Size: 502785614, Published: time.Date(2024, 3, 31, 0, 40, 6, 0, time.UTC)},
		{Title: "Frieren - 01 [ITA]", Token: "NFYNE-", SeriesPath: "/play/frieren-ita.v26Gn", Dub: true,
			Size: 300 << 20, Estimated: true, Published: time.Date(2024, 4, 1, 0, 0, 0, 0, time.UTC)},
	}, nil
}

func newHandler(b *fakeBackend) http.Handler {
	return New(Options{APIKey: "secret", SiteURL: "https://www.animeworld.ac"}, b)
}

func get(h http.Handler, target string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	req.Host = "indexer:9698"
	h.ServeHTTP(rec, req)
	return rec
}

func TestAPIKeyIsRequired(t *testing.T) {
	h := newHandler(&fakeBackend{})
	for _, target := range []string{"/api?t=caps", "/api?t=caps&apikey=wrong", "/api?t=search&q=x&apikey="} {
		rec := get(h, target)
		if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), `<error code="100"`) {
			t.Errorf("%s: code=%d body=%s", target, rec.Code, rec.Body)
		}
	}
	if rec := get(h, "/api?t=caps&apikey=secret"); rec.Code != http.StatusOK {
		t.Errorf("right key rejected: %d", rec.Code)
	}
}

func TestEmptyAPIKeyDisablesAuth(t *testing.T) {
	h := New(Options{}, &fakeBackend{})
	if rec := get(h, "/api?t=caps"); rec.Code != http.StatusOK {
		t.Errorf("code=%d", rec.Code)
	}
}

func TestCaps(t *testing.T) {
	rec := get(newHandler(&fakeBackend{}), "/api?t=caps&apikey=secret")
	body := rec.Body.String()
	var caps struct {
		Searching struct {
			Search struct {
				Available string `xml:"available,attr"`
			} `xml:"search"`
			TVSearch struct {
				Available string `xml:"available,attr"`
			} `xml:"tv-search"`
			MovieSearch struct {
				Available string `xml:"available,attr"`
			} `xml:"movie-search"`
		} `xml:"searching"`
		Categories []struct {
			ID     string `xml:"id,attr"`
			Subcat []struct {
				ID string `xml:"id,attr"`
			} `xml:"subcat"`
		} `xml:"categories>category"`
	}
	if err := xml.Unmarshal([]byte(body), &caps); err != nil {
		t.Fatalf("caps is not valid XML: %v\n%s", err, body)
	}
	s := caps.Searching
	if s.Search.Available != "yes" || s.TVSearch.Available != "yes" || s.MovieSearch.Available != "yes" {
		t.Errorf("search modes: %+v", s)
	}
	if !strings.Contains(body, `id="5070"`) || !strings.Contains(body, `id="2000"`) {
		t.Errorf("missing categories:\n%s", body)
	}
}

type feed struct {
	Items []struct {
		Title     string `xml:"title"`
		GUID      string `xml:"guid"`
		Link      string `xml:"link"`
		Comments  string `xml:"comments"`
		PubDate   string `xml:"pubDate"`
		Size      int64  `xml:"size"`
		Enclosure struct {
			URL    string `xml:"url,attr"`
			Length int64  `xml:"length,attr"`
		} `xml:"enclosure"`
		Attrs []struct {
			Name  string `xml:"name,attr"`
			Value string `xml:"value,attr"`
		} `xml:"http://torznab.com/schemas/2015/feed attr"`
	} `xml:"channel>item"`
}

func attr(i int, f feed, name string) string {
	for _, a := range f.Items[i].Attrs {
		if a.Name == name {
			return a.Value
		}
	}
	return ""
}

func TestSearchFeed(t *testing.T) {
	b := &fakeBackend{}
	rec := get(newHandler(b), "/api?t=search&q=frieren&limit=20&offset=5&apikey=secret")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Header().Get("Content-Type"), "xml") {
		t.Fatalf("code=%d type=%q", rec.Code, rec.Header().Get("Content-Type"))
	}
	if b.q != "frieren" || b.limit != 20 || b.offset != 5 {
		t.Errorf("backend got q=%q limit=%d offset=%d", b.q, b.limit, b.offset)
	}
	var f feed
	if err := xml.Unmarshal(rec.Body.Bytes(), &f); err != nil {
		t.Fatalf("feed is not valid XML (is the & escaped?): %v\n%s", err, rec.Body)
	}
	if len(f.Items) != 2 {
		t.Fatalf("want 2 items, got %d", len(f.Items))
	}
	it := f.Items[0]
	if it.Title != "Frieren & Co - 01 [SUB ITA]" || it.Size != 502785614 || it.Enclosure.Length != 502785614 {
		t.Errorf("item wrong: %+v", it)
	}
	if want := "http://indexer:9698/download?ep=vlhlNp&n=Frieren+%26+Co+-+01+%5BSUB+ITA%5D"; it.Link != want || it.Enclosure.URL != want {
		t.Errorf("link %q enclosure %q, want %q", it.Link, it.Enclosure.URL, want)
	}
	if it.Comments != "https://www.animeworld.ac/play/sousou-no-frieren.kbcPv" || it.GUID == "" {
		t.Errorf("comments=%q guid=%q", it.Comments, it.GUID)
	}
	if _, err := time.Parse(time.RFC1123Z, it.PubDate); err != nil {
		t.Errorf("pubDate %q: %v", it.PubDate, err)
	}
	if attr(0, f, "category") != "5070" || attr(0, f, "size") != "502785614" || attr(0, f, "language") != "Italian" {
		t.Errorf("attrs wrong: %+v", it.Attrs)
	}
	if f.Items[0].GUID == f.Items[1].GUID {
		t.Error("guids must be unique")
	}
}

func TestCapsAdvertiseTVParams(t *testing.T) {
	body := get(newHandler(&fakeBackend{}), "/api?t=caps&apikey=secret").Body.String()
	if !strings.Contains(body, `<tv-search available="yes" supportedParams="q,season,ep,tvdbid"/>`) {
		t.Errorf("tv-search params missing:\n%s", body)
	}
}

func TestTVSearchPassesIDsToTheBackend(t *testing.T) {
	tests := []struct {
		target string
		want   indexer.TVQuery
	}{
		{"/api?t=tvsearch&tvdbid=424536&season=2&ep=3&q=frieren", indexer.TVQuery{Q: "frieren", TVDBID: 424536, Season: 2, Ep: 3}},
		{"/api?t=tvsearch&tvdbid=424536", indexer.TVQuery{TVDBID: 424536}},
		{"/api?t=tvsearch&season=x&ep=03&tvdbid=", indexer.TVQuery{Ep: 3}},
		{"/api?t=tvsearch&ep=2024/05/01", indexer.TVQuery{}}, // daily-style ep is not ours to read
	}
	for _, tc := range tests {
		b := &fakeBackend{}
		get(newHandler(b), tc.target+"&apikey=secret")
		if b.tv == nil || *b.tv != tc.want {
			t.Errorf("%s: backend got %+v, want %+v", tc.target, b.tv, tc.want)
		}
	}
}

func TestCapsAdvertiseMovieParams(t *testing.T) {
	body := get(newHandler(&fakeBackend{}), "/api?t=caps&apikey=secret").Body.String()
	if !strings.Contains(body, `<movie-search available="yes" supportedParams="q,imdbid,tmdbid"/>`) {
		t.Errorf("movie-search params missing:\n%s", body)
	}
}

func TestMovieSearchPassesIDsToTheBackend(t *testing.T) {
	tests := []struct {
		target string
		want   indexer.MovieQuery
	}{
		{"/api?t=movie&imdbid=tt5311514&q=Your+Name", indexer.MovieQuery{Q: "Your Name", IMDBID: "tt5311514"}},
		{"/api?t=movie&imdbid=5311514", indexer.MovieQuery{IMDBID: "5311514"}},
		{"/api?t=movie&tmdbid=372058&cat=2000,2010", indexer.MovieQuery{TMDBID: 372058}},
		{"/api?t=movie&q=your+name", indexer.MovieQuery{Q: "your name"}},
		{"/api?t=movie&tmdbid=x", indexer.MovieQuery{}},
	}
	for _, tc := range tests {
		b := &fakeBackend{}
		get(newHandler(b), tc.target+"&apikey=secret")
		if b.movie == nil || *b.movie != tc.want {
			t.Errorf("%s: backend got %+v, want %+v", tc.target, b.movie, tc.want)
		}
	}
}

func TestMovieItemsAreMoviesWithTheirIDs(t *testing.T) {
	rec := get(newHandler(&fakeBackend{}), "/api?t=movie&tmdbid=372058&apikey=secret")
	var f feed
	if err := xml.Unmarshal(rec.Body.Bytes(), &f); err != nil || len(f.Items) != 1 {
		t.Fatalf("err=%v body=%s", err, rec.Body)
	}
	for name, want := range map[string]string{"category": "2000", "imdb": "5311514", "imdbid": "tt5311514", "tmdbid": "372058"} {
		if got := attr(0, f, name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	if attr(0, f, "season") != "" || attr(0, f, "tvdbid") != "" {
		t.Errorf("a film carries no TV numbering: %+v", f.Items[0].Attrs)
	}
}

func TestMovieSearchCategories(t *testing.T) {
	for cat, reaches := range map[string]bool{"2000": true, "2000,2010,2040": true, "5070": false, "5000,5070": false, "": true} {
		b := &fakeBackend{}
		get(newHandler(b), "/api?t=movie&q=x&cat="+cat+"&apikey=secret")
		if (b.movie != nil) != reaches {
			t.Errorf("cat=%q: backend reached = %v, want %v", cat, b.movie != nil, reaches)
		}
	}
}

func TestPlainSearchIgnoresTVParams(t *testing.T) {
	b := &fakeBackend{}
	get(newHandler(b), "/api?t=search&q=x&tvdbid=1&season=2&ep=3&apikey=secret")
	if b.tv != nil || b.q != "x" {
		t.Errorf("t=search must use the plain search: tv=%+v q=%q", b.tv, b.q)
	}
}

func TestTVItemsCarryTheirNumbering(t *testing.T) {
	rec := get(newHandler(&fakeBackend{}), "/api?t=tvsearch&tvdbid=424536&apikey=secret")
	var f feed
	if err := xml.Unmarshal(rec.Body.Bytes(), &f); err != nil {
		t.Fatal(err)
	}
	if attr(0, f, "tvdbid") != "424536" || attr(0, f, "season") != "2" || attr(0, f, "episode") != "3" {
		t.Errorf("attrs: %+v", f.Items[0].Attrs)
	}
	if attr(1, f, "tvdbid") != "" || attr(1, f, "season") != "" {
		t.Errorf("an unnumbered item must not claim numbering: %+v", f.Items[1].Attrs)
	}
}

func TestSearchDefaultsAndCaps(t *testing.T) {
	b := &fakeBackend{}
	h := newHandler(b)
	get(h, "/api?t=tvsearch&apikey=secret")
	if b.q != "" || b.limit != defaultLimit || b.offset != 0 {
		t.Errorf("defaults: q=%q limit=%d offset=%d", b.q, b.limit, b.offset)
	}
	get(h, "/api?t=search&limit=99999&apikey=secret")
	if b.limit != maxLimit {
		t.Errorf("limit not capped: %d", b.limit)
	}
}

func TestSearchOutsideOurCategoriesIsEmpty(t *testing.T) {
	b := &fakeBackend{}
	rec := get(newHandler(b), "/api?t=search&q=x&cat=3000,3030&apikey=secret")
	var f feed
	if err := xml.Unmarshal(rec.Body.Bytes(), &f); err != nil || len(f.Items) != 0 {
		t.Fatalf("err=%v items=%d", err, len(f.Items))
	}
	if b.q != "" || b.limit != 0 {
		t.Error("backend should not be queried for foreign categories")
	}
	get(newHandler(b), "/api?t=search&q=x&cat=2000,5070&apikey=secret")
	if b.q != "x" {
		t.Error("5070 among the categories must reach the backend")
	}
}

func TestSearchBackendError(t *testing.T) {
	rec := get(newHandler(&fakeBackend{err: errors.New("site down")}), "/api?t=search&q=x&apikey=secret")
	if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), "<error") {
		t.Errorf("code=%d body=%s", rec.Code, rec.Body)
	}
}

func TestUnknownFunction(t *testing.T) {
	rec := get(newHandler(&fakeBackend{}), "/api?t=nope&apikey=secret")
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), `code="202"`) {
		t.Errorf("code=%d body=%s", rec.Code, rec.Body)
	}
}

func TestDownload(t *testing.T) {
	h := newHandler(&fakeBackend{})
	rec := get(h, "/download?ep=vlhlNp&n=Frieren+-+01+%5BSUB+ITA%5D")
	loc := rec.Header().Get("Location")
	if rec.Code != http.StatusFound || !strings.HasPrefix(loc, "magnet:?xt=urn:btih:") {
		t.Fatalf("code=%d location=%q", rec.Code, loc)
	}
	token, name, err := downloader.ParseMagnet(loc)
	if err != nil || token != "vlhlNp" || name != "Frieren - 01 [SUB ITA]" {
		t.Errorf("magnet %q parsed to token=%q name=%q err=%v", loc, token, name, err)
	}
	for _, bad := range []string{"/download", "/download?ep=a/b", "/download?ep=a%26b"} {
		if rec := get(h, bad); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: code=%d, want 400", bad, rec.Code)
		}
	}
}
