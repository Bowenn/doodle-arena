package rooms

import (
	"cmp"
	"context"
	"errors"
	"slices"
	"sync"
	"time"

	gamev1 "github.com/Bowenn/gap-project_private/proto/gen/go/doodle/game/v1"
	"github.com/Bowenn/gap-project_private/services/lobby/internal/registry"
	"github.com/redis/go-redis/v9"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
	grpccodes "google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Reassignment (design doc, section 7.5; ADR 0007).
const (
	ReassignEvery = 2 * time.Second
	reassignLock  = "lobby:reassigner"
	// lockTTL outlasts one AllocateRoom attempt with its redial (2 x allocateTimeout): the lock
	// is taken again before every attempt, so it can't expire while a call is out.
	lockTTL = 10 * time.Second
	// passTimeout is when a pass stops starting rooms; the rest wait for the next pass, which
	// begins at once with a fresh view of the servers. Rooms already started finish.
	passTimeout = 2 * ReassignEvery
	// reassignWorkers is how many rooms a pass moves at once. Each AllocateRoom keeps its own
	// 2-second deadline, so a dead server's rooms take about ceil(rooms/8) calls' time, not
	// rooms calls' time.
	reassignWorkers = 8
	// roomTimeout bounds one room's moves: every attempt with its redial, and the writes.
	roomTimeout = maxAttempts*2*allocateTimeout + time.Second
)

// errLockLost stops a pass whose instance no longer holds the reassigner lock.
var errLockLost = errors.New("reassigner lock lost")

// Reassign makes one pass over every room the index knows: rooms of a server whose heartbeats
// stopped, and rooms a live server no longer reports (it restarted, or the room ended), are
// allocated again, up to reassignWorkers at a time. The game server restores a room from
// Redis, or answers NOT_FOUND for one that ended, which is then removed.
func (s *Service) Reassign(ctx context.Context) error {
	return s.reassignPass(ctx, time.Now().Add(passTimeout))
}

// move is a room to allocate again.
type move struct {
	room      Room
	hostAlive bool
}

func (s *Service) reassignPass(ctx context.Context, startBy time.Time) error {
	began := time.Now()
	all, moves, err := s.findMoves(ctx)
	if err != nil {
		return err
	}
	if len(moves) == 0 {
		return nil
	}
	plan := newPlacement(all)
	var (
		mu     sync.Mutex
		counts = map[string]int{}
		lost   bool
		wg     sync.WaitGroup
	)
	sem := make(chan struct{}, reassignWorkers)
	for _, m := range moves {
		sem <- struct{}{}
		mu.Lock()
		stop := lost || ctx.Err() != nil || time.Now().After(startBy)
		if stop {
			counts["deferred"]++ // retried on the next pass
		}
		mu.Unlock()
		if stop {
			<-sem
			continue
		}
		wg.Go(func() {
			defer func() { <-sem }()
			// Not the pass's start deadline: a room that started finishes, and records its move.
			rctx, cancel := context.WithTimeout(ctx, roomTimeout)
			defer cancel()
			result, err := s.reassign(rctx, m.room, plan, m.hostAlive)
			mu.Lock()
			defer mu.Unlock()
			if errors.Is(err, errLockLost) {
				lost = true
			}
			counts[result]++
		})
	}
	wg.Wait()
	s.log.InfoContext(ctx, "reassignment pass done", "rooms", len(moves), "moved", counts["moved"],
		"ended", counts["ended"], "failed", counts["failed"], "deferred", counts["deferred"], "skipped", counts[""],
		"duration_ms", time.Since(began).Milliseconds())
	if lost {
		return errLockLost
	}
	return nil
}

// findMoves lists the servers and the rooms to move. Stale index entries are dropped on the way.
func (s *Service) findMoves(ctx context.Context) ([]registry.Server, []move, error) {
	ctx, cancel := context.WithTimeout(ctx, passTimeout)
	defer cancel()
	all, err := s.servers.Servers(ctx)
	if err != nil {
		return nil, nil, err
	}
	live := make(map[string]registry.Server, len(all))
	for _, srv := range all {
		live[srv.ID] = srv
	}
	hosts, err := s.store.Hosts(ctx)
	if err != nil {
		return nil, nil, err
	}
	var moves []move
	for _, host := range hosts {
		ids, err := s.store.RoomsOf(ctx, host)
		if err != nil {
			return nil, nil, err
		}
		srv, alive := live[host]
		for _, id := range ids {
			r, err := s.store.Get(ctx, id)
			if errors.Is(err, ErrRoomNotFound) || (err == nil && r.ServerID != host) {
				// Expired, or moved already: just a stale index entry.
				_ = s.store.Unindex(ctx, host, id)
				continue
			}
			if err != nil {
				return nil, nil, err
			}
			if alive {
				if _, reported := srv.Room(id); reported || s.now().Sub(r.placedAt()) <= reportGrace {
					continue
				}
			}
			moves = append(moves, move{room: r, hostAlive: alive})
		}
	}
	return all, moves, nil
}

// placement spreads one pass's rooms over the servers that may take them: each room goes to
// the server with the fewest rooms counting those this pass already sent it (then the
// registry's order: fewest sockets). Without it, every room of a pass would go to the server
// that was least loaded when the pass began.
type placement struct {
	mu      sync.Mutex
	servers []registry.Server // registry.Candidates order
	added   map[string]int
}

func newPlacement(all []registry.Server) *placement {
	return &placement{servers: registry.Candidates(all), added: map[string]int{}}
}

// choose returns the candidates for the next room, least loaded first after reorder (which
// moves suspects last), and counts the room on the first one. Ordering and counting happen
// together, so rooms that workers start at the same moment still spread.
func (p *placement) choose(reorder func([]registry.Server) []registry.Server) []registry.Server {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := slices.Clone(p.servers)
	slices.SortStableFunc(out, func(a, b registry.Server) int {
		return cmp.Compare(len(a.Rooms)+p.added[a.ID], len(b.Rooms)+p.added[b.ID])
	})
	out = reorder(out)
	if len(out) > 0 {
		p.added[out[0].ID]++
	}
	return out
}

// add counts a room sent to (n = 1) or taken back from (n = -1) a server.
func (p *placement) add(serverID string, n int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.added[serverID] += n
}

// reassign allocates one room again and returns its lobby.reassignments result, or "" when
// another pass moved it first or the lock was lost (errLockLost) before an attempt.
func (s *Service) reassign(ctx context.Context, r Room, plan *placement, hostAlive bool) (string, error) {
	ctx, span := s.tracer.Start(ctx, "lobby.reassign_room", trace.WithAttributes(
		attribute.String("room.id", r.ID), attribute.String("from_server_id", r.ServerID), attribute.Bool("host_alive", hostAlive)))
	defer span.End()
	from := r.ServerID
	req := &gamev1.AllocateRoomRequest{
		RoomId: r.ID, HostUserId: r.HostID, Mode: gamev1.GameMode_GAME_MODE_CLASSIC,
		Settings: &gamev1.RoomSettings{
			MaxPlayers: r.Settings.MaxPlayers, ChooseSeconds: r.Settings.ChooseSeconds,
			DrawSeconds: r.Settings.DrawSeconds, WordOptions: r.Settings.WordOptions,
		},
	}
	candidates := plan.choose(s.withoutSuspects)
	for i, srv := range candidates[:min(maxAttempts, len(candidates))] {
		if i > 0 {
			plan.add(srv.ID, 1)
		}
		if s.lockHolder != nil && !s.lockHolder(ctx) {
			plan.add(srv.ID, -1)
			return "", errLockLost
		}
		err := s.tryAllocate(ctx, srv, req)
		switch status.Code(err) {
		case grpccodes.OK:
			r.ServerID = srv.ID
			r.PlacedAt = s.now().UTC()
			err := s.store.Move(ctx, r, from)
			if errors.Is(err, ErrMovedMeanwhile) {
				s.log.WarnContext(ctx, "room moved by another pass meanwhile", "room_id", r.ID, "from_server_id", from)
				return "", nil
			}
			if err != nil {
				s.log.ErrorContext(ctx, "moved room not saved", "room_id", r.ID, "server_id", srv.ID, "err", err)
				s.countReassign(ctx, "failed")
				return "failed", nil
			}
			s.countReassign(ctx, "moved")
			s.log.InfoContext(ctx, "room reassigned", "room_id", r.ID, "from_server_id", from, "server_id", srv.ID)
			return "moved", nil
		case grpccodes.NotFound:
			plan.add(srv.ID, -1)
			if err := s.store.Remove(ctx, r); err != nil {
				s.log.ErrorContext(ctx, "ended room not removed", "room_id", r.ID, "err", err)
			}
			s.countReassign(ctx, "ended")
			s.log.InfoContext(ctx, "room ended; removed", "room_id", r.ID, "from_server_id", from)
			return "ended", nil
		case grpccodes.ResourceExhausted, grpccodes.FailedPrecondition:
		default:
			if ctx.Err() == nil { // not the lobby shutting down: the server didn't answer
				s.markSuspect(srv.ID)
			}
		}
		plan.add(srv.ID, -1)
		s.log.WarnContext(ctx, "reassignment failed, trying the next server", "room_id", r.ID, "server_id", srv.ID, "err", err)
	}
	s.countReassign(ctx, "failed") // retried on the next pass
	return "failed", nil
}

func (s *Service) countReassign(ctx context.Context, result string) {
	s.reassigns.Add(ctx, 1, metric.WithAttributes(attribute.String("result", result)))
}

// RunReassigner calls Reassign every ReassignEvery while this lobby instance holds the
// reassigner lock, so with several lobbies only one moves rooms at a time. It returns when
// ctx ends.
func (s *Service) RunReassigner(ctx context.Context, rdb redis.UniversalClient, instance string) {
	t := time.NewTicker(ReassignEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if !holdLock(ctx, rdb, instance) {
			continue
		}
		s.lockHolder = func(ctx context.Context) bool { return holdLock(ctx, rdb, instance) }
		if err := s.Reassign(ctx); err != nil {
			s.log.WarnContext(ctx, "reassignment pass stopped", "err", err)
		}
	}
}

// holdLock takes the reassigner lock or extends it if this instance holds it.
var lockScript = redis.NewScript(`
local cur = redis.call('GET', KEYS[1])
if cur == ARGV[1] then return redis.call('PEXPIRE', KEYS[1], ARGV[2]) end
if cur then return 0 end
redis.call('SET', KEYS[1], ARGV[1], 'PX', ARGV[2])
return 1
`)

func holdLock(ctx context.Context, rdb redis.UniversalClient, instance string) bool {
	ok, err := lockScript.Run(ctx, rdb, []string{reassignLock}, instance, lockTTL.Milliseconds()).Int()
	return err == nil && ok == 1
}
