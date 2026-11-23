package events

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"
)

const defaultCapacity = 1000

// Event is one log record kept for pgswitch log.
type Event struct {
	Time    time.Time         `json:"time"`
	Level   string            `json:"level"`
	Message string            `json:"message"`
	Attrs   map[string]string `json:"attrs,omitempty"`
}

// Log keeps the latest events and hands new ones to subscribers.
type Log struct {
	mu       sync.Mutex
	capacity int
	events   []Event
	subs     map[chan Event]struct{}
}

func New() *Log {
	return newWithCapacity(defaultCapacity)
}

func newWithCapacity(capacity int) *Log {
	return &Log{capacity: capacity, subs: make(map[chan Event]struct{})}
}

func (l *Log) Add(e Event) {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.events = append(l.events, e)
	if len(l.events) > l.capacity {
		l.events = l.events[len(l.events)-l.capacity:]
	}
	for ch := range l.subs {
		select {
		case ch <- e:
		default: // a reader too slow to keep up misses events rather than stalls serve
		}
	}
}

// Recent returns the events kept, oldest first.
func (l *Log) Recent() []Event {
	l.mu.Lock()
	defer l.mu.Unlock()

	return append([]Event(nil), l.events...)
}

// Follow returns the events kept and a channel of the ones that come after,
// closed when ctx ends.
func (l *Log) Follow(ctx context.Context) ([]Event, <-chan Event) {
	ch := make(chan Event, l.capacity)

	l.mu.Lock()
	recent := append([]Event(nil), l.events...)
	l.subs[ch] = struct{}{}
	l.mu.Unlock()

	context.AfterFunc(ctx, func() {
		l.mu.Lock()
		defer l.mu.Unlock()

		delete(l.subs, ch)
		close(ch)
	})

	return recent, ch
}

// Handler is a slog.Handler that records Info and above into a Log.
type Handler struct {
	log    *Log
	attrs  []slog.Attr
	groups []string
}

var _ slog.Handler = Handler{}

func NewHandler(log *Log) Handler {
	return Handler{log: log}
}

func (h Handler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= slog.LevelInfo
}

func (h Handler) Handle(_ context.Context, r slog.Record) error {
	attrs := make(map[string]string)
	for _, a := range h.attrs {
		addAttr(attrs, "", a)
	}
	prefix := h.prefix()
	r.Attrs(func(a slog.Attr) bool {
		addAttr(attrs, prefix, a)

		return true
	})
	if len(attrs) == 0 {
		attrs = nil
	}

	h.log.Add(Event{Time: r.Time, Level: r.Level.String(), Message: r.Message, Attrs: attrs})

	return nil
}

func (h Handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	prefix := h.prefix()
	next := h
	next.attrs = append([]slog.Attr(nil), h.attrs...)
	for _, a := range attrs {
		next.attrs = append(next.attrs, slog.Attr{Key: prefix + a.Key, Value: a.Value})
	}

	return next
}

func (h Handler) WithGroup(name string) slog.Handler {
	next := h
	next.groups = append(append([]string(nil), h.groups...), name)

	return next
}

// prefix qualifies attribute keys with the groups opened so far.
func (h Handler) prefix() string {
	if len(h.groups) == 0 {
		return ""
	}

	return strings.Join(h.groups, ".") + "."
}

func addAttr(dst map[string]string, prefix string, a slog.Attr) {
	v := a.Value.Resolve()
	if v.Kind() == slog.KindGroup {
		for _, g := range v.Group() {
			addAttr(dst, prefix+a.Key+".", g)
		}

		return
	}
	dst[prefix+a.Key] = v.String()
}
