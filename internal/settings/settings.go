// Package settings keeps the values changed from mircrewrr's admin panel in
// DATA_DIR/settings.json. They override the environment from the next start:
// the service builds its Config once, so a change needs a restart.
package settings

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/easly1989/animeworldtv-indexer/internal/config"
)

// Field is one setting the panel may change; Key is its environment variable.
type Field struct {
	Key, Label, Help string
}

// Fields lists the editable settings in the order the panel shows them.
var Fields = []Field{
	{"MAX_DOWNLOADS", "Simultaneous downloads", "Episodes downloaded at once; the others wait in the queue"},
	{"ORIGIN_RPS", "Requests per second to AnimeWorld", "Higher is faster but the site bans IPs it finds abusive"},
	{"ORIGIN_BURST", "Request burst", "Requests to AnimeWorld that may go out back to back"},
	{"SIZE_LOOKUPS", "File sizes looked up per search", "The other sizes are estimated; 0 never looks them up"},
	{"MAX_SERIES", "Series per search", "Series pages opened for one search"},
	{"CONCURRENCY", "Parallel requests", "Simultaneous requests to AnimeWorld"},
	{"SEARCH_BUDGET", "Search time budget", "How long a search waits for file sizes, e.g. 40s"},
	{"LOG_LEVEL", "Log level", "DEBUG, INFO, WARNING or ERROR"},
}

func editable(key string) bool {
	for _, f := range Fields {
		if f.Key == key {
			return true
		}
	}
	return false
}

// Store reads and writes the saved values.
type Store struct {
	path string
	mu   sync.Mutex
}

func Open(path string) *Store { return &Store{path: path} }

// Load returns the saved values; a missing file means none.
func (s *Store) Load() (map[string]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.load()
}

func (s *Store) load() (map[string]string, error) {
	m := map[string]string{}
	b, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return m, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("%s: %w: %w", s.path, errInvalid, err)
	}
	// Only the panel's keys: a hand-edited {"API_KEY": ""} must not turn auth off.
	for k := range m {
		if !editable(k) {
			slog.Warn("ignoring a setting the panel cannot change", "file", s.path, "key", k)
			delete(m, k)
		}
	}
	return m, nil
}

// errInvalid marks a file that was read but is not valid JSON of strings.
var errInvalid = errors.New("not a JSON object of strings")

// Resolve returns the Config to start with: base (from the environment) with
// the saved values on top. Saved values that cannot be parsed or no longer
// validate would keep the container in a restart loop the panel cannot fix, so
// they are moved aside and base is used; problem says why. A file that cannot
// be read right now (permissions, I/O) is left where it is.
func (s *Store) Resolve(base config.Config, getenv func(string) string) (cfg config.Config, saved map[string]string, problem error) {
	saved, err := s.Load()
	if err != nil && !errors.Is(err, errInvalid) {
		return base, nil, fmt.Errorf("%w (left in place)", err)
	}
	if err == nil {
		if cfg, err = config.Load(Overlay(saved, getenv)); err == nil {
			return cfg, saved, nil
		}
	}
	moved, qerr := s.Quarantine()
	if qerr != nil {
		return base, nil, fmt.Errorf("%w (could not move it aside: %v)", err, qerr)
	}
	return base, nil, fmt.Errorf("%w (moved to %s)", err, moved)
}

// Quarantine moves an unusable file aside so the service can start from the
// environment and the panel can save again; it returns the new name, dated so
// an earlier one is kept.
func (s *Store) Quarantine() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	dst := s.path + ".corrupt-" + time.Now().UTC().Format("20060102T150405.000Z")
	for i, base := 1, dst; fileExists(dst); i++ { // two in the same millisecond
		dst = fmt.Sprintf("%s-%d", base, i)
	}
	return dst, os.Rename(s.path, dst)
}

func fileExists(p string) bool { _, err := os.Lstat(p); return err == nil }

// save replaces the file atomically and durably: a crash leaves the old file or the new one.
func (s *Store) save(m map[string]string) error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(s.path)
	f, err := os.CreateTemp(dir, ".settings-*.tmp") // mode 0600
	if err != nil {
		return err
	}
	defer os.Remove(f.Name()) // no-op once renamed
	if _, err = f.Write(b); err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if err := os.Rename(f.Name(), s.path); err != nil {
		return err
	}
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync() // persists the rename; not supported on every OS
		d.Close()
	}
	return nil
}

// Overlay reads saved values first and getenv for the rest; only editable keys
// come from saved, whoever built the map.
func Overlay(saved map[string]string, getenv func(string) string) func(string) string {
	return func(key string) string {
		if v, ok := saved[key]; ok && editable(key) {
			return v
		}
		return getenv(key)
	}
}

// Path is where the saved values live inside the data directory.
func Path(dataDir string) string { return filepath.Join(dataDir, "settings.json") }

// API serves GET and PUT /api/settings. Running holds the values in use since
// the start (Config.Editable).
type API struct {
	Store   *Store
	Getenv  func(string) string
	Running map[string]string
}

type item struct {
	Key     string `json:"key"`
	Label   string `json:"label"`
	Help    string `json:"help"`
	Value   string `json:"value"`   // in use after the next start
	Running string `json:"running"` // in use now
	Saved   bool   `json:"saved"`   // overridden from the panel, not the environment
}

// payload describes every field: the value the next start will use, the one in use now.
func (a *API) payload(saved map[string]string) (map[string]any, error) {
	next, err := config.Load(Overlay(saved, a.Getenv))
	if err != nil {
		return nil, err
	}
	values := next.Editable()
	items := make([]item, 0, len(Fields))
	restart := false
	for _, f := range Fields {
		_, ok := saved[f.Key]
		items = append(items, item{f.Key, f.Label, f.Help, values[f.Key], a.Running[f.Key], ok})
		restart = restart || values[f.Key] != a.Running[f.Key]
	}
	return map[string]any{"settings": items, "restart_required": restart}, nil
}

func (a *API) Get(w http.ResponseWriter, _ *http.Request) {
	saved, err := a.Store.Load()
	if err == nil {
		var body map[string]any
		if body, err = a.payload(saved); err == nil {
			writeJSON(w, http.StatusOK, body)
			return
		}
	}
	writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
}

// Put merges {"KEY": "value"} into the saved values; an empty value goes back
// to the environment. Nothing is saved unless the result is a valid Config.
func (a *API) Put(w http.ResponseWriter, r *http.Request) {
	var changes map[string]string
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&changes); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "body must be a JSON object of strings"})
		return
	}
	a.Store.mu.Lock()
	defer a.Store.mu.Unlock()
	saved, err := a.Store.load()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	for k, v := range changes {
		if !editable(k) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": k + " cannot be changed from the panel"})
			return
		}
		if v == "" {
			delete(saved, k)
		} else {
			saved[k] = v
		}
	}
	body, err := a.payload(saved)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if err := a.Store.save(saved); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, body)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
