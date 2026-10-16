// Package control serves doodle.game.v1.ControlService, which the lobby calls to place rooms
// on this server, and keeps the server's table of rooms.
package control

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"sync"
	"time"

	gamev1 "github.com/Bowenn/gap-project_private/proto/gen/go/doodle/game/v1"
	"github.com/Bowenn/gap-project_private/services/game-server/internal/room"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Room is what this server reports about one of its rooms.
type Room struct {
	ID      string
	Players uint32
	Playing bool
}

// Server owns this process's table of rooms. The map is server bookkeeping, not room state:
// each room's state belongs to its actor.
type Server struct {
	gamev1.UnimplementedControlServiceServer

	id        string
	wsAddress string
	maxRooms  int
	clock     room.Clock
	metrics   *room.Metrics
	log       *slog.Logger
	ctx       context.Context // rooms run until it ends
	wg        sync.WaitGroup

	mu       sync.Mutex
	rooms    map[string]*room.Room
	draining bool
}

// Config creates a Server.
type Config struct {
	ID        string
	WSAddress string
	MaxRooms  int
	Clock     room.Clock
	Metrics   *room.Metrics
	Log       *slog.Logger
}

// New returns a control server. Rooms run until ctx ends; Wait waits for them.
func New(ctx context.Context, cfg Config) *Server {
	if cfg.Clock == nil {
		cfg.Clock = room.RealClock
	}
	return &Server{id: cfg.ID, wsAddress: cfg.WSAddress, maxRooms: cfg.MaxRooms, clock: cfg.Clock,
		metrics: cfg.Metrics, log: cfg.Log, ctx: ctx, rooms: map[string]*room.Room{}}
}

var roomIDPattern = regexp.MustCompile(`^[a-z0-9]{1,64}$`)

// AllocateRoom starts a room. It is idempotent: the lobby retries on timeouts, so asking again
// for a room this server already runs succeeds without creating a second one.
func (s *Server) AllocateRoom(ctx context.Context, req *gamev1.AllocateRoomRequest) (*gamev1.AllocateRoomResponse, error) {
	if !roomIDPattern.MatchString(req.GetRoomId()) {
		return nil, status.Error(codes.InvalidArgument, "room_id must be 1-64 lowercase letters or digits")
	}
	if req.GetMode() != gamev1.GameMode_GAME_MODE_CLASSIC {
		return nil, status.Error(codes.InvalidArgument, "unsupported mode")
	}
	settings, err := Resolve(req.GetSettings())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	resp := &gamev1.AllocateRoomResponse{ServerId: s.id, WsAddress: s.wsAddress}

	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.rooms[req.GetRoomId()]; ok {
		return resp, nil
	}
	if s.draining {
		return nil, status.Error(codes.FailedPrecondition, "server is draining")
	}
	if len(s.rooms) >= s.maxRooms {
		return nil, status.Error(codes.ResourceExhausted, "server is full")
	}
	r := room.New(room.Config{
		ID: req.GetRoomId(), Mode: "classic", HostID: req.GetHostUserId(), Settings: settings,
		Clock: s.clock, Log: s.log, Metrics: s.metrics,
	})
	s.rooms[r.ID()] = r
	s.wg.Go(func() {
		r.Run(s.ctx)
		s.mu.Lock()
		// A new room with the same id may already have replaced this one.
		if s.rooms[r.ID()] == r {
			delete(s.rooms, r.ID())
		}
		s.mu.Unlock()
	})
	s.log.InfoContext(ctx, "room allocated", "room_id", r.ID(), "server_id", s.id, "user_id", req.GetHostUserId())
	return resp, nil
}

// DrainServer stops new allocations. Handing rooms over is task 2.5.
func (s *Server) DrainServer(ctx context.Context, _ *gamev1.DrainServerRequest) (*gamev1.DrainServerResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.draining {
		s.log.InfoContext(ctx, "draining", "server_id", s.id, "rooms", len(s.rooms))
	}
	s.draining = true
	return &gamev1.DrainServerResponse{RoomsRemaining: uint32(len(s.rooms))}, nil
}

// Room finds a running room (for the WebSocket handler).
func (s *Server) Room(id string) *room.Room {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rooms[id]
}

// Status returns the rooms, sorted by id, and whether the server drains, for heartbeats. It
// reads each room's published summary, so a busy room never delays a heartbeat.
func (s *Server) Status() (rooms []Room, draining bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, r := range s.rooms {
		sum := r.Summary()
		if sum.Closed {
			continue
		}
		rooms = append(rooms, Room{ID: id, Players: uint32(sum.Players), Playing: sum.Playing})
	}
	slices.SortFunc(rooms, func(a, b Room) int { return cmp.Compare(a.ID, b.ID) })
	return rooms, s.draining
}

// Wait blocks until every room has stopped (after the context given to New ends).
func (s *Server) Wait() { s.wg.Wait() }

// Resolve turns the lobby's settings into room settings: zero means the default, and the
// ranges are the lobby's (services/lobby/internal/rooms).
func Resolve(in *gamev1.RoomSettings) (room.Settings, error) {
	d := room.DefaultSettings
	fields := []struct {
		name           string
		v, def, lo, hi int
	}{
		{"max_players", int(in.GetMaxPlayers()), d.MaxPlayers, 2, 8},
		{"choose_seconds", int(in.GetChooseSeconds()), int(d.Choose / time.Second), 5, 30},
		{"draw_seconds", int(in.GetDrawSeconds()), int(d.Draw / time.Second), 30, 180},
		{"word_options", int(in.GetWordOptions()), d.WordOptions, 1, 5},
	}
	out := make([]int, len(fields))
	for i, f := range fields {
		v := f.v
		if v == 0 {
			v = f.def
		}
		if v < f.lo || v > f.hi {
			return room.Settings{}, fmt.Errorf("%s must be between %d and %d", f.name, f.lo, f.hi)
		}
		out[i] = v
	}
	return room.Settings{MaxPlayers: out[0], Choose: time.Duration(out[1]) * time.Second,
		Draw: time.Duration(out[2]) * time.Second, WordOptions: out[3]}, nil
}
