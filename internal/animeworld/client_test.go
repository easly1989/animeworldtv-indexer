package animeworld

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"testing"
	"time"
)

func serve(t *testing.T, files map[string]string) (*Client, *httptest.Server) {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/search", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("keyword") == "" {
			http.Redirect(w, r, "/", http.StatusFound) // what the real site does
			return
		}
		http.ServeFile(w, r, files["search"])
	})
	mux.HandleFunc("/updated", func(w http.ResponseWriter, r *http.Request) { http.ServeFile(w, r, files["search"]) })
	mux.HandleFunc("/play/sousou-no-frieren.kbcPv", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, r.URL.Path+"/vlhlNp", http.StatusFound)
	})
	mux.HandleFunc("/play/sousou-no-frieren.kbcPv/vlhlNp", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, files["series"])
	})
	mux.HandleFunc("/api/episode/info", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("id") != "vlhlNp" {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(`{"grabber":"http://` + r.Host + `/file.mp4"}`))
	})
	mux.HandleFunc("/file.mp4", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "502785614")
		w.Header().Set("Last-Modified", "Sun, 31 Mar 2024 00:40:06 GMT")
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return New(srv.URL, srv.Client()), srv
}

func testClient(t *testing.T) *Client {
	t.Helper()
	for _, f := range []string{"search_frieren.html", "series_frieren.html"} {
		if _, err := os.Stat("testdata/" + f); err != nil {
			t.Fatal(err)
		}
	}
	c, _ := serve(t, map[string]string{"search": "testdata/search_frieren.html", "series": "testdata/series_frieren.html"})
	return c
}

func TestClientSearch(t *testing.T) {
	c := testClient(t)
	hits, err := c.Search(context.Background(), "frieren")
	if err != nil || len(hits) != 5 {
		t.Fatalf("hits=%d err=%v", len(hits), err)
	}
}

func TestClientUpdated(t *testing.T) {
	c, _ := serve(t, map[string]string{"search": "testdata/updated.html"})
	ups, err := c.Updated(context.Background())
	if err != nil || len(ups) != 40 {
		t.Fatalf("updates=%d err=%v", len(ups), err)
	}
}

func TestClientSeriesFollowsRedirect(t *testing.T) {
	s, err := testClient(t).Series(context.Background(), "/play/sousou-no-frieren.kbcPv")
	if err != nil {
		t.Fatal(err)
	}
	if s.Path != "/play/sousou-no-frieren.kbcPv" || len(s.Episodes) != 28 {
		t.Fatalf("unexpected series: %+v", s)
	}
}

func TestClientFileURLAndMeta(t *testing.T) {
	c := testClient(t)
	u, err := c.FileURL(context.Background(), "vlhlNp")
	if err != nil {
		t.Fatal(err)
	}
	m, err := c.FileMeta(context.Background(), u)
	if err != nil {
		t.Fatal(err)
	}
	if m.Size != 502785614 || m.Modified.Year() != 2024 {
		t.Errorf("meta wrong: %+v", m)
	}
	if _, err := c.FileURL(context.Background(), "nope"); err == nil {
		t.Error("expected an error for an unknown token")
	}
}

func TestClientRetriesOnce(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		w.Write([]byte(`{"grabber":"x"}`))
	}))
	defer srv.Close()
	c := New(srv.URL, srv.Client())
	c.retryDelay = time.Millisecond
	if u, err := c.FileURL(context.Background(), "t"); err != nil || u != "x" || calls != 2 {
		t.Fatalf("u=%q err=%v calls=%s", u, err, strconv.Itoa(calls))
	}
}
