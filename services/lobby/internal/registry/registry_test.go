package registry

import (
	"context"
	"io"
	"log/slog"
	"slices"
	"testing"
	"time"

	"github.com/Bowenn/gap-project_private/libs/go/redistest"
	lobbyv1 "github.com/Bowenn/gap-project_private/proto/gen/go/doodle/lobby/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestMain(m *testing.M) { redistest.Main(m) }

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

type touched struct {
	server string
	ids    []string
}

func (t *touched) Touch(_ context.Context, server string, ids []string) error {
	t.server = server
	t.ids = append(t.ids, ids...)
	return nil
}

func beat(id string, sockets uint32) *lobbyv1.HeartbeatRequest {
	return &lobbyv1.HeartbeatRequest{
		ServerId: id, WsAddress: "/ws/" + id, ControlAddress: id + ":9090", Sockets: sockets,
	}
}

func TestHeartbeatRegistersAndExpires(t *testing.T) {
	rdb := redistest.New(t)
	reg := New(rdb, 300*time.Millisecond, quiet)
	rooms := &touched{}
	srv := NewGRPCServer(reg, rooms, time.Now, quiet)
	ctx := t.Context()

	req := beat("game-server-1", 3)
	req.Rooms = []*lobbyv1.RoomStatus{
		{RoomId: "r1", Players: 2, Seats: 3, Phase: lobbyv1.RoomPhase_ROOM_PHASE_PLAYING},
		{RoomId: "r2", Players: 0},
	}
	if _, err := srv.Heartbeat(ctx, req); err != nil {
		t.Fatal(err)
	}
	got, err := reg.Servers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "game-server-1" || got[0].ControlAddress != "game-server-1:9090" || got[0].Sockets != 3 {
		t.Fatalf("servers = %+v", got)
	}
	want := []Room{{ID: "r1", Players: 2, Seats: 3, Phase: PhasePlaying}, {ID: "r2", Phase: PhaseWaiting}}
	if !slices.Equal(got[0].Rooms, want) {
		t.Fatalf("rooms = %+v, want %+v", got[0].Rooms, want)
	}
	if rooms.server != "game-server-1" || !slices.Equal(rooms.ids, []string{"r1", "r2"}) {
		t.Fatalf("touched rooms = %v", rooms.ids)
	}

	// No heartbeat for longer than the TTL: the server is gone, and forgotten from the set.
	time.Sleep(500 * time.Millisecond)
	got, expired, err := reg.servers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 || !slices.Equal(expired, []string{"game-server-1"}) {
		t.Fatalf("live = %+v, expired = %v", got, expired)
	}
	if n := rdb.SCard(ctx, serversKey).Val(); n != 0 {
		t.Fatalf("servers set still has %d members", n)
	}
	if _, err := reg.Server(ctx, "game-server-1"); err != ErrNotFound {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestRegularHeartbeatsKeepAServerAlive(t *testing.T) {
	rdb := redistest.New(t)
	reg := New(rdb, 300*time.Millisecond, quiet)
	srv := NewGRPCServer(reg, &touched{}, time.Now, quiet)
	for range 6 {
		if _, err := srv.Heartbeat(t.Context(), beat("game-server-1", 0)); err != nil {
			t.Fatal(err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if got, err := reg.Servers(t.Context()); err != nil || len(got) != 1 {
		t.Fatalf("servers = %+v, err = %v", got, err)
	}
}

func TestRecordReportsNewServers(t *testing.T) {
	reg := New(redistest.New(t), time.Minute, quiet)
	s := Server{ID: "game-server-1", WSAddress: "/ws/game-server-1", ControlAddress: "x:1"}
	first, err := reg.Record(t.Context(), s)
	if err != nil || !first {
		t.Fatalf("first = %v, err = %v", first, err)
	}
	again, err := reg.Record(t.Context(), s)
	if err != nil || again {
		t.Fatalf("again = %v, err = %v", again, err)
	}
}

func TestOlderGameServersReportOnlyRoomIDs(t *testing.T) {
	reg := New(redistest.New(t), time.Minute, quiet)
	srv := NewGRPCServer(reg, &touched{}, time.Now, quiet)
	req := beat("game-server-1", 0)
	req.RoomIds = []string{"r1"}
	if _, err := srv.Heartbeat(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	s, err := reg.Server(t.Context(), "game-server-1")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(s.Rooms, []Room{{ID: "r1", Phase: PhaseWaiting}}) {
		t.Fatalf("rooms = %+v", s.Rooms)
	}
}

func TestHeartbeatValidation(t *testing.T) {
	srv := NewGRPCServer(New(redistest.New(t), time.Minute, quiet), &touched{}, time.Now, quiet)
	tests := []struct {
		name string
		req  *lobbyv1.HeartbeatRequest
	}{
		{"no server id", &lobbyv1.HeartbeatRequest{WsAddress: "/ws", ControlAddress: "x:1"}},
		{"server id with a key separator", &lobbyv1.HeartbeatRequest{ServerId: "a:b", WsAddress: "/ws", ControlAddress: "x:1"}},
		{"no ws address", &lobbyv1.HeartbeatRequest{ServerId: "gs-1", ControlAddress: "x:1"}},
		{"ws address on another host", &lobbyv1.HeartbeatRequest{ServerId: "gs-1", WsAddress: "wss://evil.example/ws", ControlAddress: "x:1"}},
		{"ws address outside /ws", &lobbyv1.HeartbeatRequest{ServerId: "gs-1", WsAddress: "/api/auth", ControlAddress: "x:1"}},
		{"ws address with a protocol-relative URL", &lobbyv1.HeartbeatRequest{ServerId: "gs-1", WsAddress: "//evil.example/ws", ControlAddress: "x:1"}},
		{"no control address", &lobbyv1.HeartbeatRequest{ServerId: "gs-1", WsAddress: "/ws"}},
		{"control address without a port", &lobbyv1.HeartbeatRequest{ServerId: "gs-1", WsAddress: "/ws", ControlAddress: "game-server-1"}},
		{"control address with a URL", &lobbyv1.HeartbeatRequest{ServerId: "gs-1", WsAddress: "/ws", ControlAddress: "http://x:1"}},
		{"bad room id", &lobbyv1.HeartbeatRequest{ServerId: "gs-1", WsAddress: "/ws", ControlAddress: "x:1",
			Rooms: []*lobbyv1.RoomStatus{{RoomId: "lobby:servers"}}}},
		{"bad legacy room id", &lobbyv1.HeartbeatRequest{ServerId: "gs-1", WsAddress: "/ws", ControlAddress: "x:1",
			RoomIds: []string{"../x"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := srv.Heartbeat(t.Context(), tt.req)
			if status.Code(err) != codes.InvalidArgument {
				t.Fatalf("err = %v, want InvalidArgument", err)
			}
		})
	}
}

func TestCandidatesLeastLoadedFirst(t *testing.T) {
	servers := []Server{
		{ID: "c", Sockets: 5},
		{ID: "a", Sockets: 2, Rooms: []Room{{ID: "x"}, {ID: "y"}}},
		{ID: "d", Sockets: 0, Draining: true},
		{ID: "b", Sockets: 2, Rooms: []Room{{ID: "z"}}},
		{ID: "e", Sockets: 2, Rooms: []Room{{ID: "w"}}},
	}
	var ids []string
	for _, s := range Candidates(servers) {
		ids = append(ids, s.ID)
	}
	// Draining d is skipped; fewest sockets, then fewest rooms, then id.
	if want := []string{"b", "e", "a", "c"}; !slices.Equal(ids, want) {
		t.Fatalf("order = %v, want %v", ids, want)
	}
}
