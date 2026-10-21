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
//	room:{id}:outbox    stream of finished rounds and matches not yet in Kafka (ADR 0009), auto
//	                    IDs (their ms part is the entry's age), fields event_id, kind, payload,
//	                    traceparent. Written in the commit that ends the round, deleted once
//	                    published; a room isn't closed while it has entries
//
// Every write checks the lease against the writer's server id and token in a Lua script. The
// one exception is deleting a published outbox entry (Ack): any owner may, since every owner
// publishes the same event_id.
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
	// ErrFenced means another server took the room: this server must stop it. It is the
	// room package's ErrMoved, so the actor recognizes it.
	ErrFenced = room.ErrMoved
	// ErrClosed means the room ended; it must not be restored.
	ErrClosed = errors.New("room has ended")
	// ErrUnavailable means Redis couldn't be reached or answered with an error.
	ErrUnavailable = errors.New("room store unavailable")
	// ErrOutboxPending means the room can't close yet: its outbox still has entries. It is the
	// room package's, so the actor recognizes it.
	ErrOutboxPending = room.ErrOutboxPending
)

// Keys of a room.
func eventsKey(id string) string   { return "room:{" + id + "}:events" }
func snapshotKey(id string) string { return "room:{" + id + "}:snapshot" }

// LeaseKey is the room's lease key.
func LeaseKey(id string) string  { return "room:{" + id + "}:lease" }
func fenceKey(id string) string  { return "room:{" + id + "}:fence" }
func closedKey(id string) string { return "room:{" + id + "}:closed" }
func outboxKey(id string) string { return "room:{" + id + "}:outbox" }

// TicketKey marks a used join ticket of the room (ADR 0005).
func TicketKey(roomID, ticketID string) string { return "room:{" + roomID + "}:ticket:" + ticketID }

// ViewerTicketKey marks a used viewer ticket of the room (ADR 0017).
func ViewerTicketKey(roomID, ticketID string) string {
	return "room:{" + roomID + "}:viewer-ticket:" + ticketID
}

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
	Outbox   []room.OutboxEntry // finished rounds and matches the old owner didn't publish
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

	// Read after taking the lease: whatever the old owner committed is in here.
	pending, err := s.rdb.XRange(ctx, outboxKey(roomID), "-", "+").Result()
	if err != nil {
		return Owned{}, fmt.Errorf("%w: read outbox: %w", ErrUnavailable, err)
	}
	for _, e := range pending {
		o.Outbox = append(o.Outbox, decodeOutbox(roomID, e))
	}
	return o, nil
}

func decodeOutbox(roomID string, e redis.XMessage) room.OutboxEntry {
	str := func(k string) string { v, _ := e.Values[k].(string); return v }
	return room.OutboxEntry{ID: e.ID, RoomID: roomID, EventID: str("event_id"), Kind: str("kind"),
		Payload: []byte(str("payload")), Traceparent: str("traceparent")}
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

// Commit checks the lease, appends the events (stream ID <seq>-0), with a snapshot stores it
// and trims the stream, and adds the step's finished rounds and matches to the outbox, all in
// one script: either everything is written by the rightful owner or nothing is. It returns the
// outbox entries' IDs.
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
i = i + 2
local ids = {}
for _ = 1, tonumber(ARGV[i]) do
  table.insert(ids, redis.call('XADD', KEYS[4], '*', 'event_id', ARGV[i+1], 'kind', ARGV[i+2],
    'payload', ARGV[i+3], 'traceparent', ARGV[i+4]))
  i = i + 4
end
return ids
`)

// Commit writes one actor step: events (may be empty when only a snapshot is written), an
// optional snapshot, and the step's outbox entries, whose IDs it fills in. It returns ErrFenced
// when serverID no longer holds the room with token.
func (s *Store) Commit(ctx context.Context, roomID, serverID string, token int64, events []room.StoredEvent, snap *room.Snapshot, outbox []room.OutboxEntry) error {
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
	args = append(args, len(outbox))
	for _, e := range outbox {
		args = append(args, e.EventID, e.Kind, e.Payload, e.Traceparent)
	}
	ids, err := commitScript.Run(ctx, s.rdb,
		[]string{LeaseKey(roomID), eventsKey(roomID), snapshotKey(roomID), outboxKey(roomID)}, args...).StringSlice()
	if err := classify(err); err != nil {
		return err
	}
	for i := range outbox {
		outbox[i].ID = ids[i]
	}
	return nil
}

func classify(err error) error {
	switch {
	case err == nil:
		return nil
	case strings.Contains(err.Error(), "FENCED"):
		return ErrFenced
	case strings.Contains(err.Error(), "OUTBOX"):
		return ErrOutboxPending
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

// Close marks an ended room with a tombstone and drops its stream, snapshot, outbox and lease,
// unless the outbox still has entries: then the room stays (ADR 0009). The fence counter
// stays, so tokens keep growing if the id were ever used again.
var closeScript = redis.NewScript(`
if redis.call('GET', KEYS[1]) ~= ARGV[1] then return redis.error_reply('FENCED') end
if redis.call('XLEN', KEYS[5]) > 0 then return redis.error_reply('OUTBOX') end
redis.call('SET', KEYS[4], '1', 'PX', ARGV[2])
redis.call('DEL', KEYS[1], KEYS[2], KEYS[3], KEYS[5])
return 1
`)

// Close ends a room for good (it was empty long enough). It returns ErrOutboxPending while
// finished rounds or matches are still waiting to be published.
func (s *Store) Close(ctx context.Context, roomID, serverID string, token int64) error {
	err := closeScript.Run(ctx, s.rdb,
		[]string{LeaseKey(roomID), eventsKey(roomID), snapshotKey(roomID), closedKey(roomID), outboxKey(roomID)},
		LeaseValue(serverID, token), tombstoneTTL.Milliseconds()).Err()
	return classify(err)
}

// Ack deletes a published outbox entry. It isn't fenced: an old owner that published before a
// takeover may still ack, because every owner publishes the same event_id, and deleting an
// entry that is already gone does nothing.
func (s *Store) Ack(ctx context.Context, roomID, entryID string) error {
	if err := s.rdb.XDel(ctx, outboxKey(roomID), entryID).Err(); err != nil {
		return fmt.Errorf("%w: ack outbox entry: %w", ErrUnavailable, err)
	}
	return nil
}

// Pending returns, per room, the outbox entries added before olderThan: the ones that should
// have been published by now (a failed or dropped publish). Rooms without such entries are
// left out.
func (s *Store) Pending(ctx context.Context, roomIDs []string, olderThan time.Time) (map[string][]room.OutboxEntry, error) {
	if len(roomIDs) == 0 {
		return nil, nil
	}
	end := strconv.FormatInt(olderThan.UnixMilli(), 10)
	cmds := make([]*redis.XMessageSliceCmd, len(roomIDs))
	_, err := s.rdb.Pipelined(ctx, func(p redis.Pipeliner) error {
		for i, id := range roomIDs {
			cmds[i] = p.XRange(ctx, outboxKey(id), "-", end)
		}
		return nil
	})
	if err != nil && !errors.Is(err, redis.Nil) {
		return nil, fmt.Errorf("%w: read outboxes: %w", ErrUnavailable, err)
	}
	out := map[string][]room.OutboxEntry{}
	for i, id := range roomIDs {
		for _, e := range cmds[i].Val() {
			out[id] = append(out[id], decodeOutbox(id, e))
		}
	}
	return out, nil
}

// Handle is one owner's access to one room: what the room's actor uses (room.Durable).
type Handle struct {
	store    *Store
	roomID   string
	serverID string
	token    int64
}

// Handle binds a room to its owner and fencing token.
func (s *Store) Handle(roomID, serverID string, token int64) *Handle {
	return &Handle{store: s, roomID: roomID, serverID: serverID, token: token}
}

// Commit implements room.Durable.
func (h *Handle) Commit(ctx context.Context, events []room.StoredEvent, snap *room.Snapshot, outbox []room.OutboxEntry) error {
	return h.store.Commit(ctx, h.roomID, h.serverID, h.token, events, snap, outbox)
}

// Close implements room.Durable.
func (h *Handle) Close(ctx context.Context) error {
	return h.store.Close(ctx, h.roomID, h.serverID, h.token)
}

// Lease is what the owner renews.
func (h *Handle) Lease() Lease { return Lease{RoomID: h.roomID, Token: h.token} }

// ---------------------------------------------------------------- reading, for relays

// ErrNotFound means Redis has no such room: it never existed, or its keys are gone.
var ErrNotFound = errors.New("room not found")

// Snapshot returns a room's latest snapshot, for a relay (ADR 0017): no lease, never a write.
// It returns ErrClosed for a room that ended and ErrNotFound for one Redis doesn't have.
func (s *Store) Snapshot(ctx context.Context, roomID string) (room.Snapshot, error) {
	raw, err := s.rdb.Get(ctx, snapshotKey(roomID)).Bytes()
	if errors.Is(err, redis.Nil) {
		if closed, cerr := s.Closed(ctx, roomID); cerr != nil {
			return room.Snapshot{}, cerr
		} else if closed {
			return room.Snapshot{}, ErrClosed
		}
		return room.Snapshot{}, ErrNotFound
	}
	if err != nil {
		return room.Snapshot{}, fmt.Errorf("%w: read snapshot: %w", ErrUnavailable, err)
	}
	var snap room.Snapshot
	if err := json.Unmarshal(raw, &snap); err != nil {
		return room.Snapshot{}, fmt.Errorf("decode snapshot: %w", err)
	}
	return snap, nil
}

// EventsAfter returns a room's stored events with a seq above after, in order.
func (s *Store) EventsAfter(ctx context.Context, roomID string, after int64) ([]room.StoredEvent, error) {
	entries, err := s.rdb.XRange(ctx, eventsKey(roomID), "("+strconv.FormatInt(after, 10)+"-0", "+").Result()
	if err != nil {
		return nil, fmt.Errorf("%w: read events: %w", ErrUnavailable, err)
	}
	return decodeEntries(entries)
}

// Tail waits up to block for events with a seq above after (XREAD BLOCK) and returns at most
// count of them; none when the wait ends empty.
func (s *Store) Tail(ctx context.Context, roomID string, after int64, block time.Duration, count int64) ([]room.StoredEvent, error) {
	streams, err := s.rdb.XRead(ctx, &redis.XReadArgs{
		Streams: []string{eventsKey(roomID), strconv.FormatInt(after, 10) + "-0"},
		Count:   count, Block: block,
	}).Result()
	switch {
	case errors.Is(err, redis.Nil):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("%w: tail events: %w", ErrUnavailable, err)
	case len(streams) == 0:
		return nil, nil
	}
	return decodeEntries(streams[0].Messages)
}

// Closed reports whether the room ended (its tombstone is set).
func (s *Store) Closed(ctx context.Context, roomID string) (bool, error) {
	n, err := s.rdb.Exists(ctx, closedKey(roomID)).Result()
	if err != nil {
		return false, fmt.Errorf("%w: read tombstone: %w", ErrUnavailable, err)
	}
	return n == 1, nil
}

func decodeEntries(entries []redis.XMessage) ([]room.StoredEvent, error) {
	out := make([]room.StoredEvent, 0, len(entries))
	for _, e := range entries {
		ev, err := decodeEntry(e)
		if err != nil {
			return nil, err
		}
		out = append(out, ev)
	}
	return out, nil
}
