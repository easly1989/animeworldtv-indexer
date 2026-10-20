// Package torznab serves the Torznab API (caps and search) and the /download
// endpoint the feed's links point to.
package torznab

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/easly1989/animeworldtv-indexer/internal/animeworld"
	"github.com/easly1989/animeworldtv-indexer/internal/downloader"
	"github.com/easly1989/animeworldtv-indexer/internal/indexer"
)

const (
	catTV        = 5000
	catAnime     = 5070
	catMovies    = 2000
	defaultLimit = 100
	maxLimit     = 500
)

// Backend is what the handler needs from the indexer service.
type Backend interface {
	Search(ctx context.Context, q string, offset, limit int) ([]indexer.Release, error)
	SearchTV(ctx context.Context, tq indexer.TVQuery, offset, limit int) ([]indexer.Release, error)
	SearchMovie(ctx context.Context, mq indexer.MovieQuery, offset, limit int) ([]indexer.Release, error)
}

type Options struct {
	APIKey  string // empty disables the check
	SiteURL string // used for each item's <comments> link
}

type handler struct {
	opt Options
	b   Backend
}

// New returns a handler serving /api and /download.
func New(opt Options, b Backend) http.Handler {
	h := &handler{opt, b}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api", h.api)
	mux.HandleFunc("GET /download", h.download)
	return mux
}

const capsXML = `<?xml version="1.0" encoding="UTF-8"?>
<caps>
<server title="AnimeWorld"/>
<limits max="%d" default="%d"/>
<searching>
<search available="yes" supportedParams="q"/>
<tv-search available="yes" supportedParams="q,season,ep,tvdbid"/>
<movie-search available="yes" supportedParams="q,imdbid,tmdbid"/>
</searching>
<categories>
<category id="5000" name="TV"><subcat id="5070" name="TV/Anime"/></category>
<category id="2000" name="Movies"/>
</categories>
</caps>`

func (h *handler) api(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if h.opt.APIKey != "" && q.Get("apikey") != h.opt.APIKey {
		writeError(w, http.StatusUnauthorized, 100, "Incorrect user credentials")
		return
	}
	switch t := q.Get("t"); t {
	case "caps":
		writeXML(w, http.StatusOK, "application/xml", fmt.Sprintf(capsXML, maxLimit, defaultLimit))
	case "search", "tvsearch", "movie":
		h.search(w, r)
	default:
		writeError(w, http.StatusBadRequest, 202, "No such function: "+t)
	}
}

func (h *handler) search(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit := atoiOr(q.Get("limit"), defaultLimit)
	if limit < 1 || limit > maxLimit {
		limit = maxLimit
	}
	offset := max(atoiOr(q.Get("offset"), 0), 0)

	var rels []indexer.Release
	movie := q.Get("t") == "movie"
	if movie && wantsMovies(q.Get("cat")) || !movie && wantsAnime(q.Get("cat")) {
		var err error
		switch {
		case q.Get("t") == "tvsearch":
			rels, err = h.b.SearchTV(r.Context(), indexer.TVQuery{
				Q: strings.TrimSpace(q.Get("q")), TVDBID: atoiOr(q.Get("tvdbid"), 0),
				Season: atoiOr(q.Get("season"), 0), Ep: atoiOr(q.Get("ep"), 0),
			}, offset, limit)
		case movie:
			rels, err = h.b.SearchMovie(r.Context(), indexer.MovieQuery{
				Q: strings.TrimSpace(q.Get("q")), IMDBID: q.Get("imdbid"), TMDBID: atoiOr(q.Get("tmdbid"), 0),
			}, offset, limit)
		default:
			rels, err = h.b.Search(r.Context(), strings.TrimSpace(q.Get("q")), offset, limit)
		}
		if errors.Is(err, animeworld.ErrBlocked) { // Cloudflare in the way: an empty feed, not an error
			slog.Error("search blocked by Cloudflare, returning no results", "q", q.Get("q"), "err", err)
			rels, err = nil, nil
		}
		if err != nil {
			slog.Error("search failed", "q", q.Get("q"), "err", err)
			writeError(w, http.StatusBadGateway, 900, "Search failed: "+err.Error())
			return
		}
	}

	base := scheme(r) + "://" + r.Host
	var sb strings.Builder
	sb.WriteString(`<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0" xmlns:atom="http://www.w3.org/2005/Atom" xmlns:torznab="http://torznab.com/schemas/2015/feed">
<channel>
<title>AnimeWorld</title>
<link>` + esc(h.opt.SiteURL) + `</link>
`)
	for _, rel := range rels {
		h.item(&sb, base, rel)
	}
	sb.WriteString("</channel>\n</rss>\n")
	slog.Info("search", "q", q.Get("q"), "results", len(rels))
	writeXML(w, http.StatusOK, "application/rss+xml", sb.String())
}

func (h *handler) item(sb *strings.Builder, base string, r indexer.Release) {
	link := base + "/download?ep=" + r.Token + "&n=" + url.QueryEscape(r.Title)
	cat := catAnime
	if r.Movie {
		cat = catMovies
	}
	date := r.Published
	if date.IsZero() {
		date = time.Now()
	}
	fmt.Fprintf(sb, `<item>
<title>%s</title>
<guid>animeworld-%s</guid>
<link>%s</link>
<comments>%s</comments>
<pubDate>%s</pubDate>
<size>%d</size>
<enclosure url="%s" length="%d" type="application/x-bittorrent"/>
<torznab:attr name="category" value="%d"/>
<torznab:attr name="size" value="%d"/>
<torznab:attr name="seeders" value="1"/>
<torznab:attr name="peers" value="1"/>
<torznab:attr name="language" value="Italian"/>
<torznab:attr name="downloadvolumefactor" value="0"/>
<torznab:attr name="uploadvolumefactor" value="1"/>
`, esc(r.Title), esc(r.Token), esc(link), esc(h.opt.SiteURL+r.SeriesPath), date.UTC().Format(time.RFC1123Z),
		r.Size, esc(link), r.Size, cat, r.Size)
	if r.IMDB != "" { // "imdb" is the bare number Newznab uses, "imdbid" the tt form; Radarr reads one or the other
		fmt.Fprintf(sb, "<torznab:attr name=\"imdb\" value=\"%s\"/>\n<torznab:attr name=\"imdbid\" value=\"%s\"/>\n",
			strings.TrimPrefix(r.IMDB, "tt"), esc(r.IMDB))
	}
	if r.TMDBID != 0 {
		fmt.Fprintf(sb, "<torznab:attr name=\"tmdbid\" value=\"%d\"/>\n", r.TMDBID)
	}
	if r.TVDBID != 0 {
		fmt.Fprintf(sb, "<torznab:attr name=\"tvdbid\" value=\"%d\"/>\n", r.TVDBID)
	}
	if r.Season != 0 || r.Episode != 0 {
		fmt.Fprintf(sb, "<torznab:attr name=\"season\" value=\"%d\"/>\n<torznab:attr name=\"episode\" value=\"%d\"/>\n", r.Season, r.Episode)
	}
	sb.WriteString("</item>\n")
}

// download hands the grab to the download client: Sonarr/Radarr follow this
// redirect to a magnet and pass it to the emulated qBittorrent API, which does
// the actual HTTP download (see internal/qbit). n is the release title, which
// names the file.
func (h *handler) download(w http.ResponseWriter, r *http.Request) {
	token := r.URL.Query().Get("ep")
	if !downloader.ValidToken(token) {
		http.Error(w, "missing or malformed ep", http.StatusBadRequest)
		return
	}
	http.Redirect(w, r, downloader.Magnet(token, r.URL.Query().Get("n")), http.StatusFound)
}

// wantsAnime reports whether a cat= filter leaves room for our category. No
// filter means everything.
func wantsAnime(cat string) bool {
	if cat == "" {
		return true
	}
	for _, c := range strings.Split(cat, ",") {
		if n, _ := strconv.Atoi(strings.TrimSpace(c)); n == catTV || n == catAnime {
			return true
		}
	}
	return false
}

// wantsMovies is wantsAnime for a movie search: any Movies category (2000, and
// the 20xx subcategories Radarr lists) leaves room for ours.
func wantsMovies(cat string) bool {
	if cat == "" {
		return true
	}
	for _, c := range strings.Split(cat, ",") {
		if n, _ := strconv.Atoi(strings.TrimSpace(c)); n >= catMovies && n < catMovies+1000 {
			return true
		}
	}
	return false
}

func atoiOr(s string, def int) int {
	if n, err := strconv.Atoi(s); err == nil {
		return n
	}
	return def
}

func scheme(r *http.Request) string {
	if p := r.Header.Get("X-Forwarded-Proto"); p == "https" || p == "http" {
		return p
	}
	if r.TLS != nil {
		return "https"
	}
	return "http"
}

func esc(s string) string {
	var sb strings.Builder
	_ = xml.EscapeText(&sb, []byte(s))
	return sb.String()
}

func writeError(w http.ResponseWriter, status, code int, desc string) {
	writeXML(w, status, "application/xml", fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<error code="%d" description="%s"/>`, code, esc(desc)))
}

func writeXML(w http.ResponseWriter, status int, contentType, body string) {
	w.Header().Set("Content-Type", contentType+"; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
}
