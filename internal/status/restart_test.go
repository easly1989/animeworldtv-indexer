package status

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/easly1989/animeworldtv-indexer/internal/settings"
)

func send(t *testing.T, method, url, key, body string) int {
	t.Helper()
	req, _ := http.NewRequest(method, url, strings.NewReader(body))
	if key != "" {
		req.Header.Set("X-Api-Key", key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func TestSettingsAndRestartNeedTheKey(t *testing.T) {
	restarted := make(chan struct{}, 1)
	api := &settings.API{Store: settings.Open(filepath.Join(t.TempDir(), "s.json")), Getenv: func(string) string { return "" }}
	s, _, srv := fixture(t, Options{Settings: api, Restart: func() { restarted <- struct{}{} }})

	if c := send(t, "PUT", srv.URL+"/api/settings", "wrong", `{"MAX_DOWNLOADS":"3"}`); c != 401 {
		t.Errorf("PUT with a wrong key: %d", c)
	}
	// Changes take the key as a header only, never from the URL.
	if c := send(t, "PUT", srv.URL+"/api/settings?apikey="+key, "", `{"MAX_DOWNLOADS":"3"}`); c != 401 {
		t.Errorf("PUT with the key in the URL: %d", c)
	}
	if c := send(t, "POST", srv.URL+"/api/restart?apikey="+key, "", ""); c != 401 {
		t.Errorf("restart with the key in the URL: %d", c)
	}
	// No restart right after a start.
	if c := send(t, "POST", srv.URL+"/api/restart", key, ""); c != 429 {
		t.Errorf("restart during the cooldown: %d", c)
	}
	s.started = time.Now().Add(-time.Minute)
	if c := send(t, "POST", srv.URL+"/api/restart", "", ""); c != 401 {
		t.Errorf("restart without key: %d", c)
	}
	if c := send(t, "GET", srv.URL+"/api/settings", key, ""); c != 200 {
		t.Errorf("GET: %d", c)
	}
	if c := send(t, "PUT", srv.URL+"/api/settings", key, `{"MAX_DOWNLOADS":"3"}`); c != 200 {
		t.Errorf("PUT: %d", c)
	}
	if c := send(t, "POST", srv.URL+"/api/restart", key, ""); c != 202 {
		t.Errorf("restart: %d", c)
	}
	select {
	case <-restarted:
	case <-time.After(2 * time.Second):
		t.Fatal("Restart not called")
	}
}

func TestChangesNeedAConfiguredKey(t *testing.T) {
	api := &settings.API{Store: settings.Open(filepath.Join(t.TempDir(), "s.json")), Getenv: func(string) string { return "" }}
	s := New(NewHub(), Options{Settings: api, Restart: func() {}})
	mux := http.NewServeMux()
	s.Register(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()
	if c := send(t, "PUT", srv.URL+"/api/settings", "", `{"MAX_DOWNLOADS":"3"}`); c != 403 {
		t.Errorf("PUT without API_KEY: %d", c)
	}
	if c := send(t, "POST", srv.URL+"/api/restart", "", ""); c != 403 {
		t.Errorf("restart without API_KEY: %d", c)
	}
}
