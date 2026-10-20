// Package control serves doodle.game.v1.ControlService, which the lobby calls to place rooms
// on this server, and keeps the server's table of rooms.
package control

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"sync"
	"time"

	gamev1 "github.com/Bowenn/gap-project_private/proto/gen/go/doodle/game/v1"
	"github.com/Bowenn/gap-project_private/services/game-server/internal/room"
	"github.com/Bowenn/gap-project_private/services/game-server/internal/roomstore"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Room is what this server reports about one of its rooms.
type Room struct {
	ID      string
	Players uint32 // connected
	Seats   uint32 // held, connected or not
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
	events    room.Sink
	log       *slog.Logger
	store     *roomstore.Store // nil: rooms live only in memory (tests)
	requeuer  Requeuer         // nil: outbox entries aren't queued again
	scanEvery time.Duration
	staleAge  time.Duration
	ctx       context.Context // rooms run until it ends
	wg        sync.WaitGroup

	mu       sync.Mutex
	rooms    map[string]*entry
	starting map[string]chan struct{} // ids an AllocateRoom is starting; closed when it is done
	draining bool

	afterReserve func(roomID string) // tests only: runs once a call has claimed the room id
}

// entry is a running room and, with a store, its lease.
type entry struct {
	room   *room.Room
	handle *roomstore.Handle // nil without a store
}

// Requeuer queues outbox entries for publishing again (events.Publisher).
type Requeuer interface {
	Requeue(entries []room.OutboxEntry) int
}

// Outbox scan defaults (ADR 0009): an entry still there after OutboxStaleAfter failed or was
// dropped, and is queued again.
const (
	OutboxScanEvery  = 30 * time.Second
	OutboxStaleAfter = time.Minute
)

// Config creates a Server.
type Config struct {
	ID        string
	WSAddress string
	MaxRooms  int
	Clock     room.Clock
	Metrics   *room.Metrics
	Events    room.Sink // finished rounds and matches; nil: discarded
	Store     *roomstore.Store
	Log       *slog.Logger
	// Outbox queues what a room's outbox still holds: a previous owner's entries when this
	// server takes a room, and this server's own stale ones every OutboxScan.
	Outbox     Requeuer
	OutboxScan time.Duration // 0: OutboxScanEvery
	StaleAfter time.Duration // 0: OutboxStaleAfter
}

// New returns a control server. Rooms run until ctx ends; Wait waits for them. With a store,
// it also keeps every room's lease alive until ctx ends.
func New(ctx context.Context, cfg Config) *Server {
	if cfg.Clock == nil {
		cfg.Clock = room.RealClock
	}
	s := &Server{id: cfg.ID, wsAddress: cfg.WSAddress, maxRooms: cfg.MaxRooms, clock: cfg.Clock,
		metrics: cfg.Metrics, events: cfg.Events, store: cfg.Store, log: cfg.Log, ctx: ctx, rooms: map[string]*entry{}, starting: map[string]chan struct{}{},
		requeuer: cfg.Outbox, scanEvery: cmp.Or(cfg.OutboxScan, OutboxScanEvery), staleAge: cmp.Or(cfg.StaleAfter, OutboxStaleAfter)}
	if s.store != nil {
		s.wg.Go(s.keepLeases)
		if s.requeuer != nil {
			s.wg.Go(s.scanOutboxes)
		}
	}
	return s
}

var roomIDPattern = regexp.MustCompile(`^[a-z0-9]{1,64}$`)

// AllocateRoom starts a room. It is idempotent: the lobby retries on timeouts, so asking again
// for a room this server already runs succeeds without creating a second one.
func (s *Server) AllocateRoom(ctx context.Context, req *gamev1.AllocateRoomRequest) (*gamev1.AllocateRoomResponse, error) {
	// A request meant for another instance arrives when the lobby's cached connection points
	// at an IP address that now belongs to this server. Create nothing; the lobby redials.
	if target := req.GetServerId(); target != "" && target != s.id {
		s.log.WarnContext(ctx, "allocation meant for another server refused", "room_id", req.GetRoomId(),
			"server_id", s.id, "target_server_id", target)
		return nil, status.Errorf(codes.FailedPrecondition, "this is %s, not %s", s.id, target)
	}
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

	id := req.GetRoomId()
	started, running, err := s.reserve(ctx, id)
	if err != nil {
		return nil, err
	}
	if running {
		return resp, nil
	}
	// Installed or not, the id stops being "starting" only after the table says which: a call
	// waiting for this one then finds the room, or starts it itself.
	defer func() {
		s.mu.Lock()
		delete(s.starting, id)
		s.mu.Unlock()
		close(started)
	}()
	if s.afterReserve != nil {
		s.afterReserve(id)
	}
	cfg := room.Config{
		ID: req.GetRoomId(), Mode: "classic", HostID: req.GetHostUserId(), Settings: settings,
		Clock: s.clock, Log: s.log, Metrics: s.metrics, Events: s.events,
	}
	e := &entry{}
	var outbox []room.OutboxEntry
	if s.store != nil {
		// Take the room (by force, if another server still holds it: ADR 0007), then read it.
		actx, cancel := context.WithTimeout(ctx, 2*time.Second)
		owned, err := s.store.Acquire(actx, req.GetRoomId(), s.id)
		cancel()
		switch {
		case errors.Is(err, roomstore.ErrClosed):
			return nil, status.Error(codes.NotFound, "room has ended")
		case err != nil:
			s.log.ErrorContext(ctx, "room not acquired", "room_id", req.GetRoomId(), "err", err)
			return nil, status.Error(codes.Unavailable, "room store unavailable")
		}
		e.handle = s.store.Handle(req.GetRoomId(), s.id, owned.Token)
		outbox = owned.Outbox
		cfg.Durable, cfg.Token = e.handle, owned.Token
		if owned.Snapshot != nil {
			cfg.Restore = &room.Restored{Snapshot: *owned.Snapshot, Events: owned.Events}
		}
		if cfg.Restore != nil {
			_, span := otel.Tracer("game-server").Start(ctx, "game.room.takeover", trace.WithAttributes(
				attribute.String("room.id", req.GetRoomId()), attribute.Int64("fencing.token", owned.Token)))
			span.End()
			s.metrics.Takeover(ctx)
			s.log.InfoContext(ctx, "room taken over", "room_id", req.GetRoomId(), "server_id", s.id,
				"token", owned.Token, "seq", owned.Snapshot.Seq, "events_loaded", len(owned.Events))
		}
	}
	r, err := room.New(cfg)
	if err != nil {
		// The stored log doesn't fit its snapshot; never guess: a human looks at it.
		s.log.ErrorContext(ctx, "room not restored", "room_id", req.GetRoomId(), "err", err)
		return nil, status.Error(codes.Internal, "room state can't be restored")
	}
	e.room = r
	s.mu.Lock()
	s.rooms[r.ID()] = e
	s.wg.Go(func() {
		r.Run(s.ctx)
		s.mu.Lock()
		// A new room with the same id may already have replaced this one.
		if cur := s.rooms[r.ID()]; cur != nil && cur.room == r {
			delete(s.rooms, r.ID())
		}
		s.mu.Unlock()
	})
	s.mu.Unlock()
	if cfg.Restore == nil {
		s.log.InfoContext(ctx, "room allocated", "room_id", r.ID(), "server_id", s.id, "user_id", req.GetHostUserId())
	}
	if len(outbox) > 0 && s.requeuer != nil {
		// What the previous owner committed but never got into Kafka (ADR 0009).
		n := s.requeuer.Requeue(outbox)
		s.metrics.OutboxRequeued(ctx, "takeover", n)
		s.log.InfoContext(ctx, "outbox entries left by the previous owner queued", "room_id", r.ID(), "entries", n)
	}
	return resp, nil
}

// reserve claims a room id for one AllocateRoom call to start, or reports that this server
// already runs the room. s.mu guards only the table, never a Redis call or a restore, so the
// lobby can move many rooms here at once after a failover; a second call for a room that is
// still starting waits for the first. A starting room counts toward maxRooms.
func (s *Server) reserve(ctx context.Context, id string) (started chan struct{}, running bool, err error) {
	for {
		s.mu.Lock()
		if wait, ok := s.starting[id]; ok {
			s.mu.Unlock()
			select {
			case <-wait:
				continue
			case <-ctx.Done():
				return nil, false, status.Error(codes.Unavailable, "the room is still starting")
			}
		}
		if e, ok := s.rooms[id]; ok {
			s.mu.Unlock()
			if s.stillOwns(ctx, e) {
				return nil, true, nil
			}
			// Another server took the room (ADR 0007) and the lobby is now giving it back. The old
			// copy here must stop before a new one takes the lease, or OK would point the lobby at
			// a room that is about to close.
			if !s.awaitStopped(ctx, e) {
				return nil, false, status.Error(codes.Unavailable, "the room's previous copy is still stopping")
			}
			s.mu.Lock()
			if s.rooms[id] == e {
				delete(s.rooms, id)
			}
			s.mu.Unlock()
			continue // look again: a concurrent call may have taken it back meanwhile
		}
		switch {
		case s.draining:
			s.mu.Unlock()
			return nil, false, status.Error(codes.FailedPrecondition, "server is draining")
		case len(s.rooms)+len(s.starting) >= s.maxRooms:
			s.mu.Unlock()
			return nil, false, status.Error(codes.ResourceExhausted, "server is full")
		}
		started = make(chan struct{})
		s.starting[id] = started
		s.mu.Unlock()
		return started, false, nil
	}
}

// scanOutboxes queues again, every scanEvery, the outbox entries of this server's rooms that
// are older than staleAge: their publish failed, timed out or was dropped (ADR 0009). Entries
// still queued or in flight are skipped by the requeuer.
func (s *Server) scanOutboxes() {
	t := time.NewTicker(s.scanEvery)
	defer t.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-t.C:
		}
		s.mu.Lock()
		ids := make([]string, 0, len(s.rooms))
		for id, e := range s.rooms {
			if e.handle != nil {
				ids = append(ids, id)
			}
		}
		s.mu.Unlock()
		ctx, cancel := context.WithTimeout(s.ctx, 5*time.Second)
		pending, err := s.store.Pending(ctx, ids, s.clock.Now().Add(-s.staleAge))
		cancel()
		if err != nil {
			s.log.Warn("outboxes not scanned", "rooms", len(ids), "err", err)
			continue
		}
		stale, queued := 0, 0
		for id, entries := range pending {
			stale += len(entries)
			n := s.requeuer.Requeue(entries)
			queued += n
			s.log.Warn("stale outbox entries queued again", "room_id", id, "entries", len(entries), "queued", n)
		}
		s.metrics.OutboxPending(s.ctx, stale)
		s.metrics.OutboxRequeued(s.ctx, "stale", queued)
	}
}

// keepLeases renews every room's lease every RenewEvery in one pipeline, and stops rooms whose
// lease another server has taken (their writes would fail anyway; this catches idle rooms).
func (s *Server) keepLeases() {
	t := time.NewTicker(roomstore.RenewEvery)
	defer t.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-t.C:
		}
		s.mu.Lock()
		leases := make([]roomstore.Lease, 0, len(s.rooms))
		byID := make(map[string]*room.Room, len(s.rooms))
		for id, e := range s.rooms {
			if e.handle != nil {
				leases = append(leases, e.handle.Lease())
				byID[id] = e.room
			}
		}
		s.mu.Unlock()
		ctx, cancel := context.WithTimeout(s.ctx, time.Second)
		lost, err := s.store.Renew(ctx, s.id, leases)
		cancel()
		if err != nil {
			s.log.Warn("leases not renewed", "rooms", len(leases), "err", err)
			continue
		}
		for _, id := range lost {
			s.log.Warn("lease lost: another server took the room", "room_id", id, "server_id", s.id)
			byID[id].Fence()
		}
	}
}

// stillOwns reports whether this server's copy of a room still holds its lease. The lease keeper
// finds a lost lease within RenewEvery; this checks now, so the answer is never stale.
func (s *Server) stillOwns(ctx context.Context, e *entry) bool {
	select {
	case <-e.room.Done():
		return false
	default:
	}
	if e.room.Fenced() {
		return false
	}
	if s.store == nil || e.handle == nil {
		return true
	}
	rctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	lost, err := s.store.Renew(rctx, s.id, []roomstore.Lease{e.handle.Lease()})
	if err != nil {
		// Can't tell; the room's next write would find out. Keep the old answer.
		s.log.WarnContext(ctx, "lease not checked", "room_id", e.room.ID(), "err", err)
		return true
	}
	return len(lost) == 0
}

// awaitStopped fences a room that lost its lease and waits until its actor has stopped (at
// most stopWait).
func (s *Server) awaitStopped(ctx context.Context, e *entry) bool {
	e.room.Fence()
	timer := time.NewTimer(stopWait)
	defer timer.Stop()
	select {
	case <-e.room.Done():
		return true
	case <-timer.C:
	case <-ctx.Done():
	}
	return false
}

// stopWait bounds how long AllocateRoom waits for a fenced room's actor to stop.
const stopWait = 1500 * time.Millisecond

// DrainServer stops new allocations. Rooms aren't handed over yet: on shutdown they close, and
// the lobby moves them through ordinary failover (roadmap task 5.5).
func (s *Server) DrainServer(ctx context.Context, _ *gamev1.DrainServerRequest) (*gamev1.DrainServerResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.draining {
		s.log.InfoContext(ctx, "draining", "server_id", s.id, "rooms", len(s.rooms)+len(s.starting))
	}
	s.draining = true
	return &gamev1.DrainServerResponse{RoomsRemaining: uint32(len(s.rooms) + len(s.starting))}, nil
}

// Room finds a running room (for the WebSocket handler).
func (s *Server) Room(id string) *room.Room {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e := s.rooms[id]; e != nil {
		return e.room
	}
	return nil
}

// Status returns the rooms, sorted by id, and whether the server drains, for heartbeats. It
// reads each room's published summary, so a busy room never delays a heartbeat.
func (s *Server) Status() (rooms []Room, draining bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, e := range s.rooms {
		sum := e.room.Summary()
		if sum.Closed {
			continue
		}
		rooms = append(rooms, Room{ID: id, Players: uint32(sum.Players), Seats: uint32(sum.Seats), Playing: sum.Playing})
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
