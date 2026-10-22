package room

import (
	"testing"
	"time"

	"github.com/Bowenn/gap-project_private/services/game-server/internal/protocol"
)

// ADR 0019: the owner announces the round's first correct viewer, once.
func TestTheRoundsFirstCorrectViewerIsAnnounced(t *testing.T) {
	tb := newTable(t, DefaultSettings)
	tb.join("host", "b")
	tb.startRound()
	round := tb.g.RoundNo()

	if got := tb.g.ViewerCorrect(tb.g.matchID, round, "v1", "Vee"); got != ViewerAnnounced {
		t.Fatalf("first viewer: %s", got)
	}
	tb.collect()
	evs := tb.events("viewer_correct")
	if len(evs) != 1 || evs[0]["user_id"] != "v1" || evs[0]["name"] != "Vee" || evs[0]["round_no"] != float64(round) {
		t.Fatalf("viewer_correct = %v", evs)
	}
	if got := tb.g.ViewerCorrect(tb.g.matchID, round, "v2", "Two"); got != ViewerIgnored {
		t.Fatalf("second viewer: %s, want ignored", got)
	}
	if got := tb.g.ViewerCorrect(tb.g.matchID, round, "v1", "Vee"); got != ViewerIgnored {
		t.Fatalf("the winner again: %s, want ignored", got)
	}
	tb.collect()
	if n := len(tb.events("viewer_correct")); n != 1 {
		t.Fatalf("%d viewer_correct events, want 1", n)
	}
	// Everyone who joins now sees who it was, players and viewers alike.
	if w := tb.g.ViewFor("").ViewerWinner; w == nil || w.UserID != "v1" || w.Name != "Vee" {
		t.Fatalf("view's viewer_winner = %+v", w)
	}

	// The next round starts without a winner.
	tb.advance(DefaultSettings.Draw) // the round ends
	tb.toNext()                      // reveal over: the next round's choosing
	tb.send(tb.g.chooser(), protocol.ChooseWord{Index: 0})
	if tb.g.Phase() != PhaseDrawing || tb.g.RoundNo() != round+1 {
		t.Fatalf("phase %s round %d, want the next round drawing", tb.g.Phase(), tb.g.RoundNo())
	}
	if w := tb.g.ViewFor("").ViewerWinner; w != nil {
		t.Fatalf("a new round starts with viewer_winner %+v", w)
	}
	if got := tb.g.ViewerCorrect(tb.g.matchID, round+1, "v2", "Two"); got != ViewerAnnounced {
		t.Fatalf("the next round's first viewer: %s", got)
	}
}

func TestViewerGuessesThatDontCount(t *testing.T) {
	tb := newTable(t, DefaultSettings)
	tb.join("host", "b")
	if got := tb.g.ViewerCorrect(tb.g.matchID, 0, "v1", "Vee"); got != ViewerIgnored {
		t.Fatalf("before any round: %s", got)
	}
	tb.startRound()
	round := tb.g.RoundNo()
	cases := []struct {
		name    string
		roundNo int
		uid     string
	}{
		{"a stale round's guess (a relay behind, or a takeover replaying the stream)", round - 1, "v1"},
		{"a later round's guess", round + 1, "v1"},
		{"a player of this room watching it too (a drawer knows the word)", round, "host"},
		{"no user", round, ""},
	}
	for _, c := range cases {
		if got := tb.g.ViewerCorrect(tb.g.matchID, c.roundNo, c.uid, "x"); got != ViewerIgnored {
			t.Errorf("%s: %s, want ignored", c.name, got)
		}
	}
	// The same round number of an earlier match: round numbers start again with every match,
	// and a new owner reads every guess the stream still holds.
	if got := tb.g.ViewerCorrect("an-earlier-match", round, "v1", "Vee"); got != ViewerIgnored {
		t.Errorf("an earlier match's round %d: %s, want ignored", round, got)
	}
	tb.advance(DefaultSettings.Draw + time.Millisecond) // the round ends: reveal
	if got := tb.g.ViewerCorrect(tb.g.matchID, round, "v1", "Vee"); got != ViewerIgnored {
		t.Fatalf("after the round ended: %s", got)
	}
	tb.collect()
	if n := len(tb.events("viewer_correct")); n != 0 {
		t.Fatalf("%d viewer_correct events, want none", n)
	}
}
