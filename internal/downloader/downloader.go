// Package downloader keeps the queue of episode downloads behind the emulated
// qBittorrent API: it resolves an episode token to the direct MP4, copies it to
// <dir>/<category>/<name>.mp4 and tracks progress.
//
// Manager is the single place that owns the queue. With a Store (WithStore)
// jobs and categories survive a restart: unfinished downloads resume from the
// bytes already on disk, and at most MaxDownloads run at once.
package downloader

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// A browser-like agent: the file servers sit behind the same CDN as the site.
const userAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0 Safari/537.36"

const (
	idleTimeout     = 60 * time.Second
	deleteWait      = 5 * time.Second // how long Delete waits for the download goroutine
	maxAttempts     = 3               // tries per download before the job is marked failed
	retryBackoff    = 2 * time.Second // wait before the 2nd try; doubles after each failure
	defaultParallel = 2
)

// State values are persisted: only ever append.
type State int

const (
	Downloading State = iota
	Completed
	Failed
	Queued // waiting for a download slot
	Paused // stopped by the client; the partial file is kept
)

// ErrCategoryConflict is returned by Add when the episode is already tracked
// under another category.
var ErrCategoryConflict = errors.New("episode already queued under another category")

// partName matches the partial files this package writes (see entry.tmp).
var partName = regexp.MustCompile(`\.mp4\.[0-9a-f]{8}\.part$`)

// Resolver turns an episode token into the direct file URL.
type Resolver interface {
	FileURL(ctx context.Context, token string) (string, error)
}

// Store persists the queue. Rows are opaque JSON; *store.Store implements it.
type Store interface {
	PutDownload(hash string, data []byte) error
	DeleteDownload(hash string) error
	Downloads() ([][]byte, error)
	PutCategory(name string) error
	Categories() ([]string, error)
}

// Option configures a Manager.
type Option func(*Manager)

// WithStore makes the queue persistent and restores it from s.
func WithStore(s Store) Option { return func(m *Manager) { m.store = s } }

// WithMaxDownloads limits simultaneous downloads (default 2).
func WithMaxDownloads(n int) Option {
	return func(m *Manager) {
		if n > 0 {
			m.sem = make(chan struct{}, n)
		}
	}
}

// Job is a snapshot of one download. It is also the persisted row, so the
// exported fields are stored as they are.
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

// Path is the final file.
func (j Job) Path() string { return filepath.Join(j.SavePath, j.Name+".mp4") }

// entry is a Job plus what runs it. The path helpers read only fields that
// never change after Add, so they are safe to call while the download goroutine
// updates the progress fields under Manager.mu.
type entry struct {
	Job
	cancel context.CancelFunc // nil while a job added paused has never run
	done   chan struct{}      // closed when the current goroutine has returned
}

func (e *entry) path() string { return filepath.Join(e.SavePath, e.Name+".mp4") }

// tmp carries the hash so two jobs never write the same partial file.
func (e *entry) tmp() string { return e.path() + "." + e.Hash[:8] + ".part" }

type Manager struct {
	res     Resolver
	dir     string
	http    *http.Client
	store   Store
	idle    time.Duration // a download that receives nothing for this long fails
	backoff time.Duration // base wait between attempts
	sem     chan struct{} // one token per running download
	ctx     context.Context
	stop    context.CancelFunc
	wg      sync.WaitGroup

	mu   sync.Mutex
	jobs map[string]*entry
	cats map[string]bool
}

// New returns a Manager downloading under dir. A nil hc gets a client with
// connection timeouts but no overall one: episodes are hundreds of MB. With a
// store, the saved queue is restored and unfinished downloads resume.
func New(res Resolver, dir string, hc *http.Client, opts ...Option) *Manager {
	if hc == nil {
		hc = &http.Client{Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			TLSHandshakeTimeout:   15 * time.Second,
			ResponseHeaderTimeout: 45 * time.Second,
		}}
	}
	ctx, stop := context.WithCancel(context.Background())
	m := &Manager{res: res, dir: dir, http: hc, idle: idleTimeout, backoff: retryBackoff,
		sem: make(chan struct{}, defaultParallel), ctx: ctx, stop: stop,
		jobs: map[string]*entry{}, cats: map[string]bool{}}
	for _, o := range opts {
		o(m)
	}
	if m.store != nil {
		m.restore()
	}
	return m
}

// Dir is the download root.
func (m *Manager) Dir() string { return m.dir }

// SavePath is where files of a category go.
func (m *Manager) SavePath(category string) string {
	return filepath.Join(m.dir, SafeName(category))
}

// Close stops every running download and waits for them. Unfinished jobs stay
// in the store and resume on the next start.
func (m *Manager) Close() {
	m.stop()
	m.wg.Wait()
}

// restore loads the saved queue. Unfinished jobs restart (continuing their
// partial file), finished ones stay while their file exists, and partial files
// no job owns are deleted.
func (m *Manager) restore() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if cats, err := m.store.Categories(); err != nil {
		slog.Warn("restore categories failed", "err", err)
	} else {
		for _, c := range cats {
			m.cats[c] = true
		}
	}
	rows, err := m.store.Downloads()
	if err != nil {
		slog.Warn("restore downloads failed", "err", err)
		return
	}
	keep := map[string]bool{} // partial file names: DOWNLOAD_DIR may be spelled differently now
	var resume []*entry
	for _, raw := range rows {
		e := &entry{}
		if json.Unmarshal(raw, &e.Job) != nil || len(e.Hash) < 8 {
			slog.Warn("skipping unreadable download row")
			continue
		}
		e.Speed = 0
		m.jobs[e.Hash] = e
		if e.State == Completed {
			fi, err := os.Stat(e.path())
			switch {
			case err == nil:
				e.Done, e.Size = fi.Size(), fi.Size()
			case errors.Is(err, os.ErrNotExist) && exists(e.SavePath): // the client already took or deleted it
				delete(m.jobs, e.Hash)
				_ = m.store.DeleteDownload(e.Hash)
			} // otherwise the folder itself is missing (volume not mounted?): keep the row
			continue
		}
		keep[filepath.Base(e.tmp())] = true
		e.Done = 0
		if fi, err := os.Stat(e.tmp()); err == nil {
			e.Done = fi.Size()
		} else if fi, err := os.Stat(e.path()); err == nil {
			// The last run renamed the file but died before recording it.
			e.State, e.Done, e.Size, e.Finished = Completed, fi.Size(), fi.Size(), time.Now()
			m.save(e)
			continue
		}
		if e.State != Paused && e.State != Failed {
			resume = append(resume, e)
		}
	}
	m.sweepOrphans(keep)
	sort.Slice(resume, func(i, j int) bool { return resume[i].Added.Before(resume[j].Added) })
	for _, e := range resume {
		m.start(e)
	}
	slog.Info("download queue restored", "jobs", len(m.jobs), "resumed", len(resume))
}

// sweepOrphans deletes partial files, in the root and in category folders,
// that are not in keep.
func (m *Manager) sweepOrphans(keep map[string]bool) {
	dirs := []string{m.dir}
	if ents, err := os.ReadDir(m.dir); err == nil {
		for _, d := range ents {
			if d.IsDir() {
				dirs = append(dirs, filepath.Join(m.dir, d.Name()))
			}
		}
	}
	for _, d := range dirs {
		ents, _ := os.ReadDir(d)
		for _, f := range ents {
			if p := filepath.Join(d, f.Name()); !f.IsDir() && partName.MatchString(f.Name()) && !keep[f.Name()] {
				if err := os.Remove(p); err == nil {
					slog.Info("removed orphan partial file", "path", p)
				}
			}
		}
	}
}

// save writes the job's row (m.mu held). A job that was deleted is not written
// back by a goroutine that is still winding down.
func (m *Manager) save(e *entry) {
	if m.store == nil || m.jobs[e.Hash] != e {
		return
	}
	raw, err := json.Marshal(e.Job)
	if err == nil {
		err = m.store.PutDownload(e.Hash, raw)
	}
	if err != nil {
		slog.Warn("save download failed", "episode", e.Token, "err", err)
	}
}

// addCategory records a category (m.mu held).
func (m *Manager) addCategory(c string) {
	if c == "" || m.cats[c] {
		return
	}
	m.cats[c] = true
	if m.store != nil {
		if err := m.store.PutCategory(c); err != nil {
			slog.Warn("save category failed", "category", c, "err", err)
		}
	}
}

// Add queues an episode (idempotent per episode) and starts downloading it. A
// previous failed attempt is restarted, and so is a completed one whose file
// has disappeared (the client asks again after it lost the file). The category
// is a label kept as given; only the folder name is made filesystem-safe.
//
// The job hash derives from the episode only, so an episode is tracked under
// one category at a time: while it is live, adding it under a different
// category (Sonarr and Radarr grabbing the same episode) fails with
// ErrCategoryConflict.
func (m *Manager) Add(token, name, category string) (Job, error) {
	return m.AddPaused(token, name, category, false)
}

// AddPaused is Add with the client's "add stopped" flag: a new job then waits
// in the Paused state until Resume. An existing job is returned unchanged.
func (m *Manager) AddPaused(token, name, category string, paused bool) (Job, error) {
	if !ValidToken(token) {
		return Job{}, errors.New("malformed episode token")
	}
	hash := Hash(token)
	name = SafeName(name)
	if name == "" {
		name = token
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if old := m.jobs[hash]; old != nil {
		switch {
		case old.State == Failed:
		case old.State == Completed && !exists(old.path()):
		case old.Category != category:
			slog.Warn("episode already queued under another category, refusing", "episode", token,
				"category", old.Category, "requested", category)
			return Job{}, ErrCategoryConflict
		default:
			return old.Job, nil
		}
	}
	m.addCategory(category)
	savePath := m.SavePath(category)
	for _, o := range m.jobs { // another episode with the same file name
		if o.Hash != hash && o.SavePath == savePath && strings.EqualFold(o.Name, name) {
			name += " [" + hash[:8] + "]"
			break
		}
	}
	if old := m.jobs[hash]; old != nil {
		_ = os.Remove(old.tmp()) // a failed job's leftovers; the new job starts clean
	}
	e := &entry{Job: Job{
		Hash: hash, Token: token, Name: name, Category: category,
		SavePath: savePath, Added: time.Now(),
	}}
	m.jobs[hash] = e
	if paused {
		e.State = Paused
		m.save(e)
	} else {
		m.start(e)
	}
	return e.Job, nil
}

// start runs the job in a new goroutine (m.mu held). It takes a download slot
// at once when one is free; otherwise the job is Queued until one is.
func (m *Manager) start(e *entry) {
	prev := e.done // a paused run may still be winding down
	ctx, cancel := context.WithCancel(m.ctx)
	done := make(chan struct{})
	e.cancel, e.done = cancel, done
	e.Err = ""
	held := false
	select {
	case m.sem <- struct{}{}:
		held, e.State = true, Downloading
	default:
		e.State = Queued
	}
	m.save(e)
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		defer close(done)
		m.run(ctx, e, prev, held)
	}()
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
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
	m.mu.Lock()
	defer m.mu.Unlock()
	m.addCategory(category)
	if e := m.jobs[strings.ToLower(hash)]; e != nil {
		e.Category = category
		m.save(e)
	}
}

// AddCategory records a category so it is listed even when empty.
func (m *Manager) AddCategory(name string) {
	m.mu.Lock()
	m.addCategory(name)
	m.mu.Unlock()
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

// Pause stops a queued or running download, keeping the partial file. It
// reports whether the job exists.
func (m *Manager) Pause(hash string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	e := m.jobs[strings.ToLower(hash)]
	if e == nil {
		return false
	}
	if e.State == Queued || e.State == Downloading {
		e.cancel()
		e.State, e.Speed = Paused, 0
		m.save(e)
	}
	return true
}

// Resume restarts a paused or failed job (a paused one continues from its
// partial file). It reports whether the job exists.
func (m *Manager) Resume(hash string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	e := m.jobs[strings.ToLower(hash)]
	if e == nil {
		return false
	}
	if e.State == Paused || e.State == Failed {
		m.start(e)
	}
	return true
}

// Delete stops a job and forgets it; with files it also removes the finished
// file. The partial file always goes, since nothing could resume it. It waits
// (bounded) for the download to stop, so the files are not touched again
// afterwards and the same episode can be added right away.
func (m *Manager) Delete(hash string, files bool) bool {
	hash = strings.ToLower(hash)
	m.mu.Lock()
	e := m.jobs[hash]
	if e == nil {
		m.mu.Unlock()
		return false
	}
	delete(m.jobs, hash)
	cancel, done := e.cancel, e.done
	if m.store != nil {
		if err := m.store.DeleteDownload(hash); err != nil {
			slog.Warn("delete download row failed", "episode", e.Token, "err", err)
		}
	}
	m.mu.Unlock()
	if cancel != nil {
		cancel()
		select {
		case <-done:
		case <-time.After(deleteWait):
			slog.Warn("download did not stop in time", "episode", e.Token)
		}
	}
	remove := []string{e.tmp()}
	if files {
		remove = append(remove, e.path())
	}
	for _, p := range remove {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			slog.Warn("delete file failed", "path", p, "err", err)
		}
	}
	return true
}

// run waits for a download slot (held: start already took one), downloads with
// retries and records the outcome.
func (m *Manager) run(ctx context.Context, e *entry, prev <-chan struct{}, held bool) {
	if prev != nil {
		<-prev // even when cancelled: a later run waits on our done, so two must never write one file
	}
	if !held {
		select {
		case m.sem <- struct{}{}:
		case <-ctx.Done():
			return
		}
	}
	defer func() { <-m.sem }()

	m.mu.Lock()
	// A paused run that was resumed may have finished while this one waited.
	if ctx.Err() != nil || e.State == Completed {
		m.mu.Unlock()
		return
	}
	if e.State != Downloading {
		e.State = Downloading
		m.save(e)
	}
	m.mu.Unlock()

	err := m.fetchWithRetry(ctx, e)

	m.mu.Lock()
	defer m.mu.Unlock()
	// Deleted, paused or shutting down: nobody is watching.
	if m.jobs[e.Hash] != e || (err != nil && ctx.Err() != nil) {
		return
	}
	e.Speed = 0
	if err != nil {
		if errors.As(err, new(permanent)) { // nothing left to resume; otherwise Resume continues the partial file
			_ = os.Remove(e.tmp())
		}
		e.State, e.Err = Failed, err.Error()
		m.save(e)
		slog.Error("download failed", "episode", e.Token, "name", e.Name, "err", err)
		return
	}
	e.State, e.Finished = Completed, time.Now()
	m.save(e)
	slog.Info("download complete", "episode", e.Token, "name", e.Name, "bytes", e.Done)
}

// permanent marks an error that another attempt cannot fix.
type permanent struct{ error }

func (p permanent) Unwrap() error { return p.error }

// fetchWithRetry makes up to maxAttempts tries in a row without progress, with
// exponential backoff. Each try resolves the file URL again and continues from
// the partial file; a try that gets further than any before starts the count over.
func (m *Manager) fetchWithRetry(ctx context.Context, e *entry) error {
	best := m.done(e)
	for attempt := 1; ; attempt++ {
		err := m.fetch(ctx, e)
		if err == nil || ctx.Err() != nil || errors.As(err, new(permanent)) {
			return err
		}
		if d := m.done(e); d > best { // bounded by the file size, so this ends
			best, attempt = d, 1
		}
		if attempt == maxAttempts {
			return err
		}
		wait := m.backoff << (attempt - 1)
		slog.Warn("download attempt failed, retrying", "episode", e.Token, "attempt", attempt, "wait", wait, "err", err)
		select {
		case <-time.After(wait):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (m *Manager) done(e *entry) int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return e.Done
}

// fetch is one attempt. It continues the partial file with a Range request
// when there is one; a server that answers 200 instead starts over.
func (m *Manager) fetch(ctx context.Context, e *entry) (err error) {
	u, err := m.res.FileURL(ctx, e.Token)
	if err != nil {
		return fmt.Errorf("resolve episode: %w", err)
	}

	// attempt ends when the job is deleted or when nothing arrives for m.idle.
	attempt, cancel := context.WithCancel(ctx)
	defer cancel()
	idle := time.AfterFunc(m.idle, cancel)
	defer idle.Stop()
	defer func() {
		if err != nil && ctx.Err() == nil && attempt.Err() != nil {
			err = fmt.Errorf("stalled: no data for %s", m.idle)
		}
	}()

	tmp := e.tmp()
	var offset int64
	if fi, err := os.Stat(tmp); err == nil {
		offset = fi.Size()
	}
	m.mu.Lock()
	known := e.Size // what an earlier run was told the file size is; 0 when never told
	m.mu.Unlock()
	req, err := http.NewRequestWithContext(attempt, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", userAgent)
	if offset > 0 {
		req.Header.Set("Range", "bytes="+strconv.FormatInt(offset, 10)+"-")
	}
	resp, err := m.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	flags := os.O_WRONLY | os.O_CREATE | os.O_TRUNC
	switch resp.StatusCode {
	case http.StatusOK: // no Range, or the server ignored it: the whole file
		offset = 0
	case http.StatusPartialContent:
		var start, end, total int64
		n, _ := fmt.Sscanf(resp.Header.Get("Content-Range"), "bytes %d-%d/%d", &start, &end, &total)
		if n < 1 || offset == 0 || start != offset {
			_ = os.Remove(tmp)
			return errors.New("server answered a different range than asked")
		}
		if n == 3 && known > 0 && total != known { // the mirror now serves another file
			_ = os.Remove(tmp)
			return fmt.Errorf("file changed on the server (%d bytes, was %d), starting over", total, known)
		}
		flags = os.O_WRONLY | os.O_APPEND
	case http.StatusRequestedRangeNotSatisfiable:
		var total int64
		if _, err := fmt.Sscanf(resp.Header.Get("Content-Range"), "bytes */%d", &total); err != nil {
			total = known // some CDNs omit the header
		}
		if offset > 0 && total == offset {
			return m.finish(e, offset) // an earlier run died just before the rename
		}
		_ = os.Remove(tmp)
		return errors.New("partial file does not fit the server's file, starting over")
	case http.StatusNotFound, http.StatusGone:
		return permanent{fmt.Errorf("GET file: %s", resp.Status)}
	default:
		return fmt.Errorf("GET file: %s", resp.Status)
	}
	// A challenge or error page comes back as 200 HTML; never save it as a video.
	if ct := resp.Header.Get("Content-Type"); strings.HasPrefix(strings.ToLower(ct), "text/") {
		return fmt.Errorf("not a video: Content-Type %s", ct)
	}
	if err := os.MkdirAll(e.SavePath, 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(tmp, flags, 0o644)
	if err != nil {
		return err
	}

	m.mu.Lock()
	e.Done, e.lastCheck, e.lastDone = offset, time.Time{}, offset
	e.Size = 0
	if resp.ContentLength > 0 {
		e.Size = offset + resp.ContentLength
	}
	m.save(e)
	m.mu.Unlock()
	n, err := io.Copy(f, &progress{r: resp.Body, m: m, e: e, idle: idle})
	if err != nil {
		f.Close()
		return err
	}
	if resp.ContentLength > 0 && n != resp.ContentLength {
		f.Close()
		return fmt.Errorf("short download: %d of %d bytes", n, resp.ContentLength)
	}
	if err := f.Close(); err != nil {
		return err
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return m.finish(e, offset+n)
}

// finish publishes a complete partial file under its final name.
func (m *Manager) finish(e *entry, size int64) error {
	m.mu.Lock()
	e.Done, e.Size = size, size
	m.mu.Unlock()
	return os.Rename(e.tmp(), e.path())
}

// progress counts bytes, refreshes the job's speed about once a second and
// feeds the idle timer.
type progress struct {
	r    io.Reader
	m    *Manager
	e    *entry
	idle *time.Timer
}

func (p *progress) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	if n > 0 {
		p.idle.Reset(p.m.idle)
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
