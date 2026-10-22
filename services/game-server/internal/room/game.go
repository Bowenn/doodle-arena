package room

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
	"time"

	"github.com/Bowenn/gap-project_private/services/game-server/internal/protocol"
	"github.com/Bowenn/gap-project_private/services/game-server/internal/ratelimit"
	"github.com/Bowenn/gap-project_private/services/game-server/internal/words"
)

// Phase is where a room is in its lifecycle (design doc, section 7.3).
type Phase string

// Phases.
const (
	PhaseWaiting  Phase = "waiting"
	PhaseStarting Phase = "starting"
	PhaseChoosing Phase = "choosing"
	PhaseDrawing  Phase = "drawing"
	PhaseReveal   Phase = "reveal"
	PhaseMatchEnd Phase = "match_end"
)

// Playing reports whether a match is running.
func (p Phase) Playing() bool {
	return p == PhaseStarting || p == PhaseChoosing || p == PhaseDrawing || p == PhaseReveal
}

// ErrRoomFull means every seat is taken (seats include players within their rejoin grace).
var ErrRoomFull = errors.New("room is full")

// Guess results, for the game.guesses metric.
const (
	GuessCorrect     = "correct"
	GuessWrong       = "wrong"
	GuessRateLimited = "rate_limited"
)

// Results of a viewer's correct guess reaching the owner, for the game.viewer_correct metric.
const (
	ViewerAnnounced = "announced" // the round's first correct viewer
	ViewerIgnored   = "ignored"   // the round already had one, it ended, or the viewer plays here
)

// ViewerWinner is the first viewer of a live room to guess a round's word (ADR 0019).
type ViewerWinner struct {
	UserID string `json:"user_id"`
	Name   string `json:"name"`
}

// Guess limit per player: a burst of 5, then 1 per second.
const (
	guessBurst     = 5
	guessPerSecond = 1
)

// eventLogSize is how many recent shared events the room keeps in memory, for replays to
// resuming clients. The durable log is the Redis Stream room:{id}:events (package roomstore);
// a new owner refills this buffer from it.
const eventLogSize = 1024

// Out is one message the room wants delivered.
type Out struct {
	To     string // one player, or "" for every connected player
	Except string // with To == "": skip this player (they get a welcome instead)
	Type   string // "event" for shared events, otherwise a private message type
	Data   any    // protocol.Event for shared events
	Seq    int64  // for a welcome: the seq it is current up to, so older events are skipped
}

// FinishedRound is a round that ended, as the room hands it to the event publisher. It is a
// copy: the game never changes it after endRound.
type FinishedRound struct {
	MatchID   string
	RoomID    string
	RoundNo   int
	Word      string // safe to publish: the round is over
	DrawerIDs []string
	Correct   int
	Canvas    []CanvasBatch // in the order the room processed them; undone strokes are gone
	DrawStart time.Time     // when the drawing phase started
	EndedAt   time.Time
}

// FinishedMatch is a match that ended after at least one finished round.
type FinishedMatch struct {
	MatchID   string
	RoomID    string
	Mode      string    // the room's mode; the actor fills it in
	StartedAt time.Time // when the first round started
	EndedAt   time.Time
	Rounds    int
	Results   []PlayerResult // ordered by placement
}

// PlayerResult is one player's final result in a match.
type PlayerResult struct {
	UserID    string
	Score     int
	Placement int // competition ranking: equal scores share a placement
}

// Finished is a finished round or match: exactly one of the two is set.
type Finished struct {
	Round *FinishedRound
	Match *FinishedMatch
}

type player struct {
	id        string
	name      string
	joinOrder int
	connected bool
	leftAt    time.Time
	score     int
	guessed   bool
	bucket    ratelimit.Bucket
}

// CanvasBatch is one stroke batch on the round's canvas.
type CanvasBatch struct {
	UserID string `json:"user_id"`
	protocol.StrokeBatch
	ReceivedAt time.Time `json:"-"` // server clock; never sent to clients
}

// Game is a room's state and rules. It does no I/O and keeps no clock: every method gets
// the time, and messages to send pile up in Out. Only the room's actor goroutine calls it.
type Game struct {
	roomID   string
	settings Settings
	hostID   string // the host the lobby named
	host     string // the current host
	rng      *rand.Rand
	picker   *words.Picker

	players  map[string]*player
	nextJoin int

	phase    Phase
	deadline time.Time // end of the current phase; zero when it has none

	matchID   string
	drawOrder []string
	drawn     map[string]bool
	usedWords map[string]bool

	roundNo      int
	drawers      []string // one, or two sharing the canvas; drawers[0] chooses the word
	options      []string
	word         string
	drawStart    time.Time
	hintsGiven   int
	revealed     map[int]bool
	correct      int
	viewerWinner ViewerWinner // the round's first correct viewer; zero until one guessed it
	roundPoints  map[string]int
	canvas       []CanvasBatch
	canvasPoints int

	matchStart     time.Time // when the match's first round started
	roundsFinished int       // in the current match
	finished       []Finished

	seq    int64
	log    []protocol.Event
	out    []Out
	empty  time.Time // when the last player left (or the room was created); zero if occupied
	closed bool

	// graceUntil: after a takeover, disconnected players count as present until then, so the
	// match doesn't end before they could resume on this server (TakeoverGrace).
	graceUntil time.Time
}

// NewGame creates an empty room. rng drives word options, hints and match ids; tests seed it.
func NewGame(roomID, hostID string, s Settings, rng *rand.Rand, now time.Time) *Game {
	return &Game{
		roomID: roomID, settings: s, hostID: hostID, host: hostID, rng: rng, picker: words.NewPicker(rng),
		players: map[string]*player{}, phase: PhaseWaiting, usedWords: map[string]bool{}, empty: now,
	}
}

// TakeOut returns the messages produced since the last call.
func (g *Game) TakeOut() []Out {
	out := g.out
	g.out = nil
	return out
}

// Phase returns the current phase.
func (g *Game) Phase() Phase { return g.phase }

// Closed reports that the room has been empty long enough to close.
func (g *Game) Closed() bool { return g.closed }

// KeepOpen takes back a close the store refused (ADR 0009: finished rounds or matches are
// still to be published). The room stays, empty, and tries again after another EmptyTTL; a
// player who joins meanwhile finds it open.
func (g *Game) KeepOpen(now time.Time) {
	g.closed = false
	g.empty = now
}

// Seq is the seq of the latest shared event.
func (g *Game) Seq() int64 { return g.seq }

// MatchID is the current (or last) match's id.
func (g *Game) MatchID() string { return g.matchID }

// RoundNo is the current round's number within the match.
func (g *Game) RoundNo() int { return g.roundNo }

// TakeFinished returns the rounds and matches that ended since the last call, in order.
func (g *Game) TakeFinished() []Finished {
	f := g.finished
	g.finished = nil
	return f
}

// present reports whether p takes part right now: connected, or within the takeover grace.
func (g *Game) present(p *player, now time.Time) bool {
	return p.connected || now.Before(g.graceUntil)
}

// presentCount counts the players taking part (see present).
func (g *Game) presentCount(now time.Time) int {
	n := 0
	for _, p := range g.players {
		if g.present(p, now) {
			n++
		}
	}
	return n
}

// isDrawer reports whether uid draws this round.
func (g *Game) isDrawer(uid string) bool { return uid != "" && slices.Contains(g.drawers, uid) }

// chooser is the drawer who picks the word: drawers[0], or "" between rounds.
func (g *Game) chooser() string {
	if len(g.drawers) == 0 {
		return ""
	}
	return g.drawers[0]
}

// presentDrawer returns the first drawer other than except who is present, or "".
func (g *Game) presentDrawer(now time.Time, except string) string {
	for _, id := range g.drawers {
		if p, ok := g.players[id]; ok && id != except && g.present(p, now) {
			return id
		}
	}
	return ""
}

// Connected counts players with an open connection.
// Seated counts the players holding a seat: connected, within their rejoin grace, or (during
// a match) anyone who joined it. It is what Join compares with max_players.
func (g *Game) Seated() int { return len(g.players) }

func (g *Game) Connected() int {
	n := 0
	for _, p := range g.players {
		if p.connected {
			n++
		}
	}
	return n
}

// Join adds a player or reconnects one. A player who is already connected (a second socket)
// changes nothing; the actor swaps the socket. The joiner gets a welcome, then any private
// state (word options or the word, if they are drawing).
//
// With resume, a member who reconnects gets no welcome: the actor replays the events it
// missed, and this join's own events reach it like everyone else's.
func (g *Game) Join(now time.Time, uid, name string, resume bool) error {
	except := uid // a welcome already shows the joiner itself
	if resume {
		except = ""
	}
	p, ok := g.players[uid]
	switch {
	case ok && p.connected:
		// Same player, new socket: no event for the others.
	case ok:
		p.connected = true
		p.leftAt = time.Time{}
		p.name = name
		g.emitExcept(except, "player_joined", map[string]any{"user_id": uid, "name": name})
	default:
		if len(g.players) >= g.settings.MaxPlayers {
			return ErrRoomFull
		}
		g.nextJoin++
		p = &player{id: uid, name: name, joinOrder: g.nextJoin, connected: true, bucket: ratelimit.New(guessBurst, guessPerSecond)}
		g.players[uid] = p
		if g.phase.Playing() {
			g.drawOrder = append(g.drawOrder, uid) // joins the queue of drawers
		}
		g.emitExcept(except, "player_joined", map[string]any{"user_id": uid, "name": name})
	}
	g.empty = time.Time{}
	g.fixHost()
	if !resume {
		g.out = append(g.out, Out{To: uid, Type: "welcome", Seq: g.seq,
			Data: map[string]any{"user_id": uid, "view": g.ViewFor(uid), "seq": g.seq}})
	}
	g.sendPrivate(uid)
	if resume {
		// The last message of every resume (ADR 0008): after the replay, this join's own events
		// and the private state, so the client knows it is caught up, and can check seq. Without
		// it, a resume with nothing missed got no answer at all.
		g.send(uid, "resumed", map[string]any{"seq": g.seq})
	}
	return nil
}

// Leave marks a player as disconnected. They keep seat and score for SeatGrace.
func (g *Game) Leave(now time.Time, uid string) {
	p, ok := g.players[uid]
	if !ok || !p.connected {
		return
	}
	p.connected = false
	p.leftAt = now
	g.emit("player_left", map[string]any{"user_id": uid})
	g.fixHost()
	if g.Connected() == 0 {
		g.empty = now
	}
	switch {
	case g.phase.Playing() && g.presentCount(now) < 2:
		g.endMatch(now)
	case uid == g.chooser() && g.phase == PhaseChoosing:
		g.passChoice(now, g.presentDrawer(now, uid))
	case g.isDrawer(uid) && g.phase == PhaseDrawing:
		if g.presentDrawer(now, uid) == "" {
			g.endRound(now) // nobody is left to draw
		}
	case g.phase == PhaseDrawing:
		g.endRoundIfAllGuessed(now)
	}
}

// Handle applies one client message from a connected player. It returns a guess result for
// guess messages (for metrics) and "" otherwise.
func (g *Game) Handle(now time.Time, uid string, msg protocol.ClientMsg) string {
	p, ok := g.players[uid]
	if !ok || !p.connected {
		return ""
	}
	switch m := msg.(type) {
	case protocol.Start:
		g.handleStart(now, uid)
	case protocol.ChooseWord:
		if g.phase != PhaseChoosing || uid != g.chooser() {
			g.fail(uid, "not_your_turn", "only the first drawer can choose, while choosing")
			return ""
		}
		if m.Index >= len(g.options) {
			g.fail(uid, "bad_request", "no such word option")
			return ""
		}
		g.beginDrawing(now, g.options[m.Index])
	case protocol.StrokeBatch:
		g.handleStroke(now, uid, m)
	case protocol.Undo:
		g.handleUndo(uid, m)
	case protocol.Guess:
		return g.handleGuess(now, p, m.Text)
	}
	return ""
}

func (g *Game) handleStart(now time.Time, uid string) {
	switch {
	case g.phase != PhaseWaiting && g.phase != PhaseMatchEnd:
		g.fail(uid, "bad_phase", "a match is already running")
	case uid != g.host:
		g.fail(uid, "not_host", "only the host can start")
	case g.Connected() < 2:
		g.fail(uid, "not_enough_players", "a match needs at least 2 players")
	default:
		g.startMatch(now)
	}
}

func (g *Game) handleStroke(now time.Time, uid string, m protocol.StrokeBatch) {
	if g.phase != PhaseDrawing || !g.isDrawer(uid) {
		g.fail(uid, "not_your_turn", "only the drawers can draw, while drawing")
		return
	}
	if g.canvasPoints+len(m.Points) > maxCanvasPoints {
		g.fail(uid, "canvas_full", "the canvas is full for this round")
		return
	}
	g.canvasPoints += len(m.Points)
	b := CanvasBatch{UserID: uid, StrokeBatch: m, ReceivedAt: now}
	g.canvas = append(g.canvas, b)
	g.emit("stroke_batch", b)
}

func (g *Game) handleUndo(uid string, m protocol.Undo) {
	if g.phase != PhaseDrawing {
		g.fail(uid, "bad_phase", "nothing to undo")
		return
	}
	kept := g.canvas[:0]
	removed := 0
	for _, b := range g.canvas {
		// A player can undo only their own strokes.
		if b.StrokeID == m.StrokeID && b.UserID == uid {
			removed += len(b.Points)
			continue
		}
		kept = append(kept, b)
	}
	g.canvas = kept
	if removed == 0 {
		g.fail(uid, "bad_request", "no such stroke of yours")
		return
	}
	g.canvasPoints -= removed
	g.emit("stroke_undone", map[string]any{"stroke_id": m.StrokeID, "user_id": uid})
}

func (g *Game) handleGuess(now time.Time, p *player, text string) string {
	if !p.bucket.Allow(now) {
		g.send(p.id, "rate_limited", map[string]any{})
		return GuessRateLimited
	}
	if g.phase == PhaseChoosing && g.isDrawer(p.id) {
		g.fail(p.id, "cannot_chat_now", "the drawers can't chat while choosing a word")
		return ""
	}
	if g.phase != PhaseDrawing {
		g.emit("chat", map[string]any{"user_id": p.id, "text": text})
		return ""
	}
	if g.isDrawer(p.id) || p.guessed {
		// They know the word; their chat could give it away.
		g.fail(p.id, "cannot_chat_now", "you know the word; chat opens after the round")
		return ""
	}
	norm := Normalize(text)
	switch {
	case norm == g.word:
		g.correct++
		points := Score(g.correct)
		p.score += points
		p.guessed = true
		g.roundPoints[p.id] += points
		g.creditDrawers()
		g.emit("correct_guess", map[string]any{"user_id": p.id, "order": g.correct})
		g.send(p.id, "guess_result", map[string]any{"correct": true})
		g.endRoundIfAllGuessed(now)
		return GuessCorrect
	case strings.Contains(norm, g.word):
		// Close but not exact, and broadcasting it would show everyone the word.
		g.send(p.id, "guess_result", map[string]any{"correct": false})
		return GuessWrong
	default:
		g.emit("chat", map[string]any{"user_id": p.id, "text": text})
		g.send(p.id, "guess_result", map[string]any{"correct": false})
		return GuessWrong
	}
}

// ViewerCorrect takes a viewer's correct guess that a relay forwarded (ADR 0019). The relay
// already checked the word; the owner decides who was first, in the order guesses reach it:
// the first one for the round being drawn is announced to everyone with viewer_correct, and
// anything else is ignored. A player of this room can't win as a viewer (a drawer watching
// their own room knows the word). It returns ViewerAnnounced or ViewerIgnored.
func (g *Game) ViewerCorrect(roundNo int, uid, name string) string {
	if g.phase != PhaseDrawing || roundNo != g.roundNo || g.viewerWinner.UserID != "" || uid == "" {
		return ViewerIgnored
	}
	if _, plays := g.players[uid]; plays {
		return ViewerIgnored
	}
	g.viewerWinner = ViewerWinner{UserID: uid, Name: name}
	g.emit("viewer_correct", map[string]any{"user_id": uid, "name": name, "round_no": roundNo})
	return ViewerAnnounced
}

// Advance fires everything that is due at now: phase deadlines, hints, expired seats and
// closing an empty room. A wake-up with nothing due does nothing.
func (g *Game) Advance(now time.Time) {
	for range 16 { // a few transitions can be due at once, but never a loop
		switch {
		case !g.graceUntil.IsZero() && !now.Before(g.graceUntil):
			g.graceUntil = time.Time{}
			g.afterGrace(now)
		case g.phase == PhaseDrawing && g.hintsGiven < len(hintAt) && !now.Before(g.hintTime(g.hintsGiven)):
			g.hintsGiven++
			revealMore(g.word, g.revealed, g.rng)
			g.emit("hint", map[string]any{"mask": mask(g.word, g.revealed)})
		case !g.deadline.IsZero() && !now.Before(g.deadline):
			g.phaseDeadline(now)
		default:
			g.expireSeats(now)
			if !g.empty.IsZero() && !now.Before(g.empty.Add(EmptyTTL)) {
				g.closed = true
			}
			return
		}
	}
}

// afterGrace applies, once the takeover grace is over, what a leave would have done for the
// players who never came back.
func (g *Game) afterGrace(now time.Time) {
	// The grace is over, so present now means connected.
	switch {
	case g.phase.Playing() && g.Connected() < 2:
		g.endMatch(now)
	case g.phase == PhaseChoosing && g.presentDrawer(now, "") != g.chooser():
		g.passChoice(now, g.presentDrawer(now, ""))
	case g.phase == PhaseDrawing && g.presentDrawer(now, "") == "":
		g.endRound(now)
	case g.phase == PhaseDrawing:
		g.endRoundIfAllGuessed(now)
	}
}

func (g *Game) phaseDeadline(now time.Time) {
	switch g.phase {
	case PhaseStarting, PhaseReveal:
		g.nextRound(now)
	case PhaseChoosing:
		g.beginDrawing(now, g.options[0]) // the options are random, so the first is fair
	case PhaseDrawing:
		g.endRound(now)
	default:
		g.deadline = time.Time{}
	}
}

// NextDeadline is when Advance next has something to do; zero means never.
func (g *Game) NextDeadline() time.Time {
	var next time.Time
	consider := func(t time.Time) {
		if !t.IsZero() && (next.IsZero() || t.Before(next)) {
			next = t
		}
	}
	consider(g.deadline)
	consider(g.graceUntil)
	if g.phase == PhaseDrawing && g.hintsGiven < len(hintAt) {
		consider(g.hintTime(g.hintsGiven))
	}
	if !g.phase.Playing() {
		for _, p := range g.players {
			if !p.connected {
				consider(p.leftAt.Add(SeatGrace))
			}
		}
	}
	if !g.empty.IsZero() {
		consider(g.empty.Add(EmptyTTL))
	}
	return next
}

func (g *Game) hintTime(i int) time.Time {
	return g.drawStart.Add(time.Duration(float64(g.settings.Draw) * hintAt[i]))
}

// expireSeats frees the seats of players gone longer than SeatGrace, between matches only
// (during a match their score still counts).
func (g *Game) expireSeats(now time.Time) {
	if g.phase.Playing() {
		return
	}
	for _, id := range g.joinOrder() {
		p := g.players[id]
		if !p.connected && !now.Before(p.leftAt.Add(SeatGrace)) {
			delete(g.players, id)
			g.emit("player_removed", map[string]any{"user_id": id})
		}
	}
}

func (g *Game) startMatch(now time.Time) {
	g.matchID = fmt.Sprintf("%016x%016x", g.rng.Uint64(), g.rng.Uint64())
	g.drawOrder = nil
	for _, id := range g.joinOrder() {
		p := g.players[id]
		p.score = 0
		if p.connected {
			g.drawOrder = append(g.drawOrder, id)
		}
	}
	g.drawn = map[string]bool{}
	g.usedWords = map[string]bool{}
	g.roundNo = 0
	g.roundsFinished = 0
	g.matchStart = time.Time{}
	g.phase = PhaseStarting
	g.deadline = now.Add(StartingFor)
	g.emit("match_started", map[string]any{"match_id": g.matchID, "order": g.drawOrder, "deadline": g.deadline.UnixMilli()})
}

func (g *Game) nextRound(now time.Time) {
	g.resetRound()
	if g.presentCount(now) < 2 {
		g.endMatch(now)
		return
	}
	// With enough players, the next two in the draw order share the canvas. Each player still
	// draws once per match, so the last round can have a single drawer (5 players: 2, 2, 1).
	want := 1
	if g.presentCount(now) >= PairFrom {
		want = 2
	}
	var next []string
	for _, id := range g.drawOrder {
		if p, ok := g.players[id]; ok && g.present(p, now) && !g.drawn[id] {
			next = append(next, id)
			if len(next) == want {
				break
			}
		}
	}
	if len(next) == 0 {
		g.endMatch(now)
		return
	}
	g.roundNo++
	if g.roundNo == 1 {
		g.matchStart = now
	}
	g.drawers = next
	for _, id := range next {
		g.drawn[id] = true
	}
	g.options = g.picker.Options(g.settings.WordOptions, g.usedWords)
	g.phase = PhaseChoosing
	g.deadline = now.Add(g.settings.Choose)
	g.announceChoosing()
}

// announceChoosing tells the room who is choosing; only the first drawer gets the options.
func (g *Game) announceChoosing() {
	g.emit("choosing", map[string]any{"round_no": g.roundNo, "drawer_ids": slices.Clone(g.drawers), "deadline": g.deadline.UnixMilli()})
	g.sendPrivate(g.chooser())
}

// passChoice handles the chooser leaving while choosing. If another drawer is still here, the
// word options go to them; the one who left stays a drawer (they saw the options, so they
// must not guess this round). The deadline stays: two drawers leaving and rejoining in turn
// could otherwise pass the choice back and forth and stall the room. If no drawer is here,
// nobody saw a word yet, and the next round simply starts.
func (g *Game) passChoice(now time.Time, next string) {
	if next == "" {
		g.nextRound(now)
		return
	}
	g.drawers = append([]string{next}, slices.DeleteFunc(slices.Clone(g.drawers), func(id string) bool { return id == next })...)
	g.announceChoosing()
}

// creditDrawers gives every drawer their points for one correct guess.
func (g *Game) creditDrawers() {
	for _, id := range g.drawers {
		if d, ok := g.players[id]; ok {
			d.score += DrawerPointsPerGuess
			g.roundPoints[id] += DrawerPointsPerGuess
		}
	}
}

func (g *Game) resetRound() {
	g.drawers, g.word, g.options = nil, "", nil
	g.hintsGiven, g.correct, g.canvasPoints = 0, 0, 0
	g.viewerWinner = ViewerWinner{}
	g.revealed = map[int]bool{}
	g.roundPoints = map[string]int{}
	g.canvas = nil
	for _, p := range g.players {
		p.guessed = false
	}
}

func (g *Game) beginDrawing(now time.Time, word string) {
	g.word = word
	g.usedWords[word] = true
	g.options = nil
	g.phase = PhaseDrawing
	g.drawStart = now
	g.deadline = now.Add(g.settings.Draw)
	g.emit("round_started", map[string]any{
		"round_no": g.roundNo, "drawer_ids": slices.Clone(g.drawers), "word_length": len(word), "deadline": g.deadline.UnixMilli(),
	})
	for _, id := range g.drawers {
		g.sendPrivate(id)
	}
}

func (g *Game) endRoundIfAllGuessed(now time.Time) {
	guessers, guessed := 0, 0
	for _, p := range g.players {
		if g.present(p, now) && !g.isDrawer(p.id) {
			guessers++
			if p.guessed {
				guessed++
			}
		}
	}
	if guessers > 0 && guessed == guessers {
		g.endRound(now)
	}
}

func (g *Game) endRound(now time.Time) {
	g.roundsFinished++
	g.finished = append(g.finished, Finished{Round: &FinishedRound{
		MatchID: g.matchID, RoomID: g.roomID, RoundNo: g.roundNo, Word: g.word, DrawerIDs: slices.Clone(g.drawers),
		Correct: g.correct, Canvas: slices.Clone(g.canvas), DrawStart: g.drawStart, EndedAt: now,
	}})
	g.phase = PhaseReveal
	g.deadline = now.Add(RevealFor)
	g.emit("round_ended", map[string]any{
		"round_no": g.roundNo, "word": g.word, "scores": g.scores(), "round_points": g.roundPoints,
	})
}

func (g *Game) endMatch(now time.Time) {
	if g.phase == PhaseDrawing {
		g.endRound(now)
	}
	g.phase = PhaseMatchEnd
	g.deadline = time.Time{}
	g.drawers = nil
	results := make([]map[string]any, 0, len(g.players))
	final := make([]PlayerResult, 0, len(g.players))
	placement := 0
	for i, s := range g.scores() {
		if i == 0 || s.Score < final[i-1].Score {
			placement = i + 1 // competition ranking: 1, 2, 2, 4
		}
		results = append(results, map[string]any{"user_id": s.UserID, "name": s.Name, "score": s.Score, "placement": placement})
		final = append(final, PlayerResult{UserID: s.UserID, Score: s.Score, Placement: placement})
	}
	g.emit("match_ended", map[string]any{"match_id": g.matchID, "results": results})
	// A match that ended before any round finished (players left during the countdown or the
	// first choosing phase) is not a result: publishing it would let an aborted match earn rewards.
	if g.roundsFinished > 0 {
		g.finished = append(g.finished, Finished{Match: &FinishedMatch{
			MatchID: g.matchID, RoomID: g.roomID, StartedAt: g.matchStart, EndedAt: now,
			Rounds: g.roundsFinished, Results: final,
		}})
	}
}

// ScoreLine is one player's score, best first.
type ScoreLine struct {
	UserID string `json:"user_id"`
	Name   string `json:"name"`
	Score  int    `json:"score"`
}

func (g *Game) scores() []ScoreLine {
	out := make([]ScoreLine, 0, len(g.players))
	for _, id := range g.joinOrder() {
		p := g.players[id]
		out = append(out, ScoreLine{UserID: id, Name: p.name, Score: p.score})
	}
	slices.SortStableFunc(out, func(a, b ScoreLine) int { return b.Score - a.Score })
	return out
}

func (g *Game) joinOrder() []string {
	ids := make([]string, 0, len(g.players))
	for id := range g.players {
		ids = append(ids, id)
	}
	slices.SortFunc(ids, func(a, b string) int { return g.players[a].joinOrder - g.players[b].joinOrder })
	return ids
}

// fixHost hands hosting to the earliest-joined connected player when the host isn't
// connected. The lobby's host gets it on their first join only if nobody else has it yet.
func (g *Game) fixHost() {
	if p, ok := g.players[g.host]; ok && p.connected {
		return
	}
	if g.host == g.hostID && g.players[g.hostID] == nil && len(g.players) == 0 {
		return // nobody here yet; the lobby's host keeps the claim
	}
	for _, id := range g.joinOrder() {
		if g.players[id].connected {
			if id != g.host {
				g.host = id
				g.emit("host_changed", map[string]any{"user_id": id})
			}
			return
		}
	}
}

// --- views: the only places that can put the word in a message -------------------------

// PlayerView is a player as others see them.
type PlayerView struct {
	UserID    string `json:"user_id"`
	Name      string `json:"name"`
	Score     int    `json:"score"`
	Connected bool   `json:"connected"`
	Guessed   bool   `json:"guessed"`
}

// View is a player's picture of the room, sent in welcome.
type View struct {
	RoomID     string         `json:"room_id"`
	Phase      Phase          `json:"phase"`
	HostID     string         `json:"host_id"`
	MatchID    string         `json:"match_id,omitempty"`
	Settings   map[string]int `json:"settings"`
	Players    []PlayerView   `json:"players"`
	RoundNo    int            `json:"round_no,omitempty"`
	DrawerIDs  []string       `json:"drawer_ids,omitempty"`
	Deadline   int64          `json:"deadline,omitempty"`
	WordLength int            `json:"word_length,omitempty"`
	Mask       string         `json:"mask,omitempty"`
	Word       string         `json:"word,omitempty"` // only after the round ended
	// ViewerWinner is the round's first correct viewer (ADR 0019), while the round runs or is
	// being revealed.
	ViewerWinner *ViewerWinner `json:"viewer_winner,omitempty"`
	Strokes      []CanvasBatch `json:"strokes,omitempty"`
}

// ViewFor builds uid's view. It never contains the word while a round is running, not even
// for the drawer: the drawer gets it through sendPrivate.
func (g *Game) ViewFor(uid string) View {
	v := View{
		RoomID: g.roomID, Phase: g.phase, HostID: g.host, MatchID: g.matchID, RoundNo: g.roundNo,
		Settings: map[string]int{
			"max_players": g.settings.MaxPlayers, "choose_seconds": int(g.settings.Choose / time.Second),
			"draw_seconds": int(g.settings.Draw / time.Second), "word_options": g.settings.WordOptions,
		},
		Strokes: g.canvas,
	}
	if !g.deadline.IsZero() {
		v.Deadline = g.deadline.UnixMilli()
	}
	v.DrawerIDs = cloneOrNil(g.drawers)
	for _, id := range g.joinOrder() {
		p := g.players[id]
		v.Players = append(v.Players, PlayerView{UserID: id, Name: p.name, Score: p.score, Connected: p.connected, Guessed: p.guessed})
	}
	switch g.phase {
	case PhaseDrawing:
		v.WordLength = len(g.word)
		v.Mask = mask(g.word, g.revealed)
	case PhaseReveal, PhaseMatchEnd:
		v.Word = g.word
	}
	if g.viewerWinner.UserID != "" && (g.phase == PhaseDrawing || g.phase == PhaseReveal) {
		w := g.viewerWinner
		v.ViewerWinner = &w
	}
	return v
}

// sendPrivate gives a drawer what only they may see: the word options while choosing (the
// first drawer only), the word while drawing (every drawer). This and the Reveal views are
// the only ways the word leaves the room.
func (g *Game) sendPrivate(uid string) {
	if !g.isDrawer(uid) {
		return
	}
	switch {
	case g.phase == PhaseChoosing && uid == g.chooser():
		g.send(uid, "word_options", map[string]any{"words": g.options})
	case g.phase == PhaseDrawing:
		g.send(uid, "your_word", map[string]any{"word": g.word})
	}
}

// --- output ------------------------------------------------------------------------------

func (g *Game) emit(typ string, data any) { g.emitExcept("", typ, data) }

func (g *Game) emitExcept(except, typ string, data any) {
	g.seq++
	ev := protocol.Event{Seq: g.seq, Type: typ, Data: data}
	g.log = append(g.log, ev)
	if len(g.log) > eventLogSize {
		g.log = g.log[len(g.log)-eventLogSize:]
	}
	g.out = append(g.out, Out{Except: except, Type: "event", Data: ev})
}

func (g *Game) send(uid, typ string, data any) {
	g.out = append(g.out, Out{To: uid, Type: typ, Data: data})
}

func (g *Game) fail(uid, code, msg string) {
	g.send(uid, "error", protocol.ErrorData{Code: code, Message: msg})
}
