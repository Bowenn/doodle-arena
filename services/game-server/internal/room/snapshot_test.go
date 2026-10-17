package room

import (
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Bowenn/gap-project_private/services/game-server/internal/protocol"
)

// recorder plays a game the way the actor does: every step's events are "committed" with the
// step's time, and a snapshot is taken after any step with a phase event or SnapshotEvery
// events since the last one.
type recorder struct {
	t        *testing.T
	g        *Game
	now      time.Time
	events   []StoredEvent
	snap     Snapshot
	sinceSnp int
}

func newRecorder(t *testing.T, seed uint64) *recorder {
	g := NewGame("r1", "p0", DefaultSettings, rand.New(rand.NewPCG(seed, seed)), t0)
	r := &recorder{t: t, g: g, now: t0}
	r.snap = g.Snapshot()
	return r
}

func (r *recorder) commit() {
	phase := false
	for _, o := range r.g.TakeOut() {
		if ev, ok := o.Data.(protocol.Event); ok && o.Type == "event" {
			data, err := json.Marshal(ev.Data)
			if err != nil {
				r.t.Fatal(err)
			}
			r.events = append(r.events, StoredEvent{Seq: ev.Seq, Type: ev.Type, Data: data, At: r.now})
			r.sinceSnp++
			phase = phase || IsPhaseEvent(ev.Type)
		}
	}
	r.g.TakeFinished()
	if phase || r.sinceSnp >= SnapshotEvery {
		r.snap = r.g.Snapshot()
		r.sinceSnp = 0
	}
}

func snapJSON(t *testing.T, s Snapshot) string {
	t.Helper()
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// checkRestore rebuilds the room from the latest snapshot plus the events after it and
// compares it with the live game.
func (r *recorder) checkRestore(step int) {
	r.t.Helper()
	got, err := restore(r.snap, r.events, rand.New(rand.NewPCG(1, 1)))
	if err != nil {
		r.t.Fatalf("step %d: restore: %v", step, err)
	}
	if a, b := snapJSON(r.t, got.Snapshot()), snapJSON(r.t, r.g.Snapshot()); a != b {
		r.t.Fatalf("step %d: restored state differs\nsnapshot: %s\nrestored: %s\nlive:     %s", step, r.snap, a, b)
	}
}

// Property: at every point of a long random game, the latest snapshot plus the events after
// it rebuild exactly the live state. This is what makes a takeover lossless.
func TestSnapshotPlusEventsRebuildsTheGame(t *testing.T) {
	for seed := range uint64(20) {
		t.Run(fmt.Sprint("seed ", seed), func(t *testing.T) {
			r := newRecorder(t, seed)
			act := rand.New(rand.NewPCG(seed, 99))
			// Up to 6 players, so rounds with two drawers (4 or more present) come up often.
			users := []string{"p0", "p1", "p2", "p3", "p4", "p5"}
			for step := range 1500 {
				uid := users[act.IntN(len(users))]
				drawer := uid // either drawer of a pair, or anyone between rounds
				if len(r.g.drawers) > 0 {
					drawer = r.g.drawers[act.IntN(len(r.g.drawers))]
				}
				switch n := act.IntN(100); {
				case n < 6:
					_ = r.g.Join(r.now, uid, "Name "+uid, act.IntN(2) == 0)
				case n < 9:
					r.g.Leave(r.now, uid)
				case n < 12:
					r.g.Handle(r.now, r.g.host, protocol.Start{})
				case n < 16:
					r.g.Handle(r.now, r.g.chooser(), protocol.ChooseWord{Index: act.IntN(3)})
				case n < 55:
					pts := make([][]int, 1+act.IntN(20))
					for i := range pts {
						pts[i] = []int{act.IntN(10001), act.IntN(10001), i}
					}
					r.g.Handle(r.now, drawer, protocol.StrokeBatch{StrokeID: fmt.Sprint("s", act.IntN(6)), Points: pts, Color: "#123456", Width: 3})
				case n < 60:
					r.g.Handle(r.now, drawer, protocol.Undo{StrokeID: fmt.Sprint("s", act.IntN(6))})
				case n < 75:
					text := "nope"
					if act.IntN(3) == 0 {
						text = r.g.word
					}
					if text == "" {
						text = "hi"
					}
					r.g.Handle(r.now, uid, protocol.Guess{Text: text})
				default:
					r.now = r.now.Add(time.Duration(act.IntN(4000)) * time.Millisecond)
					r.g.Advance(r.now)
				}
				r.commit()
				if step%10 == 0 {
					r.checkRestore(step)
				}
			}
			if r.g.Seq() < 300 {
				t.Fatalf("only %d events: the walk didn't exercise much", r.g.Seq())
			}
		})
	}
}

func TestRestoreGivesPlayersAGraceToComeBack(t *testing.T) {
	r := newRecorder(t, 3)
	for _, u := range []string{"p0", "p1", "p2"} {
		_ = r.g.Join(r.now, u, u, false)
	}
	r.g.Handle(r.now, "p0", protocol.Start{})
	r.commit()
	r.now = r.now.Add(StartingFor)
	r.g.Advance(r.now)
	r.commit()
	r.g.Handle(r.now, r.g.chooser(), protocol.ChooseWord{Index: 0})
	r.commit()
	drawer, word := r.g.chooser(), r.g.word

	// The new owner restores 2 s later: everybody is disconnected, nothing ends yet.
	took := r.now.Add(2 * time.Second)
	g, err := RestoreGame(r.snap, r.events, rand.New(rand.NewPCG(5, 5)), took)
	if err != nil {
		t.Fatal(err)
	}
	if g.Connected() != 0 || g.Phase() != PhaseDrawing || g.word != word || g.chooser() != drawer {
		t.Fatalf("restored: connected %d phase %s", g.Connected(), g.Phase())
	}
	if g.deadline != r.g.deadline {
		t.Fatal("the deadline moved: timers must continue from the stored absolute time")
	}
	// The drawer and one guesser come back within the grace; the third player never does.
	other := "p1"
	if drawer == "p1" {
		other = "p2"
	}
	if err := g.Join(took.Add(time.Second), drawer, drawer, false); err != nil {
		t.Fatal(err)
	}
	if err := g.Join(took.Add(time.Second), other, other, false); err != nil {
		t.Fatal(err)
	}
	g.Advance(took.Add(TakeoverGrace))
	if g.Phase() != PhaseDrawing {
		t.Fatalf("phase = %s: two players are back, the round goes on", g.Phase())
	}
	// A correct guess from the one present guesser now ends the round: the absent one doesn't count.
	g.Handle(took.Add(TakeoverGrace+time.Second), other, protocol.Guess{Text: word})
	if g.Phase() != PhaseReveal {
		t.Fatalf("phase = %s, want reveal", g.Phase())
	}
}

func TestRestoreEndsTheMatchWhenNobodyComesBack(t *testing.T) {
	r := newRecorder(t, 4)
	for _, u := range []string{"p0", "p1"} {
		_ = r.g.Join(r.now, u, u, false)
	}
	r.g.Handle(r.now, "p0", protocol.Start{})
	r.commit()
	g, err := RestoreGame(r.snap, r.events, rand.New(rand.NewPCG(5, 5)), r.now)
	if err != nil {
		t.Fatal(err)
	}
	g.Advance(r.now.Add(StartingFor)) // the countdown ends inside the grace: the round starts
	if g.Phase() != PhaseChoosing {
		t.Fatalf("phase = %s, want choosing during the grace", g.Phase())
	}
	g.Advance(r.now.Add(TakeoverGrace))
	if g.Phase() != PhaseMatchEnd {
		t.Fatalf("phase = %s, want match_end once the grace is over and nobody came back", g.Phase())
	}
}

func TestRestoreRejectsABrokenLog(t *testing.T) {
	r := newRecorder(t, 5)
	_ = r.g.Join(r.now, "p0", "p0", false)
	r.commit()
	_ = r.g.Join(r.now, "p1", "p1", false)
	r.commit()
	gap := []StoredEvent{r.events[len(r.events)-1]} // the earlier event is missing
	if _, err := restore(r.snap, gap, rand.New(rand.NewPCG(1, 1))); err == nil || !strings.Contains(err.Error(), "gap") {
		t.Fatalf("err = %v, want a gap error", err)
	}
	phase := []StoredEvent{{Seq: r.snap.Seq + 1, Type: "round_started", Data: []byte(`{}`)}}
	if _, err := restore(r.snap, phase, rand.New(rand.NewPCG(1, 1))); err == nil {
		t.Fatal("accepted a phase event after the snapshot")
	}
}

func TestRecentEvents(t *testing.T) {
	r := newRecorder(t, 6)
	for i := range 5 {
		_ = r.g.Join(r.now, fmt.Sprint("p", i), "x", false)
	}
	evs, ok := r.g.RecentEvents(2)
	if !ok || len(evs) != int(r.g.Seq())-2 || evs[0].Seq != 3 {
		t.Fatalf("got %d events from seq %v, ok %v", len(evs), evs, ok)
	}
	if evs, ok := r.g.RecentEvents(r.g.Seq()); !ok || len(evs) != 0 {
		t.Fatal("up to date: nothing to replay")
	}
	r.g.log = r.g.log[2:] // pretend the buffer dropped the oldest
	if _, ok := r.g.RecentEvents(0); ok {
		t.Fatal("claimed to cover events it no longer has")
	}
}

// Rooms stored before task 2.6a have a single "drawer" and no "drawers". A new server must
// still restore them: the drawer keeps drawing and earns points for the events after the
// snapshot.
func TestRestoreReadsOldSingleDrawerSnapshots(t *testing.T) {
	r := newRecorder(t, 7)
	for _, u := range []string{"p0", "p1", "p2"} {
		_ = r.g.Join(r.now, u, u, false)
	}
	r.g.Handle(r.now, "p0", protocol.Start{})
	r.commit()
	r.now = r.now.Add(StartingFor)
	r.g.Advance(r.now)
	r.commit()
	r.g.Handle(r.now, "p0", protocol.ChooseWord{Index: 0})
	r.commit()
	r.g.Handle(r.now, "p1", protocol.Guess{Text: r.g.word}) // after the snapshot
	r.commit()

	// Rewrite the snapshot the way a pre-2.6a server stored it.
	var old map[string]any
	if err := json.Unmarshal([]byte(snapJSON(t, r.snap)), &old); err != nil {
		t.Fatal(err)
	}
	delete(old, "drawers")
	old["drawer"] = "p0"
	b, _ := json.Marshal(old)
	var s Snapshot
	if err := json.Unmarshal(b, &s); err != nil {
		t.Fatal(err)
	}

	g, err := restore(s, r.events, rand.New(rand.NewPCG(1, 1)))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(g.drawers, []string{"p0"}) || g.Phase() != PhaseDrawing {
		t.Fatalf("drawers = %v, phase = %s", g.drawers, g.Phase())
	}
	if g.players["p0"].score != DrawerPointsPerGuess {
		t.Fatalf("drawer score = %d, want the points of the replayed correct guess", g.players["p0"].score)
	}
	if a, b := snapJSON(t, g.Snapshot()), snapJSON(t, r.g.Snapshot()); a != b {
		t.Fatalf("restored state differs\nrestored: %s\nlive:     %s", a, b)
	}
	g.Handle(r.now, "p0", stroke("s1", 2))
	if len(g.canvas) != 1 {
		t.Fatal("the restored drawer can't draw")
	}
}

// After a takeover, if the first drawer never comes back while choosing, the options pass to
// the second drawer once the grace is over.
func TestRestorePassesTheChoiceAfterTheGrace(t *testing.T) {
	r := newRecorder(t, 8)
	for _, u := range []string{"p0", "p1", "p2", "p3", "p4"} {
		_ = r.g.Join(r.now, u, u, false)
	}
	r.g.Handle(r.now, "p0", protocol.Start{})
	r.commit()
	r.now = r.now.Add(StartingFor)
	r.g.Advance(r.now)
	r.commit()
	if !slices.Equal(r.g.drawers, []string{"p0", "p1"}) {
		t.Fatalf("drawers = %v", r.g.drawers)
	}

	took := r.now.Add(time.Second)
	g, err := RestoreGame(r.snap, r.events, rand.New(rand.NewPCG(5, 5)), took)
	if err != nil {
		t.Fatal(err)
	}
	for _, u := range []string{"p1", "p2", "p3", "p4"} { // everyone but the first drawer
		if err := g.Join(took.Add(time.Second), u, u, false); err != nil {
			t.Fatal(err)
		}
	}
	g.TakeOut()
	g.Advance(took.Add(TakeoverGrace))
	if !slices.Equal(g.drawers, []string{"p1", "p0"}) || g.Phase() != PhaseChoosing {
		t.Fatalf("drawers = %v, phase = %s; want the choice passed to p1", g.drawers, g.Phase())
	}
	got := false
	for _, o := range g.TakeOut() {
		got = got || (o.To == "p1" && o.Type == "word_options")
	}
	if !got {
		t.Fatal("p1 got no word options")
	}
}
