// Package animeworld scrapes AnimeWorld: search results, series pages and the
// per-episode direct file URL. Every selector is tied to the site's current
// markup; parsers fail loudly when it changes instead of returning nothing.
package animeworld

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"

	"github.com/PuerkitoBio/goquery"
)

// Hit is one entry of a search or "latest updates" listing. Dubbed versions
// are separate entries on the site.
type Hit struct {
	Path   string `json:"path"` // series path, e.g. /play/sousou-no-frieren.kbcPv
	Title  string `json:"title"`
	JTitle string `json:"jtitle"`
	Dub    bool   `json:"dub"`
}

// Episode is one episode of a series. Token is what /api/episode/info takes.
type Episode struct {
	Num   string `json:"num"` // usually an integer, kept as text for things like "12.5"
	Token string `json:"token"`
}

// Series is a parsed series page.
type Series struct {
	Path      string    `json:"path"`
	Title     string    `json:"title"`
	JTitle    string    `json:"jtitle"`
	Dub       bool      `json:"dub"`
	MALID     int       `json:"mal_id,omitempty"`
	AniListID int       `json:"anilist_id,omitempty"`
	Episodes  []Episode `json:"episodes"`
}

var (
	seriesPathRe = regexp.MustCompile(`^(?:https?://[^/]+)?(/play/[^/?#]+)`)
	extIDRe      = regexp.MustCompile(`/anime/(\d+)`)
	dubSlugRe    = regexp.MustCompile(`-ita\.[^./]+$`)
	dubSuffixRe  = regexp.MustCompile(`\s*\(ITA\)\s*$`)
)

// SeriesPath reduces a /play/<slug>[/<token>] link to /play/<slug>.
func SeriesPath(href string) string {
	if m := seriesPathRe.FindStringSubmatch(href); m != nil {
		return m[1]
	}
	return ""
}

// IsDub reports whether a series path is an Italian-dubbed entry
// (slug ending in -ita before the site's id suffix).
func IsDub(path string) bool { return dubSlugRe.MatchString(path) }

// CleanTitle drops the " (ITA)" marker the site appends to dubbed entries.
func CleanTitle(s string) string { return strings.TrimSpace(dubSuffixRe.ReplaceAllString(s, "")) }

// ParseSearch reads a /search or /updated page. Links to a single episode
// (as on /updated) collapse to their series; order is kept, duplicates dropped.
func ParseSearch(r io.Reader) ([]Hit, error) {
	doc, err := goquery.NewDocumentFromReader(r)
	if err != nil {
		return nil, err
	}
	if doc.Find(".film-list").Length() == 0 {
		return nil, errors.New("no .film-list in page: markup changed or request blocked")
	}
	var hits []Hit
	seen := map[string]bool{}
	doc.Find(".film-list .item a.name").Each(func(_ int, a *goquery.Selection) {
		href, _ := a.Attr("href")
		path := SeriesPath(href)
		if path == "" || seen[path] {
			return
		}
		seen[path] = true
		jt, _ := a.Attr("data-jtitle")
		hits = append(hits, Hit{Path: path, Title: CleanTitle(a.Text()), JTitle: CleanTitle(jt), Dub: IsDub(path)})
	})
	return hits, nil
}

// Update is one entry of the /updated listing: an episode the site published
// recently. The listing carries no timestamp; its order is newest first.
type Update struct {
	Path  string `json:"path"` // series path
	Title string `json:"title"`
	Dub   bool   `json:"dub"`
	Num   string `json:"num"`   // the site's episode number, as on Episode
	Token string `json:"token"` // episode token, as on Episode
}

var (
	episodePathRe = regexp.MustCompile(`^(?:https?://[^/]+)?/play/[^/?#]+/([^/?#]+)`)
	epLabelRe     = regexp.MustCompile(`(?i)^ep\.?\s*(\d+(?:\.\d+)?)$`)
)

// ParseUpdated reads the /updated page: one Update per listed episode, newest
// first. Entries without a numbered episode (movies, specials) are skipped.
func ParseUpdated(r io.Reader) ([]Update, error) {
	doc, err := goquery.NewDocumentFromReader(r)
	if err != nil {
		return nil, err
	}
	if doc.Find(".film-list").Length() == 0 {
		return nil, errors.New("no .film-list in page: markup changed or request blocked")
	}
	var ups []Update
	seen := map[string]bool{}
	doc.Find(".film-list .item").Each(func(_ int, it *goquery.Selection) {
		a := it.Find("a.name").First()
		href, _ := a.Attr("href")
		path, tok := SeriesPath(href), ""
		if m := episodePathRe.FindStringSubmatch(href); m != nil {
			tok = m[1]
		}
		lbl := epLabelRe.FindStringSubmatch(strings.TrimSpace(it.Find(".status .ep").First().Text()))
		if path == "" || tok == "" || lbl == nil || seen[tok] {
			return
		}
		seen[tok] = true
		ups = append(ups, Update{Path: path, Title: CleanTitle(a.Text()), Dub: IsDub(path), Num: lbl[1], Token: tok})
	})
	return ups, nil
}

// ParseSeries reads a series (episode) page. The caller sets Path.
func ParseSeries(r io.Reader) (Series, error) {
	doc, err := goquery.NewDocumentFromReader(r)
	if err != nil {
		return Series{}, err
	}
	h1 := doc.Find("h1#anime-title").First()
	if h1.Length() == 0 {
		return Series{}, errors.New("no h1#anime-title in page: markup changed or request blocked")
	}
	s := Series{Title: CleanTitle(h1.Text())}
	s.JTitle, _ = h1.Attr("data-jtitle")
	s.JTitle = CleanTitle(s.JTitle)
	s.MALID = externalID(doc, "a#mal-button")
	s.AniListID = externalID(doc, "a#anilist-button")

	seen := map[string]bool{}
	doc.Find("a[data-episode-num][data-id]").Each(func(_ int, a *goquery.Selection) {
		num, _ := a.Attr("data-episode-num")
		tok, _ := a.Attr("data-id")
		// The same episode is listed once per server; keep the first.
		if num == "" || tok == "" || seen[num] {
			return
		}
		seen[num] = true
		s.Episodes = append(s.Episodes, Episode{Num: num, Token: tok})
	})
	return s, nil
}

func externalID(doc *goquery.Document, sel string) int {
	href, _ := doc.Find(sel).First().Attr("href")
	if m := extIDRe.FindStringSubmatch(href); m != nil {
		id, _ := strconv.Atoi(m[1])
		return id
	}
	return 0
}

// ParseEpisodeInfo extracts the direct file URL from /api/episode/info.
func ParseEpisodeInfo(r io.Reader) (string, error) {
	var v struct {
		Grabber string `json:"grabber"`
	}
	if err := json.NewDecoder(r).Decode(&v); err != nil {
		return "", fmt.Errorf("episode info: %w", err)
	}
	if v.Grabber == "" {
		return "", errors.New("episode info: no grabber URL")
	}
	return v.Grabber, nil
}
