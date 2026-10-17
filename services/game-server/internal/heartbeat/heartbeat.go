// Package heartbeat reports this game server to the lobby every 2 seconds
// (doodle.lobby.v1.RegistryService). The lobby forgets a server after 6 seconds of silence.
package heartbeat

import (
	"context"
	"log/slog"
	"time"

	lobbyv1 "github.com/Bowenn/gap-project_private/proto/gen/go/doodle/lobby/v1"
	"github.com/Bowenn/gap-project_private/services/game-server/internal/control"
	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/backoff"
	"google.golang.org/grpc/credentials/insecure"
)

// Interval is the time between heartbeats.
const Interval = 2 * time.Second

// Dial returns a client connection to the lobby's registry at target (host:port, or any gRPC
// target). It connects lazily, on the first heartbeat.
func Dial(target string) (*grpc.ClientConn, error) {
	return grpc.NewClient(target,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithStatsHandler(otelgrpc.NewClientHandler()),
		// gRPC's default reconnect backoff grows to 2 minutes. Cap it at the heartbeat interval,
		// so a restarted lobby sees this server again within a few seconds.
		grpc.WithConnectParams(grpc.ConnectParams{
			Backoff:           backoff.Config{BaseDelay: 250 * time.Millisecond, Multiplier: 1.6, Jitter: 0.2, MaxDelay: Interval},
			MinConnectTimeout: time.Second,
		}),
	)
}

// Status reports the server's rooms and whether it drains.
type Status interface {
	Status() (rooms []control.Room, draining bool)
}

// Identity is how the lobby reaches this server.
type Identity struct {
	ServerID       string
	WSAddress      string
	ControlAddress string
}

// Sender sends heartbeats.
type Sender struct {
	client   lobbyv1.RegistryServiceClient
	id       Identity
	status   Status
	interval time.Duration
	log      *slog.Logger
}

// NewSender returns a sender that beats every interval.
func NewSender(client lobbyv1.RegistryServiceClient, id Identity, status Status, interval time.Duration, log *slog.Logger) *Sender {
	return &Sender{client: client, id: id, status: status, interval: interval, log: log}
}

// Run sends a heartbeat right away and then every interval, until ctx is done. A failed beat
// is logged once (and once more when beats get through again); the next tick retries.
func (s *Sender) Run(ctx context.Context) {
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	failing := false
	for {
		err := s.beat(ctx)
		switch {
		case err != nil && !failing && ctx.Err() == nil:
			s.log.WarnContext(ctx, "heartbeat failed", "server_id", s.id.ServerID, "err", err)
			failing = true
		case err == nil && failing:
			s.log.InfoContext(ctx, "heartbeat recovered", "server_id", s.id.ServerID)
			failing = false
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (s *Sender) beat(ctx context.Context) error {
	// A beat that takes longer than half an interval is useless; the next one replaces it.
	ctx, cancel := context.WithTimeout(ctx, s.interval/2)
	defer cancel()
	rooms, draining := s.status.Status()
	req := &lobbyv1.HeartbeatRequest{
		ServerId:       s.id.ServerID,
		WsAddress:      s.id.WSAddress,
		ControlAddress: s.id.ControlAddress,
		Draining:       draining,
	}
	for _, r := range rooms {
		phase := lobbyv1.RoomPhase_ROOM_PHASE_WAITING
		if r.Playing {
			phase = lobbyv1.RoomPhase_ROOM_PHASE_PLAYING
		}
		req.RoomIds = append(req.RoomIds, r.ID)
		req.Rooms = append(req.Rooms, &lobbyv1.RoomStatus{RoomId: r.ID, Players: r.Players, Phase: phase})
		req.Sockets += r.Players
	}
	_, err := s.client.Heartbeat(ctx, req)
	return err
}
