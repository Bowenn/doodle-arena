package control

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"slices"
	"sync"
	"testing"
	"time"

	gamev1 "github.com/Bowenn/gap-project_private/proto/gen/go/doodle/game/v1"
	"github.com/Bowenn/gap-project_private/services/game-server/internal/protocol"
	"github.com/Bowenn/gap-project_private/services/game-server/internal/redistest"
	"github.com/Bowenn/gap-project_private/services/game-server/internal/room"
	"github.com/Bowenn/gap-project_private/services/game-server/internal/roomstore"
	"github.com/Bowenn/gap-project_private/services/game-server/internal/ticket"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestMain(m *testing.M) { redistest.Main(m) }

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

// conn is a socket as the room sees it, recording messages and the close code.
type conn struct {
	uid    string
	mu     sync.Mutex
	types  []string
	closed int
}

func (c *conn) UserID() string { return c.uid }
func (c *conn) Send(b []byte) {
	var m struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal(b, &m)
	c.mu.Lock()
	c.types = append(c.types, m.Type)
	c.mu.Unlock()
}
func (c *conn) Close(code int, _ string) { c.mu.Lock(); c.closed = code; c.mu.Unlock() }
func (c *conn) state() ([]string, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.types), c.closed
}

func storedServer(t *testing.T, id string, store *roomstore.Store) *Server {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	s := New(ctx, Config{ID: id, WSAddress: "/ws/" + id, MaxRooms: 10, Metrics: room.NoopMetrics(), Store: store, Log: quiet})
	t.Cleanup(func() { cancel(); s.Wait() })
	return s
}

// fakeRequeuer records the outbox entries a server queues again, by event_id.
type fakeRequeuer struct {
	mu  sync.Mutex
	got []string
}

func (f *fakeRequeuer) Requeue(entries []room.OutboxEntry) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, e := range entries {
		f.got = append(f.got, e.EventID)
	}
	return len(entries)
}

func (f *fakeRequeuer) all() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.got)
}

func outboxServer(t *testing.T, id string, store *roomstore.Store, rq Requeuer, scan, stale time.Duration) *Server {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	s := New(ctx, Config{ID: id, WSAddress: "/ws/" + id, MaxRooms: 10, Metrics: room.NoopMetrics(), Store: store, Log: quiet,
		Outbox: rq, OutboxScan: scan, StaleAfter: stale})
	t.Cleanup(func() { cancel(); s.Wait() })
	return s
}

// ADR 0009: gs-1 committed a finished round and died before Kafka had it; gs-2, taking the
// room, queues the entry it finds.
func TestTakeoverRequeuesWhatThePreviousOwnerLeft(t *testing.T) {
	store := roomstore.New(redistest.New(t))
	ctx := t.Context()
	a := outboxServer(t, "gs-1", store, &fakeRequeuer{}, time.Hour, time.Hour)
	onA, onB := req("r1"), req("r1")
	onA.ServerId, onB.ServerId = "gs-1", "gs-2"
	if _, err := a.AllocateRoom(ctx, onA); err != nil {
		t.Fatal(err)
	}
	left := []room.OutboxEntry{{EventID: "evt-left", Kind: room.OutboxRound, Payload: []byte("x")}}
	if err := store.Commit(ctx, "r1", "gs-1", a.Room("r1").Token(), nil, nil, left); err != nil {
		t.Fatal(err)
	}

	rq := &fakeRequeuer{}
	b := outboxServer(t, "gs-2", store, rq, time.Hour, time.Hour)
	if _, err := b.AllocateRoom(ctx, onB); err != nil {
		t.Fatal(err)
	}
	if got := rq.all(); !slices.Equal(got, []string{"evt-left"}) {
		t.Fatalf("gs-2 queued %v, want the entry gs-1 left", got)
	}
}

// An entry its own server failed to publish is queued again by the scan once it is stale.
func TestStaleOutboxEntriesAreQueuedAgain(t *testing.T) {
	store := roomstore.New(redistest.New(t))
	ctx := t.Context()
	rq := &fakeRequeuer{}
	s := outboxServer(t, "gs-1", store, rq, 50*time.Millisecond, time.Millisecond)
	if _, err := s.AllocateRoom(ctx, req("r1")); err != nil {
		t.Fatal(err)
	}
	stuck := []room.OutboxEntry{{EventID: "evt-stuck", Kind: room.OutboxMatch, Payload: []byte("x")}}
	if err := store.Commit(ctx, "r1", "gs-1", s.Room("r1").Token(), nil, nil, stuck); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the stale entry queued again", func() bool { return slices.Contains(rq.all(), "evt-stuck") })
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// M2 exit criterion, across two servers and a real Redis: after gs-2 takes the room over,
// gs-1's room is fenced (its lease keeper finds the lease gone), tells its clients the room
// moved and stops; clients resume on gs-2 from their last seq.
func TestTakeoverAcrossServers(t *testing.T) {
	rdb := redistest.New(t)
	store := roomstore.New(rdb)
	a, b := storedServer(t, "gs-1", store), storedServer(t, "gs-2", store)
	ctx := t.Context()

	reqA := req("r1")
	reqA.ServerId = "gs-1"
	if _, err := a.AllocateRoom(ctx, reqA); err != nil {
		t.Fatal(err)
	}
	if got := a.Room("r1").Token(); got != 1 {
		t.Fatalf("gs-1's token = %d, want 1", got)
	}
	u1, u2 := &conn{uid: "u1"}, &conn{uid: "u2"}
	for _, c := range []*conn{u1, u2} {
		if err := a.Room("r1").Join(ctx, c, c.uid, 0); err != nil {
			t.Fatal(err)
		}
	}
	eventually(t, "both joins stored", func() bool { return rdb.XLen(ctx, "room:{r1}:events").Val() >= 2 })

	reqB := req("r1")
	reqB.ServerId = "gs-2"
	if _, err := b.AllocateRoom(ctx, reqB); err != nil {
		t.Fatal(err)
	}
	if b.Room("r1") == nil {
		t.Fatal("gs-2 didn't take the room")
	}
	// Each owner's room carries the fencing token it took the lease with, and tickets redeem
	// only against the current one (ADR 0007): gs-1's token 1 no longer works.
	if got := b.Room("r1").Token(); got != 2 {
		t.Fatalf("gs-2's token = %d, want 2", got)
	}
	tk := ticket.Ticket{ID: "j1", RoomID: "r1", ExpiresAt: time.Now().Add(ticket.TTL)}
	if err := ticket.NewRedisRedeemer(rdb, "gs-1", time.Now).Redeem(ctx, tk, 1); !errors.Is(err, ticket.ErrNotOwner) {
		t.Fatalf("redeem on gs-1 after the takeover: err = %v, want ErrNotOwner", err)
	}
	if err := ticket.NewRedisRedeemer(rdb, "gs-2", time.Now).Redeem(ctx, tk, b.Room("r1").Token()); err != nil {
		t.Fatalf("redeem on gs-2: %v", err)
	}

	// gs-1 is still running and unaware; its lease keeper must find out within ~1.5 s.
	eventually(t, "gs-1 fenced", func() bool { return a.Room("r1") == nil })
	for _, c := range []*conn{u1, u2} {
		types, closed := c.state()
		if closed != protocol.CloseRoomMoved || !slices.Contains(types, "room_moved") {
			t.Fatalf("%s on gs-1: close %d, messages %v", c.uid, closed, types)
		}
	}
	// gs-1's zombie writes are rejected (stale token).
	if err := store.Handle("r1", "gs-1", 1).Commit(ctx, []room.StoredEvent{{Seq: 99, Type: "chat", Data: []byte(`{}`)}}, nil, nil); !errors.Is(err, roomstore.ErrFenced) {
		t.Fatalf("zombie commit: err = %v", err)
	}

	// u1 resumes on gs-2 from what it saw on gs-1: no welcome, the room continues.
	back := &conn{uid: "u1"}
	if err := b.Room("r1").Join(ctx, back, "u1", 2); err != nil {
		t.Fatal(err)
	}
	eventually(t, "u1's own player_joined on gs-2", func() bool {
		types, _ := back.state()
		return slices.Contains(types, "event")
	})
	if types, _ := back.state(); slices.Contains(types, "welcome") {
		t.Fatalf("resume on the new owner sent a welcome: %v", types)
	}
	if rooms, _ := b.Status(); len(rooms) != 1 || rooms[0].Players != 1 {
		t.Fatalf("gs-2 status = %+v", rooms)
	}
}

// gs-1 lost r1 to gs-2, and the lobby gives it back to gs-1 before gs-1's lease keeper has
// noticed. The old copy must not count as "already running": gs-1 checks its lease, stops the
// old copy and takes the room again with a new token.
func TestAllocateRoomTakesBackARoomItLost(t *testing.T) {
	rdb := redistest.New(t)
	store := roomstore.New(rdb)
	a, b := storedServer(t, "gs-1", store), storedServer(t, "gs-2", store)
	ctx := t.Context()
	onA, onB := req("r1"), req("r1")
	onA.ServerId, onB.ServerId = "gs-1", "gs-2"

	if _, err := a.AllocateRoom(ctx, onA); err != nil {
		t.Fatal(err)
	}
	old := a.Room("r1")
	if _, err := b.AllocateRoom(ctx, onB); err != nil {
		t.Fatal(err)
	}
	// Right away, within gs-1's renew interval.
	if _, err := a.AllocateRoom(ctx, onA); err != nil {
		t.Fatal(err)
	}
	now := a.Room("r1")
	if now == nil || now == old || now.Token() != 3 {
		t.Fatalf("gs-1 answered with its old copy (token %d), want a new one with token 3", old.Token())
	}
	select {
	case <-old.Done():
	default:
		t.Fatal("the old copy is still running")
	}
	eventually(t, "gs-2 fenced", func() bool { return b.Room("r1") == nil })
}

func TestEndedRoomIsNotRestored(t *testing.T) {
	rdb := redistest.New(t)
	store := roomstore.New(rdb)
	a, b := storedServer(t, "gs-1", store), storedServer(t, "gs-2", store)
	if _, err := a.AllocateRoom(t.Context(), req("r1")); err != nil {
		t.Fatal(err)
	}
	if err := store.Handle("r1", "gs-1", 1).Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := b.AllocateRoom(t.Context(), req("r1")); status.Code(err) != codes.NotFound {
		t.Fatalf("err = %v, want NotFound", err)
	}
}
