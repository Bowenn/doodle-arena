// Command game-server runs a game server: room actors, rounds, guessing and the per-room event log.
// Task 2.1: it registers with the lobby and accepts rooms; the game itself arrives in task 2.2.
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
	"strconv"
	"syscall"
	"time"

	gamev1 "github.com/Bowenn/gap-project_private/proto/gen/go/doodle/game/v1"
	lobbyv1 "github.com/Bowenn/gap-project_private/proto/gen/go/doodle/lobby/v1"
	"github.com/Bowenn/gap-project_private/services/game-server/internal/control"
	"github.com/Bowenn/gap-project_private/services/game-server/internal/heartbeat"
	"github.com/Bowenn/gap-project_private/services/game-server/internal/logging"
	"github.com/Bowenn/gap-project_private/services/game-server/internal/server"
	"github.com/Bowenn/gap-project_private/services/game-server/internal/telemetry"
	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

const serviceName = "game-server"

type config struct {
	port      string
	grpcPort  string
	version   string
	instance  string
	logLevel  slog.Level
	lobbyAddr string
	// How the lobby and clients reach this server; both default from the instance id.
	wsAddress      string
	controlAddress string
	maxRooms       int
}

func loadConfig() config {
	host, _ := os.Hostname()
	instance := envOr("SERVICE_INSTANCE_ID", host)
	grpcPort := envOr("GRPC_PORT", "9090")
	maxRooms, err := strconv.Atoi(envOr("MAX_ROOMS", "100"))
	if err != nil || maxRooms < 1 {
		maxRooms = 100
	}
	return config{
		port:           envOr("PORT", "8080"),
		grpcPort:       grpcPort,
		version:        envOr("SERVICE_VERSION", "dev"),
		instance:       instance,
		logLevel:       logging.ParseLevel(os.Getenv("LOG_LEVEL")),
		lobbyAddr:      envOr("LOBBY_GRPC_ADDR", "localhost:9090"),
		wsAddress:      envOr("WS_ADDRESS", "/ws/"+instance),
		controlAddress: envOr("CONTROL_ADDRESS", instance+":"+grpcPort),
		maxRooms:       maxRooms,
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

	// Internal gRPC port for the lobby's ControlService calls; the gateway never routes it.
	ctrl := control.New(cfg.instance, cfg.wsAddress, cfg.maxRooms, logger)
	grpcSrv := grpc.NewServer(grpc.StatsHandler(otelgrpc.NewServerHandler()))
	gamev1.RegisterControlServiceServer(grpcSrv, ctrl)
	lis, err := net.Listen("tcp", ":"+cfg.grpcPort)
	if err != nil {
		return fmt.Errorf("grpc listen: %w", err)
	}

	lobbyConn, err := grpc.NewClient(cfg.lobbyAddr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithStatsHandler(otelgrpc.NewClientHandler()),
	)
	if err != nil {
		return fmt.Errorf("lobby client: %w", err)
	}
	defer lobbyConn.Close()
	sender := heartbeat.NewSender(lobbyv1.NewRegistryServiceClient(lobbyConn), heartbeat.Identity{
		ServerID: cfg.instance, WSAddress: cfg.wsAddress, ControlAddress: cfg.controlAddress,
	}, ctrl, heartbeat.Interval, logger)
	beatCtx, stopBeats := context.WithCancel(context.Background())
	beatsDone := make(chan struct{})
	go func() { sender.Run(beatCtx); close(beatsDone) }()
	defer func() { stopBeats(); <-beatsDone }()

	serveErr := make(chan error, 2)
	go func() {
		logger.Info("listening", "addr", srv.Addr, "instance", cfg.instance)
		serveErr <- srv.ListenAndServe()
	}()
	go func() {
		logger.Info("grpc listening", "addr", lis.Addr().String(), "lobby", cfg.lobbyAddr)
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

	// SIGTERM: stop heartbeats (the lobby stops sending rooms here within 6 seconds), stop
	// accepting connections, let in-flight requests finish, then exit.
	logger.Info("shutting down")
	stopBeats()
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
