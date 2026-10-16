package room

import (
	"encoding/json"
	"errors"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Bowenn/gap-project_private/services/game-server/internal/protocol"
	"github.com/Bowenn/gap-project_private/services/game-server/internal/words"
)

var t0 = time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)

// table drives a Game with explicit times and records everything it produced.
type table struct {
	t   *testing.T
	g   *Game
	now time.Time
	out []Out
}

func newTable(t *testing.T, s Settings) *table {
	t.Helper()
	return &table{t: t, g: NewGame("r1", "host", s, rand.New(rand.NewPCG(1, 2)), t0), now: t0}
}

func (tb *table) collect() { tb.out = append(tb.out, tb.g.TakeOut()...) }

func (tb *table) join(ids ...string) {
	tb.t.Helper()
	for _, id := range ids {
		if err := tb.g.Join(tb.now, id, "Name "+id); err != nil {
			tb.t.Fatalf("join %s: %v", id, err)
		}
	}
	tb.collect()
}

func (tb *table) leave(id string) { tb.g.Leave(tb.now, id); tb.collect() }

func (tb *table) send(id string, msg protocol.ClientMsg) string {
	r := tb.g.Handle(tb.now, id, msg)
	tb.collect()
	return r
}

func (tb *table) advance(d time.Duration) {
	tb.now = tb.now.Add(d)
	tb.g.Advance(tb.now)
	tb.collect()
}

// toNext jumps to the game's next deadline.
func (tb *table) toNext() {
	tb.t.Helper()
	next := tb.g.NextDeadline()
	if next.IsZero() {
		tb.t.Fatal("no deadline")
	}
	tb.advance(next.Sub(tb.now))
}

// events returns the shared events of a type, in order.
func (tb *table) events(typ string) []map[string]any {
	var out []map[string]any
	for _, o := range tb.out {
		if ev, ok := o.Data.(protocol.Event); ok && ev.Type == typ {
			b, _ := json.Marshal(ev.Data)
			var m map[string]any
			_ = json.Unmarshal(b, &m)
			out = append(out, m)
		}
	}
	return out
}

// private returns the private messages of a type sent to uid.
func (tb *table) private(uid, typ string) []map[string]any {
	var out []map[string]any
	for _, o := range tb.out {
		if o.To == uid && o.Type == typ {
			b, _ := json.Marshal(o.Data)
			var m map[string]any
			_ = json.Unmarshal(b, &m)
			out = append(out, m)
		}
	}
	return out
}

func (tb *table) lastError(uid string) string {
	errs := tb.private(uid, "error")
	if len(errs) == 0 {
		return ""
	}
	return errs[len(errs)-1]["code"].(string)
}

// startRound starts a match and has the drawer pick the first option. It returns the word.
func (tb *table) startRound() string {
	tb.t.Helper()
	tb.send(tb.g.host, protocol.Start{})
	tb.toNext() // Starting -> Choosing
	tb.send(tb.g.drawer, protocol.ChooseWord{Index: 0})
	if tb.g.Phase() != PhaseDrawing {
		tb.t.Fatalf("phase = %s, want drawing", tb.g.Phase())
	}
	return tb.g.word
}

func TestNormalize(t *testing.T) {
	for in, want := range map[string]string{" Ze-Bra! ": "zebra", "APPLE": "apple", "ap ple": "apple", "café": "caf", "": ""} {
		if got := Normalize(in); got != want {
			t.Errorf("Normalize(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestScore(t *testing.T) {
	want := []int{100, 85, 70, 55, 40, 25, 20, 20}
	for i, w := range want {
		if got := Score(i + 1); got != w {
			t.Errorf("Score(%d) = %d, want %d", i+1, got, w)
		}
	}
}

func TestHintsNeverRevealTheWholeWord(t *testing.T) {
	rng := rand.New(rand.NewPCG(3, 4))
	for _, w := range words.List {
		revealed := map[int]bool{}
		for range hintAt {
			revealMore(w, revealed, rng)
		}
		m := mask(w, revealed)
		if strings.Count(m, "_") < len(w)-len(w)/2 || m == w {
			t.Errorf("%s: mask %s shows too much", w, m)
		}
	}
}

func TestFullMatchAndRematch(t *testing.T) {
	tb := newTable(t, DefaultSettings)
	tb.join("host", "b", "c")
	if tb.g.host != "host" {
		t.Fatalf("host = %s", tb.g.host)
	}
	tb.send("host", protocol.Start{})
	if tb.g.Phase() != PhaseStarting || len(tb.events("match_started")) != 1 {
		t.Fatalf("phase = %s", tb.g.Phase())
	}
	drawers := []string{}
	for range 3 {
		tb.toNext() // Starting/Reveal -> Choosing
		if tb.g.Phase() != PhaseChoosing {
			t.Fatalf("phase = %s, want choosing", tb.g.Phase())
		}
		drawers = append(drawers, tb.g.drawer)
		if opts := tb.private(tb.g.drawer, "word_options"); len(opts) == 0 {
			t.Fatal("drawer got no word options")
		}
		tb.toNext() // choose timeout auto-picks options[0]
		if tb.g.Phase() != PhaseDrawing {
			t.Fatalf("phase = %s, want drawing", tb.g.Phase())
		}
		tb.toNext() // hint 1
		tb.toNext() // hint 2
		tb.toNext() // time is up
		if tb.g.Phase() != PhaseReveal {
			t.Fatalf("phase = %s, want reveal", tb.g.Phase())
		}
	}
	tb.toNext() // everyone has drawn
	if tb.g.Phase() != PhaseMatchEnd {
		t.Fatalf("phase = %s, want match_end", tb.g.Phase())
	}
	if !slices.Equal(drawers, []string{"host", "b", "c"}) {
		t.Fatalf("drawers = %v", drawers)
	}
	if n := len(tb.events("hint")); n != 6 {
		t.Fatalf("hints = %d, want 2 per round", n)
	}
	if n := len(tb.events("match_ended")); n != 1 {
		t.Fatalf("match_ended = %d", n)
	}
	// Rematch from MatchEnd.
	tb.send("host", protocol.Start{})
	if tb.g.Phase() != PhaseStarting || len(tb.events("match_started")) != 2 {
		t.Fatalf("rematch: phase = %s", tb.g.Phase())
	}
}

func TestChooseTimeoutPicksTheFirstOption(t *testing.T) {
	tb := newTable(t, DefaultSettings)
	tb.join("host", "b")
	tb.send("host", protocol.Start{})
	tb.toNext()
	opts := tb.private("host", "word_options")[0]["words"].([]any)
	tb.advance(DefaultSettings.Choose)
	if got := tb.private("host", "your_word"); len(got) != 1 || got[0]["word"] != opts[0] {
		t.Fatalf("your_word = %v, options = %v", got, opts)
	}
}

func TestScoringAndEarlyEndWhenAllGuessed(t *testing.T) {
	tb := newTable(t, DefaultSettings)
	tb.join("host", "b", "c")
	word := tb.startRound()
	if r := tb.send("b", protocol.Guess{Text: strings.ToUpper(word) + "!"}); r != GuessCorrect {
		t.Fatalf("result = %s", r)
	}
	if tb.g.Phase() != PhaseDrawing {
		t.Fatal("round ended before everyone guessed")
	}
	tb.send("c", protocol.Guess{Text: word})
	if tb.g.Phase() != PhaseReveal {
		t.Fatalf("phase = %s, want reveal once all guessed", tb.g.Phase())
	}
	pts := map[string]int{}
	for _, p := range tb.g.players {
		pts[p.id] = p.score
	}
	if pts["b"] != 100 || pts["c"] != 85 || pts["host"] != 20 {
		t.Fatalf("scores = %v", pts)
	}
	ended := tb.events("round_ended")[0]
	if ended["word"] != word {
		t.Fatalf("round_ended = %v", ended)
	}
}

func TestRoundEndsWhenTheLastGuesserLeaves(t *testing.T) {
	tb := newTable(t, DefaultSettings)
	tb.join("host", "b", "c")
	word := tb.startRound()
	tb.send("b", protocol.Guess{Text: word})
	tb.leave("c")
	if tb.g.Phase() != PhaseReveal {
		t.Fatalf("phase = %s, want reveal", tb.g.Phase())
	}
}

func TestDrawerLeaving(t *testing.T) {
	t.Run("while drawing ends the round", func(t *testing.T) {
		tb := newTable(t, DefaultSettings)
		tb.join("host", "b", "c")
		tb.startRound()
		tb.leave("host")
		if tb.g.Phase() != PhaseReveal {
			t.Fatalf("phase = %s", tb.g.Phase())
		}
	})
	t.Run("while choosing picks the next drawer", func(t *testing.T) {
		tb := newTable(t, DefaultSettings)
		tb.join("host", "b", "c")
		tb.send("host", protocol.Start{})
		tb.toNext()
		tb.leave("host")
		if tb.g.Phase() != PhaseChoosing || tb.g.drawer != "b" {
			t.Fatalf("phase = %s, drawer = %s", tb.g.Phase(), tb.g.drawer)
		}
		if len(tb.events("round_ended")) != 0 {
			t.Fatal("a round without a word was revealed")
		}
	})
	t.Run("leaving one player ends the match", func(t *testing.T) {
		tb := newTable(t, DefaultSettings)
		tb.join("host", "b")
		tb.startRound()
		tb.leave("b")
		if tb.g.Phase() != PhaseMatchEnd || len(tb.events("round_ended")) != 1 || len(tb.events("match_ended")) != 1 {
			t.Fatalf("phase = %s", tb.g.Phase())
		}
	})
}

func TestStartRules(t *testing.T) {
	tb := newTable(t, DefaultSettings)
	tb.join("host")
	tb.send("host", protocol.Start{})
	if tb.lastError("host") != "not_enough_players" {
		t.Fatalf("error = %q", tb.lastError("host"))
	}
	tb.join("b")
	tb.send("b", protocol.Start{})
	if tb.lastError("b") != "not_host" || tb.g.Phase() != PhaseWaiting {
		t.Fatalf("error = %q", tb.lastError("b"))
	}
}

func TestHostHandover(t *testing.T) {
	tb := newTable(t, DefaultSettings)
	tb.join("b", "host", "c") // b joins before the lobby's host and takes hosting
	if tb.g.host != "b" {
		t.Fatalf("host = %s", tb.g.host)
	}
	tb.leave("b")
	if tb.g.host != "host" || tb.events("host_changed")[1]["user_id"] != "host" {
		t.Fatalf("host = %s", tb.g.host)
	}
	tb.join("b") // coming back doesn't take it back
	if tb.g.host != "host" {
		t.Fatalf("host = %s", tb.g.host)
	}
}

func TestSeatsAndReconnects(t *testing.T) {
	tb := newTable(t, Settings{MaxPlayers: 2, Choose: 10 * time.Second, Draw: 80 * time.Second, WordOptions: 3})
	tb.join("host", "b")
	if err := tb.g.Join(tb.now, "c", "C"); !errors.Is(err, ErrRoomFull) {
		t.Fatalf("err = %v, want ErrRoomFull", err)
	}
	tb.leave("b")
	if err := tb.g.Join(tb.now, "c", "C"); !errors.Is(err, ErrRoomFull) {
		t.Fatal("a disconnected player's seat was given away during the grace period")
	}
	tb.join("b") // reconnecting is always allowed
	tb.leave("b")
	tb.advance(SeatGrace)
	if len(tb.events("player_removed")) != 1 {
		t.Fatal("seat not freed after the grace period")
	}
	tb.join("c")
}

func TestEmptyRoomCloses(t *testing.T) {
	tb := newTable(t, DefaultSettings)
	tb.advance(EmptyTTL - time.Second)
	tb.join("host")
	tb.advance(time.Hour)
	if tb.g.Closed() {
		t.Fatal("closed with a player inside")
	}
	tb.leave("host")
	tb.advance(EmptyTTL - time.Second)
	if tb.g.Closed() {
		t.Fatal("closed too early")
	}
	tb.toNext()
	if !tb.g.Closed() {
		t.Fatal("empty room didn't close")
	}
}

func TestGuessHandling(t *testing.T) {
	tb := newTable(t, DefaultSettings)
	tb.join("host", "b", "c")
	word := tb.startRound()

	// Contains the word but isn't it: private answer only.
	tb.send("b", protocol.Guess{Text: "my " + word + " guess"})
	for _, c := range tb.events("chat") {
		if strings.Contains(strings.ToLower(c["text"].(string)), word) {
			t.Fatalf("chat leaked the word: %v", c)
		}
	}
	// The drawer can't chat while drawing.
	tb.send("host", protocol.Guess{Text: "hello"})
	if tb.lastError("host") != "cannot_chat_now" {
		t.Fatalf("drawer chat: %q", tb.lastError("host"))
	}
	// A wrong guess is chat.
	tb.send("c", protocol.Guess{Text: "zzz"})
	if chats := tb.events("chat"); len(chats) != 1 || chats[0]["text"] != "zzz" {
		t.Fatalf("chats = %v", chats)
	}
	// Someone who guessed right can't chat any more this round.
	tb.send("b", protocol.Guess{Text: word})
	tb.send("b", protocol.Guess{Text: "it's a hint"})
	if tb.lastError("b") != "cannot_chat_now" {
		t.Fatalf("guessed chat: %q", tb.lastError("b"))
	}
}

func TestRateLimitedGuessesAreNotProcessed(t *testing.T) {
	tb := newTable(t, DefaultSettings)
	tb.join("host", "b", "c")
	word := tb.startRound()
	for range guessBurst {
		tb.send("b", protocol.Guess{Text: "zzz"})
	}
	if r := tb.send("b", protocol.Guess{Text: word}); r != GuessRateLimited {
		t.Fatalf("result = %s", r)
	}
	if len(tb.events("correct_guess")) != 0 || len(tb.private("b", "rate_limited")) != 1 {
		t.Fatal("a rate-limited guess was processed")
	}
	tb.advance(time.Second)
	if r := tb.send("b", protocol.Guess{Text: word}); r != GuessCorrect {
		t.Fatalf("after refill: %s", r)
	}
}

func stroke(id string, n int) protocol.StrokeBatch {
	pts := make([][]int, n)
	for i := range pts {
		pts[i] = []int{1, 1, 1}
	}
	return protocol.StrokeBatch{StrokeID: id, Points: pts, Color: "#000000", Width: 3}
}

func TestStrokesAndUndo(t *testing.T) {
	tb := newTable(t, DefaultSettings)
	tb.join("host", "b")
	tb.startRound()
	tb.send("b", stroke("s1", 2))
	if tb.lastError("b") != "not_your_turn" {
		t.Fatal("a guesser could draw")
	}
	tb.send("host", stroke("s1", 2))
	tb.send("host", stroke("s1", 2))
	tb.send("host", stroke("s2", 2))
	tb.send("b", protocol.Undo{StrokeID: "s1"})
	if tb.lastError("b") != "bad_request" || len(tb.g.canvas) != 3 {
		t.Fatal("a player undid someone else's stroke")
	}
	tb.send("host", protocol.Undo{StrokeID: "s1"})
	if len(tb.g.canvas) != 1 || tb.g.canvasPoints != 2 || len(tb.events("stroke_undone")) != 1 {
		t.Fatalf("canvas = %d batches, %d points", len(tb.g.canvas), tb.g.canvasPoints)
	}
	if v := tb.g.ViewFor("b"); len(v.Strokes) != 1 {
		t.Fatalf("view strokes = %d", len(v.Strokes))
	}
}

func TestCanvasCap(t *testing.T) {
	tb := newTable(t, DefaultSettings)
	tb.join("host", "b")
	tb.startRound()
	for i := range maxCanvasPoints / protocol.MaxPointsPerMsg {
		tb.send("host", stroke("s"+string(rune('a'+i%26)), protocol.MaxPointsPerMsg))
	}
	tb.send("host", stroke("x", protocol.MaxPointsPerMsg))
	if tb.lastError("host") != "canvas_full" {
		t.Fatalf("error = %q", tb.lastError("host"))
	}
}

func TestViewsHideTheWordWhileDrawing(t *testing.T) {
	tb := newTable(t, DefaultSettings)
	tb.join("host", "b")
	word := tb.startRound()
	for _, uid := range []string{"host", "b"} {
		b, _ := json.Marshal(tb.g.ViewFor(uid))
		if strings.Contains(string(b), word) {
			t.Fatalf("%s's view contains the word: %s", uid, b)
		}
	}
	tb.toNext()
	tb.toNext()
	tb.toNext() // Reveal
	if v := tb.g.ViewFor("b"); v.Word != word {
		t.Fatalf("reveal view word = %q", v.Word)
	}
}

func TestFinishedRoundRecord(t *testing.T) {
	tb := newTable(t, DefaultSettings)
	tb.join("host", "b")
	word := tb.startRound()
	drawStart := tb.now
	tb.advance(time.Second)
	tb.send("host", stroke("s1", 2))
	tb.advance(time.Second)
	tb.send("host", stroke("s1", 3))
	tb.send("host", stroke("s2", 1))
	tb.send("host", protocol.Undo{StrokeID: "s2"})
	tb.advance(500 * time.Millisecond)
	tb.send("host", stroke("s3", 1))
	if len(tb.g.TakeFinished()) != 0 {
		t.Fatal("a running round was reported as finished")
	}
	tb.send("b", protocol.Guess{Text: word}) // the only guesser: the round ends

	f := tb.g.TakeFinished()
	if len(f) != 1 || f[0].Round == nil {
		t.Fatalf("finished = %+v, want one round", f)
	}
	r := *f[0].Round
	if r.MatchID != tb.g.MatchID() || r.RoomID != "r1" || r.RoundNo != 1 || r.Word != word ||
		!slices.Equal(r.DrawerIDs, []string{"host"}) || r.Correct != 1 {
		t.Fatalf("round = %+v", r)
	}
	if !r.DrawStart.Equal(drawStart) || !r.EndedAt.Equal(tb.now) {
		t.Fatalf("draw start %v, ended %v", r.DrawStart, r.EndedAt)
	}
	var ids []string
	for _, b := range r.Canvas {
		ids = append(ids, b.StrokeID)
	}
	if !slices.Equal(ids, []string{"s1", "s1", "s3"}) {
		t.Fatalf("canvas strokes = %v, want the undone s2 left out", ids)
	}
	if got := r.Canvas[1].ReceivedAt.Sub(drawStart); got != 2*time.Second {
		t.Fatalf("second batch received %v after the draw start, want 2s (server clock)", got)
	}
	// The record is a copy: the next round must not change it.
	tb.toNext()
	if len(r.Canvas) != 3 || r.Canvas[0].StrokeID != "s1" {
		t.Fatal("the record changed after the round")
	}
}

// playRound plays one round from Starting or Reveal: the drawer auto-picks, guessers guess,
// and the round runs out of time unless everyone guessed.
func (tb *table) playRound(guessers ...string) {
	tb.t.Helper()
	tb.toNext() // -> Choosing
	tb.toNext() // auto-pick -> Drawing
	for _, id := range guessers {
		tb.send(id, protocol.Guess{Text: tb.g.word})
	}
	for tb.g.Phase() == PhaseDrawing {
		tb.toNext()
	}
}

func TestFinishedMatchRecord(t *testing.T) {
	tb := newTable(t, DefaultSettings)
	tb.join("host", "b", "c", "d")
	tb.send("host", protocol.Start{})
	firstRound := tb.now.Add(StartingFor)
	tb.playRound("b") // host draws: b 100, host 10
	tb.playRound()    // b draws, nobody guesses
	tb.playRound("d") // c draws: d 100, c 10
	tb.playRound()    // d draws
	tb.toNext()       // everyone has drawn
	if tb.g.Phase() != PhaseMatchEnd {
		t.Fatalf("phase = %s", tb.g.Phase())
	}

	f := tb.g.TakeFinished()
	if len(f) != 5 || f[4].Match == nil {
		t.Fatalf("finished = %d items, want 4 rounds then the match", len(f))
	}
	for i, x := range f[:4] {
		if x.Round == nil || x.Round.RoundNo != i+1 {
			t.Fatalf("item %d = %+v, want round %d", i, x, i+1)
		}
	}
	m := *f[4].Match
	if m.MatchID != tb.g.MatchID() || m.RoomID != "r1" || m.Rounds != 4 {
		t.Fatalf("match = %+v", m)
	}
	if !m.StartedAt.Equal(firstRound) || !m.EndedAt.Equal(tb.now) {
		t.Fatalf("started %v (want %v), ended %v", m.StartedAt, firstRound, m.EndedAt)
	}
	want := []PlayerResult{{"b", 100, 1}, {"d", 100, 1}, {"host", 10, 3}, {"c", 10, 3}}
	if !slices.Equal(m.Results, want) {
		t.Fatalf("results = %v, want %v", m.Results, want)
	}
}

func TestEarlyEndsAndAbortedMatches(t *testing.T) {
	t.Run("a match aborted before any round finished is not reported", func(t *testing.T) {
		tb := newTable(t, DefaultSettings)
		tb.join("host", "b")
		tb.send("host", protocol.Start{})
		tb.leave("b") // during the countdown
		if tb.g.Phase() != PhaseMatchEnd || len(tb.events("match_ended")) != 1 {
			t.Fatalf("phase = %s", tb.g.Phase())
		}
		if f := tb.g.TakeFinished(); len(f) != 0 {
			t.Fatalf("finished = %+v, want nothing", f)
		}
	})
	t.Run("the last guesser leaving reports the round and the match", func(t *testing.T) {
		tb := newTable(t, DefaultSettings)
		tb.join("host", "b")
		tb.startRound()
		tb.send("host", stroke("s1", 2))
		tb.leave("b")
		f := tb.g.TakeFinished()
		if len(f) != 2 || f[0].Round == nil || len(f[0].Round.Canvas) != 1 || f[1].Match == nil || f[1].Match.Rounds != 1 {
			t.Fatalf("finished = %+v, want the round then the match", f)
		}
	})
	t.Run("a rematch reports only its own rounds", func(t *testing.T) {
		tb := newTable(t, DefaultSettings)
		tb.join("host", "b")
		tb.send("host", protocol.Start{})
		tb.playRound()
		tb.playRound()
		tb.toNext()
		first := tb.g.TakeFinished()
		tb.send("host", protocol.Start{})
		tb.leave("b")
		if len(first) != 3 || first[2].Match.Rounds != 2 {
			t.Fatalf("first match = %d items", len(first))
		}
		if f := tb.g.TakeFinished(); len(f) != 0 {
			t.Fatalf("aborted rematch reported %+v", f)
		}
	})
}
