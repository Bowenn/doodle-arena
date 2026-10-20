package logging

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/log/logtest"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

func decode(t *testing.T, buf *bytes.Buffer) map[string]any {
	t.Helper()
	var got map[string]any
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("log line is not JSON: %v (%q)", err, buf.String())
	}
	return got
}

func TestNewWritesRequiredFields(t *testing.T) {
	var buf bytes.Buffer
	New(&buf, "lobby", "1.2.3", slog.LevelInfo, false).Warn("room allocated", "room_id", "r_1")

	got := decode(t, &buf)
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
	if _, ok := got["trace_id"]; ok {
		t.Errorf("trace_id written without a span in the context")
	}
}

func TestLogLinesCarryTheSpanFromTheContext(t *testing.T) {
	tp := sdktrace.NewTracerProvider()
	ctx, span := tp.Tracer("test").Start(context.Background(), "op")
	defer span.End()

	var buf bytes.Buffer
	New(&buf, "lobby", "dev", slog.LevelInfo, false).InfoContext(ctx, "inside a span")

	got := decode(t, &buf)
	sc := span.SpanContext()
	if got["trace_id"] != sc.TraceID().String() || got["span_id"] != sc.SpanID().String() {
		t.Errorf("trace_id/span_id = %v/%v, want %s/%s", got["trace_id"], got["span_id"], sc.TraceID(), sc.SpanID())
	}
}

func TestOTLPGetsTheSameRecordsAboveTheLevel(t *testing.T) {
	recorder := logtest.NewRecorder()
	previous := otel.GetLoggerProvider()
	otel.SetLoggerProvider(recorder)
	t.Cleanup(func() { otel.SetLoggerProvider(previous) })

	var buf bytes.Buffer
	logger := New(&buf, "lobby", "dev", slog.LevelInfo, true)
	logger.Debug("hidden")
	logger.Info("room allocated")

	var msgs []string
	for _, records := range recorder.Result() {
		for _, r := range records {
			msgs = append(msgs, r.Body.AsString())
		}
	}
	if len(msgs) != 1 || msgs[0] != "room allocated" {
		t.Errorf("OTLP records = %q, want only %q", msgs, "room allocated")
	}
	if bytes.Count(buf.Bytes(), []byte("\n")) != 1 {
		t.Errorf("stdout got %q, want one line", buf.String())
	}
}

func TestNewRespectsLevel(t *testing.T) {
	var buf bytes.Buffer
	New(&buf, "lobby", "dev", slog.LevelInfo, false).Debug("hidden")
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
