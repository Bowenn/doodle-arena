package registry

import (
	"context"
	"log/slog"
	"net"
	"regexp"
	"strconv"
	"time"

	lobbyv1 "github.com/Bowenn/gap-project_private/proto/gen/go/doodle/lobby/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// RoomToucher keeps the lobby's records of the rooms a server still hosts from expiring.
type RoomToucher interface {
	Touch(ctx context.Context, serverID string, roomIDs []string) error
}

// GRPCServer serves doodle.lobby.v1.RegistryService on the lobby's internal gRPC port.
type GRPCServer struct {
	lobbyv1.UnimplementedRegistryServiceServer
	reg   *Registry
	rooms RoomToucher
	now   func() time.Time
	log   *slog.Logger
}

// NewGRPCServer returns the Heartbeat handler.
func NewGRPCServer(reg *Registry, rooms RoomToucher, now func() time.Time, log *slog.Logger) *GRPCServer {
	return &GRPCServer{reg: reg, rooms: rooms, now: now, log: log}
}

var (
	serverIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
	// ws_address is a path on the gateway, never a full URL, so a bogus heartbeat can't send
	// players to another host (the join response hands it to browsers).
	wsAddressPattern = regexp.MustCompile(`^/ws(/[a-z0-9-]{1,63}){0,4}$`)
	hostPattern      = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]{0,251}[a-z0-9])?$`)
	roomIDPattern    = regexp.MustCompile(`^[a-z0-9]{1,64}$`)
)

// validControlAddress accepts host:port with a DNS name or IPv4 address.
func validControlAddress(addr string) bool {
	host, port, err := net.SplitHostPort(addr)
	if err != nil || !hostPattern.MatchString(host) {
		return false
	}
	n, err := strconv.Atoi(port)
	return err == nil && n > 0 && n < 65536
}

// Heartbeat records that a game server is alive, with its load and rooms.
func (g *GRPCServer) Heartbeat(ctx context.Context, req *lobbyv1.HeartbeatRequest) (*lobbyv1.HeartbeatResponse, error) {
	if !serverIDPattern.MatchString(req.GetServerId()) {
		return nil, status.Error(codes.InvalidArgument, "server_id must be 1-63 lowercase letters, digits or dashes")
	}
	if !wsAddressPattern.MatchString(req.GetWsAddress()) {
		return nil, status.Error(codes.InvalidArgument, "ws_address must be a gateway path under /ws")
	}
	if !validControlAddress(req.GetControlAddress()) {
		return nil, status.Error(codes.InvalidArgument, "control_address must be host:port")
	}
	for _, r := range req.GetRooms() {
		if !roomIDPattern.MatchString(r.GetRoomId()) {
			return nil, status.Error(codes.InvalidArgument, "room ids are 1-64 lowercase letters or digits")
		}
	}
	for _, id := range req.GetRoomIds() {
		if !roomIDPattern.MatchString(id) {
			return nil, status.Error(codes.InvalidArgument, "room ids are 1-64 lowercase letters or digits")
		}
	}

	s := Server{
		ID:             req.GetServerId(),
		WSAddress:      req.GetWsAddress(),
		ControlAddress: req.GetControlAddress(),
		Sockets:        req.GetSockets(),
		Draining:       req.GetDraining(),
		SeenAt:         g.now().UTC(),
	}
	for _, r := range req.GetRooms() {
		phase := PhaseWaiting
		if r.GetPhase() == lobbyv1.RoomPhase_ROOM_PHASE_PLAYING {
			phase = PhasePlaying
		}
		s.Rooms = append(s.Rooms, Room{ID: r.GetRoomId(), Players: r.GetPlayers(), Phase: phase})
	}
	// A server that only fills the older room_ids field: rooms without status.
	if len(s.Rooms) == 0 {
		for _, id := range req.GetRoomIds() {
			s.Rooms = append(s.Rooms, Room{ID: id, Phase: PhaseWaiting})
		}
	}

	registered, err := g.reg.Record(ctx, s)
	if err != nil {
		g.log.ErrorContext(ctx, "heartbeat not recorded", "server_id", s.ID, "err", err)
		return nil, status.Error(codes.Unavailable, "registry unavailable")
	}
	if registered {
		g.log.InfoContext(ctx, "game server registered", "server_id", s.ID,
			"ws_address", s.WSAddress, "control_address", s.ControlAddress)
	}
	if len(s.Rooms) > 0 {
		ids := make([]string, len(s.Rooms))
		for i, r := range s.Rooms {
			ids[i] = r.ID
		}
		if err := g.rooms.Touch(ctx, s.ID, ids); err != nil {
			// The heartbeat itself counted; the rooms are refreshed by the next one.
			g.log.WarnContext(ctx, "rooms not refreshed", "server_id", s.ID, "err", err)
		}
	}
	return &lobbyv1.HeartbeatResponse{}, nil
}
