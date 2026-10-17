package rooms

import (
	"context"
	"errors"
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
	lockTTL       = 10 * time.Second // longer than a pass may run (passTimeout)
	passTimeout   = 2 * ReassignEvery
)

// Reassign makes one pass over every room the index knows: rooms of a server whose heartbeats
// stopped, and rooms a live server no longer reports (it restarted, or the room ended), are
// allocated again. The game server restores a room from Redis, or answers NOT_FOUND for one
// that ended, which is then removed.
func (s *Service) Reassign(ctx context.Context) error {
	all, err := s.servers.Servers(ctx)
	if err != nil {
		return err
	}
	live := make(map[string]registry.Server, len(all))
	for _, srv := range all {
		live[srv.ID] = srv
	}
	hosts, err := s.store.Hosts(ctx)
	if err != nil {
		return err
	}
	for _, host := range hosts {
		ids, err := s.store.RoomsOf(ctx, host)
		if err != nil {
			return err
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
				return err
			}
			if alive {
				if _, reported := srv.Room(id); reported || s.now().Sub(r.placedAt()) <= reportGrace {
					continue
				}
			}
			if s.lockHolder != nil && !s.lockHolder(ctx) {
				return errors.New("reassigner lock lost")
			}
			s.reassign(ctx, r, all, alive)
		}
	}
	return nil
}

func (s *Service) reassign(ctx context.Context, r Room, all []registry.Server, hostAlive bool) {
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
	candidates := s.withoutSuspects(registry.Candidates(all))
	for _, srv := range candidates[:min(maxAttempts, len(candidates))] {
		err := s.tryAllocate(ctx, srv, req)
		switch status.Code(err) {
		case grpccodes.OK:
			r.ServerID = srv.ID
			r.PlacedAt = s.now().UTC()
			err := s.store.Move(ctx, r, from)
			if errors.Is(err, ErrMovedMeanwhile) {
				s.log.WarnContext(ctx, "room moved by another pass meanwhile", "room_id", r.ID, "from_server_id", from)
				return
			}
			if err != nil {
				s.log.ErrorContext(ctx, "moved room not saved", "room_id", r.ID, "server_id", srv.ID, "err", err)
				s.countReassign(ctx, "failed")
				return
			}
			s.countReassign(ctx, "moved")
			s.log.InfoContext(ctx, "room reassigned", "room_id", r.ID, "from_server_id", from, "server_id", srv.ID)
			return
		case grpccodes.NotFound:
			if err := s.store.Remove(ctx, r); err != nil {
				s.log.ErrorContext(ctx, "ended room not removed", "room_id", r.ID, "err", err)
			}
			s.countReassign(ctx, "ended")
			s.log.InfoContext(ctx, "room ended; removed", "room_id", r.ID, "from_server_id", from)
			return
		case grpccodes.ResourceExhausted, grpccodes.FailedPrecondition:
		default:
			s.markSuspect(srv.ID)
		}
		s.log.WarnContext(ctx, "reassignment failed, trying the next server", "room_id", r.ID, "server_id", srv.ID, "err", err)
	}
	s.countReassign(ctx, "failed") // retried on the next pass
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
		pctx, cancel := context.WithTimeout(ctx, passTimeout)
		s.lockHolder = func(ctx context.Context) bool { return holdLock(ctx, rdb, instance) }
		if err := s.Reassign(pctx); err != nil {
			s.log.WarnContext(ctx, "reassignment pass stopped", "err", err)
		}
		cancel()
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
