package roomstore

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Bowenn/doodle-arena/libs/go/redistest"
	"github.com/Bowenn/doodle-arena/services/game-server/internal/room"
	"github.com/redis/go-redis/v9"
)

func TestMain(m *testing.M) { redistest.Main(m) }

var t0 = time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)

func ev(seq int64, typ, data string) room.StoredEvent {
	return room.StoredEvent{Seq: seq, Type: typ, Data: json.RawMessage(data), At: t0.Add(time.Duration(seq) * time.Second)}
}

func TestNewRoomGetsTokenOne(t *testing.T) {
	s := New(redistest.New(t))
	o, err := s.Acquire(t.Context(), "r1", "gs-1")
	if err != nil {
		t.Fatal(err)
	}
	if o.Token != 1 || o.Snapshot != nil || len(o.Events) != 0 {
		t.Fatalf("owned = %+v", o)
	}
}

// The M2 fencing criterion at the store level: after a takeover, the old owner's writes and
// renewals are rejected, and nothing it tried reaches the stream.
func TestTakeoverFencesTheOldOwner(t *testing.T) {
	rdb := redistest.New(t)
	s := New(rdb)
	ctx := t.Context()
	a, err := s.Acquire(ctx, "r1", "gs-1")
	if err != nil {
		t.Fatal(err)
	}
	snap := &room.Snapshot{RoomID: "r1", Seq: 1, Phase: room.PhaseWaiting}
	if err := s.Commit(ctx, "r1", "gs-1", a.Token, []room.StoredEvent{ev(1, "player_joined", `{"user_id":"u1","name":"A"}`)}, snap, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.Commit(ctx, "r1", "gs-1", a.Token, []room.StoredEvent{ev(2, "chat", `{"user_id":"u1","text":"hi"}`)}, nil, nil); err != nil {
		t.Fatal(err)
	}

	// gs-2 takes the room by force while gs-1 is still alive (ADR 0007).
	b, err := s.Acquire(ctx, "r1", "gs-2")
	if err != nil {
		t.Fatal(err)
	}
	if b.Token != a.Token+1 {
		t.Fatalf("token = %d, want %d", b.Token, a.Token+1)
	}
	if b.Snapshot == nil || b.Snapshot.Seq != 1 || len(b.Events) != 2 || b.Events[1].Type != "chat" ||
		string(b.Events[1].Data) != `{"user_id":"u1","text":"hi"}` || !b.Events[1].At.Equal(t0.Add(2*time.Second)) {
		t.Fatalf("loaded snapshot %v, events %+v", b.Snapshot, b.Events)
	}

	// The old owner, unaware, tries to write and to renew.
	err = s.Commit(ctx, "r1", "gs-1", a.Token, []room.StoredEvent{ev(3, "chat", `{"text":"zombie"}`)}, nil, nil)
	if !errors.Is(err, ErrFenced) {
		t.Fatalf("old owner's commit: err = %v, want ErrFenced", err)
	}
	lost, err := s.Renew(ctx, "gs-1", []Lease{{RoomID: "r1", Token: a.Token}})
	if err != nil || len(lost) != 1 {
		t.Fatalf("old owner's renew: lost = %v, err = %v", lost, err)
	}
	if n := rdb.XLen(ctx, eventsKey("r1")).Val(); n != 2 {
		t.Fatalf("stream has %d events: the zombie's write got in", n)
	}
	// The new owner carries on.
	if err := s.Commit(ctx, "r1", "gs-2", b.Token, []room.StoredEvent{ev(3, "chat", `{"text":"new owner"}`)}, nil, nil); err != nil {
		t.Fatal(err)
	}
	// Even if the same server comes back with its old token, it stays fenced.
	if err := s.Commit(ctx, "r1", "gs-2", a.Token, []room.StoredEvent{ev(4, "chat", `{}`)}, nil, nil); !errors.Is(err, ErrFenced) {
		t.Fatalf("stale token: err = %v", err)
	}
}

func TestRenewKeepsTheLeaseAlive(t *testing.T) {
	rdb := redistest.New(t)
	s := New(rdb)
	o, err := s.Acquire(t.Context(), "r1", "gs-1")
	if err != nil {
		t.Fatal(err)
	}
	rdb.PExpire(t.Context(), LeaseKey("r1"), 200*time.Millisecond)
	lost, err := s.Renew(t.Context(), "gs-1", []Lease{{RoomID: "r1", Token: o.Token}})
	if err != nil || len(lost) != 0 {
		t.Fatalf("lost = %v, err = %v", lost, err)
	}
	if ttl := rdb.PTTL(t.Context(), LeaseKey("r1")).Val(); ttl < LeaseTTL-time.Second {
		t.Fatalf("ttl = %v after renewal", ttl)
	}
	// A lease that expired is lost, too: someone may already have taken it.
	rdb.Del(t.Context(), LeaseKey("r1"))
	lost, _ = s.Renew(t.Context(), "gs-1", []Lease{{RoomID: "r1", Token: o.Token}})
	if len(lost) != 1 {
		t.Fatal("an expired lease was renewed")
	}
}

func TestSnapshotTrimsTheStream(t *testing.T) {
	rdb := redistest.New(t)
	s := New(rdb)
	o, _ := s.Acquire(t.Context(), "r1", "gs-1")
	var evs []room.StoredEvent
	for i := int64(1); i <= 700; i++ {
		evs = append(evs, ev(i, "chat", `{}`))
	}
	if err := s.Commit(t.Context(), "r1", "gs-1", o.Token, evs, &room.Snapshot{RoomID: "r1", Seq: 700}, nil); err != nil {
		t.Fatal(err)
	}
	first := rdb.XRangeN(t.Context(), eventsKey("r1"), "-", "+", 1).Val()
	if len(first) != 1 || first[0].ID != "200-0" {
		t.Fatalf("first entry after trim = %v, want 200-0 (keeps 500 before the snapshot)", first)
	}
}

func TestClosedRoomIsNeverRestored(t *testing.T) {
	rdb := redistest.New(t)
	s := New(rdb)
	o, _ := s.Acquire(t.Context(), "r1", "gs-1")
	if err := s.Commit(t.Context(), "r1", "gs-1", o.Token, []room.StoredEvent{ev(1, "chat", `{}`)}, &room.Snapshot{Seq: 1}, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.AddViewerGuess(t.Context(), "r1", room.ViewerGuess{UserID: "v1", Name: "Vee", RoundNo: 1}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(t.Context(), "r1", "gs-1", o.Token); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Acquire(t.Context(), "r1", "gs-2"); !errors.Is(err, ErrClosed) {
		t.Fatalf("acquire after close: err = %v", err)
	}
	if rdb.Exists(t.Context(), eventsKey("r1"), snapshotKey("r1"), LeaseKey("r1"), ViewerGuessesKey("r1")).Val() != 0 {
		t.Fatal("closed room kept its data")
	}
	if rdb.Get(t.Context(), fenceKey("r1")).Val() != "1" {
		t.Fatal("the fence counter must survive a close")
	}
}

func TestRedisDownIsUnavailable(t *testing.T) {
	rdb := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", DialTimeout: 100 * time.Millisecond, MaxRetries: -1})
	defer rdb.Close()
	s := New(rdb)
	ctx := context.Background()
	if _, err := s.Acquire(ctx, "r1", "gs-1"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("acquire: %v", err)
	}
	if err := s.Commit(ctx, "r1", "gs-1", 1, nil, nil, nil); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("commit: %v", err)
	}
}

func TestAllKeysOfARoomShareASlot(t *testing.T) {
	keys := []string{eventsKey("abc"), snapshotKey("abc"), LeaseKey("abc"), fenceKey("abc"), closedKey("abc"), outboxKey("abc"), TicketKey("abc", "jti"),
		ViewerTicketKey("abc", "jti"), ViewerGuessesKey("abc")}
	for _, k := range keys {
		if want := "{abc}"; !contains(k, want) {
			t.Fatalf("%s has no hash tag %s", k, want)
		}
	}
}

func contains(s, sub string) bool { return strings.Contains(s, sub) }

func entry(eventID, kind string) room.OutboxEntry {
	return room.OutboxEntry{EventID: eventID, Kind: kind, Payload: []byte("gz:" + eventID), Traceparent: "00-tp-" + eventID}
}

// ADR 0009: the finished round goes into the outbox in the same script as the event that ends
// it, a new owner finds it there, and an old owner fenced out writes neither.
func TestOutboxIsCommittedWithTheRoundEnd(t *testing.T) {
	rdb := redistest.New(t)
	s := New(rdb)
	ctx := t.Context()
	a, err := s.Acquire(ctx, "r1", "gs-1")
	if err != nil {
		t.Fatal(err)
	}
	out := []room.OutboxEntry{entry("ev-round", room.OutboxRound), entry("ev-match", room.OutboxMatch)}
	if err := s.Commit(ctx, "r1", "gs-1", a.Token, []room.StoredEvent{ev(1, "round_ended", `{}`), ev(2, "match_ended", `{}`)},
		&room.Snapshot{RoomID: "r1", Seq: 2}, out); err != nil {
		t.Fatal(err)
	}
	if out[0].ID == "" || out[1].ID == "" || out[0].ID >= out[1].ID {
		t.Fatalf("entry ids = %q, %q: want both set, in order", out[0].ID, out[1].ID)
	}

	b, err := s.Acquire(ctx, "r1", "gs-2")
	if err != nil {
		t.Fatal(err)
	}
	if len(b.Outbox) != 2 {
		t.Fatalf("new owner's outbox = %+v, want 2 entries", b.Outbox)
	}
	for i, got := range b.Outbox {
		want := out[i]
		if got.ID != want.ID || got.RoomID != "r1" || got.EventID != want.EventID || got.Kind != want.Kind ||
			string(got.Payload) != string(want.Payload) || got.Traceparent != want.Traceparent {
			t.Fatalf("entry %d = %+v, want %+v", i, got, want)
		}
	}

	// gs-1 is fenced: its commit, outbox entry included, writes nothing.
	zombie := []room.OutboxEntry{entry("ev-zombie", room.OutboxRound)}
	if err := s.Commit(ctx, "r1", "gs-1", a.Token, []room.StoredEvent{ev(3, "round_ended", `{}`)}, nil, zombie); !errors.Is(err, ErrFenced) {
		t.Fatalf("zombie commit: err = %v", err)
	}
	if n := rdb.XLen(ctx, outboxKey("r1")).Val(); n != 2 || zombie[0].ID != "" {
		t.Fatalf("outbox has %d entries after a fenced commit, want 2", n)
	}
}

func TestCloseWaitsForAnEmptyOutbox(t *testing.T) {
	rdb := redistest.New(t)
	s := New(rdb)
	ctx := t.Context()
	o, err := s.Acquire(ctx, "r1", "gs-1")
	if err != nil {
		t.Fatal(err)
	}
	out := []room.OutboxEntry{entry("ev1", room.OutboxMatch)}
	if err := s.Commit(ctx, "r1", "gs-1", o.Token, []room.StoredEvent{ev(1, "match_ended", `{}`)}, &room.Snapshot{Seq: 1}, out); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(ctx, "r1", "gs-1", o.Token); !errors.Is(err, ErrOutboxPending) {
		t.Fatalf("close with a pending entry: err = %v, want ErrOutboxPending", err)
	}
	if rdb.Exists(ctx, snapshotKey("r1"), LeaseKey("r1"), outboxKey("r1")).Val() != 3 || rdb.Exists(ctx, closedKey("r1")).Val() != 0 {
		t.Fatal("a refused close changed the room's keys")
	}

	// Published: the ack deletes it (twice is harmless), and now the room closes, outbox key and all.
	for range 2 {
		if err := s.Ack(ctx, "r1", out[0].ID); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Close(ctx, "r1", "gs-1", o.Token); err != nil {
		t.Fatal(err)
	}
	if rdb.Exists(ctx, outboxKey("r1"), snapshotKey("r1")).Val() != 0 || rdb.Exists(ctx, closedKey("r1")).Val() != 1 {
		t.Fatal("closed room left keys behind or no tombstone")
	}
}

func TestPendingReturnsOnlyOlderEntries(t *testing.T) {
	rdb := redistest.New(t)
	s := New(rdb)
	ctx := t.Context()
	for _, id := range []string{"r1", "r2"} {
		o, err := s.Acquire(ctx, id, "gs-1")
		if err != nil {
			t.Fatal(err)
		}
		if id == "r2" {
			continue // r2 has nothing pending
		}
		if err := s.Commit(ctx, id, "gs-1", o.Token, nil, nil, []room.OutboxEntry{entry("ev1", room.OutboxRound)}); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.Pending(ctx, []string{"r1", "r2"}, time.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || len(got["r1"]) != 1 || got["r1"][0].EventID != "ev1" {
		t.Fatalf("pending = %+v, want r1's entry only", got)
	}
	if got, _ := s.Pending(ctx, []string{"r1", "r2"}, time.Now().Add(-time.Minute)); len(got) != 0 {
		t.Fatalf("entries younger than the cutoff came back: %+v", got)
	}
}

// ADR 0019: what a relay adds, the owner reads in order, from the start or on from a cursor,
// with an expiry so a stream outliving its room goes away.
func TestViewerGuessesStream(t *testing.T) {
	rdb := redistest.New(t)
	s := New(rdb)
	ctx := t.Context()
	for _, g := range []room.ViewerGuess{{UserID: "v1", Name: "Vee", MatchID: "m1", RoundNo: 2}, {UserID: "v2", Name: "Two", MatchID: "m1", RoundNo: 3}} {
		if err := s.AddViewerGuess(ctx, "r1", g); err != nil {
			t.Fatal(err)
		}
	}
	if ttl := rdb.PTTL(ctx, ViewerGuessesKey("r1")).Val(); ttl <= 0 {
		t.Fatalf("viewer guesses' TTL = %v", ttl)
	}
	got, cursor, err := s.ViewerGuesses("r1").Next(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != (room.ViewerGuess{UserID: "v1", Name: "Vee", MatchID: "m1", RoundNo: 2}) || got[1].RoundNo != 3 {
		t.Fatalf("read %+v", got)
	}
	// Nothing new: the read waits, then comes back empty with the same cursor.
	start := time.Now()
	again, next, err := s.ViewerGuesses("r1").Next(ctx, cursor)
	if err != nil || len(again) != 0 || next != cursor || time.Since(start) < time.Second {
		t.Fatalf("second read: %+v %q %v after %v", again, next, err, time.Since(start))
	}
}
