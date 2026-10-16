package logging

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"testing"
	"time"
)

func TestNewWritesRequiredFields(t *testing.T) {
	var buf bytes.Buffer
	New(&buf, "lobby", "1.2.3", slog.LevelInfo).Warn("room allocated", "room_id", "r_1")

	var got map[string]any
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("log line is not JSON: %v (%q)", err, buf.String())
	}
	want := map[string]any{"level": "warn", "service": "lobby", "version": "1.2.3", "msg": "room allocated", "room_id": "r_1"}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %v, want %v", k, got[k], v)
		}
	}
	ts, ok := got["timestamp"].(string)
	if !ok {
		t.Fatalf("timestamp missing: %v", got)
	}
	if _, err := time.Parse(time.RFC3339Nano, ts); err != nil || ts[len(ts)-1] != 'Z' {
		t.Errorf("timestamp %q is not a UTC RFC 3339 time", ts)
	}
	if _, ok := got["time"]; ok {
		t.Errorf("slog's default time key leaked into the output")
	}
}

func TestNewRespectsLevel(t *testing.T) {
	var buf bytes.Buffer
	New(&buf, "lobby", "dev", slog.LevelInfo).Debug("hidden")
	if buf.Len() != 0 {
		t.Errorf("debug line written at info level: %q", buf.String())
	}
}

func TestParseLevel(t *testing.T) {
	tests := []struct {
		in   string
		want slog.Level
	}{
		{"debug", slog.LevelDebug},
		{"INFO", slog.LevelInfo},
		{"warn", slog.LevelWarn},
		{"error", slog.LevelError},
		{"", slog.LevelInfo},
		{"loud", slog.LevelInfo},
	}
	for _, tt := range tests {
		if got := ParseLevel(tt.in); got != tt.want {
			t.Errorf("ParseLevel(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}
