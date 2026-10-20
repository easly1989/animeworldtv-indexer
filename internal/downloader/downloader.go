// Package downloader keeps the queue of episode downloads behind the emulated
// qBittorrent API: it resolves an episode token to the direct MP4, copies it to
// <dir>/<category>/<name>.mp4 and tracks progress.
//
// The queue lives in memory. Manager is the single place that owns it, so a
// persistent store (and resume, a parallelism limit) can replace the jobs map
// without touching the API layer.
package downloader

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// A browser-like agent: the file servers sit behind the same CDN as the site.
const userAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0 Safari/537.36"

type State int

const (
	Downloading State = iota
	Completed
	Failed
)

// Resolver turns an episode token into the direct file URL.
type Resolver interface {
	FileURL(ctx context.Context, token string) (string, error)
}

// Job is a snapshot of one download.
type Job struct {
	Hash      string
	Token     string
	Name      string // release name, without extension
	Category  string
	SavePath  string // directory holding the file
	State     State
	Err       string
	Size      int64 // 0 until the server announces it
	Done      int64
	Speed     int64 // bytes/s over the last second or so
	Added     time.Time
	Finished  time.Time
	lastCheck time.Time // speed sampling
	lastDone  int64
}

// Path is the final file, TempPath the one being written.
func (j Job) Path() string     { return filepath.Join(j.SavePath, j.Name+".mp4") }
func (j Job) TempPath() string { return j.Path() + ".part" }

type entry struct {
	Job
	cancel context.CancelFunc
}

type Manager struct {
	res  Resolver
	dir  string
	http *http.Client
	ctx  context.Context
	stop context.CancelFunc
	wg   sync.WaitGroup

	mu   sync.Mutex
	jobs map[string]*entry
	cats map[string]bool
}

// New returns a Manager downloading under dir. A nil hc gets a client with
// connection timeouts but no overall one: episodes are hundreds of MB.
func New(res Resolver, dir string, hc *http.Client) *Manager {
	if hc == nil {
		hc = &http.Client{Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			TLSHandshakeTimeout:   15 * time.Second,
			ResponseHeaderTimeout: 45 * time.Second,
		}}
	}
	ctx, stop := context.WithCancel(context.Background())
	return &Manager{res: res, dir: dir, http: hc, ctx: ctx, stop: stop,
		jobs: map[string]*entry{}, cats: map[string]bool{}}
}

// Dir is the download root.
func (m *Manager) Dir() string { return m.dir }

// SavePath is where files of a category go.
func (m *Manager) SavePath(category string) string {
	return filepath.Join(m.dir, SafeName(category))
}

// Close stops every running download and waits for them.
func (m *Manager) Close() {
	m.stop()
	m.wg.Wait()
}

// Add queues an episode (idempotent per episode) and starts downloading it. A
// previous failed attempt is restarted.
func (m *Manager) Add(token, name, category string) (Job, error) {
	if !ValidToken(token) {
		return Job{}, errors.New("malformed episode token")
	}
	hash := Hash(token)
	name = SafeName(name)
	if name == "" {
		name = token
	}
	category = SafeName(category)

	m.mu.Lock()
	defer m.mu.Unlock()
	if old := m.jobs[hash]; old != nil && old.State != Failed {
		return old.Job, nil
	}
	if category != "" {
		m.cats[category] = true
	}
	ctx, cancel := context.WithCancel(m.ctx)
	e := &entry{Job: Job{
		Hash: hash, Token: token, Name: name, Category: category,
		SavePath: m.SavePath(category), Added: time.Now(),
	}, cancel: cancel}
	m.jobs[hash] = e
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		m.run(ctx, e)
	}()
	return e.Job, nil
}

// List returns every job, oldest first.
func (m *Manager) List() []Job {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Job, 0, len(m.jobs))
	for _, e := range m.jobs {
		out = append(out, e.Job)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Added.Before(out[j].Added) })
	return out
}

func (m *Manager) Get(hash string) (Job, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if e := m.jobs[strings.ToLower(hash)]; e != nil {
		return e.Job, true
	}
	return Job{}, false
}

// SetCategory relabels a job. Files are not moved: like qBittorrent without
// automatic torrent management, the save path stays where it was.
func (m *Manager) SetCategory(hash, category string) {
	category = SafeName(category)
	m.mu.Lock()
	defer m.mu.Unlock()
	if category != "" {
		m.cats[category] = true
	}
	if e := m.jobs[strings.ToLower(hash)]; e != nil {
		e.Category = category
	}
}

// AddCategory records a category so it is listed even when empty.
func (m *Manager) AddCategory(name string) {
	if name = SafeName(name); name != "" {
		m.mu.Lock()
		m.cats[name] = true
		m.mu.Unlock()
	}
}

func (m *Manager) Categories() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, 0, len(m.cats))
	for c := range m.cats {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}

// Delete stops a job and forgets it; with files it also removes what was
// downloaded. A partial file is always removed.
func (m *Manager) Delete(hash string, files bool) bool {
	hash = strings.ToLower(hash)
	m.mu.Lock()
	e := m.jobs[hash]
	delete(m.jobs, hash)
	m.mu.Unlock()
	if e == nil {
		return false
	}
	e.cancel()
	if files {
		if err := os.Remove(e.Path()); err != nil && !errors.Is(err, os.ErrNotExist) {
			slog.Warn("delete file failed", "path", e.Path(), "err", err)
		}
	}
	return true
}

func (m *Manager) run(ctx context.Context, e *entry) {
	err := m.fetch(ctx, e)
	m.mu.Lock()
	defer m.mu.Unlock()
	if ctx.Err() != nil { // deleted or shutting down: nobody is watching
		return
	}
	e.Speed = 0
	if err != nil {
		e.State, e.Err = Failed, err.Error()
		slog.Error("download failed", "episode", e.Token, "name", e.Name, "err", err)
		return
	}
	e.State, e.Finished = Completed, time.Now()
	slog.Info("download complete", "episode", e.Token, "name", e.Name, "bytes", e.Done)
}

func (m *Manager) fetch(ctx context.Context, e *entry) (err error) {
	u, err := m.res.FileURL(ctx, e.Token)
	if err != nil {
		return fmt.Errorf("resolve episode: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := m.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET file: %s", resp.Status)
	}
	if err := os.MkdirAll(e.SavePath, 0o755); err != nil {
		return err
	}
	tmp := e.TempPath()
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			f.Close()
			os.Remove(tmp)
		}
	}()

	m.mu.Lock()
	e.Size = max(resp.ContentLength, 0)
	m.mu.Unlock()
	n, err := io.Copy(f, &progress{r: resp.Body, m: m, e: e})
	if err != nil {
		return err
	}
	if resp.ContentLength > 0 && n != resp.ContentLength {
		return fmt.Errorf("short download: %d of %d bytes", n, resp.ContentLength)
	}
	if err := f.Close(); err != nil {
		return err
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	m.mu.Lock()
	e.Size = n
	m.mu.Unlock()
	return os.Rename(tmp, e.Path())
}

// progress counts bytes and refreshes the job's speed about once a second.
type progress struct {
	r io.Reader
	m *Manager
	e *entry
}

func (p *progress) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	if n > 0 {
		now := time.Now()
		p.m.mu.Lock()
		e := p.e
		e.Done += int64(n)
		if dt := now.Sub(e.lastCheck); dt >= time.Second {
			if !e.lastCheck.IsZero() {
				e.Speed = int64(float64(e.Done-e.lastDone) / dt.Seconds())
			}
			e.lastCheck, e.lastDone = now, e.Done
		}
		p.m.mu.Unlock()
	}
	return n, err
}

// SafeName makes s usable as one path element: no separators or characters
// Windows and SMB shares reject, no leading/trailing dots or spaces, bounded.
func SafeName(s string) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || strings.ContainsRune(`/\:*?"<>|`, r) {
			return '_'
		}
		return r
	}, s)
	s = strings.Trim(s, " .")
	if len(s) > 200 {
		s = strings.TrimRight(strings.ToValidUTF8(s[:200], ""), " .")
	}
	return s
}
