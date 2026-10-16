package rooms

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"io"
	"log/slog"
	"net"
	"slices"
	"sync"
	"testing"
	"time"

	gamev1 "github.com/Bowenn/gap-project_private/proto/gen/go/doodle/game/v1"
	"github.com/Bowenn/gap-project_private/services/lobby/internal/redistest"
	"github.com/Bowenn/gap-project_private/services/lobby/internal/registry"
	"github.com/Bowenn/gap-project_private/services/lobby/internal/ticket"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestMain(m *testing.M) { redistest.Main(m) }

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

// fakeAllocator answers AllocateRoom per control address and records the calls.
type fakeAllocator struct {
	mu      sync.Mutex
	answers map[string]error
	calls   []string
}

func (f *fakeAllocator) AllocateRoom(_ context.Context, addr string, _ *gamev1.AllocateRoomRequest) (*gamev1.AllocateRoomResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, addr)
	if err := f.answers[addr]; err != nil {
		return nil, err
	}
	return &gamev1.AllocateRoomResponse{ServerId: addr[:len(addr)-len(":9090")]}, nil
}

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time      { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }

type env struct {
	svc      *Service
	reg      *registry.Registry
	store    *Store
	alloc    *fakeAllocator
	clock    *clock
	verifier *ticket.Verifier
	reader   *sdkmetric.ManualReader
}

func newEnv(t *testing.T) *env {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))
	rdb := redistest.New(t)
	e := &env{
		reg:    registry.New(rdb, time.Minute, quiet),
		store:  NewStore(rdb),
		alloc:  &fakeAllocator{answers: map[string]error{}},
		clock:  &clock{t: time.Now()},
		reader: reader,
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	e.verifier = ticket.NewVerifier([]ed25519.PublicKey{pub}, e.clock.now)
	e.svc, err = NewService(e.store, e.reg, e.alloc, ticket.NewSigner(priv, e.clock.now), e.clock.now, quiet)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// beat records a heartbeat for a game server hosting the given rooms.
func (e *env) beat(t *testing.T, id string, sockets uint32, draining bool, rooms ...registry.Room) {
	t.Helper()
	_, err := e.reg.Record(t.Context(), registry.Server{
		ID: id, WSAddress: "/ws/" + id, ControlAddress: id + ":9090", Sockets: sockets, Draining: draining, Rooms: rooms,
	})
	if err != nil {
		t.Fatal(err)
	}
}

// allocations returns the lobby.allocations counter by result.
func (e *env) allocations(t *testing.T) map[string]int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := e.reader.Collect(t.Context(), &rm); err != nil {
		t.Fatal(err)
	}
	out := map[string]int64{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "lobby.allocations" {
				continue
			}
			for _, dp := range m.Data.(metricdata.Sum[int64]).DataPoints {
				v, _ := dp.Attributes.Value(attribute.Key("result"))
				out[v.AsString()] += dp.Value
			}
		}
	}
	return out
}

func TestCreatePicksTheLeastLoadedServer(t *testing.T) {
	e := newEnv(t)
	e.beat(t, "gs-1", 10, false)
	e.beat(t, "gs-2", 3, false)
	e.beat(t, "gs-3", 0, true) // draining: never chosen

	room, err := e.svc.Create(t.Context(), "u-host", true, Settings{})
	if err != nil {
		t.Fatal(err)
	}
	if room.ServerID != "gs-2" || room.HostID != "u-host" || room.Settings != DefaultSettings || len(room.ID) != 16 {
		t.Fatalf("room = %+v", room)
	}
	stored, err := e.store.Get(t.Context(), room.ID)
	if err != nil || stored.ServerID != "gs-2" {
		t.Fatalf("stored = %+v, err = %v", stored, err)
	}
	if got := e.allocations(t); got["success"] != 1 {
		t.Fatalf("allocations = %v", got)
	}
}

func TestCreateFallsBackWhenAServerIsFullOrDraining(t *testing.T) {
	e := newEnv(t)
	e.beat(t, "gs-1", 0, false)
	e.beat(t, "gs-2", 1, false)
	e.alloc.answers["gs-1:9090"] = status.Error(codes.ResourceExhausted, "full")

	room, err := e.svc.Create(t.Context(), "u-host", false, Settings{})
	if err != nil {
		t.Fatal(err)
	}
	if room.ServerID != "gs-2" || !slices.Equal(e.alloc.calls, []string{"gs-1:9090", "gs-2:9090"}) {
		t.Fatalf("server = %s, calls = %v", room.ServerID, e.alloc.calls)
	}
}

func TestCreateGivesUpAfterThreeServers(t *testing.T) {
	e := newEnv(t)
	for _, id := range []string{"gs-1", "gs-2", "gs-3", "gs-4"} {
		e.beat(t, id, 0, false)
		e.alloc.answers[id+":9090"] = status.Error(codes.FailedPrecondition, "draining")
	}
	_, err := e.svc.Create(t.Context(), "u-host", true, Settings{})
	if !errors.Is(err, ErrNoServers) || len(e.alloc.calls) != maxAttempts {
		t.Fatalf("err = %v, calls = %v", err, e.alloc.calls)
	}
	if got := e.allocations(t); got["exhausted"] != 1 {
		t.Fatalf("allocations = %v", got)
	}
	// Nothing was stored for the failed room.
	if page, _, err := e.store.PublicPage(t.Context(), nil, 10); err != nil || len(page) != 0 {
		t.Fatalf("public rooms = %v, err = %v", page, err)
	}
}

func TestCreateWithoutServers(t *testing.T) {
	e := newEnv(t)
	if _, err := e.svc.Create(t.Context(), "u-host", true, Settings{}); !errors.Is(err, ErrNoServers) {
		t.Fatalf("err = %v", err)
	}
	if got := e.allocations(t); got["no_servers"] != 1 {
		t.Fatalf("allocations = %v", got)
	}
}

// slowGame is a game server whose AllocateRoom never answers in time.
type slowGame struct {
	gamev1.UnimplementedControlServiceServer
}

func (slowGame) AllocateRoom(ctx context.Context, _ *gamev1.AllocateRoomRequest) (*gamev1.AllocateRoomResponse, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

// The real gRPC client honours the per-call deadline, so a hung game server costs the user
// at most allocateTimeout per attempt.
func TestAllocateRoomDeadlineOverGRPC(t *testing.T) {
	e := newEnv(t)
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	gs := grpc.NewServer()
	gamev1.RegisterControlServiceServer(gs, slowGame{})
	go func() { _ = gs.Serve(lis) }()
	defer gs.Stop()

	alloc := NewGRPCAllocator()
	defer alloc.Close()
	e.svc.alloc = alloc
	_, err = e.reg.Record(t.Context(), registry.Server{ID: "gs-slow", WSAddress: "/ws/gs-slow", ControlAddress: lis.Addr().String()})
	if err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	_, err = e.svc.Create(t.Context(), "u-host", true, Settings{})
	if !errors.Is(err, ErrNoServers) {
		t.Fatalf("err = %v", err)
	}
	if took := time.Since(start); took < allocateTimeout || took > allocateTimeout+time.Second {
		t.Fatalf("took %v, want about %v", took, allocateTimeout)
	}
	if got := e.allocations(t); got["error"] != 1 {
		t.Fatalf("allocations = %v", got)
	}
}

func TestJoinReturnsATicketForThatUserAndRoom(t *testing.T) {
	e := newEnv(t)
	e.beat(t, "gs-1", 0, false)
	room, err := e.svc.Create(t.Context(), "u-host", false, Settings{})
	if err != nil {
		t.Fatal(err)
	}
	j, err := e.svc.Join(t.Context(), "u-guest", room.ID)
	if err != nil {
		t.Fatal(err)
	}
	if j.WSURL != "/ws/gs-1/rooms/"+room.ID || j.RoomID != room.ID {
		t.Fatalf("join = %+v", j)
	}
	claims, err := e.verifier.Verify(j.Ticket, room.ID)
	if err != nil {
		t.Fatal(err)
	}
	if claims.Subject != "u-guest" || !claims.ExpiresAt.Equal(j.ExpiresAt) {
		t.Fatalf("claims = %+v", claims)
	}
	if _, err := e.verifier.Verify(j.Ticket, "another-room"); err == nil {
		t.Fatal("ticket opened another room")
	}
}

func TestJoinErrors(t *testing.T) {
	e := newEnv(t)
	e.beat(t, "gs-1", 0, false)
	room, err := e.svc.Create(t.Context(), "u-host", true, Settings{})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := e.svc.Join(t.Context(), "u-1", "no-such-room"); !errors.Is(err, ErrRoomNotFound) {
		t.Fatalf("unknown room: err = %v", err)
	}

	// The server stops reporting the room after the grace period: the room has ended.
	e.clock.add(reportGrace + time.Second)
	if _, err := e.svc.Join(t.Context(), "u-1", room.ID); !errors.Is(err, ErrRoomNotFound) {
		t.Fatalf("ended room: err = %v", err)
	}

	// The server is gone: the room exists but can't be joined until failover moves it.
	orphan := Room{ID: "orphan", ServerID: "gs-dead", CreatedAt: e.clock.now()}
	if err := e.store.Save(t.Context(), orphan); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Join(t.Context(), "u-1", orphan.ID); !errors.Is(err, ErrRoomUnavailable) {
		t.Fatalf("dead server: err = %v", err)
	}
}

func TestListShowsPublicRoomsWithLiveCounts(t *testing.T) {
	e := newEnv(t)
	e.beat(t, "gs-1", 0, false)
	var public []string
	for range 5 {
		r, err := e.svc.Create(t.Context(), "u-host", true, Settings{})
		if err != nil {
			t.Fatal(err)
		}
		public = append(public, r.ID)
		e.clock.add(time.Millisecond)
	}
	private, err := e.svc.Create(t.Context(), "u-host", false, Settings{})
	if err != nil {
		t.Fatal(err)
	}
	reported := []registry.Room{{ID: private.ID, Players: 1}}
	for i, id := range public {
		reported = append(reported, registry.Room{ID: id, Players: uint32(i), Phase: registry.PhaseWaiting})
	}
	e.beat(t, "gs-1", 10, false, reported...)

	var got []string
	players := map[string]uint32{}
	var cursor *Cursor
	for {
		page, next, err := e.svc.List(t.Context(), cursor, 2)
		if err != nil {
			t.Fatal(err)
		}
		for _, l := range page {
			got = append(got, l.ID)
			players[l.ID] = l.Players
		}
		if next == nil {
			break
		}
		cursor = next
	}
	slices.Reverse(public) // newest first
	if !slices.Equal(got, public) {
		t.Fatalf("listed %v, want %v (no private room, no duplicates)", got, public)
	}
	if players[public[0]] != 4 {
		t.Fatalf("players = %v", players)
	}
}

// Rooms created in the same millisecond still page correctly.
func TestPublicPageWithTiedScores(t *testing.T) {
	e := newEnv(t)
	at := time.UnixMilli(1_800_000_000_000)
	var want []string
	for _, id := range []string{"aa", "bb", "cc", "dd", "ee"} {
		if err := e.store.Save(t.Context(), Room{ID: id, Public: true, CreatedAt: at}); err != nil {
			t.Fatal(err)
		}
		want = append(want, id)
	}
	slices.Reverse(want)
	var got []string
	var cursor *Cursor
	for {
		page, next, err := e.store.PublicPage(t.Context(), cursor, 2)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range page {
			got = append(got, r.ID)
		}
		if next == nil {
			break
		}
		parsed, err := ParseCursor(next.String())
		if err != nil {
			t.Fatal(err)
		}
		cursor = &parsed
	}
	if !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestListDropsEndedRoomsAndHidesDeadServers(t *testing.T) {
	e := newEnv(t)
	e.beat(t, "gs-1", 0, false)
	ended, err := e.svc.Create(t.Context(), "u-host", true, Settings{})
	if err != nil {
		t.Fatal(err)
	}
	e.clock.add(reportGrace + time.Second)
	// gs-1 is alive but no longer reports the room: it has ended.
	e.beat(t, "gs-1", 0, false)
	page, _, err := e.svc.List(t.Context(), nil, 10)
	if err != nil || len(page) != 0 {
		t.Fatalf("page = %+v, err = %v", page, err)
	}
	if _, err := e.store.Get(t.Context(), ended.ID); !errors.Is(err, ErrRoomNotFound) {
		t.Fatalf("ended room still stored: %v", err)
	}

	// A room whose server is gone is hidden, but kept for failover.
	orphan := Room{ID: "orphan", Public: true, ServerID: "gs-dead", CreatedAt: e.clock.now()}
	if err := e.store.Save(t.Context(), orphan); err != nil {
		t.Fatal(err)
	}
	page, _, err = e.svc.List(t.Context(), nil, 10)
	if err != nil || len(page) != 0 {
		t.Fatalf("page = %+v, err = %v", page, err)
	}
	if _, err := e.store.Get(t.Context(), "orphan"); err != nil {
		t.Fatalf("orphan removed: %v", err)
	}
}

func TestQuickPlayJoinsTheFullestWaitingRoomWithSpace(t *testing.T) {
	e := newEnv(t)
	e.beat(t, "gs-1", 0, false)
	ids := make([]string, 4)
	for i := range ids {
		r, err := e.svc.Create(t.Context(), "u-host", true, Settings{MaxPlayers: 4})
		if err != nil {
			t.Fatal(err)
		}
		ids[i] = r.ID
	}
	e.beat(t, "gs-1", 0, false,
		registry.Room{ID: ids[0], Players: 1, Phase: registry.PhaseWaiting},
		registry.Room{ID: ids[1], Players: 4, Phase: registry.PhaseWaiting}, // full
		registry.Room{ID: ids[2], Players: 3, Phase: registry.PhasePlaying}, // match running
		registry.Room{ID: ids[3], Players: 2, Phase: registry.PhaseWaiting}, // the one
	)
	j, err := e.svc.QuickPlay(t.Context(), "u-1")
	if err != nil {
		t.Fatal(err)
	}
	if j.RoomID != ids[3] {
		t.Fatalf("joined %s, want %s", j.RoomID, ids[3])
	}
}

func TestQuickPlayCreatesARoomWhenNoneHasSpace(t *testing.T) {
	e := newEnv(t)
	e.beat(t, "gs-1", 0, false)
	j, err := e.svc.QuickPlay(t.Context(), "u-1")
	if err != nil {
		t.Fatal(err)
	}
	room, err := e.store.Get(t.Context(), j.RoomID)
	if err != nil {
		t.Fatal(err)
	}
	if !room.Public || room.HostID != "u-1" {
		t.Fatalf("room = %+v", room)
	}
}

func TestSettingsResolve(t *testing.T) {
	tests := []struct {
		name string
		in   Settings
		want Settings
		ok   bool
	}{
		{"defaults", Settings{}, DefaultSettings, true},
		{"custom", Settings{MaxPlayers: 4, DrawSeconds: 60}, Settings{4, 10, 60, 3}, true},
		{"one player", Settings{MaxPlayers: 1}, Settings{}, false},
		{"too many players", Settings{MaxPlayers: 9}, Settings{}, false},
		{"draw too long", Settings{DrawSeconds: 600}, Settings{}, false},
		{"too many words", Settings{WordOptions: 6}, Settings{}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.in.Resolve()
			if (err == nil) != tt.ok || got != tt.want {
				t.Fatalf("got %+v, err %v", got, err)
			}
			if err != nil && !errors.Is(err, ErrInvalidSettings) {
				t.Fatalf("err = %v, want ErrInvalidSettings", err)
			}
		})
	}
}

// A heartbeat keeps a room alive only if it comes from the room's own server.
func TestTouchOnlyRefreshesTheOwnersRooms(t *testing.T) {
	e := newEnv(t)
	rdb := e.store.rdb
	ctx := t.Context()
	if err := e.store.Save(ctx, Room{ID: "r1", ServerID: "gs-1", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	rdb.Expire(ctx, roomKey("r1"), 5*time.Second)

	if err := e.store.Touch(ctx, "gs-2", []string{"r1", "unknown"}); err != nil {
		t.Fatal(err)
	}
	if ttl := rdb.TTL(ctx, roomKey("r1")).Val(); ttl > 5*time.Second {
		t.Fatalf("another server refreshed the room: ttl = %v", ttl)
	}
	if err := e.store.Touch(ctx, "gs-1", []string{"r1"}); err != nil {
		t.Fatal(err)
	}
	if ttl := rdb.TTL(ctx, roomKey("r1")).Val(); ttl < RoomTTL-time.Minute {
		t.Fatalf("the owner didn't refresh the room: ttl = %v", ttl)
	}
	if rdb.Exists(ctx, roomKey("unknown")).Val() != 0 {
		t.Fatal("touch created a room")
	}
}
