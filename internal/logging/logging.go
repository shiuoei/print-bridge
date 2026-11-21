package logging

import (
	"context"
	"log/slog"
	"os"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"time"
)

type Entry struct {
	Time    time.Time `json:"time"`
	Level   string    `json:"level"`
	Message string    `json:"message"`
}

type Buffer struct {
	mu       sync.RWMutex
	entries  []Entry
	capacity int
}

func NewBuffer(capacity int) *Buffer {
	if capacity < 1 {
		capacity = 500
	}
	return &Buffer{capacity: capacity}
}

func (b *Buffer) append(e Entry) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.entries) == b.capacity {
		copy(b.entries, b.entries[1:])
		b.entries = b.entries[:len(b.entries)-1]
	}
	b.entries = append(b.entries, e)
}

// Tail returns the newest matching entries in chronological order.
func (b *Buffer) Tail(since time.Time, minLevel slog.Level, limit int) []Entry {
	if limit <= 0 || limit > 200 {
		limit = 100
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	out := make([]Entry, 0, limit)
	for i := len(b.entries) - 1; i >= 0 && len(out) < limit; i-- {
		e := b.entries[i]
		if e.Time.Before(since) || levelOf(e.Level) < minLevel {
			continue
		}
		out = append(out, e)
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

func levelOf(s string) slog.Level {
	var l slog.Level
	if err := l.UnmarshalText([]byte(s)); err != nil {
		return slog.LevelInfo
	}
	return l
}

var bearerPattern = regexp.MustCompile(`(?i)bearer\s+[^\s"']+`)
var secretPattern = regexp.MustCompile(`(?i)((?:token|password|secret|authorization|api[_-]?key)\s*[=:]\s*)[^\s&"']+`)

func Redact(s string) string {
	s = bearerPattern.ReplaceAllString(s, "Bearer [REDACTED]")
	return secretPattern.ReplaceAllStringFunc(s, func(match string) string {
		for i, r := range match {
			if r == '=' || r == ':' {
				return match[:i+1] + "[REDACTED]"
			}
		}
		return "[REDACTED]"
	})
}

func sensitive(key string) bool {
	key = strings.ToLower(key)
	for _, part := range []string{"token", "password", "secret", "authorization", "api_key", "apikey"} {
		if strings.Contains(key, part) {
			return true
		}
	}
	return false
}

// Handler fans one slog record out to the in-memory buffer the UI reads and to a
// console handler, with credentials redacted on both.
//
// The console handler must be an ordinary slog handler. It must NOT be the
// handler slog itself installed, because slog.SetDefault points the standard
// `log` package at whatever the default logger holds, and slog's own
// defaultHandler writes through `log`:
//
//	slog.Info → this Handler → slog's defaultHandler → log.Print
//	          → log's output mutex → handlerWriter → this Handler → …
//
// That cycle never unwinds on its own, and because the `log` output mutex is
// not reentrant it deadlocks on the second trip rather than recursing forever.
// The goroutine then owns the mutex, so every later log call in the process
// blocks permanently. This is not hypothetical: `test.print` submitted the page
// and then hung inside its own success log, leaving the desktop button reading
// "printing…" until the overdue safety net fired 30 seconds later — the reported
// symptom. Install builds the wiring correctly; NewHandler substitutes a safe
// console handler if it is ever handed slog's own.
type Handler struct {
	next   slog.Handler
	buffer *Buffer
	attrs  []slog.Attr
	group  string
}

// NewHandler fans records out to buffer and to next, the console handler.
//
// Passing nil, or slog.Default().Handler(), selects a plain text handler on
// standard error instead. Both are replaced rather than honoured because a
// console handler that logs through the `log` package while `log` is pointed at
// this Handler deadlocks the process; see Handler. Prefer Install.
func NewHandler(next slog.Handler, buffer *Buffer) *Handler {
	if next == nil || writesThroughLog(next) {
		next = consoleHandler()
	}
	return &Handler{next: next, buffer: buffer}
}

// Install replaces slog's default logger with a Handler that fills buffer and
// still prints to the console, and returns it for the log-panel buffer wiring.
//
// Use this instead of the slog.SetDefault(slog.New(logging.NewHandler(...)))
// spelling, which is how the deadlock described on Handler was introduced.
func Install(buffer *Buffer) *Handler {
	// consoleHandler is built from scratch. Nothing here reads slog's default
	// handler, so the installed logger cannot be handed its own output.
	handler := NewHandler(consoleHandler(), buffer)
	slog.SetDefault(slog.New(handler))
	return handler
}

// consoleHandler is where forwarded records go, written directly rather than
// through slog's default logger or the `log` package.
func consoleHandler() slog.Handler { return slog.NewTextHandler(os.Stderr, nil) }

// writesThroughLog reports whether h is slog's own default handler, which
// forwards to the `log` package and so can close the cycle described on Handler.
// slog keeps that type unexported, so it is recognised by name: a change to that
// name makes the check stop recognising it, which restores nothing worse than
// the previous behaviour.
func writesThroughLog(h slog.Handler) bool {
	t := reflect.TypeOf(h)
	if t == nil {
		return false
	}
	return t.String() == "*slog.defaultHandler"
}

func (h *Handler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.buffer != nil || h.next.Enabled(ctx, level)
}

func (h *Handler) Handle(ctx context.Context, r slog.Record) error {
	clean := slog.NewRecord(r.Time, r.Level, Redact(r.Message), r.PC)
	parts := []string{clean.Message}
	var add func(string, slog.Attr)
	add = func(prefix string, a slog.Attr) {
		if a.Value.Kind() == slog.KindGroup {
			for _, child := range a.Value.Group() {
				add(prefix+a.Key+".", child)
			}
			return
		}
		key := prefix + a.Key
		value := "[REDACTED]"
		if !sensitive(key) {
			value = Redact(a.Value.String())
		}
		clean.AddAttrs(slog.String(key, value))
		parts = append(parts, key+"="+value)
	}
	for _, a := range h.attrs {
		add(h.group, a)
	}
	r.Attrs(func(a slog.Attr) bool { add(h.group, a); return true })
	if h.buffer != nil {
		h.buffer.append(Entry{Time: r.Time, Level: r.Level.String(), Message: strings.Join(parts, " ")})
	}
	if h.next.Enabled(ctx, r.Level) {
		return h.next.Handle(ctx, clean)
	}
	return nil
}

func (h *Handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	c := *h
	c.attrs = append(append([]slog.Attr{}, h.attrs...), attrs...)
	return &c
}

func (h *Handler) WithGroup(name string) slog.Handler { c := *h; c.group += name + "."; return &c }
