// Package server holds the HTTP handlers of the service.
package server

import (
	"encoding/json"
	"net/http"
)

// Info identifies this process in health responses.
type Info struct {
	Service  string `json:"service"`
	Version  string `json:"version"`
	Instance string `json:"instance"`
}

// NewHandler returns the service's HTTP routes.
func NewHandler(info Info) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(struct {
			Status string `json:"status"`
			Info
		}{"ok", info})
	})
	return mux
}
