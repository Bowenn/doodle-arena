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

// Guess limit per player: a burst of 5, then 1 per second.
const (
	guessBurst     = 5
	guessPerSecond = 1
)

// eventLogSize is how many recent shared events the room keeps in memory. Task 2.5 replaces
// this with the Redis Stream room:{id}:events.
const eventLogSize = 1024

// Out is one message the room wants delivered.
type Out struct {
	To     string // one player, or "" for every connected player
	Except string // with To == "": skip this player (they get a welcome instead)
	Type   string // "event" for shared events, otherwise a private message type
	Data   any    // protocol.Event for shared events
	Seq    int64  // for a welcome: the seq it is current up to, so older events are skipped
}

// RoundStats describe the last finished round, for logs.
type RoundStats struct {
	RoundNo  int
	Word     string
	Strokes  int
	Correct  int
	DrawerID string
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

type canvasBatch struct {
	UserID string `json:"user_id"`
	protocol.StrokeBatch
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
	drawer       string
	options      []string
	word         string
	drawStart    time.Time
	hintsGiven   int
	revealed     map[int]bool
	correct      int
	roundPoints  map[string]int
	canvas       []canvasBatch
	canvasPoints int

	lastRound RoundStats

	seq    int64
	log    []protocol.Event
	out    []Out
	empty  time.Time // when the last player left (or the room was created); zero if occupied
	closed bool
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

// Seq is the seq of the latest shared event.
func (g *Game) Seq() int64 { return g.seq }

// MatchID is the current (or last) match's id.
func (g *Game) MatchID() string { return g.matchID }

// RoundNo is the current round's number within the match.
func (g *Game) RoundNo() int { return g.roundNo }

// LastRound describes the last finished round. Its word is for logs after the round ended.
func (g *Game) LastRound() RoundStats { return g.lastRound }

// Connected counts players with an open connection.
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
func (g *Game) Join(now time.Time, uid, name string) error {
	p, ok := g.players[uid]
	switch {
	case ok && p.connected:
		// Same player, new socket: no event for the others.
	case ok:
		p.connected = true
		p.leftAt = time.Time{}
		p.name = name
		g.emitExcept(uid, "player_joined", map[string]any{"user_id": uid, "name": name})
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
		g.emitExcept(uid, "player_joined", map[string]any{"user_id": uid, "name": name})
	}
	g.empty = time.Time{}
	g.fixHost()
	g.out = append(g.out, Out{To: uid, Type: "welcome", Seq: g.seq,
		Data: map[string]any{"user_id": uid, "view": g.ViewFor(uid), "seq": g.seq}})
	g.sendPrivate(uid)
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
	case g.phase.Playing() && g.Connected() < 2:
		g.endMatch(now)
	case uid == g.drawer && g.phase == PhaseChoosing:
		g.nextRound(now) // nobody saw a word yet: just pick the next drawer
	case uid == g.drawer && g.phase == PhaseDrawing:
		g.endRound(now)
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
		if g.phase != PhaseChoosing || uid != g.drawer {
			g.fail(uid, "not_your_turn", "only the drawer can choose, while choosing")
			return ""
		}
		if m.Index >= len(g.options) {
			g.fail(uid, "bad_request", "no such word option")
			return ""
		}
		g.beginDrawing(now, g.options[m.Index])
	case protocol.StrokeBatch:
		g.handleStroke(uid, m)
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

func (g *Game) handleStroke(uid string, m protocol.StrokeBatch) {
	if g.phase != PhaseDrawing || uid != g.drawer {
		g.fail(uid, "not_your_turn", "only the drawer can draw, while drawing")
		return
	}
	if g.canvasPoints+len(m.Points) > maxCanvasPoints {
		g.fail(uid, "canvas_full", "the canvas is full for this round")
		return
	}
	g.canvasPoints += len(m.Points)
	b := canvasBatch{UserID: uid, StrokeBatch: m}
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
	if g.phase == PhaseChoosing && p.id == g.drawer {
		g.fail(p.id, "cannot_chat_now", "the drawer can't chat while choosing a word")
		return ""
	}
	if g.phase != PhaseDrawing {
		g.emit("chat", map[string]any{"user_id": p.id, "text": text})
		return ""
	}
	if p.id == g.drawer || p.guessed {
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
		if d, ok := g.players[g.drawer]; ok {
			d.score += DrawerPointsPerGuess
			g.roundPoints[d.id] += DrawerPointsPerGuess
		}
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

// Advance fires everything that is due at now: phase deadlines, hints, expired seats and
// closing an empty room. A wake-up with nothing due does nothing.
func (g *Game) Advance(now time.Time) {
	for range 16 { // a few transitions can be due at once, but never a loop
		switch {
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
	g.phase = PhaseStarting
	g.deadline = now.Add(StartingFor)
	g.emit("match_started", map[string]any{"match_id": g.matchID, "order": g.drawOrder, "deadline": g.deadline.UnixMilli()})
}

func (g *Game) nextRound(now time.Time) {
	g.resetRound()
	if g.Connected() < 2 {
		g.endMatch(now)
		return
	}
	next := ""
	for _, id := range g.drawOrder {
		if p, ok := g.players[id]; ok && p.connected && !g.drawn[id] {
			next = id
			break
		}
	}
	if next == "" {
		g.endMatch(now)
		return
	}
	g.roundNo++
	g.drawer = next
	g.drawn[next] = true
	g.options = g.picker.Options(g.settings.WordOptions, g.usedWords)
	g.phase = PhaseChoosing
	g.deadline = now.Add(g.settings.Choose)
	g.emit("choosing", map[string]any{"round_no": g.roundNo, "drawer_ids": []string{next}, "deadline": g.deadline.UnixMilli()})
	g.sendPrivate(next)
}

func (g *Game) resetRound() {
	g.drawer, g.word, g.options = "", "", nil
	g.hintsGiven, g.correct, g.canvasPoints = 0, 0, 0
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
		"round_no": g.roundNo, "drawer_ids": []string{g.drawer}, "word_length": len(word), "deadline": g.deadline.UnixMilli(),
	})
	g.sendPrivate(g.drawer)
}

func (g *Game) endRoundIfAllGuessed(now time.Time) {
	guessers, guessed := 0, 0
	for _, p := range g.players {
		if p.connected && p.id != g.drawer {
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
	g.lastRound = RoundStats{RoundNo: g.roundNo, Word: g.word, Strokes: len(g.canvas), Correct: g.correct, DrawerID: g.drawer}
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
	g.drawer = ""
	results := make([]map[string]any, 0, len(g.players))
	placement := 0
	for i, s := range g.scores() {
		if i == 0 || s.Score < results[i-1]["score"].(int) {
			placement = i + 1 // competition ranking: 1, 2, 2, 4
		}
		results = append(results, map[string]any{"user_id": s.UserID, "name": s.Name, "score": s.Score, "placement": placement})
	}
	g.emit("match_ended", map[string]any{"match_id": g.matchID, "results": results})
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
	Strokes    []canvasBatch  `json:"strokes,omitempty"`
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
	if g.drawer != "" {
		v.DrawerIDs = []string{g.drawer}
	}
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
	return v
}

// sendPrivate gives a drawer what only they may see: the word options while choosing, the
// word while drawing. This and the Reveal views are the only ways the word leaves the room.
func (g *Game) sendPrivate(uid string) {
	if uid == "" || uid != g.drawer {
		return
	}
	switch g.phase {
	case PhaseChoosing:
		g.send(uid, "word_options", map[string]any{"words": g.options})
	case PhaseDrawing:
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
