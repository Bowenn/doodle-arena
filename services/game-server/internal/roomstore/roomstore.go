// Package roomstore keeps a room's durable state in Redis (design doc, section 9; ADR 0007):
// the event stream, the snapshot, the lease and the fencing counter.
//
// Keys (the game server owns room:*; the braces are a hash tag, so every key of one room is in
// the same Redis Cluster slot and one script can touch them all):
//
//	room:{id}:events    stream of shared events, entry ID <seq>-0, fields t (unix ms), type, data
//	room:{id}:snapshot  the room's state as JSON, written at phase changes and every 200 events
//	room:{id}:lease     "<server_id>:<token>", 5 s TTL, renewed every 1.5 s by the owner
//	room:{id}:fence     counter; INCR on every takeover gives the new owner its token. No TTL:
//	                    it only goes up, so an old token can never match again
//	room:{id}:closed    tombstone of a room that ended (1 h), so it is never restored
//
// Every write checks the lease against the writer's server id and token in a Lua script.
package roomstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/Bowenn/gap-project_private/services/game-server/internal/room"
	"github.com/redis/go-redis/v9"
)

// Timing of leases (design doc, section 9).
const (
	LeaseTTL   = 5 * time.Second
	RenewEvery = 1500 * time.Millisecond
	// keepBeforeSnapshot is how many events before the latest snapshot the stream keeps, so a
	// client a little behind can still resume by replay after a snapshot.
	keepBeforeSnapshot = 500
	// loadEvents is how many recent events a new owner loads: the ones after the snapshot
	// (rebuild) and some before it (replay for resuming clients).
	loadEvents   = 1024
	tombstoneTTL = time.Hour
)

// Errors.
var (
	// ErrFenced means another server took the room: this server must stop it.
	ErrFenced = errors.New("room lease lost to another server")
	// ErrClosed means the room ended; it must not be restored.
	ErrClosed = errors.New("room has ended")
	// ErrUnavailable means Redis couldn't be reached or answered with an error.
	ErrUnavailable = errors.New("room store unavailable")
)

// Keys of a room.
func eventsKey(id string) string   { return "room:{" + id + "}:events" }
func snapshotKey(id string) string { return "room:{" + id + "}:snapshot" }

// LeaseKey is the room's lease key.
func LeaseKey(id string) string  { return "room:{" + id + "}:lease" }
func fenceKey(id string) string  { return "room:{" + id + "}:fence" }
func closedKey(id string) string { return "room:{" + id + "}:closed" }

// TicketKey marks a used join ticket of the room (ADR 0005).
func TicketKey(roomID, ticketID string) string { return "room:{" + roomID + "}:ticket:" + ticketID }

// LeaseValue is what the lease key holds for an owner.
func LeaseValue(serverID string, token int64) string {
	return serverID + ":" + strconv.FormatInt(token, 10)
}

// Store reads and writes rooms in Redis.
type Store struct {
	rdb redis.UniversalClient
}

// New returns a store.
func New(rdb redis.UniversalClient) *Store { return &Store{rdb: rdb} }

// Take the lease first, then read (ADR 0007): writes the old owner made before the INCR are
// in what the new owner reads; writes after it fail the lease check.
var acquireScript = redis.NewScript(`
if redis.call('EXISTS', KEYS[3]) == 1 then return -1 end
local token = redis.call('INCR', KEYS[1])
redis.call('SET', KEYS[2], ARGV[1] .. ':' .. token, 'PX', ARGV[2])
return token
`)

// Owned is what a new owner gets: its token and what is needed to rebuild the room.
type Owned struct {
	Token    int64
	Snapshot *room.Snapshot // nil for a room that never had one (a new room)
	Events   []room.StoredEvent
}

// Acquire takes a room for serverID by force (a takeover if someone else held it) and loads
// its snapshot and recent events.
func (s *Store) Acquire(ctx context.Context, roomID, serverID string) (Owned, error) {
	token, err := acquireScript.Run(ctx, s.rdb,
		[]string{fenceKey(roomID), LeaseKey(roomID), closedKey(roomID)},
		serverID, LeaseTTL.Milliseconds()).Int64()
	if err != nil {
		return Owned{}, fmt.Errorf("%w: acquire: %w", ErrUnavailable, err)
	}
	if token < 0 {
		return Owned{}, ErrClosed
	}
	o := Owned{Token: token}

	raw, err := s.rdb.Get(ctx, snapshotKey(roomID)).Bytes()
	switch {
	case errors.Is(err, redis.Nil):
	case err != nil:
		return Owned{}, fmt.Errorf("%w: read snapshot: %w", ErrUnavailable, err)
	default:
		var snap room.Snapshot
		if err := json.Unmarshal(raw, &snap); err != nil {
			return Owned{}, fmt.Errorf("decode snapshot: %w", err)
		}
		o.Snapshot = &snap
	}

	entries, err := s.rdb.XRevRangeN(ctx, eventsKey(roomID), "+", "-", loadEvents).Result()
	if err != nil {
		return Owned{}, fmt.Errorf("%w: read events: %w", ErrUnavailable, err)
	}
	slices.Reverse(entries)
	for _, e := range entries {
		ev, err := decodeEntry(e)
		if err != nil {
			return Owned{}, err
		}
		o.Events = append(o.Events, ev)
	}
	return o, nil
}

func decodeEntry(e redis.XMessage) (room.StoredEvent, error) {
	seqStr, _, _ := strings.Cut(e.ID, "-")
	seq, err := strconv.ParseInt(seqStr, 10, 64)
	if err != nil {
		return room.StoredEvent{}, fmt.Errorf("bad stream id %q", e.ID)
	}
	t, _ := strconv.ParseInt(fmt.Sprint(e.Values["t"]), 10, 64)
	typ, _ := e.Values["type"].(string)
	data, _ := e.Values["data"].(string)
	return room.StoredEvent{Seq: seq, Type: typ, Data: json.RawMessage(data), At: time.UnixMilli(t).UTC()}, nil
}

// Commit checks the lease, appends the events (stream ID <seq>-0) and, with a snapshot,
// stores it and trims the stream, all in one script: either everything is written by the
// rightful owner or nothing is.
var commitScript = redis.NewScript(`
if redis.call('GET', KEYS[1]) ~= ARGV[1] then return redis.error_reply('FENCED') end
local n = tonumber(ARGV[2])
local i = 3
for _ = 1, n do
  redis.call('XADD', KEYS[2], ARGV[i], 't', ARGV[i+1], 'type', ARGV[i+2], 'data', ARGV[i+3])
  i = i + 4
end
if ARGV[i] ~= '' then
  redis.call('SET', KEYS[3], ARGV[i])
  redis.call('XTRIM', KEYS[2], 'MINID', ARGV[i+1])
end
return n
`)

// Commit writes one actor step: events (may be empty when only a snapshot is written) and an
// optional snapshot. It returns ErrFenced when serverID no longer holds the room with token.
func (s *Store) Commit(ctx context.Context, roomID, serverID string, token int64, events []room.StoredEvent, snap *room.Snapshot) error {
	args := []any{LeaseValue(serverID, token), len(events)}
	for _, ev := range events {
		args = append(args, strconv.FormatInt(ev.Seq, 10)+"-0", ev.At.UnixMilli(), ev.Type, string(ev.Data))
	}
	if snap != nil {
		b, err := json.Marshal(snap)
		if err != nil {
			return fmt.Errorf("encode snapshot: %w", err)
		}
		args = append(args, string(b), max(1, snap.Seq-keepBeforeSnapshot))
	} else {
		args = append(args, "", 0)
	}
	err := commitScript.Run(ctx, s.rdb, []string{LeaseKey(roomID), eventsKey(roomID), snapshotKey(roomID)}, args...).Err()
	return classify(err)
}

func classify(err error) error {
	switch {
	case err == nil:
		return nil
	case strings.Contains(err.Error(), "FENCED"):
		return ErrFenced
	default:
		return fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
}

// Renew extends the lease only if it is still this owner's.
var renewScript = redis.NewScript(`
if redis.call('GET', KEYS[1]) ~= ARGV[1] then return 0 end
redis.call('PEXPIRE', KEYS[1], ARGV[2])
return 1
`)

// Lease is one room's lease as its owner holds it.
type Lease struct {
	RoomID string
	Token  int64
}

// Renew extends the leases of serverID's rooms in one pipeline. It returns the rooms whose
// lease is no longer this server's (taken over or expired): their owner must stop them.
func (s *Store) Renew(ctx context.Context, serverID string, leases []Lease) (lost []string, err error) {
	if len(leases) == 0 {
		return nil, nil
	}
	// Load the script once so the pipeline can use EVALSHA.
	if err := renewScript.Load(ctx, s.rdb).Err(); err != nil {
		return nil, fmt.Errorf("%w: load renew script: %w", ErrUnavailable, err)
	}
	cmds := make([]*redis.Cmd, len(leases))
	_, err = s.rdb.Pipelined(ctx, func(p redis.Pipeliner) error {
		for i, l := range leases {
			cmds[i] = renewScript.EvalSha(ctx, p, []string{LeaseKey(l.RoomID)}, LeaseValue(serverID, l.Token), LeaseTTL.Milliseconds())
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("%w: renew: %w", ErrUnavailable, err)
	}
	for i, c := range cmds {
		if ok, _ := c.Int(); ok == 0 {
			lost = append(lost, leases[i].RoomID)
		}
	}
	return lost, nil
}

// Close marks an ended room with a tombstone and drops its stream, snapshot and lease. The
// fence counter stays, so tokens keep growing if the id were ever used again.
var closeScript = redis.NewScript(`
if redis.call('GET', KEYS[1]) ~= ARGV[1] then return redis.error_reply('FENCED') end
redis.call('SET', KEYS[4], '1', 'PX', ARGV[2])
redis.call('DEL', KEYS[1], KEYS[2], KEYS[3])
return 1
`)

// Close ends a room for good (it was empty long enough).
func (s *Store) Close(ctx context.Context, roomID, serverID string, token int64) error {
	err := closeScript.Run(ctx, s.rdb,
		[]string{LeaseKey(roomID), eventsKey(roomID), snapshotKey(roomID), closedKey(roomID)},
		LeaseValue(serverID, token), tombstoneTTL.Milliseconds()).Err()
	return classify(err)
}
