package status

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/easly1989/animeworldtv-indexer/internal/downloader"
	"github.com/easly1989/animeworldtv-indexer/internal/solver"
)

// contract mirrors the agreed /api/status payload (issue #8), so a renamed
// field or changed type fails here.
type contract struct {
	Service       string `json:"service"`
	Version       string `json:"version"`
	UptimeSeconds int64  `json:"uptime_seconds"`
	Status        string `json:"status"`
	Site          struct {
		URL       string `json:"url"`
		Reachable bool   `json:"reachable"`
	} `json:"site"`
	Solver struct {
		Configured   bool       `json:"configured"`
		URL          string     `json:"url"`
		CookiesValid bool       `json:"cookies_valid"`
		ExpiresAt    *time.Time `json:"expires_at"`
	} `json:"solver"`
	Queue []struct {
		Hash       string  `json:"hash"`
		Name       string  `json:"name"`
		Category   string  `json:"category"`
		State      string  `json:"state"`
		Progress   float64 `json:"progress"`
		Size       int64   `json:"size"`
		Downloaded int64   `json:"downloaded"`
		DLSpeed    int64   `json:"dlspeed"`
		ETA        int64   `json:"eta"`
		AddedAt    string  `json:"added_at"`
	} `json:"queue"`
	Errors []struct {
		Timestamp string `json:"timestamp"`
		Message   string `json:"message"`
	} `json:"errors"`
}

const key = "s3cret"

func fixture(t *testing.T, opt Options) (*Server, *Hub, *httptest.Server) {
	t.Helper()
	hub := NewHub()
	opt.APIKey = key
	s := New(hub, opt)
	mux := http.NewServeMux()
	s.Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return s, hub, srv
}

func get(t *testing.T, url, header string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	if header != "" {
		req.Header.Set("X-Api-Key", header)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func TestStatusPayload(t *testing.T) {
	added := time.Date(2026, 10, 20, 21, 30, 0, 0, time.UTC)
	exp := added.Add(12 * time.Hour)
	jobs := []downloader.Job{
		{Hash: "b", Name: "Done", Category: "tv-sonarr", State: downloader.Completed, Size: 100, Done: 100, Added: added.Add(time.Minute)},
		{Hash: "a", Name: "Frieren S01E03 [SUB ITA]", Category: "tv-sonarr", State: downloader.Downloading, Size: 400, Done: 100, Speed: 50, Added: added},
		{Hash: "c", Name: "Broken", State: downloader.Failed, Err: "boom", Added: added.Add(2 * time.Minute)},
		{Hash: "d", Name: "Future", State: downloader.State(99), Added: added.Add(3 * time.Minute)},
	}
	_, hub, srv := fixture(t, Options{
		Version: "1.2.0", SiteURL: "https://www.animeworld.ac",
		Reachable: func() bool { return true },
		Solver: func() solver.Status {
			return solver.Status{Configured: true, URL: "http://byparr:8191", CookiesValid: true, ExpiresAt: &exp}
		},
		Jobs: func() []downloader.Job { return jobs },
	})
	newLogger(&bytes.Buffer{}, hub).Error("file size lookup failed", "err", "x")

	resp := get(t, srv.URL+"/api/status", key)
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("status %d, type %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	var p contract
	if err := json.NewDecoder(resp.Body).Decode(&p); err != nil {
		t.Fatal(err)
	}
	if p.Service != "animeworldtv-indexer" || p.Version != "1.2.0" || p.Status != "ok" || p.UptimeSeconds < 0 {
		t.Errorf("header fields: %+v", p)
	}
	if p.Site.URL != "https://www.animeworld.ac" || !p.Site.Reachable {
		t.Errorf("site: %+v", p.Site)
	}
	if !p.Solver.Configured || p.Solver.URL != "http://byparr:8191" || !p.Solver.CookiesValid || p.Solver.ExpiresAt == nil || !p.Solver.ExpiresAt.Equal(exp) {
		t.Errorf("solver: %+v", p.Solver)
	}
	if len(p.Queue) != 4 || p.Queue[0].Hash != "a" { // oldest first
		t.Fatalf("queue: %+v", p.Queue)
	}
	q := p.Queue[0]
	if q.State != "downloading" || q.Progress != 0.25 || q.Size != 400 || q.Downloaded != 100 || q.DLSpeed != 50 || q.ETA != 6 ||
		q.AddedAt != "2026-10-20T21:30:00Z" || q.Name != "Frieren S01E03 [SUB ITA]" || q.Category != "tv-sonarr" {
		t.Errorf("downloading item: %+v", q)
	}
	if p.Queue[1].State != "completed" || p.Queue[1].Progress != 1 || p.Queue[2].State != "error" || p.Queue[3].State != "queued" {
		t.Errorf("states: %+v", p.Queue)
	}
	if len(p.Errors) != 1 || p.Errors[0].Message != "file size lookup failed err=x" || len(p.Errors[0].Timestamp) != len(timeLayout) {
		t.Errorf("errors: %+v", p.Errors)
	}
}

func TestStatusEmptyAndDegraded(t *testing.T) {
	_, _, srv := fixture(t, Options{Reachable: func() bool { return false }})
	resp := get(t, srv.URL+"/api/status", key)
	var raw map[string]json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		t.Fatal(err)
	}
	// Empty lists must be [] and an absent solver an object, never null.
	for k, want := range map[string]string{"queue": "[]", "errors": "[]", "status": `"degraded"`} {
		if string(raw[k]) != want {
			t.Errorf("%s = %s, want %s", k, raw[k], want)
		}
	}
	if !strings.Contains(string(raw["solver"]), `"configured":false`) || !strings.Contains(string(raw["solver"]), `"expires_at":null`) {
		t.Errorf("solver = %s", raw["solver"])
	}
}

func TestAuth(t *testing.T) {
	_, _, srv := fixture(t, Options{})
	for _, path := range []string{"/api/status", "/api/logs"} {
		if c := get(t, srv.URL+path, "").StatusCode; c != 401 {
			t.Errorf("%s without key: %d", path, c)
		}
		if c := get(t, srv.URL+path, "wrong").StatusCode; c != 401 {
			t.Errorf("%s with a wrong key: %d", path, c)
		}
	}
	if c := get(t, srv.URL+"/api/status?apikey="+key, "").StatusCode; c != 200 {
		t.Errorf("apikey query: %d", c)
	}
	if c := get(t, srv.URL+"/api/status", key).StatusCode; c != 200 {
		t.Errorf("header: %d", c)
	}
}

// events reads SSE frames off a response: "data" payloads and comments.
func events(t *testing.T, resp *http.Response) <-chan string {
	t.Helper()
	ch := make(chan string, 32)
	go func() {
		defer close(ch)
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			if l := sc.Text(); l != "" {
				ch <- l
			}
		}
	}()
	return ch
}

func next(t *testing.T, ch <-chan string) string {
	t.Helper()
	select {
	case l := <-ch:
		return l
	case <-time.After(5 * time.Second):
		t.Fatal("no event within 5 s")
		return ""
	}
}

func decode(t *testing.T, line string) []Entry {
	t.Helper()
	var es []Entry
	if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &es); err != nil || !strings.HasPrefix(line, "data: ") {
		t.Fatalf("bad data line %q: %v", line, err)
	}
	return es
}

func TestLogsStream(t *testing.T) {
	_, hub, srv := fixture(t, Options{Keepalive: 50 * time.Millisecond})
	log := newLogger(&bytes.Buffer{}, hub)
	for i := 0; i < 130; i++ {
		log.Info("old", "i", i)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/api/logs?apikey="+key, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content type %q", ct)
	}
	ch := events(t, resp)

	first := decode(t, next(t, ch))
	if len(first) != 100 || first[0].Message != "old i=30" || first[99].Message != "old i=129" {
		t.Fatalf("first batch: %d entries, %+v ... %+v", len(first), first[0], first[len(first)-1])
	}
	if l := next(t, ch); l != ": keepalive" { // idle: only keepalives
		t.Fatalf("unexpected line while idle: %q", l)
	}

	log.With("component", "torznab").Error("live")
	for {
		l := next(t, ch)
		if l == ": keepalive" {
			continue
		}
		es := decode(t, l)
		if len(es) != 1 || es[0].Message != "live" || es[0].Level != "ERROR" || es[0].Logger != "torznab" {
			t.Fatalf("live entry: %+v", es)
		}
		break
	}

	cancel() // a disconnect must free the subscription
	deadline := time.Now().Add(5 * time.Second)
	for {
		hub.mu.Lock()
		n := len(hub.subs)
		hub.mu.Unlock()
		if n == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("subscriber still registered after disconnect")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestLogsEmptyFirstBatchIsAnArray(t *testing.T) {
	_, _, srv := fixture(t, Options{})
	resp := get(t, srv.URL+"/api/logs", key)
	if l := next(t, events(t, resp)); l != "data: []" {
		t.Errorf("first event = %q", l)
	}
}
