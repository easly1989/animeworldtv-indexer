package mapping

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// AniListURL is AniList's public GraphQL API. Its romaji title is MyAnimeList's,
// which AnimeWorld uses, so it is a search term the site understands.
const AniListURL = "https://graphql.anilist.co"

// Titles finds a series' title from its MAL id, for searches that arrive with a
// TVDB id but no title. Answers are cached for good.
type Titles struct {
	url   string
	cache Cache
	hc    *http.Client
}

func NewTitles(url string, cache Cache) *Titles {
	return &Titles{url: url, cache: cache, hc: &http.Client{Timeout: 5 * time.Second}}
}

// Title returns "" when the id is unknown or the API is unreachable.
func (t *Titles) Title(ctx context.Context, mal int) string {
	key := fmt.Sprintf("title:mal:%d", mal)
	var title string
	if t.cache.Get(key, 365*24*time.Hour, &title) {
		return title
	}
	body := fmt.Sprintf(`{"query":"{Media(idMal:%d,type:ANIME){title{romaji}}}"}`, mal)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.url, strings.NewReader(body))
	if err != nil {
		return ""
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := t.hc.Do(req)
	if err != nil {
		slog.Warn("title lookup failed", "mal", mal, "err", err)
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		slog.Warn("title lookup failed", "mal", mal, "status", resp.Status)
		return ""
	}
	var v struct {
		Data struct {
			Media struct {
				Title struct {
					Romaji string `json:"romaji"`
				} `json:"title"`
			} `json:"Media"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil || v.Data.Media.Title.Romaji == "" {
		return ""
	}
	title = v.Data.Media.Title.Romaji
	if err := t.cache.Put(key, title); err != nil {
		slog.Warn("title cache write failed", "err", err)
	}
	return title
}
