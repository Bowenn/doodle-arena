package relay

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Bowenn/doodle-arena/services/game-server/internal/room"
	"github.com/Bowenn/doodle-arena/services/game-server/internal/ticket"
	"github.com/coder/websocket"
)

// recordingPublisher stands in for Kafka: the wrong guesses the relay published.
type recordingPublisher struct {
	mu  sync.Mutex
	got []string // "<room>/<round>/<text>"
}

func (p *recordingPublisher) PublishGuess(_ context.Context, roomID string, roundNo int, text string, _ time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.got = append(p.got, roomID+"/"+itoa(roundNo)+"/"+text)
}

func (p *recordingPublisher) all() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.got...)
}

func itoa(n int) string { return strconv.Itoa(n) }

func (v *viewer) send(typ string, data any) {
	v.t.Helper()
	b, _ := json.Marshal(map[string]any{"type": typ, "data": data})
	if err := v.c.Write(v.t.Context(), websocket.MessageText, b); err != nil {
		v.t.Fatal(err)
	}
}

// until reads until a message of typ and returns it (events and other messages are skipped).
func (v *viewer) until(typ string) msg {
	v.t.Helper()
	for {
		if m := v.next(); m.Type == typ {
			return m
		}
	}
}

func (v *viewer) guess(text string) msg {
	v.t.Helper()
	v.send("guess", map[string]string{"text": text})
	for {
		switch m := v.next(); m.Type {
		case "guess_result", "error", "rate_limited":
			return m
		}
	}
}

func correct(t *testing.T, m msg) bool {
	t.Helper()
	if m.Type != "guess_result" {
		t.Fatalf("got %s %s, want guess_result", m.Type, m.Data)
	}
	var d struct {
		Correct bool `json:"correct"`
	}
	_ = json.Unmarshal(m.Data, &d)
	return d.Correct
}

func errCode(m msg) string {
	var d struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal(m.Data, &d)
	return d.Code
}

// ADR 0019: the relay checks a viewer's guess against the word it holds. Wrong ones go to the
// top-guesses job, close ones nowhere, a correct one to the room's owner; the word never leaves.
func TestAViewersGuessesGoWhereTheyBelong(t *testing.T) {
	e := newEnv(t)
	o := newOwner(t, e.store, "r1")
	word, _ := o.drawing()
	v := e.watch("v1", "r1", ticket.WatchAudience, 0)
	v.until("welcome")

	if correct(t, v.guess("Zebra!")) {
		t.Fatal("a wrong guess was correct")
	}
	if correct(t, v.guess(word+"s")) {
		t.Fatal("a guess containing the word was correct")
	}
	if !correct(t, v.guess(" "+strings.ToUpper(word)+"!")) {
		t.Fatal("the word, in capitals and with punctuation, wasn't correct")
	}
	// Knowing the word, the viewer can't guess again this round (their misses would hint it).
	if m := v.guess("tiger"); m.Type != "error" || errCode(m) != "already_guessed" {
		t.Fatalf("a guess after guessing it: %s %s", m.Type, m.Data)
	}

	match := o.g.Snapshot().MatchID
	waitFor(t, func() bool {
		gs, _, err := e.store.ViewerGuesses("r1").Next(t.Context(), "")
		return err == nil && len(gs) == 1 && gs[0] == (room.ViewerGuess{UserID: "v1", Name: "Viewer v1", MatchID: match, RoundNo: 1})
	})
	if got := e.pub.all(); len(got) != 1 || got[0] != "r1/1/zebra" {
		t.Fatalf("published %v, want only the normalized wrong guess", got)
	}
	for _, g := range e.pub.all() {
		if strings.Contains(g, word) {
			t.Fatalf("the word reached Kafka: %s", g)
		}
	}
	for result, want := range map[string]int64{"wrong": 1, "close": 1, "correct": 1, "already_guessed": 1} {
		if n := e.counter("relay.viewer_guesses", "result", result); n != want {
			t.Errorf("relay.viewer_guesses{result=%s} = %d, want %d", result, n, want)
		}
	}
}

func TestGuessesCountOnlyWhileARoundIsDrawn(t *testing.T) {
	e := newEnv(t)
	o := newOwner(t, e.store, "r1")
	o.step(func(g *room.Game, now time.Time) { _ = g.Join(now, "a", "Ada", false) })
	v := e.watch("v1", "r1", ticket.WatchAudience, 0)
	v.until("welcome")
	if m := v.guess("cat"); m.Type != "error" || errCode(m) != "not_drawing" {
		t.Fatalf("a guess in the waiting room: %s %s", m.Type, m.Data)
	}
	if got := e.pub.all(); len(got) != 0 {
		t.Fatalf("published %v outside a round", got)
	}
}

func TestViewersGuessesAreRateLimited(t *testing.T) {
	e := newEnv(t)
	o := newOwner(t, e.store, "r1")
	o.drawing()
	v := e.watch("v1", "r1", ticket.WatchAudience, 0)
	v.until("welcome")
	limited := 0
	for range guessBurst + 3 {
		if v.guess("nope").Type == "rate_limited" {
			limited++
		}
	}
	if limited == 0 {
		t.Fatal("no guess was rate limited")
	}
}

// Once the owner announced the round's viewer, the relay stops forwarding correct guesses: the
// owner would only ignore them.
func TestCorrectGuessesStopGoingToTheOwnerOnceAViewerWon(t *testing.T) {
	e := newEnv(t)
	o := newOwner(t, e.store, "r1")
	word, _ := o.drawing()
	first := e.watch("v1", "r1", ticket.WatchAudience, 0)
	first.until("welcome")
	if !correct(t, first.guess(word)) {
		t.Fatal("not correct")
	}
	waitFor(t, func() bool { return e.counter("relay.correct_guesses.forwarded", "result", "success") == 1 })
	match := o.g.Snapshot().MatchID
	o.step(func(g *room.Game, _ time.Time) { g.ViewerCorrect(match, 1, "v1", "Viewer v1") })
	for { // viewer_correct reached the relay
		m := first.until("event")
		var ev event
		_ = json.Unmarshal(m.Data, &ev)
		if ev.Type == "viewer_correct" {
			break
		}
	}

	second := e.watch("v2", "r1", ticket.WatchAudience, 0)
	if w := second.until("welcome"); !strings.Contains(string(w.Data), `"viewer_winner"`) {
		t.Fatalf("a welcome after the announcement doesn't name the winner: %s", w.Data)
	}
	if !correct(t, second.guess(word)) {
		t.Fatal("the second viewer's guess wasn't correct")
	}
	time.Sleep(100 * time.Millisecond)
	if n := e.counter("relay.correct_guesses.forwarded", "result", "success"); n != 1 {
		t.Fatalf("%d correct guesses forwarded, want only the first", n)
	}
}

// ADR 0018 and 0019: a room's top guesses reach its viewers, the latest list greets a viewer
// who joins, and another round's list is dropped.
func TestTopGuessesReachTheRoomsViewers(t *testing.T) {
	e := newEnv(t)
	o := newOwner(t, e.store, "r1")
	o.drawing()
	v := e.watch("v1", "r1", ticket.WatchAudience, 0)
	v.until("welcome")

	e.relay.ShowTopGuesses("r1", TopGuesses{RoundNo: 1, WindowEndMS: 1000, Guesses: []GuessCount{{Text: "dog", Count: 3}}})
	m := v.until("top_guesses")
	var top TopGuesses
	if err := json.Unmarshal(m.Data, &top); err != nil || top.RoundNo != 1 || len(top.Guesses) != 1 || top.Guesses[0] != (GuessCount{Text: "dog", Count: 3}) {
		t.Fatalf("top_guesses = %s", m.Data)
	}
	e.relay.ShowTopGuesses("r1", TopGuesses{RoundNo: 7, WindowEndMS: 2000}) // not this round
	e.relay.ShowTopGuesses("r1", TopGuesses{RoundNo: 1, WindowEndMS: 500})  // an older window
	e.relay.ShowTopGuesses("nobody-watches", TopGuesses{RoundNo: 1})
	waitFor(t, func() bool { return e.counter("relay.top_guesses.received", "result", "stale") == 2 })
	if n := e.counter("relay.top_guesses.received", "result", "not_watched"); n != 1 {
		t.Fatalf("not_watched = %d", n)
	}

	late := e.watch("v2", "r1", ticket.WatchAudience, 0)
	late.until("welcome")
	if m := late.until("top_guesses"); !strings.Contains(string(m.Data), `"dog"`) {
		t.Fatalf("a viewer who joined got %s", m.Data)
	}
}

// The review's two holes: a player watching their own room (the drawer knows the word) can't
// guess as a viewer, and reconnecting doesn't let a viewer who got it guess again.
func TestPlayersAndViewersWhoKnowTheWordCantFeedTheList(t *testing.T) {
	e := newEnv(t)
	o := newOwner(t, e.store, "r1")
	word, drawer := o.drawing()

	d := e.watch(drawer, "r1", ticket.WatchAudience, 0)
	d.until("welcome")
	if m := d.guess(word[:len(word)-1]); m.Type != "error" || errCode(m) != "player" {
		t.Fatalf("the drawer's viewer guess: %s %s", m.Type, m.Data)
	}

	v := e.watch("v1", "r1", ticket.WatchAudience, 0)
	v.until("welcome")
	if !correct(t, v.guess(word)) {
		t.Fatal("not correct")
	}
	_ = v.c.CloseNow()
	again := e.watch("v1", "r1", ticket.WatchAudience, 0)
	again.until("welcome")
	if m := again.guess("tiger"); m.Type != "error" || errCode(m) != "already_guessed" {
		t.Fatalf("a viewer who got it, after reconnecting: %s %s", m.Type, m.Data)
	}
	if got := e.pub.all(); len(got) != 0 {
		t.Fatalf("published %v, want nothing from people who know the word", got)
	}
}
