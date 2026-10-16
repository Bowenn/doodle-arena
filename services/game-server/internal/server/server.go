// Package server holds the HTTP handlers of the service.
package server

import (
	"encoding/json"
	"net/http"
	"strings"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

// Info identifies this process in health responses.
type Info struct {
	Service  string `json:"service"`
	Version  string `json:"version"`
	Instance string `json:"instance"`
}

// NewHandler returns the service's HTTP routes, traced with OpenTelemetry (a server span per
// request, continuing the caller's traceparent). Health checks are not traced, and neither
// are WebSockets: a span per connection would last as long as the game, so the ws package
// traces the handshake itself. ws may be nil (tests of the health route).
func NewHandler(info Info, ws http.Handler, opts ...otelhttp.Option) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(struct {
			Status string `json:"status"`
			Info
		}{"ok", info})
	})

	if ws != nil {
		mux.Handle("GET /ws/{server_id}/rooms/{room_id}", ws)
	}

	opts = append([]otelhttp.Option{
		otelhttp.WithFilter(func(r *http.Request) bool {
			return r.URL.Path != "/healthz" && !strings.HasPrefix(r.URL.Path, "/ws/")
		}),
	}, opts...)
	return otelhttp.NewHandler(mux, "http.server", opts...)
}
