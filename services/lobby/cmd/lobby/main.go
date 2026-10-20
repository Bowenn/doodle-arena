// Command lobby runs the lobby service: rooms, the game-server registry, room allocation,
// join tickets and the weekly leaderboard (design doc, section 4.3).
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	lobbyv1 "github.com/Bowenn/gap-project_private/proto/gen/go/doodle/lobby/v1"
	"github.com/Bowenn/gap-project_private/services/lobby/internal/authn"
	"github.com/Bowenn/gap-project_private/services/lobby/internal/leaderboard"
	"github.com/Bowenn/gap-project_private/services/lobby/internal/logging"
	"github.com/Bowenn/gap-project_private/services/lobby/internal/registry"
	"github.com/Bowenn/gap-project_private/services/lobby/internal/rooms"
	"github.com/Bowenn/gap-project_private/services/lobby/internal/server"
	"github.com/Bowenn/gap-project_private/services/lobby/internal/telemetry"
	"github.com/Bowenn/gap-project_private/services/lobby/internal/ticket"
	"github.com/redis/go-redis/extra/redisotel/v9"
	"github.com/redis/go-redis/v9"
	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"
	"google.golang.org/grpc"
)

const serviceName = "lobby"

type config struct {
	port       string
	grpcPort   string
	version    string
	instance   string
	logLevel   slog.Level
	redisAddr  string
	jwksURL    string
	jwtIssuer  string
	ticketPath string
	ticketPEM  string
	kafka      []string
}

func loadConfig() config {
	host, _ := os.Hostname()
	return config{
		port:      envOr("PORT", "8080"),
		grpcPort:  envOr("GRPC_PORT", "9090"),
		version:   envOr("SERVICE_VERSION", "dev"),
		instance:  envOr("SERVICE_INSTANCE_ID", host),
		logLevel:  logging.ParseLevel(os.Getenv("LOG_LEVEL")),
		redisAddr: envOr("REDIS_ADDR", "localhost:6379"),
		// auth's internal address: the JWKS is public, but the lobby doesn't need the gateway.
		jwksURL:    envOr("AUTH_JWKS_URL", "http://localhost:8081/api/auth/.well-known/jwks.json"),
		jwtIssuer:  envOr("JWT_ISSUER", "http://localhost:8080/api/auth"),
		ticketPath: os.Getenv("TICKET_PRIVATE_KEY_PATH"),
		ticketPEM:  os.Getenv("TICKET_PRIVATE_KEY_PEM"),
		kafka:      strings.Split(envOr("KAFKA_BROKERS", "localhost:9094"), ","),
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

	var signer *ticket.Signer
	var err error
	if cfg.ticketPEM != "" {
		signer, err = ticket.ParseSigner([]byte(cfg.ticketPEM))
	} else {
		signer, err = ticket.LoadSigner(cfg.ticketPath)
	}
	if err != nil {
		return fmt.Errorf("join tickets (TICKET_PRIVATE_KEY_PEM or TICKET_PRIVATE_KEY_PATH, scripts/gen-ticket-key.sh): %w", err)
	}

	rdb := redis.NewClient(&redis.Options{
		Addr: cfg.redisAddr, DialTimeout: 2 * time.Second, ReadTimeout: time.Second, WriteTimeout: time.Second,
	})
	defer rdb.Close()
	// Without the arguments: a whole room record (JSON) per write adds bytes, not insight.
	if err := redisotel.InstrumentTracing(rdb, redisotel.WithDBStatement(false)); err != nil {
		return fmt.Errorf("redis tracing: %w", err)
	}

	reg := registry.New(rdb, registry.DefaultTTL, logger)
	store := rooms.NewStore(rdb)
	alloc := rooms.NewGRPCAllocator()
	defer alloc.Close()
	svc, err := rooms.NewService(store, reg, alloc, signer, time.Now, logger)
	if err != nil {
		return err
	}
	if err := registerServerGauge(reg); err != nil {
		return err
	}
	board := leaderboard.NewStore(rdb)
	consumer, err := leaderboard.NewConsumer(cfg.kafka, board, logger)
	if err != nil {
		return err
	}

	jwks, err := authn.NewJWKS(cfg.jwksURL,
		&http.Client{Transport: otelhttp.NewTransport(http.DefaultTransport)}, 30*time.Second)
	if err != nil {
		return err
	}
	verifier := authn.NewVerifier(cfg.jwtIssuer, jwks)
	srv := &http.Server{
		Addr: ":" + cfg.port,
		Handler: server.NewHandler(
			server.Info{Service: serviceName, Version: cfg.version, Instance: cfg.instance},
			server.Deps{Verifier: verifier, Rooms: svc, Leaderboard: board, Log: logger},
		),
		ReadHeaderTimeout: 5 * time.Second,
	}

	// Internal gRPC port for game-server heartbeats; the gateway never routes it (ADR 0001).
	grpcSrv := grpc.NewServer(grpc.StatsHandler(otelgrpc.NewServerHandler()))
	lobbyv1.RegisterRegistryServiceServer(grpcSrv, registry.NewGRPCServer(reg, store, time.Now, logger))
	lis, err := net.Listen("tcp", ":"+cfg.grpcPort)
	if err != nil {
		return fmt.Errorf("grpc listen: %w", err)
	}

	// Moves the rooms of dead or restarted game servers (ADR 0007); one lobby at a time.
	reassignCtx, stopReassign := context.WithCancel(context.Background())
	reassignDone := make(chan struct{})
	go func() { svc.RunReassigner(reassignCtx, rdb, cfg.instance); close(reassignDone) }()
	defer func() { stopReassign(); <-reassignDone }()

	// Counts finished matches on the leaderboard. On shutdown it commits what it applied and
	// leaves the consumer group, so another lobby takes over its partitions at once.
	consumeCtx, stopConsumer := context.WithCancel(context.Background())
	consumerDone := make(chan struct{})
	go func() { consumer.Run(consumeCtx); close(consumerDone) }()
	defer func() { stopConsumer(); <-consumerDone }()

	serveErr := make(chan error, 2)
	go func() {
		logger.Info("listening", "addr", srv.Addr, "instance", cfg.instance)
		serveErr <- srv.ListenAndServe()
	}()
	go func() {
		logger.Info("grpc listening", "addr", lis.Addr().String())
		if err := grpcSrv.Serve(lis); err != nil {
			serveErr <- fmt.Errorf("grpc: %w", err)
		}
	}()

	select {
	case err := <-serveErr:
		grpcSrv.Stop()
		_ = srv.Close()
		return fmt.Errorf("serve: %w", err)
	case <-ctx.Done():
	}

	// SIGTERM: stop accepting connections, let in-flight requests finish, then exit.
	logger.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	stopped := make(chan struct{})
	go func() { grpcSrv.GracefulStop(); close(stopped) }()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	select {
	case <-stopped:
	case <-shutdownCtx.Done():
		grpcSrv.Stop()
	}
	if err := <-serveErr; !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serve: %w", err)
	}
	logger.Info("stopped")
	return nil
}

// registerServerGauge exports lobby.servers.healthy: game servers whose heartbeats arrive
// (draining ones included). The GameServerDown alert compares it with the expected count.
func registerServerGauge(reg *registry.Registry) error {
	_, err := otel.Meter("lobby").Int64ObservableGauge("lobby.servers.healthy",
		metric.WithDescription("Game servers that sent a heartbeat in the last 6 seconds"),
		metric.WithUnit("{server}"),
		metric.WithInt64Callback(func(ctx context.Context, o metric.Int64Observer) error {
			ctx, cancel := context.WithTimeout(ctx, time.Second)
			defer cancel()
			servers, err := reg.Servers(ctx)
			if err != nil {
				return err
			}
			o.Observe(int64(len(servers)))
			return nil
		}))
	if err != nil {
		return fmt.Errorf("lobby.servers.healthy: %w", err)
	}
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
