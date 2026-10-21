package status

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"time"

	"github.com/easly1989/animeworldtv-indexer/internal/downloader"
	"github.com/easly1989/animeworldtv-indexer/internal/settings"
	"github.com/easly1989/animeworldtv-indexer/internal/solver"
)

const (
	service       = "animeworldtv-indexer"
	firstBatch    = 100
	keepaliveWait = 15 * time.Second
)

// Options wires the server to the rest of the service.
type Options struct {
	APIKey  string // empty disables the check, like the other APIs
	Version string
	SiteURL string

	Reachable func() bool             // did the last request to the site go through
	Reason    func() string           // nil or "": why the site is unusable right now (an IP ban, say)
	Solver    func() solver.Status    // nil: no solver
	Jobs      func() []downloader.Job // nil: empty queue

	Settings *settings.API // nil: no /api/settings
	Restart  func()        // nil: no /api/restart; otherwise stops the service (Docker starts it again)

	Keepalive time.Duration // 0: 15 s
}

// Server answers /api/status and /api/logs.
type Server struct {
	hub     *Hub
	opt     Options
	started time.Time
}

func New(hub *Hub, opt Options) *Server {
	if opt.Keepalive <= 0 {
		opt.Keepalive = keepaliveWait
	}
	return &Server{hub: hub, opt: opt, started: time.Now()}
}

// Register mounts the endpoints on mux. /health needs no key.
func (s *Server) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /health", s.health)
	mux.HandleFunc("GET /api/status", s.auth(s.status))
	mux.HandleFunc("GET /api/logs", s.auth(s.logs))
	if s.opt.Settings != nil {
		mux.HandleFunc("GET /api/settings", s.auth(s.opt.Settings.Get))
		mux.HandleFunc("PUT /api/settings", s.keyed(s.opt.Settings.Put))
	}
	if s.opt.Restart != nil {
		mux.HandleFunc("POST /api/restart", s.keyed(s.restart))
	}
}

// keyed guards changes: the key must be configured (an open read API is a
// choice the operator made, an open write API should not follow from it) and
// sent as the X-Api-Key header, never in the URL, where logs keep it.
func (s *Server) keyed(f http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.opt.APIKey == "" {
			http.Error(w, "set API_KEY to change settings", http.StatusForbidden)
			return
		}
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Api-Key")), []byte(s.opt.APIKey)) != 1 {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		f(w, r)
	}
}

// restartCooldown stops a restart loop driven from the panel (or a script).
const restartCooldown = 30 * time.Second

// restart answers first, then stops the service; the container's restart
// policy starts it again with the saved settings.
func (s *Server) restart(w http.ResponseWriter, _ *http.Request) {
	if up := time.Since(s.started); up < restartCooldown {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"error": fmt.Sprintf("started %ds ago; try again in %ds", int(up.Seconds()), int((restartCooldown-up).Seconds())+1),
		})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_, _ = w.Write([]byte(`{"restarting":true}` + "\n"))
	if fl, ok := w.(http.Flusher); ok {
		fl.Flush()
	}
	slog.Info("restart requested from the admin panel")
	go s.opt.Restart()
}

// degraded says whether the site is unusable, and why when it is known.
func (s *Server) degraded() (reachable bool, reason string) {
	reachable = s.opt.Reachable == nil || s.opt.Reachable()
	if s.opt.Reason != nil {
		reason = s.opt.Reason()
	}
	return reachable, reason
}

// health is the liveness probe: always 200, with "degraded" and a reason while
// the site is refusing us. A restart would not help, so the code stays 200.
func (s *Server) health(w http.ResponseWriter, _ *http.Request) {
	reachable, reason := s.degraded()
	body := map[string]any{"status": "ok", "version": s.opt.Version, "site": s.opt.SiteURL}
	if !reachable || reason != "" {
		body["status"] = "degraded"
	}
	if reason != "" {
		body["reason"] = reason
	}
	if s.opt.Solver != nil {
		body["solver"] = s.opt.Solver()
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(body)
}

// auth accepts the key as the X-Api-Key header or the apikey query parameter.
func (s *Server) auth(f http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.opt.APIKey != "" {
			got := r.Header.Get("X-Api-Key")
			if got == "" {
				got = r.URL.Query().Get("apikey")
			}
			if subtle.ConstantTimeCompare([]byte(got), []byte(s.opt.APIKey)) != 1 {
				http.Error(w, "Unauthorized", http.StatusUnauthorized)
				return
			}
		}
		f(w, r)
	}
}

type siteInfo struct {
	URL       string `json:"url"`
	Reachable bool   `json:"reachable"`
}

type solverInfo struct {
	Configured   bool       `json:"configured"`
	URL          string     `json:"url,omitempty"`
	CookiesValid bool       `json:"cookies_valid"`
	ExpiresAt    *time.Time `json:"expires_at"`
}

type queueItem struct {
	Hash       string  `json:"hash"`
	Name       string  `json:"name"`
	Category   string  `json:"category"`
	State      string  `json:"state"`
	Progress   float64 `json:"progress"`
	Size       int64   `json:"size"`
	Downloaded int64   `json:"downloaded"`
	DLSpeed    int64   `json:"dlspeed"`
	ETA        int64   `json:"eta"` // seconds; 0 when unknown or not downloading
	AddedAt    string  `json:"added_at"`
	Error      string  `json:"error,omitempty"`
}

type errorItem struct {
	Timestamp string `json:"timestamp"`
	Message   string `json:"message"`
}

func (s *Server) status(w http.ResponseWriter, _ *http.Request) {
	reachable, reason := s.degraded()
	state := "ok"
	if !reachable || reason != "" {
		state = "degraded"
	}
	var sv solver.Status
	if s.opt.Solver != nil {
		sv = s.opt.Solver()
	}

	queue := []queueItem{}
	if s.opt.Jobs != nil {
		jobs := s.opt.Jobs()
		sort.SliceStable(jobs, func(i, j int) bool { return jobs[i].Added.Before(jobs[j].Added) })
		for _, j := range jobs {
			queue = append(queue, item(j))
		}
	}
	errs := []errorItem{}
	for _, e := range s.hub.Errors() {
		errs = append(errs, errorItem{e.Timestamp, e.Message})
	}

	w.Header().Set("Content-Type", "application/json")
	body := map[string]any{
		"service":        service,
		"version":        s.opt.Version,
		"uptime_seconds": int64(time.Since(s.started).Seconds()),
		"status":         state,
		"site":           siteInfo{URL: s.opt.SiteURL, Reachable: reachable},
		"solver":         solverInfo{sv.Configured, sv.URL, sv.CookiesValid, sv.ExpiresAt},
		"queue":          queue,
		"errors":         errs,
	}
	if reason != "" {
		body["reason"] = reason
	}
	_ = json.NewEncoder(w).Encode(body)
}

// item maps a download job to the contract's states. A state this package does
// not know (the queue grows new ones) counts as downloading when bytes flow and
// as queued otherwise.
func item(j downloader.Job) queueItem {
	q := queueItem{
		Hash: j.Hash, Name: j.Name, Category: j.Category, Size: j.Size, Downloaded: j.Done,
		DLSpeed: j.Speed, AddedAt: j.Added.UTC().Format(time.RFC3339),
	}
	switch j.State {
	case downloader.Completed:
		q.State = "completed"
	case downloader.Failed:
		q.State = "error"
		q.Error = j.Err
	case downloader.Downloading:
		q.State = "downloading"
	case downloader.Queued:
		q.State = "queued"
	case downloader.Paused:
		q.State = "paused"
	default:
		q.State = "queued"
		if j.Speed > 0 {
			q.State = "downloading"
		}
	}
	if j.Size > 0 {
		q.Progress = min(float64(j.Done)/float64(j.Size), 1)
	}
	if q.State == "completed" {
		q.Progress = 1
	}
	if q.State == "downloading" && j.Speed > 0 && j.Size > j.Done {
		q.ETA = (j.Size - j.Done) / j.Speed
	}
	return q
}

// logs streams the log as server-sent events: the last 100 entries, then each
// new one, with a comment line while idle so proxies keep the connection.
func (s *Server) logs(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no") // nginx: do not buffer the stream
	recent, ch, cancel := s.hub.Subscribe(firstBatch)
	defer cancel()

	send := func(es []Entry) bool {
		b, _ := json.Marshal(es)
		_, err := w.Write(append(append([]byte("data: "), b...), '\n', '\n'))
		fl.Flush()
		return err == nil
	}
	if !send(recent) {
		return
	}
	tick := time.NewTicker(s.opt.Keepalive)
	defer tick.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case e := <-ch:
			if !send([]Entry{e}) {
				return
			}
		case <-tick.C:
			if _, err := w.Write([]byte(": keepalive\n\n")); err != nil {
				return
			}
			fl.Flush()
		}
	}
}
