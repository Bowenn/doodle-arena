// Package logging builds the JSON logger every service uses (docs/observability.md, section 2).
package logging

import (
	"io"
	"log/slog"
	"strings"
)

// New returns a logger that writes one JSON object per line with the required fields
// timestamp, level, service, version and msg. trace_id and span_id are added once
// OpenTelemetry is wired in (task 1.5).
func New(w io.Writer, service, version string, level slog.Level) *slog.Logger {
	h := slog.NewJSONHandler(w, &slog.HandlerOptions{Level: level, ReplaceAttr: rename})
	return slog.New(h).With("service", service, "version", version)
}

// ParseLevel turns "debug", "info", "warn" or "error" into a level, defaulting to info.
func ParseLevel(s string) slog.Level {
	var l slog.Level
	if err := l.UnmarshalText([]byte(s)); err != nil {
		return slog.LevelInfo
	}
	return l
}

func rename(groups []string, a slog.Attr) slog.Attr {
	if len(groups) > 0 {
		return a
	}
	switch a.Key {
	case slog.TimeKey:
		a.Key = "timestamp"
		a.Value = slog.StringValue(a.Value.Time().UTC().Format("2006-01-02T15:04:05.000Z07:00"))
	case slog.LevelKey:
		if l, ok := a.Value.Any().(slog.Level); ok {
			a.Value = slog.StringValue(strings.ToLower(l.String()))
		}
	}
	return a
}
