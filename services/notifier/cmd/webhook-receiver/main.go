// Command webhook-receiver is a test endpoint for webhook drills (task 4.7): it verifies every
// delivery's Doodle-Signature with the notifier's own Verify, records what it got, and answers
// 200 or 500 as told. A hook without a secret takes unsigned requests and keeps their bodies:
// Grafana's alert notifications land on /hooks/alerts when no Discord webhook is configured
// (task 5.1). Local only; it keeps everything in memory.
//
//	POST /hooks/{name}          a webhook endpoint
//	PUT  /admin/{name}/secret   the endpoint's signing secret (the body, as text)
//	PUT  /admin/{name}/mode     "ok" (200) or "fail" (500)
//	GET  /admin/{name}          {"mode", "received": [{event_id, event_type, delivery_id, verified, status, at}]}
package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/Bowenn/doodle-arena/services/notifier/internal/webhook"
)

type received struct {
	EventID    string    `json:"event_id"`
	EventType  string    `json:"event_type"`
	DeliveryID string    `json:"delivery_id"`
	Verified   bool      `json:"verified"`
	Status     int       `json:"status"`
	At         time.Time `json:"at"`
	Body       string    `json:"body,omitempty"` // unsigned hooks only
}

type hook struct {
	Secret   string     `json:"-"`
	Mode     string     `json:"mode"`
	Received []received `json:"received"`
}

type receiver struct {
	mu    sync.Mutex
	hooks map[string]*hook
}

func (rv *receiver) get(name string) *hook {
	h, ok := rv.hooks[name]
	if !ok {
		h = &hook{Mode: "ok", Received: []received{}}
		rv.hooks[name] = h
	}
	return h
}

func (rv *receiver) routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{"status":"ok"}`)) })
	mux.HandleFunc("POST /hooks/{name}", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		rv.mu.Lock()
		h := rv.get(r.PathValue("name"))
		status := http.StatusOK
		if h.Mode == "fail" {
			status = http.StatusInternalServerError
		}
		unsigned := h.Secret == ""
		verified := !unsigned && webhook.Verify(h.Secret, r.Header.Get("Doodle-Signature"), string(body), time.Now()) == nil
		got := received{
			EventID: r.Header.Get("Doodle-Event-Id"), EventType: r.Header.Get("Doodle-Event-Type"),
			DeliveryID: r.Header.Get("Doodle-Delivery-Id"), Verified: verified, Status: status, At: time.Now(),
		}
		if unsigned {
			got.Body = string(body)
		}
		h.Received = append(h.Received, got)
		rv.mu.Unlock()
		if !verified && !unsigned {
			status = http.StatusUnauthorized // a real receiver refuses an unverified webhook
		}
		w.WriteHeader(status)
	})
	mux.HandleFunc("PUT /admin/{name}/secret", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(io.LimitReader(r.Body, 1024))
		rv.mu.Lock()
		rv.get(r.PathValue("name")).Secret = strings.TrimSpace(string(b))
		rv.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("PUT /admin/{name}/mode", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(io.LimitReader(r.Body, 16))
		mode := strings.TrimSpace(string(b))
		if mode != "ok" && mode != "fail" {
			http.Error(w, `mode is "ok" or "fail"`, http.StatusBadRequest)
			return
		}
		rv.mu.Lock()
		rv.get(r.PathValue("name")).Mode = mode
		rv.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /admin/{name}", func(w http.ResponseWriter, r *http.Request) {
		rv.mu.Lock()
		defer rv.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(rv.get(r.PathValue("name")))
	})
	return mux
}

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "9000"
	}
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		resp, err := http.Get("http://127.0.0.1:" + port + "/healthz")
		if err != nil || resp.StatusCode != http.StatusOK {
			os.Exit(1)
		}
		os.Exit(0)
	}
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil)).With("service", "webhook-receiver")
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	srv := &http.Server{Addr: ":" + port, Handler: (&receiver{hooks: map[string]*hook{}}).routes(), ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()
	log.Info("listening", "addr", srv.Addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("serve", "err", err)
		os.Exit(1)
	}
}
