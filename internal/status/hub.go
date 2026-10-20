// Package status serves the operational API of the indexer: GET /api/status
// (health, queue, recent errors) and GET /api/logs (live log stream over SSE).
// It is what the mircrewrr panel reads server-side.
package status

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
)

const (
	ringSize  = 500 // log entries kept for new SSE clients
	errorKeep = 20  // ERROR entries shown by /api/status
	subBuffer = 64  // per-subscriber queue; a full one drops entries
)

const timeLayout = "2006-01-02 15:04:05"

// Entry is one log line as the panel renders it.
type Entry struct {
	Timestamp string `json:"timestamp"` // local time
	Level     string `json:"level"`     // DEBUG, INFO, WARNING or ERROR
	Logger    string `json:"logger"`
	Message   string `json:"message"`
}

// Hub keeps the recent log entries and fans new ones out to SSE subscribers.
type Hub struct {
	mu   sync.Mutex
	ring []Entry
	errs []Entry
	subs map[chan Entry]struct{}
}

func NewHub() *Hub { return &Hub{subs: map[chan Entry]struct{}{}} }

// Handler returns a slog.Handler that writes through to next and also records
// every record the next handler would accept.
func (h *Hub) Handler(next slog.Handler) slog.Handler { return &handler{hub: h, next: next} }

func (h *Hub) add(e Entry) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.ring = push(h.ring, e, ringSize)
	if e.Level == "ERROR" {
		h.errs = push(h.errs, e, errorKeep)
	}
	for ch := range h.subs {
		select {
		case ch <- e:
		default: // a slow client loses entries rather than stalling the logger
		}
	}
}

func push(s []Entry, e Entry, max int) []Entry {
	if len(s) >= max {
		copy(s, s[1:])
		s = s[:max-1]
	}
	return append(s, e)
}

// Recent returns the last n entries, oldest first.
func (h *Hub) Recent(n int) []Entry {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.recent(n)
}

func (h *Hub) recent(n int) []Entry {
	if n > len(h.ring) {
		n = len(h.ring)
	}
	return append([]Entry{}, h.ring[len(h.ring)-n:]...)
}

// Errors returns the last ERROR entries, oldest first.
func (h *Hub) Errors() []Entry {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]Entry{}, h.errs...)
}

// Subscribe returns the last n entries and a channel with every entry logged
// after them: no gap, no duplicate. Call cancel when done.
func (h *Hub) Subscribe(n int) (recent []Entry, ch <-chan Entry, cancel func()) {
	c := make(chan Entry, subBuffer)
	h.mu.Lock()
	recent = h.recent(n)
	h.subs[c] = struct{}{}
	h.mu.Unlock()
	return recent, c, func() {
		h.mu.Lock()
		delete(h.subs, c)
		h.mu.Unlock()
	}
}

// handler tees records into the hub. Attributes and groups are kept as text so
// the panel gets one readable message line.
type handler struct {
	hub    *Hub
	next   slog.Handler
	attrs  []string // from WithAttrs, already prefixed with the group path
	logger string   // a "logger" or "component" attribute, if any
	group  string   // "a.b." prefix for attributes added from here on
}

func (h *handler) Enabled(ctx context.Context, l slog.Level) bool { return h.next.Enabled(ctx, l) }

func (h *handler) Handle(ctx context.Context, r slog.Record) error {
	err := h.next.Handle(ctx, r)
	h.hub.add(h.entry(r))
	return err
}

func (h *handler) WithAttrs(as []slog.Attr) slog.Handler {
	c := *h
	c.next = h.next.WithAttrs(as)
	c.attrs = append([]string(nil), h.attrs...)
	for _, a := range as {
		c.take(a, h.group)
	}
	return &c
}

func (h *handler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	c := *h
	c.next = h.next.WithGroup(name)
	c.group += name + "."
	return &c
}

// take records a as text, or as the logger name when it is one. It only
// mutates the receiver, which is always a private copy.
func (h *handler) take(a slog.Attr, group string) {
	a.Value = a.Value.Resolve()
	if a.Equal(slog.Attr{}) {
		return
	}
	if a.Value.Kind() == slog.KindGroup {
		if a.Key != "" {
			group += a.Key + "."
		}
		for _, g := range a.Value.Group() {
			h.take(g, group)
		}
		return
	}
	if group == "" && (a.Key == "logger" || a.Key == "component") && h.logger == "" {
		h.logger = a.Value.String()
		return
	}
	h.attrs = append(h.attrs, fmt.Sprintf("%s%s=%v", group, a.Key, a.Value))
}

func (h *handler) entry(r slog.Record) Entry {
	t := r.Time
	if t.IsZero() {
		t = time.Now()
	}
	c := *h
	c.attrs = append([]string(nil), h.attrs...)
	r.Attrs(func(a slog.Attr) bool { c.take(a, h.group); return true })

	msg := r.Message
	if len(c.attrs) > 0 {
		msg += " " + strings.Join(c.attrs, " ")
	}
	if c.logger == "" {
		c.logger = "app"
	}
	return Entry{Timestamp: t.Local().Format(timeLayout), Level: levelName(r.Level), Logger: c.logger, Message: msg}
}

func levelName(l slog.Level) string {
	switch {
	case l >= slog.LevelError:
		return "ERROR"
	case l >= slog.LevelWarn:
		return "WARNING"
	case l >= slog.LevelInfo:
		return "INFO"
	}
	return "DEBUG"
}
