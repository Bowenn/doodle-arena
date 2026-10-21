// Command game-server runs a game server: room actors, rounds and guessing over WebSockets,
// registered with the lobby through heartbeats (design doc, sections 4.4 and 9).
package main

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Bowenn/gap-project_private/libs/go/kafkaauth"
	"github.com/Bowenn/gap-project_private/libs/go/logging"
	gamev1 "github.com/Bowenn/gap-project_private/proto/gen/go/doodle/game/v1"
	lobbyv1 "github.com/Bowenn/gap-project_private/proto/gen/go/doodle/lobby/v1"
	"github.com/Bowenn/gap-project_private/services/game-server/internal/control"
	"github.com/Bowenn/gap-project_private/services/game-server/internal/events"
	"github.com/Bowenn/gap-project_private/services/game-server/internal/heartbeat"
	"github.com/Bowenn/gap-project_private/services/game-server/internal/room"
	"github.com/Bowenn/gap-project_private/services/game-server/internal/roomstore"
	"github.com/Bowenn/gap-project_private/services/game-server/internal/server"
	"github.com/Bowenn/gap-project_private/services/game-server/internal/telemetry"
	"github.com/Bowenn/gap-project_private/services/game-server/internal/ticket"
	"github.com/Bowenn/gap-project_private/services/game-server/internal/ws"
	"github.com/redis/go-redis/extra/redisotel/v9"
	"github.com/redis/go-redis/v9"
	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"google.golang.org/grpc"
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
	redisAddr      string
	ticketKeys     string   // comma-separated PEM files with the lobby's public keys
	ticketKeysPEM  string   // or the keys themselves, as PEM text (AWS)
	origins        []string // Origin hosts allowed to open a WebSocket
	helloTimeout   time.Duration
	kafkaBrokers   []string
	s3             events.S3Config
	roundLogs      string // bucket for stroke logs
	// How long SIGTERM waits for the lobby to move this server's rooms away before closing the
	// rest (task 5.5). Under ECS's stop timeout (60 s) and compose's stop_grace_period.
	drainTimeout time.Duration
}

func loadConfig() config {
	host, _ := os.Hostname()
	instance := envOr("SERVICE_INSTANCE_ID", host)
	grpcPort := envOr("GRPC_PORT", "9090")
	maxRooms, err := strconv.Atoi(envOr("MAX_ROOMS", "100"))
	if err != nil || maxRooms < 1 {
		maxRooms = 100
	}
	drainTimeout, err := time.ParseDuration(envOr("DRAIN_TIMEOUT", "40s"))
	if err != nil || drainTimeout < 0 {
		drainTimeout = 40 * time.Second
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
		redisAddr:      envOr("REDIS_ADDR", "localhost:6379"),
		ticketKeys:     os.Getenv("TICKET_PUBLIC_KEY_PATH"),
		ticketKeysPEM:  os.Getenv("TICKET_PUBLIC_KEY_PEM"),
		origins:        strings.Split(envOr("WS_ALLOWED_ORIGINS", "localhost:8080"), ","),
		helloTimeout:   ws.DefaultHelloTimeout,
		kafkaBrokers:   strings.Split(envOr("KAFKA_BROKERS", "localhost:9094"), ","),
		s3: events.S3Config{
			Endpoint: os.Getenv("S3_ENDPOINT"), Region: envOr("S3_REGION", "us-east-1"),
			PathStyle: envOr("S3_PATH_STYLE", "true") == "true",
			AccessKey: os.Getenv("S3_ACCESS_KEY"), SecretKey: os.Getenv("S3_SECRET_KEY"),
		},
		roundLogs:    envOr("ROUND_LOGS_BUCKET", "round-logs"),
		drainTimeout: drainTimeout,
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

	var keys []ed25519.PublicKey
	var err error
	if cfg.ticketKeysPEM != "" {
		keys, err = ticket.ParsePublicKeys([]byte(cfg.ticketKeysPEM))
	} else {
		keys, err = ticket.LoadPublicKeys(cfg.ticketKeys)
	}
	if err != nil {
		return fmt.Errorf("join tickets (TICKET_PUBLIC_KEY_PEM or TICKET_PUBLIC_KEY_PATH, scripts/gen-ticket-key.sh): %w", err)
	}
	rdb := redis.NewClient(&redis.Options{
		Addr: cfg.redisAddr, DialTimeout: 2 * time.Second, ReadTimeout: time.Second, WriteTimeout: time.Second,
		// The game-server Redis user (task 5.5): it may touch room:* and ticket:* only. Empty in tests.
		Username: os.Getenv("REDIS_USERNAME"), Password: os.Getenv("REDIS_PASSWORD"),
	})
	defer rdb.Close()
	// Without the arguments: a commit's carry the room's events and snapshot, a redeem's the
	// join ticket.
	if err := redisotel.InstrumentTracing(rdb, redisotel.WithDBStatement(false)); err != nil {
		return fmt.Errorf("redis tracing: %w", err)
	}
	meter := otel.Meter("game-server")
	roomMetrics, err := room.NewMetrics(meter)
	if err != nil {
		return fmt.Errorf("metrics: %w", err)
	}

	// Finished rounds and matches leave through the publisher's own goroutine; room actors
	// only queue them. Each is already in its room's outbox in Redis (ADR 0009), and the
	// publisher deletes it there once Kafka has it. It runs until the rooms have stopped (see
	// the shutdown below); what it can't finish stays in the outbox for the next owner.
	// Stroke logs go to the round-logs bucket: static keys locally, the task role in AWS.
	s3Client, err := events.NewS3Client(ctx, cfg.s3)
	if err != nil {
		return err
	}
	// The game-server Kafka user (ADR 0013): it may write game.rounds.v1 and game.matches.v1.
	kafkaAuth, err := kafkaauth.FromEnv().Opts()
	if err != nil {
		return err
	}
	kafka, err := events.NewKafkaClient(cfg.kafkaBrokers, kafkaAuth...)
	if err != nil {
		return err
	}
	defer kafka.Close()
	// Rooms are stored in Redis under a lease, so another server can take them over (ADR 0007).
	store := roomstore.New(rdb)
	publisher, err := events.New(events.Config{
		Kafka: kafka, S3: s3Client, Bucket: cfg.roundLogs, Log: logger, Meter: meter,
		Outbox: store,
	})
	if err != nil {
		return err
	}
	pubCtx, abortPublishing := context.WithCancel(context.Background())
	defer abortPublishing()
	go publisher.Run(pubCtx)
	defer func() { publisher.Close(); abortPublishing(); <-publisher.Done() }()

	// Rooms run until roomsCtx ends; on shutdown they close their sockets with 1001.
	roomsCtx, stopRooms := context.WithCancel(context.Background())
	defer stopRooms()
	ctrl := control.New(roomsCtx, control.Config{
		ID: cfg.instance, WSAddress: cfg.wsAddress, MaxRooms: cfg.maxRooms, Metrics: roomMetrics, Events: publisher,
		Store: store, Log: logger,
		// What a room's outbox still holds is queued again: on takeover, and when stale (ADR 0009).
		Outbox: publisher,
	})
	wsHandler, err := ws.NewHandler(ws.Config{
		ServerID: cfg.instance, Rooms: ctrl, Verifier: ticket.NewVerifier(keys, time.Now),
		Redeemer: ticket.NewRedisRedeemer(rdb, cfg.instance, time.Now), Origins: cfg.origins,
		HelloTimeout: cfg.helloTimeout, Log: logger, Meter: meter,
	})
	if err != nil {
		return fmt.Errorf("websocket handler: %w", err)
	}

	srv := &http.Server{
		Addr:              ":" + cfg.port,
		Handler:           server.NewHandler(server.Info{Service: serviceName, Version: cfg.version, Instance: cfg.instance}, wsHandler),
		ReadHeaderTimeout: 5 * time.Second,
	}

	// Internal gRPC port for the lobby's ControlService calls; the gateway never routes it.
	grpcSrv := grpc.NewServer(grpc.StatsHandler(otelgrpc.NewServerHandler()))
	gamev1.RegisterControlServiceServer(grpcSrv, ctrl)
	lis, err := net.Listen("tcp", ":"+cfg.grpcPort)
	if err != nil {
		return fmt.Errorf("grpc listen: %w", err)
	}

	lobbyConn, err := heartbeat.Dial(cfg.lobbyAddr)
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
	case <-ctrl.DrainRequested():
		logger.Info("drain requested over gRPC")
	}
	// A second SIGINT or SIGTERM now stops the process at once instead of waiting for the drain.
	stop()

	// SIGTERM (a deploy, ECS stopping the task, docker stop) or the DrainServer RPC drains first
	// (task 5.5): the server takes no new rooms and tells the lobby at once, and the lobby moves
	// every room to another server, where it carries on from Redis; this server's copies stop
	// with room_moved as they are fenced, and their players rejoin there. Until then everything
	// keeps running.
	drain(ctx, ctrl, sender, cfg.drainTimeout, logger)

	// Then: stop heartbeats, stop accepting connections, close the rooms that are left (their
	// sockets get 1001 and can reconnect; the lobby moves them by ordinary failover once the
	// heartbeats stop), wait for the sockets, publish the queued events (what isn't published in
	// time stays in the rooms' outboxes for the next owner, ADR 0009), then exit.
	logger.Info("shutting down")
	stopBeats()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	stopped := make(chan struct{})
	go func() { grpcSrv.GracefulStop(); close(stopped) }()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	stopRooms()
	if err := wsHandler.Shutdown(shutdownCtx); err != nil {
		logger.Warn("sockets still open at shutdown", "err", err)
	}
	ctrl.Wait()
	// The rooms are gone, so nothing new is queued: publish what is left, within the deadline.
	publisher.Close()
	select {
	case <-publisher.Done():
	case <-shutdownCtx.Done():
		logger.Warn("shutdown deadline: unpublished events dropped")
		abortPublishing()
		<-publisher.Done()
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

// drain stops new rooms, tells the lobby at once and waits until the lobby has moved every room
// away, or timeout (no other server can take them, or the lobby is down).
func drain(ctx context.Context, ctrl *control.Server, sender *heartbeat.Sender, timeout time.Duration, logger *slog.Logger) {
	_, span := otel.Tracer("game-server").Start(context.Background(), "game.server.drain")
	defer span.End()
	began := time.Now()
	rooms := ctrl.Drain(ctx)
	span.SetAttributes(attribute.Int("rooms.start", rooms))
	sender.Now()
	wctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	empty := ctrl.WaitEmpty(wctx, 100*time.Millisecond)
	left := ctrl.Rooms()
	span.SetAttributes(attribute.Int("rooms.left", left), attribute.Bool("drain.complete", empty))
	if empty {
		logger.Info("drain finished", "rooms", rooms, "duration_ms", time.Since(began).Milliseconds())
		return
	}
	logger.Warn("drain timed out: closing the rooms left", "rooms", rooms, "rooms_left", left,
		"duration_ms", time.Since(began).Milliseconds())
}
