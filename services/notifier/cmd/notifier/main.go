// Command notifier delivers signed outgoing webhooks with retries and a dead-letter queue
// (design doc 4.7, ADR 0014).
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/Bowenn/gap-project_private/services/notifier/internal/api"
	"github.com/Bowenn/gap-project_private/services/notifier/internal/apikeys"
	"github.com/Bowenn/gap-project_private/services/notifier/internal/events"
	"github.com/Bowenn/gap-project_private/services/notifier/internal/logging"
	"github.com/Bowenn/gap-project_private/services/notifier/internal/secretbox"
	"github.com/Bowenn/gap-project_private/services/notifier/internal/server"
	"github.com/Bowenn/gap-project_private/services/notifier/internal/ssrf"
	"github.com/Bowenn/gap-project_private/services/notifier/internal/store"
	"github.com/Bowenn/gap-project_private/services/notifier/internal/telemetry"
)

const serviceName = "notifier"

type config struct {
	port     string
	version  string
	instance string
	logLevel slog.Level

	databaseURL string
	// Auth's internal port, where API keys are checked (ADR 0001).
	authInternalURL string
	// Encrypts the endpoints' signing secrets (ADR 0014): base64 of 32 bytes.
	secretKey string
	// Hostnames that may resolve to private addresses: only the local test receiver.
	allowedPrivateHosts string
	kafkaBrokers        []string
}

func loadConfig() config {
	host, _ := os.Hostname()
	return config{
		port:     envOr("PORT", "8080"),
		version:  envOr("SERVICE_VERSION", "dev"),
		instance: envOr("SERVICE_INSTANCE_ID", host),
		logLevel: logging.ParseLevel(os.Getenv("LOG_LEVEL")),
		databaseURL: (&url.URL{
			Scheme:   "postgres",
			User:     url.UserPassword(envOr("DB_USER", "notifier"), os.Getenv("DB_PASSWORD")),
			Host:     envOr("DB_HOST", "postgres") + ":" + envOr("DB_PORT", "5432"),
			Path:     envOr("DB_NAME", "notifier"),
			RawQuery: "sslmode=disable",
		}).String(),
		authInternalURL:     envOr("AUTH_INTERNAL_URL", "http://auth:9091"),
		secretKey:           os.Getenv("NOTIFIER_SECRET_KEY"),
		allowedPrivateHosts: os.Getenv("NOTIFIER_ALLOWED_PRIVATE_HOSTS"),
		kafkaBrokers:        strings.Split(envOr("KAFKA_BROKERS", "kafka:9092"), ","),
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func main() {
	cfg := loadConfig()

	// The runtime image has no shell or curl, so the binary checks its own health
	// for the container healthcheck.
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		os.Exit(healthcheck(cfg.port))
	}

	shutdownTelemetry, err := telemetry.Setup(context.Background())
	logger := logging.New(os.Stdout, serviceName, cfg.version, cfg.logLevel, telemetry.Enabled())
	if err != nil {
		logger.Error("telemetry setup failed", "err", err)
		os.Exit(1)
	}
	runErr := run(cfg, logger)
	if runErr != nil {
		logger.Error("exiting", "err", runErr)
	}
	// Flush the last spans, metrics and logs after the server has drained.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := shutdownTelemetry(ctx); err != nil {
		logger.Error("telemetry shutdown failed", "err", err)
	}
	if runErr != nil {
		os.Exit(1)
	}
}

func run(cfg config, logger *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	box, err := secretbox.New(cfg.secretKey)
	if err != nil {
		return err
	}
	st, err := store.Open(ctx, cfg.databaseURL)
	if err != nil {
		return fmt.Errorf("database: %w", err)
	}
	defer st.Close()
	policy := ssrf.NewPolicy(cfg.allowedPrivateHosts)
	routes := (&api.API{
		Store: st, Keys: apikeys.NewVerifier(cfg.authInternalURL), Box: box, Policy: policy,
		Log: logger, Now: time.Now,
	}).Register

	consumer, err := events.NewConsumer(cfg.kafkaBrokers, st, logger)
	if err != nil {
		return err
	}
	var workers sync.WaitGroup
	workers.Go(func() { consumer.Run(ctx) })
	// The consumer leaves its group before the pool closes.
	defer workers.Wait()

	srv := &http.Server{
		Addr:              ":" + cfg.port,
		Handler:           server.NewHandler(server.Info{Service: serviceName, Version: cfg.version, Instance: cfg.instance}, routes),
		ReadHeaderTimeout: 5 * time.Second,
	}

	serveErr := make(chan error, 1)
	go func() {
		logger.Info("listening", "addr", srv.Addr, "instance", cfg.instance)
		serveErr <- srv.ListenAndServe()
	}()

	select {
	case err := <-serveErr:
		return fmt.Errorf("serve: %w", err)
	case <-ctx.Done():
	}

	// SIGTERM: stop accepting connections, let in-flight requests finish, then exit.
	logger.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	if err := <-serveErr; !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serve: %w", err)
	}
	logger.Info("stopped")
	return nil
}

func healthcheck(port string) int {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://127.0.0.1:"+port+"/healthz", nil)
	if err != nil {
		return 1
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 1
	}
	return 0
}
