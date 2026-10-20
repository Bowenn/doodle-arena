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

	"github.com/Bowenn/gap-project_private/libs/go/redistest"
	gamev1 "github.com/Bowenn/gap-project_private/proto/gen/go/doodle/game/v1"
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
	hosts   []string
	targets []string
	// misroute[addr] calls to addr reach the other server: it answers with another id, or
	// refuses when refuse is set (as servers with the server_id check do).
	misroute  map[string]int
	refuse    bool
	forgotten []string
	// delay makes every call take that long (honouring the deadline); inFlight and maxInFlight
	// count the calls out at once.
	delay       time.Duration
	inFlight    int
	maxInFlight int
	inFlightTo  map[string]int
	maxTo       int // the most calls out to any one address at once
}

func (f *fakeAllocator) Forget(addr string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.forgotten = append(f.forgotten, addr)
}

func (f *fakeAllocator) AllocateRoom(ctx context.Context, addr string, req *gamev1.AllocateRoomRequest) (*gamev1.AllocateRoomResponse, error) {
	f.mu.Lock()
	f.inFlight++
	f.maxInFlight = max(f.maxInFlight, f.inFlight)
	if f.inFlightTo == nil {
		f.inFlightTo = map[string]int{}
	}
	f.inFlightTo[addr]++
	f.maxTo = max(f.maxTo, f.inFlightTo[addr])
	delay := f.delay
	f.mu.Unlock()
	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-ctx.Done():
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.inFlight--
	f.inFlightTo[addr]--
	if ctx.Err() != nil {
		return nil, status.FromContextError(ctx.Err()).Err()
	}
	f.calls = append(f.calls, addr)
	f.hosts = append(f.hosts, req.GetHostUserId())
	f.targets = append(f.targets, req.GetServerId())
	if f.misroute[addr] > 0 {
		f.misroute[addr]--
		if f.refuse {
			return nil, status.Error(codes.FailedPrecondition, "this is another server")
		}
		return &gamev1.AllocateRoomResponse{ServerId: "gs-other"}, nil
	}
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
		alloc:  &fakeAllocator{answers: map[string]error{}, misroute: map[string]int{}},
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
	if !slices.Equal(e.alloc.hosts, []string{"u-host"}) {
		t.Fatalf("AllocateRoom hosts = %v, want the creator", e.alloc.hosts)
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
	// Three servers, each asked twice: a refusal could come from a stale connection, so the
	// lobby redials once before moving on.
	if !errors.Is(err, ErrNoServers) || len(slices.Compact(slices.Clone(e.alloc.calls))) != maxAttempts || len(e.alloc.calls) != 2*maxAttempts {
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
	j, err := e.svc.Join(t.Context(), User{ID: "u-guest", Name: "Grace"}, room.ID)
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
	if claims.Subject != "u-guest" || claims.Name != "Grace" || !claims.ExpiresAt.Equal(j.ExpiresAt) {
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

	if _, err := e.svc.Join(t.Context(), User{ID: "u-1"}, "no-such-room"); !errors.Is(err, ErrRoomNotFound) {
		t.Fatalf("unknown room: err = %v", err)
	}

	// The server stops reporting the room after the grace period: it is being moved (or found
	// ended) by the reassigner; until then it can't be joined.
	e.clock.add(reportGrace + time.Second)
	if _, err := e.svc.Join(t.Context(), User{ID: "u-1"}, room.ID); !errors.Is(err, ErrRoomUnavailable) {
		t.Fatalf("unreported room: err = %v", err)
	}

	// The server is gone: the room exists but can't be joined until failover moves it.
	orphan := Room{ID: "orphan", ServerID: "gs-dead", CreatedAt: e.clock.now()}
	if err := e.store.Save(t.Context(), orphan); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Join(t.Context(), User{ID: "u-1"}, orphan.ID); !errors.Is(err, ErrRoomUnavailable) {
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

func TestListHidesUnreportedRoomsAndDeadServers(t *testing.T) {
	e := newEnv(t)
	e.beat(t, "gs-1", 0, false)
	ended, err := e.svc.Create(t.Context(), "u-host", true, Settings{})
	if err != nil {
		t.Fatal(err)
	}
	e.clock.add(reportGrace + time.Second)
	// gs-1 is alive but no longer reports the room: hidden; the reassigner decides its fate.
	e.beat(t, "gs-1", 0, false)
	page, _, err := e.svc.List(t.Context(), nil, 10)
	if err != nil || len(page) != 0 {
		t.Fatalf("page = %+v, err = %v", page, err)
	}
	if _, err := e.store.Get(t.Context(), ended.ID); err != nil {
		t.Fatalf("listing removed the room; only the reassigner may: %v", err)
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
	ids := make([]string, 5)
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
		// 3 connected, but the 4th seat is held by a player who stepped away (ADR 0008).
		registry.Room{ID: ids[4], Players: 3, Seats: 4, Phase: registry.PhaseWaiting},
	)
	j, err := e.svc.QuickPlay(t.Context(), User{ID: "u-1"})
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
	j, err := e.svc.QuickPlay(t.Context(), User{ID: "u-1"})
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

// A server that heartbeats but doesn't answer is skipped for a while, so it doesn't add its
// deadline to every create.
func TestUnreachableServerIsTriedLast(t *testing.T) {
	e := newEnv(t)
	e.beat(t, "gs-1", 0, false) // least loaded, but unreachable
	e.beat(t, "gs-2", 5, false)
	e.alloc.answers["gs-1:9090"] = status.Error(codes.Unavailable, "connection refused")

	for range 3 {
		if _, err := e.svc.Create(t.Context(), "u-host", true, Settings{}); err != nil {
			t.Fatal(err)
		}
	}
	want := []string{"gs-1:9090", "gs-2:9090", "gs-2:9090", "gs-2:9090"}
	if !slices.Equal(e.alloc.calls, want) {
		t.Fatalf("calls = %v, want %v", e.alloc.calls, want)
	}

	// After the cooldown it gets another chance.
	e.clock.add(suspectFor + time.Second)
	e.alloc.calls = nil
	delete(e.alloc.answers, "gs-1:9090")
	room, err := e.svc.Create(t.Context(), "u-host", true, Settings{})
	if err != nil || room.ServerID != "gs-1" {
		t.Fatalf("room = %+v, err = %v", room, err)
	}
}

// After game servers swap IP addresses, the cached connection for gs-1 reaches gs-2. The
// room must never be recorded under the wrong server; the lobby redials and asks again.
func TestStaleConnectionIsRedialed(t *testing.T) {
	for _, refuse := range []bool{true, false} {
		name := "old server answers with its own id"
		if refuse {
			name = "server refuses a request meant for another"
		}
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			e.beat(t, "gs-1", 0, false)
			e.beat(t, "gs-2", 9, false)
			e.alloc.refuse = refuse
			e.alloc.misroute["gs-1:9090"] = 1

			room, err := e.svc.Create(t.Context(), "u-host", true, Settings{})
			if err != nil {
				t.Fatal(err)
			}
			if room.ServerID != "gs-1" {
				t.Fatalf("room on %s, want gs-1 after the redial", room.ServerID)
			}
			if !slices.Equal(e.alloc.calls, []string{"gs-1:9090", "gs-1:9090"}) || !slices.Equal(e.alloc.forgotten, []string{"gs-1:9090"}) {
				t.Fatalf("calls = %v, forgotten = %v", e.alloc.calls, e.alloc.forgotten)
			}
			if !slices.Equal(e.alloc.targets, []string{"gs-1", "gs-1"}) {
				t.Fatalf("server_id sent = %v", e.alloc.targets)
			}
		})
	}
}

// A draining server refuses twice; the lobby moves on to the next one.
func TestRefusedTwiceMovesOn(t *testing.T) {
	e := newEnv(t)
	e.beat(t, "gs-1", 0, false)
	e.beat(t, "gs-2", 9, false)
	e.alloc.answers["gs-1:9090"] = status.Error(codes.FailedPrecondition, "draining")
	room, err := e.svc.Create(t.Context(), "u-host", true, Settings{})
	if err != nil || room.ServerID != "gs-2" {
		t.Fatalf("room = %+v, err = %v", room, err)
	}
	if !slices.Equal(e.alloc.calls, []string{"gs-1:9090", "gs-1:9090", "gs-2:9090"}) {
		t.Fatalf("calls = %v", e.alloc.calls)
	}
}

func TestGRPCAllocatorForgetRedials(t *testing.T) {
	a := NewGRPCAllocator()
	defer a.Close()
	first, err := a.conn("gs-1:9090")
	if err != nil {
		t.Fatal(err)
	}
	a.Forget("gs-1:9090")
	second, err := a.conn("gs-1:9090")
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("Forget kept the cached connection")
	}
	a.Forget("unknown:1") // no-op
}

// roomsOn returns the index of a server's rooms, sorted.
func (e *env) roomsOn(t *testing.T, server string) []string {
	t.Helper()
	ids, err := e.store.RoomsOf(t.Context(), server)
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(ids)
	return ids
}

// Failover in the lobby: a server's heartbeats stop, and its rooms move to a live server.
func TestReassignMovesTheRoomsOfADeadServer(t *testing.T) {
	e := newEnv(t)
	e.beat(t, "gs-1", 0, false)
	var ids []string
	for range 3 {
		r, err := e.svc.Create(t.Context(), "u-host", true, Settings{MaxPlayers: 4})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, r.ID)
	}
	slices.Sort(ids)
	// gs-1 dies (its registry entry expires); gs-2 is up.
	if err := e.store.rdb.Del(t.Context(), "lobby:{registry}:server:gs-1").Err(); err != nil {
		t.Fatal(err)
	}
	e.beat(t, "gs-2", 0, false)
	e.alloc.calls, e.alloc.targets = nil, nil

	if err := e.svc.Reassign(t.Context()); err != nil {
		t.Fatal(err)
	}
	for _, id := range ids {
		r, err := e.store.Get(t.Context(), id)
		if err != nil || r.ServerID != "gs-2" || r.HostID != "u-host" || r.Settings.MaxPlayers != 4 {
			t.Fatalf("room %s after reassign: %+v, %v", id, r, err)
		}
	}
	if got := e.roomsOn(t, "gs-2"); !slices.Equal(got, ids) {
		t.Fatalf("index gs-2 = %v, want %v", got, ids)
	}
	if got := e.roomsOn(t, "gs-1"); len(got) != 0 {
		t.Fatalf("index gs-1 still has %v", got)
	}
	if !slices.Equal(e.alloc.targets, []string{"gs-2", "gs-2", "gs-2"}) {
		t.Fatalf("AllocateRoom targets = %v", e.alloc.targets)
	}
	// The next pass has nothing to do.
	e.alloc.calls = nil
	e.beat(t, "gs-2", 0, false, reported(ids)...)
	if err := e.svc.Reassign(t.Context()); err != nil || len(e.alloc.calls) != 0 {
		t.Fatalf("second pass: calls %v, err %v", e.alloc.calls, err)
	}
}

func reported(ids []string) []registry.Room {
	var out []registry.Room
	for _, id := range ids {
		out = append(out, registry.Room{ID: id, Phase: registry.PhaseWaiting})
	}
	return out
}

// A restarted server is alive but lost its rooms: once they're missing from its heartbeat
// past the grace, they are allocated again (possibly on the same server, which restores them).
func TestReassignRestoresRoomsAServerForgot(t *testing.T) {
	e := newEnv(t)
	e.beat(t, "gs-1", 0, false)
	young, err := e.svc.Create(t.Context(), "u-host", true, Settings{})
	if err != nil {
		t.Fatal(err)
	}
	e.alloc.calls = nil
	if err := e.svc.Reassign(t.Context()); err != nil || len(e.alloc.calls) != 0 {
		t.Fatalf("a young room was reassigned: %v %v", e.alloc.calls, err)
	}
	e.clock.add(reportGrace + time.Second)
	e.beat(t, "gs-1", 0, false) // restarted: reports nothing
	if err := e.svc.Reassign(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(e.alloc.calls, []string{"gs-1:9090"}) {
		t.Fatalf("calls = %v, want the room allocated again on gs-1", e.alloc.calls)
	}
	if r, _ := e.store.Get(t.Context(), young.ID); r.ServerID != "gs-1" {
		t.Fatalf("room = %+v", r)
	}
}

// A room that ended (the game server answers NOT_FOUND) is removed, with its index entry.
func TestReassignRemovesAnEndedRoom(t *testing.T) {
	e := newEnv(t)
	e.beat(t, "gs-1", 0, false)
	r, err := e.svc.Create(t.Context(), "u-host", true, Settings{})
	if err != nil {
		t.Fatal(err)
	}
	e.clock.add(reportGrace + time.Second)
	e.beat(t, "gs-1", 0, false)
	e.alloc.answers["gs-1:9090"] = status.Error(codes.NotFound, "room has ended")
	if err := e.svc.Reassign(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.Get(t.Context(), r.ID); !errors.Is(err, ErrRoomNotFound) {
		t.Fatalf("ended room still stored: %v", err)
	}
	if got := e.roomsOn(t, "gs-1"); len(got) != 0 {
		t.Fatalf("index still has %v", got)
	}
	if page, _, _ := e.store.PublicPage(t.Context(), nil, 10); len(page) != 0 {
		t.Fatal("ended room still listed")
	}
}

func TestOnlyOneLobbyHoldsTheReassignerLock(t *testing.T) {
	rdb := redistest.New(t)
	if !holdLock(t.Context(), rdb, "lobby-1") {
		t.Fatal("first lobby didn't get the lock")
	}
	if holdLock(t.Context(), rdb, "lobby-2") {
		t.Fatal("second lobby got the lock too")
	}
	if !holdLock(t.Context(), rdb, "lobby-1") {
		t.Fatal("the holder couldn't extend its lock")
	}
}

// A stale pass can't overwrite a newer placement, and a just-moved room gets its own grace.
func TestMoveIsCompareAndSetAndRestartsTheGrace(t *testing.T) {
	e := newEnv(t)
	e.beat(t, "gs-1", 0, false)
	r, err := e.svc.Create(t.Context(), "u-host", true, Settings{})
	if err != nil {
		t.Fatal(err)
	}
	moved := r
	moved.ServerID = "gs-2"
	if err := e.store.Move(t.Context(), moved, "gs-1"); err != nil {
		t.Fatal(err)
	}
	stale := r
	stale.ServerID = "gs-3"
	if err := e.store.Move(t.Context(), stale, "gs-1"); !errors.Is(err, ErrMovedMeanwhile) {
		t.Fatalf("stale move: err = %v", err)
	}
	if got, _ := e.store.Get(t.Context(), r.ID); got.ServerID != "gs-2" {
		t.Fatalf("room on %s after a stale move", got.ServerID)
	}

	// Moved onto gs-2 just now: gs-2 hasn't reported it yet, but it isn't moved again.
	e.clock.add(time.Hour)
	moved.PlacedAt = e.clock.now()
	if err := e.store.Move(t.Context(), moved, "gs-2"); err != nil {
		t.Fatal(err)
	}
	e.beat(t, "gs-2", 0, false)
	e.alloc.calls = nil
	if err := e.svc.Reassign(t.Context()); err != nil || len(e.alloc.calls) != 0 {
		t.Fatalf("a freshly placed room was reassigned: %v %v", e.alloc.calls, err)
	}
}

// deadServerWithRooms creates n rooms on gs-1 and lets gs-1 die.
func (e *env) deadServerWithRooms(t *testing.T, n int) []string {
	t.Helper()
	e.beat(t, "gs-1", 0, false)
	ids := make([]string, n)
	for i := range ids {
		r, err := e.svc.Create(t.Context(), "u-host", true, Settings{})
		if err != nil {
			t.Fatal(err)
		}
		ids[i] = r.ID
	}
	if err := e.store.rdb.Del(t.Context(), "lobby:{registry}:server:gs-1").Err(); err != nil {
		t.Fatal(err)
	}
	e.alloc.calls, e.alloc.targets, e.alloc.maxInFlight = nil, nil, 0
	return ids
}

// A dead server's rooms move in parallel, at most perServerReassigns at a time to each live
// server: with AllocateRoom taking 300 ms and four live servers, 40 rooms one at a time would
// need 12 s (three passes' start windows); the pass moves them all in about 1.5 s.
func TestReassignMovesManyRoomsAtOnce(t *testing.T) {
	e := newEnv(t)
	ids := e.deadServerWithRooms(t, 40)
	for _, id := range []string{"gs-2", "gs-3", "gs-4", "gs-5"} {
		e.beat(t, id, 0, false)
	}
	e.alloc.delay = 300 * time.Millisecond

	began := time.Now()
	if err := e.svc.Reassign(t.Context()); err != nil {
		t.Fatal(err)
	}
	took := time.Since(began)
	if left := e.roomsOn(t, "gs-1"); len(left) != 0 {
		t.Fatalf("one pass left %d of %d rooms", len(left), len(ids))
	}
	if e.alloc.maxInFlight != reassignWorkers || e.alloc.maxTo != perServerReassigns {
		t.Fatalf("%d calls out at once, %d to one server; want %d and %d",
			e.alloc.maxInFlight, e.alloc.maxTo, reassignWorkers, perServerReassigns)
	}
	if took > 3*time.Second {
		t.Fatalf("pass took %v: rooms moved one at a time", took)
	}
}

// The rooms of a pass spread by load instead of all going to the server that was least loaded
// when the pass began.
func TestReassignSpreadsRoomsOverTheLiveServers(t *testing.T) {
	e := newEnv(t)
	e.deadServerWithRooms(t, 12)
	e.beat(t, "gs-2", 0, false)
	e.beat(t, "gs-3", 0, false, reported([]string{"x1", "x2", "x3", "x4"})...)
	if err := e.svc.Reassign(t.Context()); err != nil {
		t.Fatal(err)
	}
	if n2, n3 := len(e.roomsOn(t, "gs-2")), len(e.roomsOn(t, "gs-3")); n2 != 8 || n3 != 4 {
		t.Fatalf("gs-2 got %d rooms, gs-3 %d; want 8 and 4 (8 each with gs-3's own)", n2, n3)
	}
}

// At the start deadline a pass starts no more rooms, but those already out finish and are
// recorded, and the servers they went to aren't suspected of anything.
func TestReassignStopsStartingRoomsAtTheDeadline(t *testing.T) {
	e := newEnv(t)
	ids := e.deadServerWithRooms(t, 10)
	e.beat(t, "gs-2", 0, false)
	e.alloc.delay = 300 * time.Millisecond

	if err := e.svc.reassignPass(t.Context(), time.Now().Add(100*time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if moved, left := len(e.roomsOn(t, "gs-2")), len(e.roomsOn(t, "gs-1")); moved != perServerReassigns || left != 10-perServerReassigns {
		t.Fatalf("moved %d, left %d; want the first %d moved and the rest for the next pass", moved, left, perServerReassigns)
	}
	if got := e.svc.withoutSuspects(registry.Candidates([]registry.Server{{ID: "gs-2"}, {ID: "gs-3"}})); got[0].ID != "gs-2" {
		t.Fatal("gs-2 was marked suspect")
	}
	if err := e.svc.Reassign(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := e.roomsOn(t, "gs-2"); len(got) != len(ids) {
		t.Fatalf("after the next pass gs-2 has %d of %d rooms", len(got), len(ids))
	}
}

// Without the reassigner lock, a pass starts nothing more.
func TestReassignStopsWhenTheLockIsLost(t *testing.T) {
	e := newEnv(t)
	e.deadServerWithRooms(t, 5)
	e.beat(t, "gs-2", 0, false)
	e.svc.lockHolder = func(context.Context) bool { return false }
	if err := e.svc.Reassign(t.Context()); !errors.Is(err, errLockLost) {
		t.Fatalf("err = %v, want errLockLost", err)
	}
	if len(e.alloc.calls) != 0 || len(e.roomsOn(t, "gs-1")) != 5 {
		t.Fatalf("calls %v, rooms left on gs-1 %v", e.alloc.calls, e.roomsOn(t, "gs-1"))
	}
}

// reassignments returns the lobby.reassignments counter by "result/reason".
func (e *env) reassignments(t *testing.T) map[string]int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := e.reader.Collect(t.Context(), &rm); err != nil {
		t.Fatal(err)
	}
	out := map[string]int64{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "lobby.reassignments" {
				continue
			}
			for _, dp := range m.Data.(metricdata.Sum[int64]).DataPoints {
				result, _ := dp.Attributes.Value(attribute.Key("result"))
				reason, _ := dp.Attributes.Value(attribute.Key("reason"))
				out[result.AsString()+"/"+reason.AsString()] += dp.Value
			}
		}
	}
	return out
}

// A draining server (task 5.5: it is shutting down and waits for its rooms to leave) is alive
// and still reports its rooms, but every one of them moves at once, to the other servers only.
func TestReassignMovesEveryRoomOfADrainingServer(t *testing.T) {
	e := newEnv(t)
	e.beat(t, "gs-1", 0, false)
	var ids []string
	for range 3 {
		r, err := e.svc.Create(t.Context(), "u-host", true, Settings{MaxPlayers: 4})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, r.ID)
	}
	slices.Sort(ids)
	// gs-1 drains: its heartbeat still reports the rooms (they are young, inside the grace).
	e.beat(t, "gs-1", 3, true, reported(ids)...)
	e.beat(t, "gs-2", 0, false)
	e.alloc.calls, e.alloc.targets = nil, nil

	if err := e.svc.Reassign(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(e.alloc.targets, []string{"gs-2", "gs-2", "gs-2"}) {
		t.Fatalf("AllocateRoom targets = %v, want all three on gs-2", e.alloc.targets)
	}
	for _, id := range ids {
		if r, err := e.store.Get(t.Context(), id); err != nil || r.ServerID != "gs-2" {
			t.Fatalf("room %s after the drain pass: %+v, %v", id, r, err)
		}
	}
	if got := e.reassignments(t); got["moved/drain"] != 3 || got["moved/failover"] != 0 {
		t.Fatalf("lobby.reassignments = %v, want 3 moved/drain", got)
	}
	// The draining server still reports the rooms until its copies stop; nothing moves twice.
	e.alloc.calls = nil
	e.beat(t, "gs-1", 3, true, reported(ids)...)
	e.beat(t, "gs-2", 0, false, reported(ids)...)
	if err := e.svc.Reassign(t.Context()); err != nil || len(e.alloc.calls) != 0 {
		t.Fatalf("second pass: calls %v, err %v", e.alloc.calls, err)
	}
}

// With no other server, a draining server's rooms stay where they are (counted as failed and
// retried); the game server closes them at its drain deadline and they fail over later.
func TestDrainWithNowhereToGoKeepsTheRooms(t *testing.T) {
	e := newEnv(t)
	e.beat(t, "gs-1", 0, false)
	r, err := e.svc.Create(t.Context(), "u-host", true, Settings{})
	if err != nil {
		t.Fatal(err)
	}
	e.beat(t, "gs-1", 1, true, reported([]string{r.ID})...)
	e.alloc.calls = nil
	if err := e.svc.Reassign(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(e.alloc.calls) != 0 {
		t.Fatalf("AllocateRoom calls = %v, want none (a draining server is never a target)", e.alloc.calls)
	}
	if got, _ := e.store.Get(t.Context(), r.ID); got.ServerID != "gs-1" {
		t.Fatalf("room = %+v, want it still on gs-1", got)
	}
	if got := e.reassignments(t); got["failed/drain"] != 1 {
		t.Fatalf("lobby.reassignments = %v, want 1 failed/drain", got)
	}
}
