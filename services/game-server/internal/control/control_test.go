package control

import (
	"io"
	"log/slog"
	"testing"

	gamev1 "github.com/Bowenn/gap-project_private/proto/gen/go/doodle/game/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

func req(id string) *gamev1.AllocateRoomRequest {
	return &gamev1.AllocateRoomRequest{RoomId: id, Mode: gamev1.GameMode_GAME_MODE_CLASSIC}
}

func TestAllocateRoomIsIdempotent(t *testing.T) {
	s := New("gs-1", "/ws/gs-1", 10, quiet)
	for range 3 {
		resp, err := s.AllocateRoom(t.Context(), req("r1"))
		if err != nil {
			t.Fatal(err)
		}
		if resp.GetServerId() != "gs-1" || resp.GetWsAddress() != "/ws/gs-1" {
			t.Fatalf("resp = %v", resp)
		}
	}
	if rooms, _ := s.Status(); len(rooms) != 1 || rooms[0].ID != "r1" {
		t.Fatalf("rooms = %+v, want one r1", rooms)
	}
}

func TestAllocateRoomErrors(t *testing.T) {
	s := New("gs-1", "/ws/gs-1", 1, quiet)
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

	resp, err := s.DrainServer(t.Context(), &gamev1.DrainServerRequest{})
	if err != nil || resp.GetRoomsRemaining() != 1 {
		t.Fatalf("drain: %v, %v", resp, err)
	}
	if _, err := s.AllocateRoom(t.Context(), req("r9")); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("draining: err = %v", err)
	}
	// A retry for a room the server already has still succeeds while draining.
	if _, err := s.AllocateRoom(t.Context(), req("r1")); err != nil {
		t.Fatalf("retry while draining: %v", err)
	}
	if _, draining := s.Status(); !draining {
		t.Fatal("status doesn't report draining")
	}
}
