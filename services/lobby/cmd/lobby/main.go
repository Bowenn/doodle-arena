// Command lobby runs the lobby service: rooms, the game-server registry and room allocation.
// In task 1.1 it only serves /healthz.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Bowenn/gap-project_private/services/lobby/internal/logging"
	"github.com/Bowenn/gap-project_private/services/lobby/internal/server"
	"github.com/Bowenn/gap-project_private/services/lobby/internal/telemetry"
)

const serviceName = "lobby"

type config struct {
	port     string
	version  string
	instance string
	logLevel slog.Level
}

func loadConfig() config {
	host, _ := os.Hostname()
	return config{
		port:     envOr("PORT", "8080"),
		version:  envOr("SERVICE_VERSION", "dev"),
		instance: envOr("SERVICE_INSTANCE_ID", host),
		logLevel: logging.ParseLevel(os.Getenv("LOG_LEVEL")),
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

	srv := &http.Server{
		Addr:              ":" + cfg.port,
		Handler:           server.NewHandler(server.Info{Service: serviceName, Version: cfg.version, Instance: cfg.instance}),
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
