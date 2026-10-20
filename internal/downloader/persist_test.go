package downloader

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/easly1989/animeworldtv-indexer/internal/store"
)

// rangeServer serves one video for every token, honours Range like a real file
// server and records what it was asked and how much it sent.
type rangeServer struct {
	srv  *httptest.Server
	data []byte

	mu          sync.Mutex
	ranges      []string // Range header of each request ("" for none)
	served      int64    // body bytes sent
	stallAt     int      // first request: send this many bytes, then hang
	dropAt      int      // every request: send at most this many bytes, then cut the connection
	ignoreRange bool     // answer 200 with the whole file whatever is asked
	failN       int      // answer 503 to this many requests first
	missing     bool     // answer 404
	hold        chan struct{}
}

func newRangeServer(t *testing.T) *rangeServer {
	s := &rangeServer{data: bytes.Repeat([]byte("0123456789abcdef"), 4<<10)} // 64 KiB
	s.srv = httptest.NewServer(http.HandlerFunc(s.handle))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *rangeServer) FileURL(_ context.Context, token string) (string, error) {
	return s.srv.URL + "/" + token, nil
}

func (s *rangeServer) handle(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.ranges = append(s.ranges, r.Header.Get("Range"))
	first := len(s.ranges) == 1
	if s.missing {
		s.mu.Unlock()
		http.NotFound(w, r)
		return
	}
	if s.failN > 0 {
		s.failN--
		s.mu.Unlock()
		http.Error(w, "busy", http.StatusServiceUnavailable)
		return
	}
	stall, drop, ignore, hold := s.stallAt, s.dropAt, s.ignoreRange, s.hold
	s.mu.Unlock()

	if hold != nil {
		select {
		case <-hold:
		case <-r.Context().Done():
			return
		}
	}
	start := 0
	if rg := r.Header.Get("Range"); rg != "" && !ignore {
		_, _ = fmt.Sscanf(rg, "bytes=%d-", &start)
	}
	body := s.data[start:]
	w.Header().Set("Content-Type", "video/mp4")
	w.Header().Set("Content-Length", fmt.Sprint(len(body)))
	if start > 0 {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, len(s.data)-1, len(s.data)))
		w.WriteHeader(http.StatusPartialContent)
	}
	send := func(b []byte) {
		n, _ := w.Write(b)
		s.mu.Lock()
		s.served += int64(n)
		s.mu.Unlock()
	}
	if stall > 0 && first {
		send(body[:stall])
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		return
	}
	if drop > 0 && len(body) > drop {
		send(body[:drop])
		w.(http.Flusher).Flush()
		panic(http.ErrAbortHandler)
	}
	send(body)
}

func (s *rangeServer) requests() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.ranges...)
}

func (s *rangeServer) bytesServed() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.served
}

// persistent returns a Manager backed by the SQLite file at db, the way
// main wires it. Closing the Manager and the store is left to t.Cleanup unless
// the test restarts earlier.
func persistent(t *testing.T, res Resolver, dir, db string, opts ...Option) (*Manager, *store.Store) {
	t.Helper()
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	m := New(res, dir, nil, append([]Option{WithStore(st)}, opts...)...)
	m.backoff = 5 * time.Millisecond
	t.Cleanup(m.Close)
	return m, st
}

func waitDone(t *testing.T, m *Manager, token string, atLeast int64) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if j, _ := m.Get(Hash(token)); j.Done >= atLeast {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("job %s never reached %d bytes", token, atLeast)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// interrupted leaves a half-downloaded episode behind, as a killed process
// would, and returns the paths and the size of the partial file.
func interrupted(t *testing.T, s *rangeServer, dir, db string) (partial string, size int64) {
	t.Helper()
	s.stallAt = 20000
	m, st := persistent(t, s, dir, db)
	j, err := m.Add("aaa", "Show - 01", "c")
	if err != nil {
		t.Fatal(err)
	}
	waitDone(t, m, "aaa", int64(s.stallAt))
	m.Close() // no state change: the row stays "downloading"
	st.Close()
	partial = (&entry{Job: j}).tmp()
	fi, err := os.Stat(partial)
	if err != nil || fi.Size() == 0 {
		t.Fatalf("no partial file: %v", err)
	}
	return partial, fi.Size()
}

func TestRestartResumesWithRange(t *testing.T) {
	s := newRangeServer(t)
	dir, db := t.TempDir(), filepath.Join(t.TempDir(), "q.db")
	partial, size := interrupted(t, s, dir, db)
	served := s.bytesServed()

	m, _ := persistent(t, s, dir, db)
	// Right after the restart the job is back and already counts the partial file.
	if j, ok := m.Get(Hash("aaa")); !ok || j.Category != "c" || j.Done != size {
		t.Fatalf("restored job: %+v %v", j, ok)
	}
	j := waitFor(t, m, "aaa", Completed)
	if got := s.requests(); len(got) != 2 || got[1] != fmt.Sprintf("bytes=%d-", size) {
		t.Errorf("requests %q, want a Range request from byte %d", got, size)
	}
	if again := s.bytesServed() - served; again != int64(len(s.data))-size {
		t.Errorf("second run received %d bytes, want only the missing %d", again, int64(len(s.data))-size)
	}
	if b, err := os.ReadFile(j.Path()); err != nil || !bytes.Equal(b, s.data) {
		t.Errorf("resumed file differs from the source: %v", err)
	}
	if _, err := os.Stat(partial); err == nil {
		t.Error("partial file left behind")
	}
	if got := m.Categories(); len(got) != 1 || got[0] != "c" {
		t.Errorf("categories after restart: %v", got)
	}
}

func TestRestartWhenServerIgnoresRangeStartsOver(t *testing.T) {
	s := newRangeServer(t)
	dir, db := t.TempDir(), filepath.Join(t.TempDir(), "q.db")
	_, _ = interrupted(t, s, dir, db)
	served := s.bytesServed()
	s.mu.Lock()
	s.ignoreRange = true
	s.mu.Unlock()

	m, _ := persistent(t, s, dir, db)
	j := waitFor(t, m, "aaa", Completed)
	if again := s.bytesServed() - served; again != int64(len(s.data)) {
		t.Errorf("second run received %d bytes, want the whole %d", again, len(s.data))
	}
	if b, err := os.ReadFile(j.Path()); err != nil || !bytes.Equal(b, s.data) {
		t.Errorf("file differs from the source (stale partial kept?): %v", err)
	}
}

func TestRestoreCompletedOrphansAndCategories(t *testing.T) {
	s := newRangeServer(t)
	dir, db := t.TempDir(), filepath.Join(t.TempDir(), "q.db")
	m, st := persistent(t, s, dir, db)
	m.AddCategory("empty")
	m.Add("kept", "Kept - 01", "c")
	m.Add("gone", "Gone - 01", "c")
	kept := waitFor(t, m, "kept", Completed)
	gone := waitFor(t, m, "gone", Completed)
	m.Close()
	st.Close()

	if err := os.Remove(gone.Path()); err != nil { // the client took it
		t.Fatal(err)
	}
	orphan := filepath.Join(dir, "c", "Old - 01.mp4.0123abcd.part")
	stray := filepath.Join(dir, "c", "notes.part") // not ours: must stay
	for _, p := range []string{orphan, stray} {
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	m2, st2 := persistent(t, s, dir, db)
	if j, ok := m2.Get(kept.Hash); !ok || j.State != Completed || j.Done != int64(len(s.data)) {
		t.Errorf("completed job not restored: %+v %v", j, ok)
	}
	if _, ok := m2.Get(gone.Hash); ok {
		t.Error("completed job whose file is gone was restored")
	}
	if rows, _ := st2.Downloads(); len(rows) != 1 {
		t.Errorf("%d rows kept, want 1", len(rows))
	}
	if exists(orphan) {
		t.Error("orphan partial file not removed")
	}
	if !exists(stray) {
		t.Error("a foreign .part file was removed")
	}
	if got := strings.Join(m2.Categories(), ","); got != "c,empty" {
		t.Errorf("categories %q", got)
	}
	if n := len(s.requests()); n != 2 {
		t.Errorf("restore downloaded again: %d requests", n)
	}
}

func TestParallelLimitQueuesTheRest(t *testing.T) {
	s := newRangeServer(t)
	s.hold = make(chan struct{})
	m, _ := persistent(t, s, t.TempDir(), filepath.Join(t.TempDir(), "q.db"), WithMaxDownloads(2))
	for _, tok := range []string{"a", "b", "c", "d"} {
		if _, err := m.Add(tok, "Show "+tok, "c"); err != nil {
			t.Fatal(err)
		}
	}
	count := func() (run, queued int) {
		for _, j := range m.List() {
			switch j.State {
			case Downloading:
				run++
			case Queued:
				queued++
			}
		}
		return
	}
	if run, queued := count(); run != 2 || queued != 2 {
		t.Fatalf("%d running %d queued, want 2 and 2", run, queued)
	}
	close(s.hold)
	for _, tok := range []string{"a", "b", "c", "d"} {
		waitFor(t, m, tok, Completed)
	}
}

func TestQueuedJobPausesAndLeavesTheQueue(t *testing.T) {
	s := newRangeServer(t)
	s.hold = make(chan struct{})
	m, _ := persistent(t, s, t.TempDir(), filepath.Join(t.TempDir(), "q.db"), WithMaxDownloads(1))
	m.Add("a", "A", "c")
	m.Add("b", "B", "c")
	if j, _ := m.Get(Hash("b")); j.State != Queued {
		t.Fatalf("b is %v, want queued", j.State)
	}
	m.Pause(Hash("b"))
	close(s.hold)
	waitFor(t, m, "a", Completed)
	time.Sleep(50 * time.Millisecond)
	if j, _ := m.Get(Hash("b")); j.State != Paused {
		t.Errorf("paused job is %v", j.State)
	}
	m.Resume(Hash("b"))
	waitFor(t, m, "b", Completed)
}

func TestRetryThenSuccess(t *testing.T) {
	s := newRangeServer(t)
	s.failN = 2
	m, _ := persistent(t, s, t.TempDir(), filepath.Join(t.TempDir(), "q.db"))
	m.Add("aaa", "Show - 01", "c")
	waitFor(t, m, "aaa", Completed)
	if n := len(s.requests()); n != 3 {
		t.Errorf("%d requests, want 3 (two 503s then success)", n)
	}
}

func TestRetryExhaustedFails(t *testing.T) {
	s := newRangeServer(t)
	s.failN = 100
	m, _ := persistent(t, s, t.TempDir(), filepath.Join(t.TempDir(), "q.db"))
	m.Add("aaa", "Show - 01", "c")
	j := waitFor(t, m, "aaa", Failed)
	if n := len(s.requests()); n != maxAttempts {
		t.Errorf("%d requests, want %d", n, maxAttempts)
	}
	if !strings.Contains(j.Err, "503") {
		t.Errorf("error %q", j.Err)
	}
}

func TestMissingFileIsNotRetried(t *testing.T) {
	s := newRangeServer(t)
	s.missing = true
	m, _ := persistent(t, s, t.TempDir(), filepath.Join(t.TempDir(), "q.db"))
	m.Add("aaa", "Show - 01", "c")
	waitFor(t, m, "aaa", Failed)
	if n := len(s.requests()); n != 1 {
		t.Errorf("%d requests, want 1 for a 404", n)
	}
}

func TestAddPausedThenResume(t *testing.T) {
	s := newRangeServer(t)
	m, st := persistent(t, s, t.TempDir(), filepath.Join(t.TempDir(), "q.db"))
	j, err := m.AddPaused("aaa", "Show - 01", "c", true)
	if err != nil || j.State != Paused {
		t.Fatalf("added paused: %+v %v", j, err)
	}
	time.Sleep(50 * time.Millisecond)
	if n := len(s.requests()); n != 0 {
		t.Fatalf("a paused job downloaded (%d requests)", n)
	}
	if rows, _ := st.Downloads(); len(rows) != 1 {
		t.Errorf("paused job not persisted: %d rows", len(rows))
	}
	if !m.Resume(j.Hash) {
		t.Fatal("resume: unknown job")
	}
	waitFor(t, m, "aaa", Completed)
}

func TestPauseKeepsPartialAndResumeContinues(t *testing.T) {
	s := newRangeServer(t)
	s.stallAt = 20000
	m, _ := persistent(t, s, t.TempDir(), filepath.Join(t.TempDir(), "q.db"))
	j, _ := m.Add("aaa", "Show - 01", "c")
	waitDone(t, m, "aaa", int64(s.stallAt))
	m.Pause(j.Hash)
	if got, _ := m.Get(j.Hash); got.State != Paused {
		t.Fatalf("state %v after pause", got.State)
	}
	m.Resume(j.Hash) // right away: the old run may still be stopping
	done := waitFor(t, m, "aaa", Completed)
	if b, err := os.ReadFile(done.Path()); err != nil || !bytes.Equal(b, s.data) {
		t.Errorf("file differs after pause/resume: %v", err)
	}
	if r := s.requests(); len(r) != 2 || !strings.HasPrefix(r[1], "bytes=") {
		t.Errorf("requests %q, want a Range resume", r)
	}
}

func TestDeleteRemovesFilesAndRow(t *testing.T) {
	s := newRangeServer(t)
	m, st := persistent(t, s, t.TempDir(), filepath.Join(t.TempDir(), "q.db"))
	m.Add("aaa", "Show - 01", "c")
	j := waitFor(t, m, "aaa", Completed)
	if !m.Delete(j.Hash, true) {
		t.Fatal("delete: unknown job")
	}
	if exists(j.Path()) {
		t.Error("final file left")
	}
	if rows, _ := st.Downloads(); len(rows) != 0 {
		t.Errorf("%d rows left", len(rows))
	}

	// A paused job's partial file goes with it, even without deleteFiles.
	s.stallAt = 20000
	s.mu.Lock()
	s.ranges = nil
	s.mu.Unlock()
	m.Add("bbb", "Show - 02", "c")
	waitDone(t, m, "bbb", int64(s.stallAt))
	b, _ := m.Get(Hash("bbb"))
	m.Pause(b.Hash)
	partial := (&entry{Job: b}).tmp()
	if !exists(partial) {
		t.Fatal("no partial file to delete")
	}
	m.Delete(b.Hash, false)
	if exists(partial) {
		t.Error("partial file left")
	}
	if rows, _ := st.Downloads(); len(rows) != 0 {
		t.Errorf("%d rows left", len(rows))
	}
}

func TestSameEpisodeUnderAnotherCategoryIsRefused(t *testing.T) {
	s := newRangeServer(t)
	m, _ := persistent(t, s, t.TempDir(), filepath.Join(t.TempDir(), "q.db"))
	if _, err := m.Add("aaa", "Show - 01", "sonarr"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Add("aaa", "Show - 01", "radarr"); !errors.Is(err, ErrCategoryConflict) {
		t.Errorf("err = %v, want ErrCategoryConflict", err)
	}
	if _, err := m.Add("aaa", "Show - 01", "sonarr"); err != nil {
		t.Errorf("same category: %v", err)
	}
	if got := m.List(); len(got) != 1 || got[0].Category != "sonarr" {
		t.Errorf("jobs %+v", got)
	}
}

func TestRetriesThatProgressKeepGoing(t *testing.T) {
	s := newRangeServer(t)
	s.dropAt = 20000 // every connection breaks after 20 kB of the 64 KiB
	m, _ := persistent(t, s, t.TempDir(), filepath.Join(t.TempDir(), "q.db"))
	m.Add("aaa", "Show - 01", "c")
	j := waitFor(t, m, "aaa", Completed)
	if b, err := os.ReadFile(j.Path()); err != nil || !bytes.Equal(b, s.data) {
		t.Errorf("file differs from the source: %v", err)
	}
	if n := len(s.requests()); n != 4 {
		t.Errorf("%d requests, want 4 ranged pieces", n)
	}
}

func TestFailedJobKeepsPartialForResume(t *testing.T) {
	s := newRangeServer(t)
	s.stallAt = 20000
	m, _ := persistent(t, s, t.TempDir(), filepath.Join(t.TempDir(), "q.db"))
	m.idle = 50 * time.Millisecond
	j, _ := m.Add("aaa", "Show - 01", "c")
	waitDone(t, m, "aaa", 20000)
	s.mu.Lock()
	s.failN = 100 // after the stall, nothing but 503s
	s.mu.Unlock()
	waitFor(t, m, "aaa", Failed)
	if fi, err := os.Stat((&entry{Job: j}).tmp()); err != nil || fi.Size() != 20000 {
		t.Fatalf("partial file not kept after failure: %v", err)
	}
	s.mu.Lock()
	s.failN = 0
	s.mu.Unlock()
	m.Resume(j.Hash)
	done := waitFor(t, m, "aaa", Completed)
	if r := s.requests(); r[len(r)-1] != "bytes=20000-" {
		t.Errorf("resume asked %q, want a Range from 20000", r[len(r)-1])
	}
	if b, err := os.ReadFile(done.Path()); err != nil || !bytes.Equal(b, s.data) {
		t.Errorf("file differs from the source: %v", err)
	}
}

func TestChangedFileOnServerStartsOver(t *testing.T) {
	s := newRangeServer(t)
	dir, db := t.TempDir(), filepath.Join(t.TempDir(), "q.db")
	_, _ = interrupted(t, s, dir, db)
	s.data = bytes.Repeat([]byte("fedcba9876543210"), 5<<10+256) // another, larger file

	m, _ := persistent(t, s, dir, db)
	j := waitFor(t, m, "aaa", Completed)
	if b, err := os.ReadFile(j.Path()); err != nil || !bytes.Equal(b, s.data) {
		t.Errorf("file is a mix of the old and the new source: %v", err)
	}
}

func TestRestartWithDirSpelledDifferentlyKeepsPartial(t *testing.T) {
	s := newRangeServer(t)
	dir, db := t.TempDir(), filepath.Join(t.TempDir(), "q.db")
	_, size := interrupted(t, s, dir, db)
	served := s.bytesServed()

	t.Chdir(dir)
	m, _ := persistent(t, s, ".", db) // same folder, relative this time
	waitFor(t, m, "aaa", Completed)
	if again := s.bytesServed() - served; again != int64(len(s.data))-size {
		t.Errorf("second run received %d bytes, want only the missing %d", again, int64(len(s.data))-size)
	}
}

func TestRestartAfterRenameButBeforeSaveIsComplete(t *testing.T) {
	s := newRangeServer(t)
	dir, db := t.TempDir(), filepath.Join(t.TempDir(), "q.db")
	m, st := persistent(t, s, dir, db)
	m.Add("aaa", "Show - 01", "c")
	j := waitFor(t, m, "aaa", Completed)
	m.Close()
	j.State = Downloading // as if the process died right after the rename
	raw, _ := json.Marshal(j)
	if err := st.PutDownload(j.Hash, raw); err != nil {
		t.Fatal(err)
	}
	st.Close()

	m2, _ := persistent(t, s, dir, db)
	if got, _ := m2.Get(j.Hash); got.State != Completed || got.Done != int64(len(s.data)) {
		t.Errorf("restored %+v, want completed", got)
	}
	time.Sleep(50 * time.Millisecond)
	if n := len(s.requests()); n != 1 {
		t.Errorf("%d requests, want no new download", n)
	}
}

func TestRestoreKeepsCompletedWhenFolderIsMissing(t *testing.T) {
	s := newRangeServer(t)
	dir, db := t.TempDir(), filepath.Join(t.TempDir(), "q.db")
	m, st := persistent(t, s, dir, db)
	m.Add("aaa", "Show - 01", "c")
	j := waitFor(t, m, "aaa", Completed)
	m.Close()
	st.Close()
	if err := os.Rename(filepath.Join(dir, "c"), filepath.Join(dir, "away")); err != nil { // volume not mounted
		t.Fatal(err)
	}

	m2, st2 := persistent(t, s, dir, db)
	if _, ok := m2.Get(j.Hash); !ok {
		t.Error("completed job dropped while its folder is missing")
	}
	if rows, _ := st2.Downloads(); len(rows) != 1 {
		t.Errorf("%d rows, want 1", len(rows))
	}
}
