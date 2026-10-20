// Package qbit emulates the subset of the qBittorrent Web API v2 that Sonarr
// and Radarr use, backed by the downloader queue. A "torrent" is an AnimeWorld
// episode: torrents/add receives the magnet from the Torznab feed and the MP4
// is downloaded over HTTP.
package qbit

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"strings"
	"sync"

	"github.com/easly1989/animeworldtv-indexer/internal/downloader"
)

const (
	appVersion = "v4.6.0"
	apiVersion = "2.9.3"
	noETA      = 8640000 // what qBittorrent sends when the ETA is unknown or done
)

type Options struct {
	APIKey string // password for login and bearer token; empty disables auth
}

type handler struct {
	opt Options
	dl  *downloader.Manager

	mu   sync.Mutex
	sids map[string]bool
}

// New returns the handler for /api/v2/.
func New(opt Options, dl *downloader.Manager) http.Handler {
	h := &handler{opt: opt, dl: dl, sids: map[string]bool{}}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v2/auth/login", h.login)
	auth := func(pattern string, f http.HandlerFunc) { mux.HandleFunc(pattern, h.authed(f)) }
	auth("/api/v2/auth/logout", func(w http.ResponseWriter, _ *http.Request) { ok(w) })
	auth("GET /api/v2/app/version", func(w http.ResponseWriter, _ *http.Request) { text(w, appVersion) })
	auth("GET /api/v2/app/webapiVersion", func(w http.ResponseWriter, _ *http.Request) { text(w, apiVersion) })
	auth("GET /api/v2/app/preferences", h.preferences)
	auth("GET /api/v2/torrents/categories", h.categories)
	auth("POST /api/v2/torrents/createCategory", h.createCategory)
	auth("POST /api/v2/torrents/add", h.add)
	auth("GET /api/v2/torrents/info", h.info)
	auth("GET /api/v2/torrents/properties", h.properties)
	auth("GET /api/v2/torrents/files", h.files)
	auth("POST /api/v2/torrents/delete", h.remove)
	auth("POST /api/v2/torrents/setCategory", h.setCategory)
	// Calls Sonarr/Radarr may make that mean nothing here: a download starts at
	// once, there is no queue order, seeding or tagging.
	for _, p := range []string{"topPrio", "setShareLimits", "setForceStart", "addTags", "pause", "resume",
		"stop", "start", "increasePrio", "decreasePrio", "bottomPrio", "setSuperSeeding"} {
		auth("POST /api/v2/torrents/"+p, func(w http.ResponseWriter, _ *http.Request) { ok(w) })
	}
	return mux
}

func (h *handler) login(w http.ResponseWriter, r *http.Request) {
	if h.opt.APIKey != "" && !sameKey(r.FormValue("password"), h.opt.APIKey) {
		text(w, "Fails.")
		return
	}
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	sid := hex.EncodeToString(b)
	h.mu.Lock()
	h.sids[sid] = true
	h.mu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: "SID", Value: sid, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode})
	ok(w)
}

// authed lets a request through with a valid SID cookie or the API key as a
// bearer token. Anything else gets 403, which makes Sonarr log in again.
func (h *handler) authed(f http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if h.opt.APIKey != "" && !h.hasSession(r) && !sameKey(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), h.opt.APIKey) {
			http.Error(w, "Forbidden", http.StatusForbidden)
			return
		}
		f(w, r)
	}
}

func (h *handler) hasSession(r *http.Request) bool {
	c, err := r.Cookie("SID")
	if err != nil {
		return false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.sids[c.Value]
}

func sameKey(got, want string) bool {
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

func (h *handler) preferences(w http.ResponseWriter, _ *http.Request) {
	// No share limits: finished downloads must not look like they are to be
	// removed at a ratio. dht is on because Sonarr refuses magnets without
	// trackers when it is off.
	writeJSON(w, map[string]any{
		"save_path":                         h.dl.Dir(),
		"max_ratio_enabled":                 false,
		"max_ratio":                         -1,
		"max_seeding_time_enabled":          false,
		"max_seeding_time":                  -1,
		"max_inactive_seeding_time_enabled": false,
		"max_inactive_seeding_time":         -1,
		"max_ratio_act":                     0,
		"queueing_enabled":                  true,
		"dht":                               true,
	})
}

func (h *handler) categories(w http.ResponseWriter, _ *http.Request) {
	out := map[string]map[string]string{}
	for _, c := range h.dl.Categories() {
		out[c] = map[string]string{"name": c, "savePath": h.dl.SavePath(c)}
	}
	writeJSON(w, out)
}

func (h *handler) createCategory(w http.ResponseWriter, r *http.Request) {
	h.dl.AddCategory(r.FormValue("category"))
	ok(w)
}

func (h *handler) setCategory(w http.ResponseWriter, r *http.Request) {
	for _, hash := range h.hashes(r) {
		h.dl.SetCategory(hash, r.FormValue("category"))
	}
	ok(w)
}

// add takes one magnet per line in "urls". Uploaded .torrent files are not
// supported: the feed never serves any.
func (h *handler) add(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(8 << 20); err != nil && !errors.Is(err, http.ErrNotMultipart) {
		text(w, "Fails.")
		return
	}
	var added int
	for _, raw := range strings.Split(r.FormValue("urls"), "\n") {
		if raw = strings.TrimSpace(raw); raw == "" {
			continue
		}
		token, name, err := downloader.ParseMagnet(raw)
		if err == nil {
			_, err = h.dl.Add(token, name, r.FormValue("category"))
		}
		if err != nil {
			text(w, "Fails.")
			return
		}
		added++
	}
	if added == 0 {
		text(w, "Fails.")
		return
	}
	ok(w)
}

func (h *handler) info(w http.ResponseWriter, r *http.Request) {
	want := h.hashFilter(r)
	cat := r.FormValue("category")
	out := []torrent{}
	for _, j := range h.dl.List() {
		if (want != nil && !want[j.Hash]) || (cat != "" && j.Category != cat) {
			continue
		}
		out = append(out, toTorrent(j))
	}
	writeJSON(w, out)
}

func (h *handler) properties(w http.ResponseWriter, r *http.Request) {
	j, found := h.dl.Get(r.FormValue("hash"))
	if !found {
		http.NotFound(w, r)
		return
	}
	t := toTorrent(j)
	writeJSON(w, map[string]any{
		"hash": t.Hash, "name": t.Name, "save_path": t.SavePath, "total_size": t.Size,
		"total_downloaded": t.Downloaded, "dl_speed": t.DLSpeed, "eta": t.ETA,
		"seeding_time": 0, "addition_date": t.AddedOn, "completion_date": t.CompletionOn,
		"share_ratio": 0,
	})
}

func (h *handler) files(w http.ResponseWriter, r *http.Request) {
	j, found := h.dl.Get(r.FormValue("hash"))
	if !found {
		http.NotFound(w, r)
		return
	}
	t := toTorrent(j)
	writeJSON(w, []map[string]any{{
		"index": 0, "name": filepath.Base(j.Path()), "size": t.Size, "progress": t.Progress, "priority": 1,
		"is_seed": j.State == downloader.Completed, "piece_range": []int{0, 0}, "availability": 1,
	}})
}

func (h *handler) remove(w http.ResponseWriter, r *http.Request) {
	files := r.FormValue("deleteFiles") == "true"
	for _, hash := range h.hashes(r) {
		h.dl.Delete(hash, files)
	}
	ok(w)
}

// hashes lists the "hashes" parameter: pipe separated, or "all".
func (h *handler) hashes(r *http.Request) []string {
	v := r.FormValue("hashes")
	if v == "all" {
		var out []string
		for _, j := range h.dl.List() {
			out = append(out, j.Hash)
		}
		return out
	}
	return strings.Split(strings.ToLower(v), "|")
}

// hashFilter is the set asked for by torrents/info, or nil for everything.
func (h *handler) hashFilter(r *http.Request) map[string]bool {
	v := r.FormValue("hashes")
	if v == "" {
		return nil
	}
	m := map[string]bool{}
	for _, s := range strings.Split(strings.ToLower(v), "|") {
		m[s] = true
	}
	return m
}

func ok(w http.ResponseWriter) { text(w, "Ok.") }

func text(w http.ResponseWriter, s string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte(s))
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
