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

func TestResolve(t *testing.T) {
	base, _ := config.Load(env(nil))
	getenv := env(map[string]string{"API_KEY": "k"})
	for name, tc := range map[string]struct {
		file      string
		downloads int
		corrupt   bool
	}{
		"no file":             {"", 2, false},
		"saved values apply":  {`{"MAX_DOWNLOADS":"3"}`, 3, false},
		"empty file":          {" ", 2, true},
		"truncated":           {`{"MAX_DOWNLOADS":"3"`, 2, true},
		"a number, not text":  {`{"MAX_DOWNLOADS":3}`, 2, true},
		"out of range now":    {`{"MAX_DOWNLOADS":"999"}`, 2, true},
		"keys it cannot edit": {`{"API_KEY":"","ANIMEWORLD_URL":"http://evil.test","DATA_DIR":"/","MAX_DOWNLOADS":"4"}`, 4, false},
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "settings.json")
			if tc.file != "" {
				if err := os.WriteFile(path, []byte(tc.file), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			cfg, _, problem := Open(path).Resolve(base, getenv)
			if cfg.MaxDownloads != tc.downloads || (problem != nil) != tc.corrupt {
				t.Fatalf("downloads=%d problem=%v", cfg.MaxDownloads, problem)
			}
			if tc.corrupt {
				if m, _ := filepath.Glob(path + ".corrupt-*"); len(m) != 1 {
					t.Fatal("unusable file not moved aside:", m)
				}
				return
			}
			if tc.file != "" && (cfg.APIKey != "k" || cfg.BaseURL != "https://www.animeworld.ac" || cfg.DataDir != "/app/data") {
				t.Fatalf("a non-editable key leaked from the file: %+v", cfg)
			}
		})
	}
}

func TestResolveLeavesAnUnreadableFileInPlace(t *testing.T) {
	base, _ := config.Load(env(nil))
	path := filepath.Join(t.TempDir(), "settings.json")
	if err := os.Mkdir(path, 0o700); err != nil { // reading a directory fails, like EACCES would
		t.Fatal(err)
	}
	cfg, _, problem := Open(path).Resolve(base, env(nil))
	if problem == nil || cfg.MaxDownloads != 2 {
		t.Fatalf("problem=%v cfg=%+v", problem, cfg)
	}
	if m, _ := filepath.Glob(path + ".corrupt-*"); len(m) != 0 {
		t.Fatal("a file that could not be read was moved aside:", m)
	}
}

func TestQuarantineKeepsEarlierCopies(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	s := Open(path)
	for i := 0; i < 2; i++ {
		if err := os.WriteFile(path, []byte("{"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Quarantine(); err != nil { // back to back: often the same millisecond
			t.Fatal(err)
		}
	}
	if m, _ := filepath.Glob(path + ".corrupt-*"); len(m) != 2 {
		t.Fatal("expected two dated copies:", m)
	}
}

func TestOverlayOnlyAppliesEditableKeys(t *testing.T) {
	get := Overlay(map[string]string{"API_KEY": "", "MAX_DOWNLOADS": "5"}, env(map[string]string{"API_KEY": "k"}))
	if get("API_KEY") != "k" || get("MAX_DOWNLOADS") != "5" {
		t.Fatalf("API_KEY=%q MAX_DOWNLOADS=%q", get("API_KEY"), get("MAX_DOWNLOADS"))
	}
}

func TestPutDoesNotKeepForeignKeys(t *testing.T) {
	a, path := newAPI(t, env(nil))
	if err := os.WriteFile(path, []byte(`{"API_KEY":"","MAX_SERIES":"9"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, _ := call(t, a.Put, "PUT", `{"MAX_DOWNLOADS":"3"}`); code != 200 {
		t.Fatal(code)
	}
	b, _ := os.ReadFile(path)
	if strings.Contains(string(b), "API_KEY") || !strings.Contains(string(b), "MAX_SERIES") {
		t.Fatalf("saved: %s", b)
	}
}

func TestPutRejectsBadInput(t *testing.T) {
	a, path := newAPI(t, env(nil))
	for _, body := range []string{`{"MAX_DOWNLOADS":"0"}`, `{"ORIGIN_RPS":"fast"}`, `{"API_KEY":"x"}`, `{"DATA_DIR":"/"}`, `[1]`, `{"MAX_DOWNLOADS":3}`,
		`{"ORIGIN_RPS":"1e300"}`, `{"ORIGIN_RPS":"1e-10"}`, `{"CONCURRENCY":"100000000"}`, `{"MAX_DOWNLOADS":"1000000"}`,
		`{"ORIGIN_BURST":"1000000"}`, `{"SEARCH_BUDGET":"1ns"}`, `{"LOG_LEVEL":"BOGUS"}`} {
		if code, r := call(t, a.Put, "PUT", body); code != 400 || r.Error == "" {
			t.Errorf("%s: code=%d %+v", body, code, r)
		}
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("a rejected change was saved")
	}
}
