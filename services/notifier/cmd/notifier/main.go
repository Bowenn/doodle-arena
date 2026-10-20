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

	"github.com/twmb/franz-go/pkg/kgo"
	"github.com/twmb/franz-go/plugin/kotel"

	"github.com/Bowenn/gap-project_private/libs/go/authn"
	"github.com/Bowenn/gap-project_private/libs/go/kafkaauth"
	"github.com/Bowenn/gap-project_private/libs/go/logging"
	"github.com/Bowenn/gap-project_private/services/notifier/internal/api"
	"github.com/Bowenn/gap-project_private/services/notifier/internal/apikeys"
	"github.com/Bowenn/gap-project_private/services/notifier/internal/delivery"
	"github.com/Bowenn/gap-project_private/services/notifier/internal/events"
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
	// Auth's public keys and issuer, for the web app's access tokens (ADR 0001).
	jwksURL   string
	jwtIssuer string
	// Encrypts the endpoints' signing secrets (ADR 0014): random text of 32+ characters, hashed
	// with SHA-256 into the AES-256 key.
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
		jwksURL:             envOr("AUTH_JWKS_URL", "http://auth:8080/api/auth/.well-known/jwks.json"),
		jwtIssuer:           envOr("JWT_ISSUER", "http://localhost:8080/api/auth"),
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
	jwks, err := authn.NewJWKS(cfg.jwksURL, &http.Client{Timeout: 3 * time.Second}, 30*time.Second)
	if err != nil {
		return err
	}
	routes := (&api.API{
		Store: st, Keys: apikeys.NewVerifier(cfg.authInternalURL),
		// iat is compared with credential revocations (ADR 0015), so a token must carry one.
		Tokens: authn.NewVerifier(cfg.jwtIssuer, jwks, authn.RequireIssuedAt()),
		Box:    box, Policy: policy,
		Log: logger, Now: time.Now,
	}).Register

	// The notifier Kafka user (ADR 0013): it reads market.sales.v1, market.wallet.v1 and
	// auth.users.v1 in its own two groups and writes notifier.dlq.v1.
	kafkaAuth, err := kafkaauth.FromEnv().Opts()
	if err != nil {
		return err
	}
	consumer, err := events.NewConsumer(cfg.kafkaBrokers, st, logger, kafkaAuth...)
	if err != nil {
		return err
	}
	// Credential revocations from auth (ADR 0015): disables the user's endpoints.
	revocations, err := events.NewRevocationConsumer(cfg.kafkaBrokers, st, logger, kafkaAuth...)
	if err != nil {
		return fmt.Errorf("revocation consumer: %w", err)
	}
	dispatcher, err := delivery.NewDispatcher(st, box, policy, delivery.Defaults(), logger)
	if err != nil {
		return err
	}
	// A record that can't reach Kafka fails after 10 s, so a dead-letter pass never hangs
	// (with idempotence on, franz-go would otherwise retry it without regard for the context).
	producer, err := kgo.NewClient(append([]kgo.Opt{kgo.SeedBrokers(cfg.kafkaBrokers...), kgo.RecordDeliveryTimeout(10 * time.Second),
		kgo.WithHooks(kotel.NewKotel(kotel.WithTracer(kotel.NewTracer())).Hooks()...)}, kafkaAuth...)...)
	if err != nil {
		return fmt.Errorf("kafka producer: %w", err)
	}
	defer producer.Close()
	deadLetters, err := delivery.NewDeadLetters(st, producer, logger, 2*time.Second)
	if err != nil {
		return err
	}
	var workers sync.WaitGroup
	workers.Go(func() { consumer.Run(ctx) })
	workers.Go(func() { revocations.Run(ctx) })
	workers.Go(func() { dispatcher.Run(ctx) })
	workers.Go(func() { deadLetters.Run(ctx) })
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
