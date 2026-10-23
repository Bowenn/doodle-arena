package relay

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"github.com/Bowenn/doodle-arena/services/game-server/internal/protocol"
	"github.com/Bowenn/doodle-arena/services/game-server/internal/room"
	"github.com/Bowenn/doodle-arena/services/game-server/internal/ws"
)

// Viewer guesses (ADR 0019). A room's stream goroutine holds the room's snapshot, so the word,
// and checks each guess against it:
//
//   - correct: the viewer hears so privately, and the guess goes to the room's owner through
//     the room's viewer-guess stream; the owner announces the round's first one (viewer_correct);
//   - containing the word: the viewer hears it's wrong, and it goes nowhere (as a player's);
//   - wrong: the viewer hears so, and it goes to live.guesses.v1 for the top-guesses job.
//
// Neither side effect may hold up the stream's fan-out: correct guesses queue to one forwarder
// goroutine per relay, and wrong ones are produced without waiting.

// Viewers' guess limit: as players', a burst of 5, then 1 per second.
const (
	guessBurst     = 5
	guessPerSecond = 1
	// forwardQueue bounds the correct guesses waiting for Redis. Only a round's first one
	// matters, so a full queue drops the rest.
	forwardQueue = 256
)

// Guess results, for relay.viewer_guesses.
const (
	guessCorrect     = "correct"
	guessWrong       = "wrong"
	guessClose       = "close" // contains the word: never published
	guessNotDrawing  = "not_drawing"
	guessRateLimited = "rate_limited"
	guessAlready     = "already_guessed"
	guessPlayer      = "player" // a player of the room watching it: they guess in the room
)

// Forwarder hands a viewer's correct guess to the room's owner (roomstore.AddViewerGuess).
type Forwarder interface {
	AddViewerGuess(ctx context.Context, roomID string, g room.ViewerGuess) error
}

// Publisher sends a viewer's wrong guess to live.guesses.v1 without waiting (Kafka).
type Publisher interface {
	PublishGuess(ctx context.Context, roomID string, roundNo int, text string, at time.Time)
}

// TopGuesses is a live room's most common wrong guesses over the last window (ADR 0018).
type TopGuesses struct {
	RoundNo     int          `json:"round_no"`
	WindowEndMS int64        `json:"window_end"`
	Guesses     []GuessCount `json:"guesses"`
}

// GuessCount is one guess in a TopGuesses.
type GuessCount struct {
	Text  string `json:"text"`
	Count int    `json:"count"`
}

type guessIn struct {
	conn *ws.Conn
	name string
	text string
	at   time.Time
}

type topIn struct{ top TopGuesses }

type forward struct {
	roomID string
	guess  room.ViewerGuess
}

// guess passes a viewer's guess to the stream (a stream that already stopped drops it).
func (s *stream) guess(conn *ws.Conn, name, text string) {
	select {
	case s.in <- guessIn{conn: conn, name: name, text: text, at: time.Now()}:
	case <-s.done:
	}
}

// onGuess runs in the stream's goroutine.
func (s *stream) onGuess(ctx context.Context, in guessIn) {
	m := s.relay.metrics
	if s.snap.Phase != room.PhaseDrawing || s.snap.Word == "" {
		m.viewerGuess(ctx, guessNotDrawing)
		in.conn.Send(protocol.Marshal("error", protocol.ErrorData{Code: "not_drawing", Message: "guesses count while a round is drawn"}))
		return
	}
	round := s.snap.RoundNo
	key := roundKey(s.snap)
	uid := in.conn.UserID()
	if s.plays(uid) {
		// A drawer knows the word, and every player guesses in the room: their viewer guesses
		// could only hint the word to the top-guesses list.
		m.viewerGuess(ctx, guessPlayer)
		in.conn.Send(protocol.Marshal("error", protocol.ErrorData{Code: "player", Message: "you play in this room: guess there"}))
		return
	}
	if s.guessed[uid] == key {
		// They know the word: their next guesses could hint it to the top-guesses list.
		m.viewerGuess(ctx, guessAlready)
		in.conn.Send(protocol.Marshal("error", protocol.ErrorData{Code: "already_guessed", Message: "you guessed it; wait for the next round"}))
		return
	}
	norm := room.Normalize(in.text)
	switch {
	case norm == s.snap.Word:
		m.viewerGuess(ctx, guessCorrect)
		s.guessed[uid] = key
		in.conn.Send(protocol.Marshal("guess_result", map[string]any{"correct": true}))
		if s.winner == key {
			return // the owner already announced this round's viewer
		}
		select {
		case s.relay.forwards <- forward{roomID: s.id, guess: room.ViewerGuess{UserID: uid, Name: in.name, MatchID: s.snap.MatchID, RoundNo: round}}:
		default:
			m.forwarded(ctx, "dropped")
		}
	case norm == "" || strings.Contains(norm, s.snap.Word):
		// Close but not exact: publishing it would show every viewer the word.
		m.viewerGuess(ctx, guessClose)
		in.conn.Send(protocol.Marshal("guess_result", map[string]any{"correct": false}))
	default:
		m.viewerGuess(ctx, guessWrong)
		in.conn.Send(protocol.Marshal("guess_result", map[string]any{"correct": false}))
		if s.relay.cfg.Publisher != nil {
			s.relay.cfg.Publisher.PublishGuess(ctx, s.id, round, norm, in.at)
		}
	}
}

// onEvent keeps the stream's per-round guess state in step with the room's events.
func (s *stream) onEvent(ev room.StoredEvent) {
	switch {
	case ev.Type == "viewer_correct":
		s.winner = roundKey(s.snap)
	case room.IsPhaseEvent(ev.Type):
		// A new phase: last round's list and who guessed it no longer apply.
		s.top = nil
		if ev.Type == "round_started" {
			clear(s.guessed)
		}
	}
}

// plays reports whether uid has a seat in the room: the snapshot's players, then the joins and
// removals after it.
func (s *stream) plays(uid string) bool {
	seated := false
	for _, p := range s.snap.Players {
		if p.ID == uid {
			seated = true
		}
	}
	for _, ev := range s.events {
		if ev.Type != "player_joined" && ev.Type != "player_removed" {
			continue
		}
		var d struct {
			UserID string `json:"user_id"`
		}
		if json.Unmarshal(ev.Data, &d) == nil && d.UserID == uid {
			seated = ev.Type == "player_joined"
		}
	}
	return seated
}

// roundKey names a round: round numbers start again with every match.
func roundKey(snap room.Snapshot) string { return snap.MatchID + "/" + strconv.Itoa(snap.RoundNo) }

// onTop shows a room's latest top guesses to its viewers, and keeps them for viewers who join.
// A list for another round than the one being drawn is stale (the window closed after the
// round ended) and is dropped.
func (s *stream) onTop(ctx context.Context, t TopGuesses) {
	if s.snap.Phase != room.PhaseDrawing || t.RoundNo != s.snap.RoundNo {
		s.relay.metrics.topGuesses(ctx, "stale")
		return
	}
	if s.top != nil && s.topEnd > t.WindowEndMS {
		s.relay.metrics.topGuesses(ctx, "stale") // an older window, delivered late
		return
	}
	s.relay.metrics.topGuesses(ctx, "shown")
	s.top = protocol.Marshal("top_guesses", t)
	s.topEnd = t.WindowEndMS
	for c := range s.conns {
		c.Send(s.top)
	}
}

// ShowTopGuesses delivers a room's top guesses to its stream on this relay, if it has one.
// It never waits long: a busy stream misses one list and gets the next.
func (r *Relay) ShowTopGuesses(roomID string, t TopGuesses) {
	r.mu.Lock()
	s := r.streams[roomID]
	r.mu.Unlock()
	if s == nil {
		r.metrics.topGuesses(context.Background(), "not_watched")
		return
	}
	timeout := time.NewTimer(100 * time.Millisecond)
	defer timeout.Stop()
	select {
	case s.in <- topIn{top: t}:
	case <-s.done:
	case <-timeout.C:
		r.metrics.topGuesses(context.Background(), "dropped")
	}
}

// forwardGuesses hands correct guesses to their rooms' owners, one at a time, until ctx ends.
func (r *Relay) forwardGuesses(ctx context.Context) {
	for {
		select {
		case f := <-r.forwards:
			wctx, cancel := context.WithTimeout(ctx, 2*time.Second)
			err := r.cfg.Forwarder.AddViewerGuess(wctx, f.roomID, f.guess)
			cancel()
			if err != nil {
				r.metrics.forwarded(ctx, "failed")
				r.cfg.Log.WarnContext(ctx, "viewer's correct guess not forwarded", "room_id", f.roomID,
					"user_id", f.guess.UserID, "err", err)
				continue
			}
			r.metrics.forwarded(ctx, "success")
		case <-ctx.Done():
			return
		}
	}
}
