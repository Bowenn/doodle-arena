package room

import (
	"fmt"
	"slices"
	"testing"

	"github.com/Bowenn/gap-project_private/services/game-server/internal/protocol"
)

// startChoosing starts a match and runs the countdown, so the first round is choosing.
func (tb *table) startChoosing() {
	tb.t.Helper()
	tb.send(tb.g.host, protocol.Start{})
	tb.toNext()
	if tb.g.Phase() != PhaseChoosing {
		tb.t.Fatalf("phase = %s, want choosing", tb.g.Phase())
	}
}

func players(n int) []string {
	ids := []string{"host"}
	for i := 1; i < n; i++ {
		ids = append(ids, fmt.Sprint("p", i))
	}
	return ids
}

// From 4 present players, the next two in the draw order share the canvas; each player still
// draws once, so an odd one out draws alone at the end.
func TestPairing(t *testing.T) {
	for _, tc := range []struct {
		players int
		rounds  [][]string
	}{
		{2, [][]string{{"host"}, {"p1"}}},
		{3, [][]string{{"host"}, {"p1"}, {"p2"}}},
		{4, [][]string{{"host", "p1"}, {"p2", "p3"}}},
		{5, [][]string{{"host", "p1"}, {"p2", "p3"}, {"p4"}}},
		{8, [][]string{{"host", "p1"}, {"p2", "p3"}, {"p4", "p5"}, {"p6", "p7"}}},
	} {
		t.Run(fmt.Sprint(tc.players, " players"), func(t *testing.T) {
			tb := newTable(t, DefaultSettings)
			tb.join(players(tc.players)...)
			tb.send("host", protocol.Start{})
			for tb.g.Phase() != PhaseMatchEnd {
				tb.toNext()
			}
			var got [][]string
			for _, ev := range tb.events("choosing") {
				var ids []string
				for _, id := range ev["drawer_ids"].([]any) {
					ids = append(ids, id.(string))
				}
				got = append(got, ids)
			}
			if !slices.EqualFunc(got, tc.rounds, slices.Equal) {
				t.Fatalf("drawers per round = %v, want %v", got, tc.rounds)
			}
		})
	}
}

// Pairing counts players who are present when the round starts, not seats.
func TestPairingCountsPresentPlayers(t *testing.T) {
	tb := newTable(t, DefaultSettings)
	tb.join(players(4)...)
	tb.send("host", protocol.Start{})
	tb.leave("p3") // during the countdown: 3 present
	tb.toNext()
	if !slices.Equal(tb.g.drawers, []string{"host"}) {
		t.Fatalf("drawers = %v, want one drawer with 3 present", tb.g.drawers)
	}
}

func TestTwoDrawersShareTheCanvas(t *testing.T) {
	tb := newTable(t, DefaultSettings)
	tb.join(players(5)...)
	tb.startChoosing()

	// Only the first drawer gets the options and may choose; neither drawer can chat.
	if len(tb.private("host", "word_options")) != 1 || len(tb.private("p1", "word_options")) != 0 {
		t.Fatal("word options must go to the first drawer only")
	}
	tb.send("p1", protocol.ChooseWord{Index: 0})
	if tb.lastError("p1") != "not_your_turn" || tb.g.Phase() != PhaseChoosing {
		t.Fatal("the second drawer chose the word")
	}
	for _, d := range []string{"host", "p1"} {
		tb.send(d, protocol.Guess{Text: "hello"})
		if tb.lastError(d) != "cannot_chat_now" {
			t.Fatalf("%s chatted while choosing", d)
		}
	}
	tb.send("host", protocol.ChooseWord{Index: 0})
	word := tb.g.word

	// Both drawers get the word, and the view names both.
	for _, d := range []string{"host", "p1"} {
		if yw := tb.private(d, "your_word"); len(yw) != 1 || yw[0]["word"] != word {
			t.Fatalf("%s got your_word %v", d, yw)
		}
		tb.send(d, protocol.Guess{Text: "hello"})
		if tb.lastError(d) != "cannot_chat_now" {
			t.Fatalf("%s chatted while drawing", d)
		}
	}
	if v := tb.g.ViewFor("p2"); !slices.Equal(v.DrawerIDs, []string{"host", "p1"}) {
		t.Fatalf("view drawer_ids = %v", v.DrawerIDs)
	}
	if len(tb.private("p2", "your_word")) != 0 {
		t.Fatal("a guesser got the word")
	}

	// Both draw, with the same stroke ids: strokes are keyed by user and stroke id.
	tb.send("host", stroke("s1", 2))
	tb.send("p1", stroke("s1", 3))
	tb.send("p1", stroke("s2", 1))
	tb.send("p2", stroke("s9", 1))
	if tb.lastError("p2") != "not_your_turn" || len(tb.g.canvas) != 3 {
		t.Fatalf("canvas = %d batches; a guesser could draw", len(tb.g.canvas))
	}

	// Undo removes only the sender's own stroke, even when the other drawer used the same id.
	tb.send("host", protocol.Undo{StrokeID: "s2"})
	if tb.lastError("host") != "bad_request" || len(tb.g.canvas) != 3 {
		t.Fatal("a drawer undid the other drawer's stroke")
	}
	tb.send("host", protocol.Undo{StrokeID: "s1"})
	if len(tb.g.canvas) != 2 || tb.g.canvasPoints != 4 {
		t.Fatalf("canvas = %d batches, %d points", len(tb.g.canvas), tb.g.canvasPoints)
	}
	for _, b := range tb.g.canvas {
		if b.UserID != "p1" {
			t.Fatalf("left on the canvas: %+v", b)
		}
	}
	undone := tb.events("stroke_undone")
	if len(undone) != 1 || undone[0]["user_id"] != "host" || undone[0]["stroke_id"] != "s1" {
		t.Fatalf("stroke_undone = %v", undone)
	}
}

func TestPairScoring(t *testing.T) {
	tb := newTable(t, DefaultSettings)
	tb.join(players(5)...) // host and p1 draw; p2, p3 and p4 guess
	word := tb.startRound()
	for _, g := range []string{"p2", "p3", "p4"} {
		tb.send(g, protocol.Guess{Text: word})
	}
	if tb.g.Phase() != PhaseReveal {
		t.Fatalf("phase = %s: every guesser was correct, the round should end", tb.g.Phase())
	}
	rp := tb.g.roundPoints
	want := map[string]int{"p2": 100, "p3": 85, "p4": 70, "host": 30, "p1": 30}
	for id, pts := range want {
		if rp[id] != pts || tb.g.players[id].score != pts {
			t.Fatalf("%s: round points %d, score %d, want %d", id, rp[id], tb.g.players[id].score, pts)
		}
	}
	r := tb.g.TakeFinished()[0].Round
	if !slices.Equal(r.DrawerIDs, []string{"host", "p1"}) || r.Correct != 3 {
		t.Fatalf("finished round = %+v", r)
	}
}

func TestPairLeaving(t *testing.T) {
	t.Run("the first drawer while choosing passes the options on", func(t *testing.T) {
		tb := newTable(t, DefaultSettings)
		tb.join(players(5)...)
		tb.startChoosing()
		tb.advance(DefaultSettings.Choose / 2)
		tb.leave("host")

		ch := tb.events("choosing")
		if len(ch) != 2 || ch[1]["round_no"] != float64(1) {
			t.Fatalf("choosing events = %v, want a second one for round 1", ch)
		}
		if ids := ch[1]["drawer_ids"].([]any); len(ids) != 2 || ids[0] != "p1" || ids[1] != "host" {
			t.Fatalf("drawer_ids = %v, want p1 first, host still a drawer", ids)
		}
		if want := tb.now.Add(DefaultSettings.Choose); !tb.g.deadline.Equal(want) {
			t.Fatalf("deadline = %v, want the full choosing time from the hand-over (%v)", tb.g.deadline, want)
		}
		if len(tb.private("p1", "word_options")) != 1 {
			t.Fatal("the second drawer got no options")
		}
		tb.send("p1", protocol.ChooseWord{Index: 1})
		if tb.g.Phase() != PhaseDrawing {
			t.Fatalf("phase = %s", tb.g.Phase())
		}

		// The one who left saw the options, so they come back as a drawer, not a guesser.
		before := len(tb.private("host", "your_word"))
		tb.join("host")
		if yw := tb.private("host", "your_word"); len(yw) != before+1 || yw[before]["word"] != tb.g.word {
			t.Fatalf("returning drawer got %v", yw)
		}
		tb.send("host", protocol.Guess{Text: tb.g.word})
		if tb.lastError("host") != "cannot_chat_now" || tb.g.correct != 0 {
			t.Fatal("a drawer scored a guess")
		}
	})
	t.Run("the first drawer while choosing, with the second gone too, starts the next round", func(t *testing.T) {
		tb := newTable(t, DefaultSettings)
		tb.join(players(6)...)
		tb.startChoosing()
		tb.leave("p1") // the second drawer: nothing changes yet
		if len(tb.events("choosing")) != 1 || tb.g.chooser() != "host" {
			t.Fatal("the second drawer leaving changed the choice")
		}
		tb.leave("host")
		ch := tb.events("choosing")
		if len(ch) != 2 || ch[1]["round_no"] != float64(2) || !slices.Equal(tb.g.drawers, []string{"p2", "p3"}) {
			t.Fatalf("choosing = %v, drawers = %v; want round 2 drawn by p2 and p3", ch, tb.g.drawers)
		}
	})
	t.Run("one drawer while drawing leaves the round running", func(t *testing.T) {
		tb := newTable(t, DefaultSettings)
		tb.join(players(5)...)
		word := tb.startRound()
		tb.leave("host")
		if tb.g.Phase() != PhaseDrawing {
			t.Fatalf("phase = %s: the other drawer is still here", tb.g.Phase())
		}
		tb.send("p1", stroke("s1", 2))
		if len(tb.g.canvas) != 1 {
			t.Fatal("the remaining drawer can't draw")
		}
		for _, g := range []string{"p2", "p3", "p4"} {
			tb.send(g, protocol.Guess{Text: word})
		}
		if tb.g.Phase() != PhaseReveal || tb.g.roundPoints["host"] != 30 {
			t.Fatalf("phase = %s, host round points %d (a drawer who left still earns)", tb.g.Phase(), tb.g.roundPoints["host"])
		}
	})
	t.Run("both drawers while drawing ends the round", func(t *testing.T) {
		tb := newTable(t, DefaultSettings)
		tb.join(players(5)...)
		tb.startRound()
		tb.leave("p1")
		tb.leave("host")
		if tb.g.Phase() != PhaseReveal || len(tb.events("round_ended")) != 1 {
			t.Fatalf("phase = %s", tb.g.Phase())
		}
	})
}
