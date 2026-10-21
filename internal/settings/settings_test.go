package settings

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/easly1989/animeworldtv-indexer/internal/config"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func newAPI(t *testing.T, getenv func(string) string) (*API, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "settings.json")
	cfg, err := config.Load(getenv)
	if err != nil {
		t.Fatal(err)
	}
	return &API{Store: Open(path), Getenv: getenv, Running: cfg.Editable()}, path
}

type reply struct {
	Settings        []item `json:"settings"`
	RestartRequired bool   `json:"restart_required"`
	Error           string `json:"error"`
}

func call(t *testing.T, h http.HandlerFunc, method, body string) (int, reply) {
	t.Helper()
	w := httptest.NewRecorder()
	h(w, httptest.NewRequest(method, "/api/settings", strings.NewReader(body)))
	var r reply
	if err := json.Unmarshal(w.Body.Bytes(), &r); err != nil {
		t.Fatalf("%s: %v", w.Body, err)
	}
	return w.Code, r
}

func field(r reply, key string) item {
	for _, it := range r.Settings {
		if it.Key == key {
			return it
		}
	}
	return item{}
}

func TestGetShowsRunningValues(t *testing.T) {
	a, _ := newAPI(t, env(map[string]string{"MAX_DOWNLOADS": "3"}))
	code, r := call(t, a.Get, "GET", "")
	if code != 200 || len(r.Settings) != len(Fields) || r.RestartRequired {
		t.Fatalf("code=%d %+v", code, r)
	}
	if f := field(r, "MAX_DOWNLOADS"); f.Value != "3" || f.Running != "3" || f.Saved {
		t.Fatalf("%+v", f)
	}
	if f := field(r, "SEARCH_BUDGET"); f.Value != "40s" {
		t.Fatalf("default not shown: %+v", f)
	}
}

func TestPutSavesAndAsksForRestart(t *testing.T) {
	a, path := newAPI(t, env(nil))
	code, r := call(t, a.Put, "PUT", `{"MAX_DOWNLOADS":"4","ORIGIN_RPS":"0.5"}`)
	if code != 200 || !r.RestartRequired {
		t.Fatalf("code=%d %+v", code, r)
	}
	if f := field(r, "MAX_DOWNLOADS"); f.Value != "4" || f.Running != "2" || !f.Saved {
		t.Fatalf("%+v", f)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
	// The next start reads them over the environment.
	saved, _ := a.Store.Load()
	cfg, err := config.Load(Overlay(saved, env(nil)))
	if err != nil || cfg.MaxDownloads != 4 || cfg.OriginRPS != 0.5 {
		t.Fatalf("%v %+v", err, cfg)
	}
	// An empty value goes back to the environment.
	if _, r = call(t, a.Put, "PUT", `{"MAX_DOWNLOADS":""}`); field(r, "MAX_DOWNLOADS").Saved {
		t.Fatal("override not removed")
	}
}

func TestPutRejectsBadInput(t *testing.T) {
	a, path := newAPI(t, env(nil))
	for _, body := range []string{`{"MAX_DOWNLOADS":"0"}`, `{"ORIGIN_RPS":"fast"}`, `{"API_KEY":"x"}`, `{"DATA_DIR":"/"}`, `[1]`, `{"MAX_DOWNLOADS":3}`} {
		if code, r := call(t, a.Put, "PUT", body); code != 400 || r.Error == "" {
			t.Errorf("%s: code=%d %+v", body, code, r)
		}
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("a rejected change was saved")
	}
}
