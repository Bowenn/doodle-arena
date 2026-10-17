package roomstore

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Bowenn/gap-project_private/services/game-server/internal/redistest"
	"github.com/Bowenn/gap-project_private/services/game-server/internal/room"
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
	if err := s.Commit(ctx, "r1", "gs-1", a.Token, []room.StoredEvent{ev(1, "player_joined", `{"user_id":"u1","name":"A"}`)}, snap); err != nil {
		t.Fatal(err)
	}
	if err := s.Commit(ctx, "r1", "gs-1", a.Token, []room.StoredEvent{ev(2, "chat", `{"user_id":"u1","text":"hi"}`)}, nil); err != nil {
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
	err = s.Commit(ctx, "r1", "gs-1", a.Token, []room.StoredEvent{ev(3, "chat", `{"text":"zombie"}`)}, nil)
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
	if err := s.Commit(ctx, "r1", "gs-2", b.Token, []room.StoredEvent{ev(3, "chat", `{"text":"new owner"}`)}, nil); err != nil {
		t.Fatal(err)
	}
	// Even if the same server comes back with its old token, it stays fenced.
	if err := s.Commit(ctx, "r1", "gs-2", a.Token, []room.StoredEvent{ev(4, "chat", `{}`)}, nil); !errors.Is(err, ErrFenced) {
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
	if err := s.Commit(t.Context(), "r1", "gs-1", o.Token, evs, &room.Snapshot{RoomID: "r1", Seq: 700}); err != nil {
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
	if err := s.Commit(t.Context(), "r1", "gs-1", o.Token, []room.StoredEvent{ev(1, "chat", `{}`)}, &room.Snapshot{Seq: 1}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(t.Context(), "r1", "gs-1", o.Token); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Acquire(t.Context(), "r1", "gs-2"); !errors.Is(err, ErrClosed) {
		t.Fatalf("acquire after close: err = %v", err)
	}
	if rdb.Exists(t.Context(), eventsKey("r1"), snapshotKey("r1"), LeaseKey("r1")).Val() != 0 {
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
	if err := s.Commit(ctx, "r1", "gs-1", 1, nil, nil); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("commit: %v", err)
	}
}

func TestAllKeysOfARoomShareASlot(t *testing.T) {
	keys := []string{eventsKey("abc"), snapshotKey("abc"), LeaseKey("abc"), fenceKey("abc"), closedKey("abc"), TicketKey("abc", "jti")}
	for _, k := range keys {
		if want := "{abc}"; !contains(k, want) {
			t.Fatalf("%s has no hash tag %s", k, want)
		}
	}
}

func contains(s, sub string) bool { return strings.Contains(s, sub) }
