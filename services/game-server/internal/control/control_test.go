package control

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	gamev1 "github.com/Bowenn/gap-project_private/proto/gen/go/doodle/game/v1"
	"github.com/Bowenn/gap-project_private/services/game-server/internal/room"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

func newServer(t *testing.T, maxRooms int) *Server {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	s := New(ctx, Config{ID: "gs-1", WSAddress: "/ws/gs-1", MaxRooms: maxRooms, Metrics: room.NoopMetrics(), Log: quiet})
	t.Cleanup(func() { cancel(); s.Wait() })
	return s
}

func req(id string) *gamev1.AllocateRoomRequest {
	return &gamev1.AllocateRoomRequest{RoomId: id, Mode: gamev1.GameMode_GAME_MODE_CLASSIC, HostUserId: "u1"}
}

func TestAllocateRoomIsIdempotent(t *testing.T) {
	s := newServer(t, 10)
	for range 3 {
		resp, err := s.AllocateRoom(t.Context(), req("r1"))
		if err != nil {
			t.Fatal(err)
		}
		if resp.GetServerId() != "gs-1" || resp.GetWsAddress() != "/ws/gs-1" {
			t.Fatalf("resp = %v", resp)
		}
	}
	rooms, _ := s.Status()
	if len(rooms) != 1 || rooms[0].ID != "r1" || rooms[0].Players != 0 || rooms[0].Playing {
		t.Fatalf("rooms = %+v", rooms)
	}
	if s.Room("r1") == nil || s.Room("r2") != nil {
		t.Fatal("room lookup")
	}
}

func TestAllocateRoomErrors(t *testing.T) {
	s := newServer(t, 1)
	if _, err := s.AllocateRoom(t.Context(), req("r1")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AllocateRoom(t.Context(), req("r2")); status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("full: err = %v", err)
	}
	if _, err := s.AllocateRoom(t.Context(), req("BAD:id")); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("bad id: err = %v", err)
	}
	if _, err := s.AllocateRoom(t.Context(), &gamev1.AllocateRoomRequest{RoomId: "r3"}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("no mode: err = %v", err)
	}
	bad := req("r4")
	bad.Settings = &gamev1.RoomSettings{MaxPlayers: 20}
	if _, err := s.AllocateRoom(t.Context(), bad); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("bad settings: err = %v", err)
	}

	resp, err := s.DrainServer(t.Context(), &gamev1.DrainServerRequest{})
	if err != nil || resp.GetRoomsRemaining() != 1 {
		t.Fatalf("drain: %v, %v", resp, err)
	}
	if _, err := s.AllocateRoom(t.Context(), req("r9")); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("draining: err = %v", err)
	}
	if _, err := s.AllocateRoom(t.Context(), req("r1")); err != nil {
		t.Fatalf("retry while draining: %v", err)
	}
	if _, draining := s.Status(); !draining {
		t.Fatal("status doesn't report draining")
	}
}

func TestClosedRoomsDisappear(t *testing.T) {
	// A shutdown-cancelled context stops every room; they leave the table.
	ctx, cancel := context.WithCancel(context.Background())
	s := New(ctx, Config{ID: "gs-1", WSAddress: "/ws/gs-1", MaxRooms: 10, Metrics: room.NoopMetrics(), Log: quiet})
	if _, err := s.AllocateRoom(t.Context(), req("r1")); err != nil {
		t.Fatal(err)
	}
	cancel()
	s.Wait()
	if rooms, _ := s.Status(); len(rooms) != 0 || s.Room("r1") != nil {
		t.Fatalf("rooms = %+v", rooms)
	}
}

func TestResolveSettings(t *testing.T) {
	got, err := Resolve(&gamev1.RoomSettings{MaxPlayers: 4, DrawSeconds: 60})
	want := room.Settings{MaxPlayers: 4, Choose: 10 * time.Second, Draw: 60 * time.Second, WordOptions: 3}
	if err != nil || got != want {
		t.Fatalf("got %+v, %v", got, err)
	}
	if got, err := Resolve(nil); err != nil || got != room.DefaultSettings {
		t.Fatalf("defaults: %+v, %v", got, err)
	}
	for _, bad := range []*gamev1.RoomSettings{{MaxPlayers: 1}, {ChooseSeconds: 31}, {DrawSeconds: 600}, {WordOptions: 6}} {
		if _, err := Resolve(bad); err == nil {
			t.Errorf("accepted %v", bad)
		}
	}
}

// A stale lobby connection can reach the wrong server; it must refuse and create nothing.
func TestAllocateRoomRefusesAnotherServersRequest(t *testing.T) {
	s := newServer(t, 10)
	misrouted := req("r1")
	misrouted.ServerId = "gs-2"
	if _, err := s.AllocateRoom(t.Context(), misrouted); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("err = %v, want FailedPrecondition", err)
	}
	if s.Room("r1") != nil {
		t.Fatal("a room was created for another server")
	}
	meant := req("r1")
	meant.ServerId = "gs-1"
	if _, err := s.AllocateRoom(t.Context(), meant); err != nil || s.Room("r1") == nil {
		t.Fatalf("own request: err = %v", err)
	}
}
