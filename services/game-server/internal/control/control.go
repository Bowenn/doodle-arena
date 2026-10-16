// Package control serves doodle.game.v1.ControlService, which the lobby calls to place rooms
// on this server.
//
// Task 2.1 stub: AllocateRoom only records the room id, so the lobby's create and join flow
// works end to end. Task 2.2 replaces the table with room actors, and task 2.5 adds the lease.
package control

import (
	"cmp"
	"context"
	"log/slog"
	"regexp"
	"slices"
	"sync"

	gamev1 "github.com/Bowenn/gap-project_private/proto/gen/go/doodle/game/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Room is what this server reports about one of its rooms.
type Room struct {
	ID      string
	Players uint32
	Playing bool
}

// Server owns this process's table of rooms.
type Server struct {
	gamev1.UnimplementedControlServiceServer

	id        string
	wsAddress string
	maxRooms  int
	log       *slog.Logger

	mu       sync.Mutex
	rooms    map[string]*gamev1.AllocateRoomRequest
	draining bool
}

// New returns a control server that accepts at most maxRooms rooms.
func New(id, wsAddress string, maxRooms int, log *slog.Logger) *Server {
	return &Server{id: id, wsAddress: wsAddress, maxRooms: maxRooms, log: log,
		rooms: make(map[string]*gamev1.AllocateRoomRequest)}
}

var roomIDPattern = regexp.MustCompile(`^[a-z0-9]{1,64}$`)

// AllocateRoom takes a room. It is idempotent: the lobby retries on timeouts, so asking again
// for a room this server already has succeeds without creating a second one.
func (s *Server) AllocateRoom(ctx context.Context, req *gamev1.AllocateRoomRequest) (*gamev1.AllocateRoomResponse, error) {
	if !roomIDPattern.MatchString(req.GetRoomId()) {
		return nil, status.Error(codes.InvalidArgument, "room_id must be 1-64 lowercase letters or digits")
	}
	if req.GetMode() != gamev1.GameMode_GAME_MODE_CLASSIC {
		return nil, status.Error(codes.InvalidArgument, "unsupported mode")
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
	s.rooms[req.GetRoomId()] = req
	s.log.InfoContext(ctx, "room allocated", "room_id", req.GetRoomId(), "server_id", s.id)
	return resp, nil
}

// DrainServer stops new allocations. In the stub no room has players, so none remain to
// finish; task 2.2 counts real rooms.
func (s *Server) DrainServer(ctx context.Context, _ *gamev1.DrainServerRequest) (*gamev1.DrainServerResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.draining {
		s.log.InfoContext(ctx, "draining", "server_id", s.id, "rooms", len(s.rooms))
	}
	s.draining = true
	return &gamev1.DrainServerResponse{RoomsRemaining: uint32(len(s.rooms))}, nil
}

// Status returns the rooms, sorted by id, and whether the server is draining, for heartbeats.
func (s *Server) Status() (rooms []Room, draining bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id := range s.rooms {
		rooms = append(rooms, Room{ID: id})
	}
	slices.SortFunc(rooms, func(a, b Room) int { return cmp.Compare(a.ID, b.ID) })
	return rooms, s.draining
}
