package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

func TestHealthz(t *testing.T) {
	h := NewHandler(Info{Service: "lobby", Version: "1.2.3", Instance: "lobby-1"}, nil)

	tests := []struct {
		method string
		want   int
	}{
		{http.MethodGet, http.StatusOK},
		{http.MethodPost, http.StatusMethodNotAllowed},
	}
	for _, tt := range tests {
		t.Run(tt.method, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(tt.method, "/healthz", nil))
			if rec.Code != tt.want {
				t.Fatalf("status = %d, want %d", rec.Code, tt.want)
			}
			if tt.want != http.StatusOK {
				return
			}
			var body map[string]string
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("body is not JSON: %v", err)
			}
			want := map[string]string{"status": "ok", "service": "lobby", "version": "1.2.3", "instance": "lobby-1"}
			for k, v := range want {
				if body[k] != v {
					t.Errorf("%s = %q, want %q", k, body[k], v)
				}
			}
		})
	}
}

func TestRequestsAreTracedButHealthChecksAreNot(t *testing.T) {
	recorder := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	h := NewHandler(Info{Service: "lobby"}, nil, otelhttp.WithTracerProvider(tp))

	for _, path := range []string{"/healthz", "/api/lobby/rooms"} {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, path, nil))
	}

	spans := recorder.Ended()
	if len(spans) != 1 {
		t.Fatalf("got %d spans, want 1 (health checks are not traced)", len(spans))
	}
	if kind := spans[0].SpanKind(); kind != trace.SpanKindServer {
		t.Errorf("span kind = %v, want server", kind)
	}
}

func TestServerSpanContinuesTheCallersTrace(t *testing.T) {
	recorder := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	h := NewHandler(Info{Service: "lobby"}, nil,
		otelhttp.WithTracerProvider(tp), otelhttp.WithPropagators(propagation.TraceContext{}))

	req := httptest.NewRequest(http.MethodGet, "/api/lobby/rooms", nil)
	req.Header.Set("traceparent", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01")
	h.ServeHTTP(httptest.NewRecorder(), req)

	spans := recorder.Ended()
	if len(spans) != 1 || spans[0].SpanContext().TraceID().String() != "4bf92f3577b34da6a3ce929d0e0e4736" {
		t.Fatalf("server span did not join the caller's trace: %+v", spans)
	}
	if parent := spans[0].Parent().SpanID().String(); parent != "00f067aa0ba902b7" {
		t.Errorf("parent span = %s, want the caller's span", parent)
	}
}
