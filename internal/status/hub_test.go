package status

import (
	"bytes"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func newLogger(buf *bytes.Buffer, hub *Hub) *slog.Logger {
	return slog.New(hub.Handler(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
}

func TestHandlerTeesToOutputAndBuffer(t *testing.T) {
	var out bytes.Buffer
	hub := NewHub()
	log := newLogger(&out, hub)

	log.Info("listening", "addr", ":9698")
	log.With("component", "torznab").Warn("slow", slog.Group("req", "q", "frieren"))
	log.Debug("noise")

	if !strings.Contains(out.String(), `"msg":"listening"`) || strings.Count(out.String(), "\n") != 3 {
		t.Errorf("stdout lost records: %q", out.String())
	}
	got := hub.Recent(10)
	if len(got) != 3 {
		t.Fatalf("buffer has %d entries: %+v", len(got), got)
	}
	want := []Entry{
		{Level: "INFO", Logger: "app", Message: "listening addr=:9698"},
		{Level: "WARNING", Logger: "torznab", Message: "slow req.q=frieren"},
		{Level: "DEBUG", Logger: "app", Message: "noise"},
	}
	for i, w := range want {
		g := got[i]
		if g.Level != w.Level || g.Logger != w.Logger || g.Message != w.Message {
			t.Errorf("entry %d = %+v, want %+v", i, g, w)
		}
		if _, err := time.ParseInLocation(timeLayout, g.Timestamp, time.Local); err != nil {
			t.Errorf("entry %d timestamp %q: %v", i, g.Timestamp, err)
		}
	}
}

func TestHandlerRespectsLevel(t *testing.T) {
	hub := NewHub()
	log := slog.New(hub.Handler(slog.NewJSONHandler(&bytes.Buffer{}, &slog.HandlerOptions{Level: slog.LevelWarn})))
	log.Info("dropped")
	log.Error("kept")
	if got := hub.Recent(10); len(got) != 1 || got[0].Message != "kept" {
		t.Errorf("got %+v", got)
	}
}

func TestRingAndErrorsAreBounded(t *testing.T) {
	hub := NewHub()
	log := newLogger(&bytes.Buffer{}, hub)
	for i := 0; i < ringSize+50; i++ {
		log.Error(fmt.Sprint("e", i))
	}
	all := hub.Recent(10000)
	if len(all) != ringSize || all[0].Message != "e50" || all[len(all)-1].Message != fmt.Sprint("e", ringSize+49) {
		t.Errorf("ring: %d entries, first %q", len(all), all[0].Message)
	}
	errs := hub.Errors()
	if len(errs) != errorKeep || errs[len(errs)-1].Message != fmt.Sprint("e", ringSize+49) {
		t.Errorf("errors: %d entries", len(errs))
	}
	log.Info("not an error")
	if got := hub.Errors(); len(got) != errorKeep || got[errorKeep-1].Level != "ERROR" {
		t.Error("a non-error leaked into the error list")
	}
}

func TestSlowSubscriberDoesNotBlockLogging(t *testing.T) {
	hub := NewHub()
	log := newLogger(&bytes.Buffer{}, hub)
	_, ch, cancel := hub.Subscribe(10) // never read
	defer cancel()

	done := make(chan struct{})
	go func() {
		for i := 0; i < subBuffer*4; i++ {
			log.Info("x")
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("logging blocked on a subscriber that does not read")
	}
	if len(ch) != subBuffer {
		t.Errorf("subscriber queue holds %d, want %d", len(ch), subBuffer)
	}
}

func TestSubscribeHasNoGapAndCancels(t *testing.T) {
	hub := NewHub()
	log := newLogger(&bytes.Buffer{}, hub)
	log.Info("old")
	recent, ch, cancel := hub.Subscribe(100)
	log.Info("new")
	if len(recent) != 1 || recent[0].Message != "old" {
		t.Errorf("recent = %+v", recent)
	}
	if e := <-ch; e.Message != "new" {
		t.Errorf("live = %+v", e)
	}
	cancel()
	log.Info("after")
	if len(hub.subs) != 0 {
		t.Error("subscriber not removed")
	}
}
