package heartbeat

import (
	"context"
	"io"
	"log/slog"
	"net"
	"sync"
	"testing"
	"time"

	lobbyv1 "github.com/Bowenn/gap-project_private/proto/gen/go/doodle/lobby/v1"
	"github.com/Bowenn/gap-project_private/services/game-server/internal/control"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// fakeLobby records heartbeats.
type fakeLobby struct {
	lobbyv1.UnimplementedRegistryServiceServer
	mu    sync.Mutex
	beats []*lobbyv1.HeartbeatRequest
}

func (f *fakeLobby) Heartbeat(_ context.Context, req *lobbyv1.HeartbeatRequest) (*lobbyv1.HeartbeatResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.beats = append(f.beats, req)
	return &lobbyv1.HeartbeatResponse{}, nil
}

func (f *fakeLobby) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.beats)
}

type fixedStatus struct{}

func (fixedStatus) Status() ([]control.Room, bool) {
	return []control.Room{{ID: "r1", Players: 2, Seats: 3, Playing: true}, {ID: "r2"}}, true
}

func TestSenderBeatsUntilCancelled(t *testing.T) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	lobby := &fakeLobby{}
	gs := grpc.NewServer()
	lobbyv1.RegisterRegistryServiceServer(gs, lobby)
	go func() { _ = gs.Serve(lis) }()
	defer gs.Stop()

	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	id := Identity{ServerID: "gs-1", WSAddress: "/ws/gs-1", ControlAddress: "gs-1:9090"}
	sender := NewSender(lobbyv1.NewRegistryServiceClient(conn), id, fixedStatus{}, 50*time.Millisecond,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { sender.Run(ctx); close(done) }()

	deadline := time.Now().Add(5 * time.Second)
	for lobby.count() < 3 {
		if time.Now().After(deadline) {
			t.Fatalf("only %d heartbeats", lobby.count())
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run didn't return after cancel")
	}

	lobby.mu.Lock()
	b := lobby.beats[0]
	lobby.mu.Unlock()
	if b.GetServerId() != "gs-1" || b.GetControlAddress() != "gs-1:9090" || b.GetWsAddress() != "/ws/gs-1" || !b.GetDraining() {
		t.Fatalf("beat = %v", b)
	}
	if len(b.GetRooms()) != 2 || b.GetRooms()[0].GetPhase() != lobbyv1.RoomPhase_ROOM_PHASE_PLAYING ||
		b.GetRooms()[1].GetPhase() != lobbyv1.RoomPhase_ROOM_PHASE_WAITING || b.GetSockets() != 2 {
		t.Fatalf("rooms = %v, sockets = %d", b.GetRooms(), b.GetSockets())
	}
	if r1 := b.GetRooms()[0]; r1.GetPlayers() != 2 || r1.GetSeats() != 3 {
		t.Fatalf("r1 = %v, want 2 players in 3 seats", r1)
	}
	if len(b.GetRoomIds()) != 2 {
		t.Fatalf("room_ids = %v", b.GetRoomIds())
	}
}

// Now sends a beat without waiting for the interval: a draining server tells the lobby at once.
func TestNowBeatsWithoutWaitingForTheInterval(t *testing.T) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	lobby := &fakeLobby{}
	gs := grpc.NewServer()
	lobbyv1.RegisterRegistryServiceServer(gs, lobby)
	go func() { _ = gs.Serve(lis) }()
	defer gs.Stop()
	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	sender := NewSender(lobbyv1.NewRegistryServiceClient(conn), Identity{ServerID: "gs-1"}, fixedStatus{}, time.Hour,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go sender.Run(ctx)
	waitFor := func(n int) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for lobby.count() < n {
			if time.Now().After(deadline) {
				t.Fatalf("%d heartbeats, want %d", lobby.count(), n)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	waitFor(1) // the first beat, right away
	sender.Now()
	sender.Now() // a second request while one waits adds nothing
	waitFor(2)
	time.Sleep(50 * time.Millisecond)
	if got := lobby.count(); got > 3 {
		t.Fatalf("%d heartbeats after two Now calls, want at most 3", got)
	}
}
