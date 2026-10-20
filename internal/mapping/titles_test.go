package mapping

import (
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/easly1989/animeworldtv-indexer/internal/store"
)

func TestTitlesRemembersFailuresForAWhile(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		http.Error(w, "down", http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	st, err := store.Open(filepath.Join(t.TempDir(), "c.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	titles := NewTitles(srv.URL, st)
	for range 3 {
		if got := titles.Title(t.Context(), 5); got != "" {
			t.Errorf("got %q", got)
		}
	}
	if hits.Load() != 1 {
		t.Errorf("a failed lookup was retried %d times within the back-off", hits.Load())
	}
}

func TestTitlesLooksUpOnceAndCaches(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		body, _ := io.ReadAll(r.Body)
		if r.Method != http.MethodPost || !strings.Contains(string(body), "idMal:31240") {
			_, _ = w.Write([]byte(`{"data":{"Media":null}}`))
			return
		}
		_, _ = w.Write([]byte(`{"data":{"Media":{"title":{"romaji":"Re:Zero kara Hajimeru Isekai Seikatsu"}}}}`))
	}))
	defer srv.Close()
	st, err := store.Open(filepath.Join(t.TempDir(), "c.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	titles := NewTitles(srv.URL, st)
	for range 2 {
		if got := titles.Title(t.Context(), 31240); got != "Re:Zero kara Hajimeru Isekai Seikatsu" {
			t.Errorf("got %q", got)
		}
	}
	if hits.Load() != 1 {
		t.Errorf("want 1 request, got %d", hits.Load())
	}
	if got := titles.Title(t.Context(), 1); got != "" {
		t.Errorf("unknown id must give an empty title, got %q", got)
	}
}
