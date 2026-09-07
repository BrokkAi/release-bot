package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
)

type console struct {
	writer io.Writer
	mu     *sync.Mutex
	attrs  []slog.Attr
	group  string
}

func newConsole(w io.Writer) *console                               { return &console{writer: w, mu: new(sync.Mutex)} }
func (h *console) Enabled(_ context.Context, level slog.Level) bool { return level >= slog.LevelInfo }
func (h *console) Handle(_ context.Context, r slog.Record) error {
	var line strings.Builder
	fmt.Fprintf(&line, "%s  %s", r.Time.Format("15:04:05"), r.Message)
	if r.Level >= slog.LevelWarn {
		fmt.Fprintf(&line, " [%s]", strings.ToLower(r.Level.String()))
	}
	write := func(a slog.Attr) bool {
		fmt.Fprintf(&line, " · %s: %v", strings.ReplaceAll(h.group+a.Key, "_", " "), a.Value.Resolve())
		return true
	}
	for _, a := range h.attrs {
		write(a)
	}
	r.Attrs(write)
	h.mu.Lock()
	defer h.mu.Unlock()
	_, err := fmt.Fprintln(h.writer, line.String())
	return err
}
func (h *console) WithAttrs(attrs []slog.Attr) slog.Handler {
	next := *h
	next.attrs = append(append([]slog.Attr{}, h.attrs...), attrs...)
	return &next
}
func (h *console) WithGroup(name string) slog.Handler {
	next := *h
	if name != "" {
		next.group += name + "."
	}
	return &next
}
