package qbit

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/easly1989/animeworldtv-indexer/internal/downloader"
)

const key = "s3cret"

type fakeSite struct{ fileURL string }

func (f fakeSite) FileURL(_ context.Context, token string) (string, error) {
	return f.fileURL + "/" + token + ".mp4", nil
}

// harness wires the emulated client to a fake file server, the way Sonarr
// would meet it: a cookie-keeping HTTP client against the qBittorrent API.
type harness struct {
	t      *testing.T
	api    *httptest.Server
	dir    string
	client *http.Client
	video  []byte
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	video := bytes.Repeat([]byte("0123456789abcdef"), 64<<10) // 1 MiB
	files := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/vlhlNp.mp4" {
			http.NotFound(w, r)
			return
		}
		http.ServeContent(w, r, "x.mp4", time.Time{}, bytes.NewReader(video))
	}))
	t.Cleanup(files.Close)

	dir := t.TempDir()
	dl := downloader.New(fakeSite{files.URL}, dir, nil)
	t.Cleanup(dl.Close)
	api := httptest.NewServer(New(Options{APIKey: key}, dl))
	t.Cleanup(api.Close)

	jar, _ := cookiejar.New(nil)
	return &harness{t: t, api: api, dir: dir, client: &http.Client{Jar: jar}, video: video}
}

func (h *harness) do(method, path string, form url.Values) (int, string) {
	h.t.Helper()
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, _ := http.NewRequest(method, h.api.URL+path, body)
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	resp, err := h.client.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func (h *harness) login() {
	h.t.Helper()
	if code, body := h.do("POST", "/api/v2/auth/login", url.Values{"username": {"sonarr"}, "password": {key}}); code != 200 || body != "Ok." {
		h.t.Fatalf("login: %d %q", code, body)
	}
}

func (h *harness) info(query string) []torrent {
	h.t.Helper()
	code, body := h.do("GET", "/api/v2/torrents/info"+query, nil)
	var out []torrent
	if code != 200 || json.Unmarshal([]byte(body), &out) != nil {
		h.t.Fatalf("info: %d %s", code, body)
	}
	return out
}

func TestAuth(t *testing.T) {
	h := newHarness(t)
	if code, _ := h.do("GET", "/api/v2/app/version", nil); code != http.StatusForbidden {
		t.Errorf("unauthenticated call: %d, want 403", code)
	}
	if _, body := h.do("POST", "/api/v2/auth/login", url.Values{"username": {"a"}, "password": {"wrong"}}); body != "Fails." {
		t.Errorf("bad password: %q", body)
	}
	if code, _ := h.do("GET", "/api/v2/app/version", nil); code != http.StatusForbidden {
		t.Errorf("still unauthenticated after a failed login: %d", code)
	}
	h.login()
	if code, body := h.do("GET", "/api/v2/app/webapiVersion", nil); code != 200 || body != apiVersion {
		t.Errorf("webapiVersion: %d %q", code, body)
	}

	req, _ := http.NewRequest("GET", h.api.URL+"/api/v2/app/version", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != 200 {
		t.Errorf("bearer API key not accepted: %v %v", err, resp)
	}
}

// TestGrabDownloadComplete is the whole Sonarr path: test the client, add a
// magnet, watch it download, read what the importer needs, clean up.
func TestGrabDownloadComplete(t *testing.T) {
	h := newHarness(t)
	h.login()

	// "Test" button.
	var prefs struct {
		SavePath string `json:"save_path"`
		DHT      bool   `json:"dht"`
		RatioOn  bool   `json:"max_ratio_enabled"`
	}
	_, body := h.do("GET", "/api/v2/app/preferences", nil)
	if err := json.Unmarshal([]byte(body), &prefs); err != nil || prefs.SavePath != h.dir || !prefs.DHT || prefs.RatioOn {
		t.Fatalf("preferences %s: %+v %v", body, prefs, err)
	}
	if _, body := h.do("GET", "/api/v2/torrents/categories", nil); strings.TrimSpace(body) != "{}" {
		t.Errorf("categories before create: %s", body)
	}
	h.do("POST", "/api/v2/torrents/createCategory", url.Values{"category": {"tv-sonarr"}})
	_, body = h.do("GET", "/api/v2/torrents/categories", nil)
	var cats map[string]struct{ SavePath string }
	if err := json.Unmarshal([]byte(body), &cats); err != nil || cats["tv-sonarr"].SavePath != filepath.Join(h.dir, "tv-sonarr") {
		t.Errorf("categories: %s", body)
	}
	if got := h.info(""); len(got) != 0 {
		t.Fatalf("queue not empty: %+v", got)
	}

	// Grab: the magnet as the Torznab /download redirect builds it.
	name := "Sousou no Frieren - 01 [SUB ITA]"
	magnet := downloader.Magnet("vlhlNp", name)
	hash := downloader.Hash("vlhlNp")
	if code, body := h.do("POST", "/api/v2/torrents/add", url.Values{"urls": {magnet}, "category": {"tv-sonarr"}, "stopped": {"false"}}); code != 200 || body != "Ok." {
		t.Fatalf("add: %d %q", code, body)
	}
	if code, _ := h.do("POST", "/api/v2/torrents/add", url.Values{"urls": {magnet}, "category": {"tv-sonarr"}}); code != 200 {
		t.Errorf("adding twice must be harmless: %d", code)
	}

	// Activity queue: poll until completed.
	var tor torrent
	deadline := time.Now().Add(10 * time.Second)
	for {
		got := h.info("?category=tv-sonarr")
		if len(got) != 1 {
			t.Fatalf("want 1 torrent, got %+v", got)
		}
		tor = got[0]
		if tor.State == "pausedUP" {
			break
		}
		if tor.State != "downloading" || time.Now().After(deadline) {
			t.Fatalf("state %q after waiting: %+v", tor.State, tor)
		}
		time.Sleep(20 * time.Millisecond)
	}

	file := filepath.Join(h.dir, "tv-sonarr", name+".mp4")
	if tor.Hash != hash || tor.Name != name || tor.Category != "tv-sonarr" || tor.Progress != 1 ||
		tor.Size != int64(len(h.video)) || tor.AmountLeft != 0 || tor.ETA != noETA ||
		tor.SavePath != filepath.Join(h.dir, "tv-sonarr") || tor.ContentPath != file || tor.RatioLimit != 0 {
		t.Errorf("completed torrent: %+v", tor)
	}
	if got, err := os.ReadFile(file); err != nil || !bytes.Equal(got, h.video) {
		t.Errorf("downloaded file wrong: err=%v len=%d", err, len(got))
	}
	if _, err := os.Stat(file + ".part"); err == nil {
		t.Error("partial file left behind")
	}

	// Filters Sonarr uses.
	if got := h.info("?category=other"); len(got) != 0 {
		t.Errorf("category filter: %+v", got)
	}
	if got := h.info("?hashes=" + strings.ToUpper(hash)); len(got) != 1 {
		t.Errorf("hashes filter: %+v", got)
	}

	// properties / files (IsTorrentLoaded, GetImportItem).
	code, body := h.do("GET", "/api/v2/torrents/properties?hash="+hash, nil)
	var props struct {
		SavePath string `json:"save_path"`
	}
	if code != 200 || json.Unmarshal([]byte(body), &props) != nil || props.SavePath != tor.SavePath {
		t.Errorf("properties: %d %s", code, body)
	}
	if code, _ := h.do("GET", "/api/v2/torrents/properties?hash=deadbeef", nil); code != 404 {
		t.Errorf("unknown hash must 404 (Sonarr probes with it): %d", code)
	}
	var files []struct {
		Name string `json:"name"`
	}
	_, body = h.do("GET", "/api/v2/torrents/files?hash="+hash, nil)
	if json.Unmarshal([]byte(body), &files) != nil || len(files) != 1 || files[0].Name != name+".mp4" {
		t.Errorf("files: %s", body)
	}

	// Calls that must be harmless, then removal after import.
	for _, p := range []string{"topPrio", "setShareLimits", "setForceStart"} {
		if code, _ := h.do("POST", "/api/v2/torrents/"+p, url.Values{"hashes": {hash}}); code != 200 {
			t.Errorf("%s: %d", p, code)
		}
	}
	h.do("POST", "/api/v2/torrents/setCategory", url.Values{"hashes": {hash}, "category": {"imported"}})
	if got := h.info("?category=imported"); len(got) != 1 || got[0].ContentPath != file {
		t.Errorf("setCategory must relabel without moving the file: %+v", got)
	}
	h.do("POST", "/api/v2/torrents/delete", url.Values{"hashes": {hash}, "deleteFiles": {"true"}})
	if got := h.info(""); len(got) != 0 {
		t.Errorf("not deleted: %+v", got)
	}
	if _, err := os.Stat(file); err == nil {
		t.Error("deleteFiles left the file")
	}
}

func TestAddRejectsForeignAndBrokenLinks(t *testing.T) {
	h := newHarness(t)
	h.login()
	for _, urls := range []string{
		"", "https://example.com/x.torrent", "magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567",
		"magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567&aw=vlhlNp", // hash does not match token
	} {
		if _, body := h.do("POST", "/api/v2/torrents/add", url.Values{"urls": {urls}}); body != "Fails." {
			t.Errorf("urls=%q: %q, want Fails.", urls, body)
		}
	}
}

// Sonarr's Test creates the category, then expects to find it by the same
// name; a grab is then looked up by that name too.
func TestCategoryWithSlashRoundTrips(t *testing.T) {
	h := newHarness(t)
	h.login()
	h.do("POST", "/api/v2/torrents/createCategory", url.Values{"category": {"tv/anime"}})
	_, body := h.do("GET", "/api/v2/torrents/categories", nil)
	var cats map[string]any
	if err := json.Unmarshal([]byte(body), &cats); err != nil || cats["tv/anime"] == nil {
		t.Fatalf("categories: %s", body)
	}
	h.do("POST", "/api/v2/torrents/add", url.Values{"urls": {downloader.Magnet("vlhlNp", "Show - 01")}, "category": {"tv/anime"}})
	if got := h.info("?category=" + url.QueryEscape("tv/anime")); len(got) != 1 || got[0].Category != "tv/anime" {
		t.Errorf("grab not found by its category: %+v", got)
	}
}

func TestLoginFormIsCapped(t *testing.T) {
	h := newHarness(t)
	// The right password, followed by 1 MiB of padding.
	body := "username=a&password=" + key + "&pad=" + strings.Repeat("x", 1<<20)
	resp, err := http.Post(h.api.URL+"/api/v2/auth/login", "application/x-www-form-urlencoded", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if b, _ := io.ReadAll(resp.Body); string(b) != "Fails." {
		t.Errorf("oversized login accepted: %q", b)
	}
}

func TestFailedDownloadShowsAsError(t *testing.T) {
	h := newHarness(t)
	h.login()
	h.do("POST", "/api/v2/torrents/add", url.Values{"urls": {downloader.Magnet("missing", "Gone - 01")}, "category": {"c"}})
	deadline := time.Now().Add(5 * time.Second)
	for {
		got := h.info("")
		if len(got) == 1 && got[0].State == "error" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("want an errored torrent, got %+v", got)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
