// Package heartbeat reports this game server to the lobby every 2 seconds
// (doodle.lobby.v1.RegistryService). The lobby forgets a server after 6 seconds of silence.
package heartbeat

import (
	"context"
	"log/slog"
	"time"

	lobbyv1 "github.com/Bowenn/doodle-arena/proto/gen/go/doodle/lobby/v1"
	"github.com/Bowenn/doodle-arena/services/game-server/internal/control"
	"github.com/Bowenn/doodle-arena/services/game-server/internal/relay"
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
		// No service config from DNS. Otherwise the DNS resolver also looks up a TXT record
		// (_grpc_config.<host>) and reports the addresses only after it: Docker's DNS forwards
		// that query upstream, which took 5-8 s when cold, and every beat until then failed.
		grpc.WithDisableServiceConfig(),
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

// ViewerStatus is what a relay reports besides Status (ADR 0017): its viewers, in total and per
// room it streams.
type ViewerStatus interface {
	Viewers() (total uint32, rooms []relay.Watched)
}

// Identity is how the lobby reaches this server, and what it is.
type Identity struct {
	ServerID       string
	WSAddress      string
	ControlAddress string // empty for a relay
	Role           lobbyv1.ServerRole
}

// Sender sends heartbeats.
type Sender struct {
	client   lobbyv1.RegistryServiceClient
	id       Identity
	status   Status
	interval time.Duration
	log      *slog.Logger
	now      chan struct{} // Now's requests; one waiting is enough
}

// NewSender returns a sender that beats every interval.
func NewSender(client lobbyv1.RegistryServiceClient, id Identity, status Status, interval time.Duration, log *slog.Logger) *Sender {
	return &Sender{client: client, id: id, status: status, interval: interval, log: log, now: make(chan struct{}, 1)}
}

// Now asks Run for a beat right away, without waiting for the next tick: a draining server
// tells the lobby at once (task 5.5).
func (s *Sender) Now() {
	select {
	case s.now <- struct{}{}:
	default: // a beat is already asked for
	}
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
		case <-s.now:
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
		Role:           s.id.Role,
	}
	if vs, ok := s.status.(ViewerStatus); ok {
		total, watched := vs.Viewers()
		req.Viewers = total
		for _, w := range watched {
			req.Watched = append(req.Watched, &lobbyv1.WatchedRoom{RoomId: w.RoomID, Viewers: w.Viewers})
		}
	}
	for _, r := range rooms {
		phase := lobbyv1.RoomPhase_ROOM_PHASE_WAITING
		if r.Playing {
			phase = lobbyv1.RoomPhase_ROOM_PHASE_PLAYING
		}
		req.RoomIds = append(req.RoomIds, r.ID)
		req.Rooms = append(req.Rooms, &lobbyv1.RoomStatus{RoomId: r.ID, Players: r.Players, Seats: r.Seats, Phase: phase})
		req.Sockets += r.Players
	}
	_, err := s.client.Heartbeat(ctx, req)
	return err
}
