package room

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math/rand/v2"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Bowenn/gap-project_private/services/game-server/internal/protocol"
)

// fakeDurable keeps commits in memory, like the room store, and can be told to fail.
type fakeDurable struct {
	mu        sync.Mutex
	events    []StoredEvent
	snapshots []Snapshot
	fail      error
	closed    bool
}

func (d *fakeDurable) Commit(_ context.Context, events []StoredEvent, snap *Snapshot) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.fail != nil {
		return d.fail
	}
	d.events = append(d.events, events...)
	if snap != nil {
		// Through JSON, as Redis stores it.
		b, _ := json.Marshal(snap)
		var s Snapshot
		_ = json.Unmarshal(b, &s)
		d.snapshots = append(d.snapshots, s)
	}
	return nil
}

func (d *fakeDurable) Close(context.Context) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.closed = true
	return d.fail
}

func (d *fakeDurable) setFail(err error) { d.mu.Lock(); d.fail = err; d.mu.Unlock() }

func (d *fakeDurable) state() (Snapshot, []StoredEvent) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.snapshots[len(d.snapshots)-1], append([]StoredEvent(nil), d.events...)
}

func withDurable(d Durable) func(*Room) { return func(r *Room) { r.durable = d } }

func TestNewRoomStoresItsFirstSnapshotAndPhaseChanges(t *testing.T) {
	d := &fakeDurable{}
	h := newHarness(t, "a", DefaultSettings, nil, withDurable(d))
	h.join("a")
	h.join("b")
	h.sync()
	if len(d.snapshots) != 1 || d.snapshots[0].Seq != 0 {
		t.Fatalf("snapshots = %d, want the initial one only (joins aren't phase changes)", len(d.snapshots))
	}
	h.input("a", protocol.Start{})
	h.sync()
	if s, evs := d.state(); s.Phase != PhaseStarting || s.Seq != evs[len(evs)-1].Seq {
		t.Fatalf("after start: snapshot phase %s seq %d", s.Phase, s.Seq)
	}
}

// Write, then broadcast: if the store refuses, no socket sees the event and nothing is
// published; the room stops with 1011.
func TestNothingIsSentUnlessStored(t *testing.T) {
	d := &fakeDurable{}
	sink := &recordingSink{}
	h := newHarness(t, "a", DefaultSettings, nil, withDurable(d), func(r *Room) { r.events = sink })
	a := h.join("a")
	h.sync() // a's welcome is delivered
	before := len(a.all())
	d.setFail(errors.New("redis down"))
	_ = h.room.Join(context.Background(), &fakeConn{uid: "b"}, "B", 0)
	<-h.room.Done()
	if got := a.all()[before:]; len(got) != 0 {
		t.Fatalf("a received %d messages that were never stored: %s", len(got), got[0].raw)
	}
	if a.closed != 1011 {
		t.Fatalf("close code = %d, want 1011", a.closed)
	}
	sink.mu.Lock()
	published := len(sink.items)
	sink.mu.Unlock()
	if published != 0 {
		t.Fatal("published to Kafka without storing")
	}
}

// The old owner's side of a takeover: its next write is fenced, so it tells every client the
// room moved and closes with 4010. The lease keeper's Fence does the same for an idle room.
func TestTakenOverRoomTellsClientsToMove(t *testing.T) {
	for _, via := range []string{"commit", "fence"} {
		t.Run(via, func(t *testing.T) {
			d := &fakeDurable{}
			h := newHarness(t, "a", DefaultSettings, nil, withDurable(d))
			a := h.join("a")
			b := h.join("b")
			if via == "commit" {
				d.setFail(ErrMoved)
				_ = h.room.Input(context.Background(), a, protocol.Guess{Text: "hello"})
			} else {
				h.room.Fence()
			}
			<-h.room.Done()
			for _, c := range []*fakeConn{a, b} {
				if len(c.private("room_moved")) != 1 || c.closed != protocol.CloseRoomMoved {
					t.Fatalf("%s: room_moved %d, close %d", c.uid, len(c.private("room_moved")), c.closed)
				}
				if len(c.events("chat")) != 0 {
					t.Fatalf("%s saw a chat the room never stored", c.uid)
				}
			}
		})
	}
}

// M2 exit criterion: a client that disconnects mid-round resumes without missing events: it
// gets exactly the events it missed, in seq order, then its own events, and no welcome.
func TestResumeReplaysExactlyTheMissedEvents(t *testing.T) {
	h := newHarness(t, "a", DefaultSettings, nil, withDurable(&fakeDurable{}))
	h.join("a")
	h.join("b")
	c := h.join("c")
	drawer, word := h.startDrawing("a")
	guesser := "b"
	if drawer == "b" {
		guesser = "a"
	}
	lastSeen := c.events("")[len(c.events(""))-1].Seq

	h.room.Leave(c)
	h.sync()
	for i := range 5 {
		h.input(drawer, protocol.StrokeBatch{StrokeID: "s" + string(rune('a'+i)), Points: [][]int{{1, 2, 3}}, Color: "#000000", Width: 2})
	}
	h.input(guesser, protocol.Guess{Text: "is it a hat"})
	h.advance(DefaultSettings.Draw / 2) // the first hint

	back := &fakeConn{uid: "c"}
	if err := h.room.Join(context.Background(), back, "Name c", lastSeen); err != nil {
		t.Fatal(err)
	}
	h.sync()
	if len(back.private("welcome")) != 0 {
		t.Fatal("a resume must not send a welcome")
	}
	evs := back.events("")
	if len(evs) == 0 || evs[0].Seq != lastSeen+1 {
		t.Fatalf("first replayed seq = %v, want %d", evs, lastSeen+1)
	}
	for i := 1; i < len(evs); i++ {
		if evs[i].Seq != evs[i-1].Seq+1 {
			t.Fatalf("gap or duplicate: seq %d after %d", evs[i].Seq, evs[i-1].Seq)
		}
	}
	types := map[string]int{}
	for _, ev := range evs {
		types[ev.Type]++
	}
	if types["player_left"] != 1 || types["stroke_batch"] != 5 || types["chat"] != 1 || types["hint"] != 1 || types["player_joined"] != 1 {
		t.Fatalf("replayed %v", types)
	}
	for _, m := range back.all() {
		if containsWord(m.raw, word) {
			t.Fatalf("the replay leaked the word: %s", m.raw)
		}
	}
	// Everyone else saw the same events with the same seqs.
	other := h.conns[guesser].events("")
	if other[len(other)-1].Seq != evs[len(evs)-1].Seq {
		t.Fatal("the resumed client is not in step with the room")
	}
}

func TestResumeFarBehindGetsAWelcome(t *testing.T) {
	h := newHarness(t, "a", DefaultSettings, nil)
	h.join("a")
	h.join("b")
	c := h.join("c")
	drawer, _ := h.startDrawing("a")
	lastSeen := c.events("")[len(c.events(""))-1].Seq
	h.room.Leave(c)
	h.sync()
	for i := range maxReplay + 5 {
		h.input(drawer, protocol.StrokeBatch{StrokeID: "s", Points: [][]int{{i, 2, 3}}, Color: "#000000", Width: 2})
	}
	back := &fakeConn{uid: "c"}
	if err := h.room.Join(context.Background(), back, "Name c", lastSeen); err != nil {
		t.Fatal(err)
	}
	h.sync()
	if len(back.private("welcome")) != 1 {
		t.Fatal("over the replay cap, a full welcome is expected")
	}
}

// A takeover end to end inside the room package: room A's stored state rebuilds room B, the
// round goes on to its original deadline, and clients resume on B from their last seq.
func TestRestoredRoomResumesItsPlayers(t *testing.T) {
	d := &fakeDurable{}
	h := newHarness(t, "a", DefaultSettings, nil, withDurable(d))
	h.join("a")
	h.join("b")
	h.join("c")
	drawer, word := h.startDrawing("a")
	h.input(drawer, protocol.StrokeBatch{StrokeID: "s1", Points: [][]int{{1, 2, 3}}, Color: "#000000", Width: 2})
	h.sync()
	lastSeen := map[string]int64{}
	for uid, c := range h.conns {
		evs := c.events("")
		lastSeen[uid] = evs[len(evs)-1].Seq
	}
	snap, events := d.state()

	// Server B rebuilds the room 3 s later (server A is gone).
	clock := &fakeClock{now: h.clock.Now().Add(3 * time.Second)}
	b, err := New(Config{ID: "r1", Mode: "classic", Clock: clock, Rand: rand.New(rand.NewPCG(1, 1)),
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Metrics: NoopMetrics(), Durable: &fakeDurable{},
		Restore: &Restored{Snapshot: snap, Events: events}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go b.Run(ctx)
	t.Cleanup(func() { cancel(); <-b.Done() })

	conns := map[string]*fakeConn{}
	for _, uid := range []string{"a", "b", "c"} {
		conns[uid] = &fakeConn{uid: uid}
		if err := b.Join(context.Background(), conns[uid], "Name "+uid, lastSeen[uid]); err != nil {
			t.Fatal(err)
		}
	}
	if err := b.sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	for uid, c := range conns {
		if len(c.private("welcome")) != 0 {
			t.Fatalf("%s got a welcome instead of a resume", uid)
		}
	}
	// The drawer gets its word again; the canvas and the round's deadline survived.
	yw := conns[drawer].private("your_word")
	if len(yw) != 1 || !strings.Contains(string(yw[0]), word) {
		t.Fatalf("drawer's word after takeover: %s", yw)
	}
	if len(b.game.canvas) != 1 || !b.game.deadline.Equal(h.room.game.deadline) {
		t.Fatal("the restored round lost its canvas or deadline")
	}
	// The round ends at the original deadline.
	clock.Advance(b.game.deadline.Sub(clock.Now()))
	if err := b.sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(conns["a"].events("round_ended")) != 1 {
		t.Fatal("the restored round didn't end at its deadline")
	}
}
