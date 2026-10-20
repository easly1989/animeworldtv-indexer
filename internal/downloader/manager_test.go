package downloader

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// site maps an episode token to a handler on a fake file server.
type site struct {
	srv      *httptest.Server
	handlers map[string]http.HandlerFunc
}

func newSite(t *testing.T) *site {
	s := &site{handlers: map[string]http.HandlerFunc{}}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if h := s.handlers[strings.TrimPrefix(r.URL.Path, "/")]; h != nil {
			h(w, r)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *site) FileURL(_ context.Context, token string) (string, error) {
	return s.srv.URL + "/" + token, nil
}

func (s *site) serve(token, content string) {
	s.handlers[token] = func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "video/mp4")
		_, _ = w.Write([]byte(content))
	}
}

func newManager(t *testing.T, s *site) *Manager {
	m := New(s, t.TempDir(), nil)
	t.Cleanup(m.Close)
	return m
}

func waitFor(t *testing.T, m *Manager, token string, want State) Job {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		j, ok := m.Get(Hash(token))
		if ok && j.State == want {
			return j
		}
		if time.Now().After(deadline) {
			t.Fatalf("job %s: want state %v, have %+v (found=%v)", token, want, j, ok)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestCategoryIsKeptAsLabel(t *testing.T) {
	s := newSite(t)
	s.serve("aaa", "video")
	m := newManager(t, s)
	j, err := m.Add("aaa", "Show - 01", "tv/anime")
	if err != nil {
		t.Fatal(err)
	}
	if j.Category != "tv/anime" {
		t.Errorf("label changed to %q", j.Category)
	}
	if got := m.Categories(); len(got) != 1 || got[0] != "tv/anime" {
		t.Errorf("categories %v", got)
	}
	// Only the folder name is made filesystem-safe.
	j = waitFor(t, m, "aaa", Completed)
	if want := filepath.Join(m.Dir(), "tv_anime"); j.SavePath != want {
		t.Errorf("save path %q, want %q", j.SavePath, want)
	}
}

func TestReAddRestartsCompletedJobWhoseFileIsGone(t *testing.T) {
	s := newSite(t)
	s.serve("aaa", "video")
	m := newManager(t, s)
	j, _ := m.Add("aaa", "Show - 01", "c")
	j = waitFor(t, m, "aaa", Completed)
	if err := os.Remove(j.Path()); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Add("aaa", "Show - 01", "c"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, m, "aaa", Completed)
	if b, err := os.ReadFile(j.Path()); err != nil || string(b) != "video" {
		t.Errorf("file not downloaded again: %q %v", b, err)
	}
	// A completed job whose file is still there is left alone.
	before, _ := m.Get(Hash("aaa"))
	m.Add("aaa", "Show - 01", "c")
	if after, _ := m.Get(Hash("aaa")); !after.Added.Equal(before.Added) {
		t.Error("completed job with its file was restarted")
	}
}

func TestStalledDownloadFails(t *testing.T) {
	s := newSite(t)
	s.handlers["aaa"] = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "video/mp4")
		w.Header().Set("Content-Length", "1000")
		_, _ = w.Write([]byte("some"))
		w.(http.Flusher).Flush()
		<-r.Context().Done() // then nothing
	}
	m := newManager(t, s)
	m.idle = 150 * time.Millisecond
	m.Add("aaa", "Show - 01", "c")
	j := waitFor(t, m, "aaa", Failed)
	if !strings.Contains(j.Err, "stalled") {
		t.Errorf("error %q", j.Err)
	}
	if left, _ := filepath.Glob(filepath.Join(j.SavePath, "*")); len(left) != 0 {
		t.Errorf("partial file left: %v", left)
	}
}

func TestDeleteThenQuickReAdd(t *testing.T) {
	s := newSite(t)
	started := make(chan struct{}, 2)
	release := make(chan struct{})
	s.handlers["aaa"] = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "video/mp4")
		w.Header().Set("Content-Length", "10")
		_, _ = w.Write([]byte("01234"))
		w.(http.Flusher).Flush()
		started <- struct{}{}
		select {
		case <-release:
			_, _ = w.Write([]byte("56789"))
		case <-r.Context().Done():
		}
	}
	m := newManager(t, s)
	m.Add("aaa", "Show - 01", "c")
	<-started
	m.Delete(Hash("aaa"), true)
	m.Add("aaa", "Show - 01", "c")
	<-started
	close(release)
	j := waitFor(t, m, "aaa", Completed)
	if b, err := os.ReadFile(j.Path()); err != nil || string(b) != "0123456789" {
		t.Errorf("new download damaged by the old one: %q %v", b, err)
	}
}

func TestSameNameDifferentEpisodesDoNotShareFiles(t *testing.T) {
	s := newSite(t)
	s.serve("aaa", "first")
	s.serve("bbb", "second")
	m := newManager(t, s)
	m.Add("aaa", "Same Title", "c")
	m.Add("bbb", "Same Title", "c")
	a := waitFor(t, m, "aaa", Completed)
	b := waitFor(t, m, "bbb", Completed)
	if a.Path() == b.Path() {
		t.Fatalf("both write %s", a.Path())
	}
	ga, _ := os.ReadFile(a.Path())
	gb, _ := os.ReadFile(b.Path())
	if string(ga) != "first" || string(gb) != "second" {
		t.Errorf("contents %q %q", ga, gb)
	}
}

func TestRejectsNonVideoResponse(t *testing.T) {
	s := newSite(t)
	s.handlers["aaa"] = func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte("<html>Just a moment...</html>"))
	}
	m := newManager(t, s)
	m.Add("aaa", "Show - 01", "c")
	j := waitFor(t, m, "aaa", Failed)
	if !strings.Contains(j.Err, "not a video") {
		t.Errorf("error %q", j.Err)
	}
	if _, err := os.Stat(j.Path()); err == nil {
		t.Error("HTML saved as mp4")
	}
}

func TestShortBodyFails(t *testing.T) {
	s := newSite(t)
	s.handlers["aaa"] = func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "video/mp4")
		w.Header().Set("Content-Length", "100")
		_, _ = w.Write([]byte("short"))
	}
	m := newManager(t, s)
	m.Add("aaa", "Show - 01", "c")
	waitFor(t, m, "aaa", Failed)
}
