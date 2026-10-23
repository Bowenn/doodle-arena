package room

import (
	"encoding/json"
	"fmt"
	"maps"
	"math/rand/v2"
	"slices"
	"strings"
	"time"

	"github.com/Bowenn/doodle-arena/services/game-server/internal/protocol"
	"github.com/Bowenn/doodle-arena/services/game-server/internal/ratelimit"
	"github.com/Bowenn/doodle-arena/services/game-server/internal/words"
)

// SnapshotEvery is how many events may pass without a snapshot. Snapshots are also written
// at every phase change, so the events after a snapshot only ever change a room within its
// phase (strokes, guesses, hints, players coming and going): Apply handles exactly those.
const SnapshotEvery = 200

// TakeoverGrace is how long players count as present after a room was restored on another
// server: everyone is disconnected at that moment, and the match must not end before they
// had time to reconnect.
const TakeoverGrace = 15 * time.Second

// Snapshot is a room's whole state, stored in Redis so another server can take the room over
// (design doc, section 9). It contains the word: it is never sent to a client.
type Snapshot struct {
	RoomID   string `json:"room_id"`
	HostID   string `json:"host_id"`
	Host     string `json:"host"`
	Settings struct {
		MaxPlayers  int   `json:"max_players"`
		ChooseMS    int64 `json:"choose_ms"`
		DrawMS      int64 `json:"draw_ms"`
		WordOptions int   `json:"word_options"`
	} `json:"settings"`
	Players  []PlayerState `json:"players"`
	NextJoin int           `json:"next_join"`

	Phase    Phase     `json:"phase"`
	Deadline time.Time `json:"deadline"`

	MatchID   string   `json:"match_id"`
	DrawOrder []string `json:"draw_order"`
	Drawn     []string `json:"drawn"`
	UsedWords []string `json:"used_words"`

	RoundNo int      `json:"round_no"`
	Drawers []string `json:"drawers"`
	// Drawer is the single drawer of snapshots written before task 2.6a. It is only read:
	// a room stored by an older server restores with Drawers = [Drawer].
	Drawer     string    `json:"drawer,omitempty"`
	Options    []string  `json:"options"`
	Word       string    `json:"word"`
	DrawStart  time.Time `json:"draw_start"`
	HintsGiven int       `json:"hints_given"`
	Revealed   []int     `json:"revealed"`
	Correct    int       `json:"correct"`
	// ViewerWinner is the round's first correct viewer (ADR 0019), if any.
	ViewerWinner *ViewerWinner  `json:"viewer_winner,omitempty"`
	RoundPoints  map[string]int `json:"round_points"`
	Canvas       []StoredBatch  `json:"canvas"`

	MatchStart     time.Time `json:"match_start"`
	RoundsFinished int       `json:"rounds_finished"`

	Seq   int64     `json:"seq"`
	Empty time.Time `json:"empty"`
}

// PlayerState is one player in a snapshot. The guess bucket isn't kept: a restored player
// starts with a full one.
type PlayerState struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	JoinOrder int       `json:"join_order"`
	Connected bool      `json:"connected"`
	LeftAt    time.Time `json:"left_at"`
	Score     int       `json:"score"`
	Guessed   bool      `json:"guessed"`
}

// StoredBatch is a canvas batch with its server receive time, which clients never see but
// RoundFinished needs.
type StoredBatch struct {
	CanvasBatch
	ReceivedAt time.Time `json:"received_at"`
}

// StoredEvent is a shared event as the room's stream keeps it, with the server time it was
// produced at.
type StoredEvent struct {
	Seq  int64
	Type string
	Data json.RawMessage
	At   time.Time
}

// phaseEvents start a new phase; a commit that carries one also writes a snapshot.
var phaseEvents = map[string]bool{
	"match_started": true, "choosing": true, "round_started": true, "round_ended": true, "match_ended": true,
}

// cloneOrNil copies a slice; empty becomes nil, so equal states give equal snapshots.
func cloneOrNil[T any](s []T) []T {
	if len(s) == 0 {
		return nil
	}
	return slices.Clone(s)
}

// IsPhaseEvent reports whether an event type changes the room's phase.
func IsPhaseEvent(typ string) bool { return phaseEvents[typ] }

// Snapshot captures the room's state as of its latest event.
func (g *Game) Snapshot() Snapshot {
	s := Snapshot{
		RoomID: g.roomID, HostID: g.hostID, Host: g.host, NextJoin: g.nextJoin,
		Phase: g.phase, Deadline: g.deadline, MatchID: g.matchID,
		DrawOrder: cloneOrNil(g.drawOrder), RoundNo: g.roundNo, Drawers: cloneOrNil(g.drawers), Options: cloneOrNil(g.options),
		Word: g.word, DrawStart: g.drawStart, HintsGiven: g.hintsGiven, Correct: g.correct,
		MatchStart: g.matchStart, RoundsFinished: g.roundsFinished,
		Seq: g.seq, Empty: g.empty,
	}
	// Copies, not aliases: the game keeps changing after the snapshot is taken.
	if len(g.roundPoints) > 0 {
		s.RoundPoints = maps.Clone(g.roundPoints)
	}
	if g.viewerWinner.UserID != "" {
		w := g.viewerWinner
		s.ViewerWinner = &w
	}
	s.Settings.MaxPlayers = g.settings.MaxPlayers
	s.Settings.ChooseMS = g.settings.Choose.Milliseconds()
	s.Settings.DrawMS = g.settings.Draw.Milliseconds()
	s.Settings.WordOptions = g.settings.WordOptions
	for _, id := range g.joinOrder() {
		p := g.players[id]
		s.Players = append(s.Players, PlayerState{ID: id, Name: p.name, JoinOrder: p.joinOrder,
			Connected: p.connected, LeftAt: p.leftAt, Score: p.score, Guessed: p.guessed})
	}
	// Map contents are sorted so equal states give equal snapshots.
	for id := range g.drawn {
		s.Drawn = append(s.Drawn, id)
	}
	slices.Sort(s.Drawn)
	for w := range g.usedWords {
		s.UsedWords = append(s.UsedWords, w)
	}
	slices.Sort(s.UsedWords)
	for i := range g.revealed {
		s.Revealed = append(s.Revealed, i)
	}
	slices.Sort(s.Revealed)
	for _, b := range g.canvas {
		s.Canvas = append(s.Canvas, StoredBatch{CanvasBatch: b, ReceivedAt: b.ReceivedAt})
	}
	return s
}

// RestoreGame rebuilds a room on a new owner: the snapshot, then the events after it
// (applied in seq order), then every player marked disconnected, with TakeoverGrace to come
// back. events may also contain older events: they only fill the replay buffer.
func RestoreGame(s Snapshot, events []StoredEvent, rng *rand.Rand, now time.Time) (*Game, error) {
	g, err := restore(s, events, rng)
	if err != nil {
		return nil, err
	}
	// The sockets were on the old owner: everyone is disconnected until they resume here.
	for _, p := range g.players {
		if p.connected {
			p.connected = false
			p.leftAt = now
		}
	}
	if g.empty.IsZero() {
		g.empty = now
	}
	g.graceUntil = now.Add(TakeoverGrace)
	return g, nil
}

// ViewAt is what a viewer sees (ADR 0017): the room rebuilt from the snapshot and the events
// after it, exactly as its owner has it, and its seq. It is a guesser's view, so it never holds
// the word while a round runs.
func ViewAt(s Snapshot, events []StoredEvent) (View, int64, error) {
	g, err := restore(s, events, rand.New(rand.NewPCG(0, 0)))
	if err != nil {
		return View{}, 0, err
	}
	return g.ViewFor(""), g.seq, nil
}

// restore is the snapshot plus the events after it, exactly as the old owner had it.
func restore(s Snapshot, events []StoredEvent, rng *rand.Rand) (*Game, error) {
	g := &Game{
		roomID: s.RoomID, hostID: s.HostID, host: s.Host, rng: rng, picker: words.NewPicker(rng),
		settings: Settings{MaxPlayers: s.Settings.MaxPlayers, Choose: time.Duration(s.Settings.ChooseMS) * time.Millisecond,
			Draw: time.Duration(s.Settings.DrawMS) * time.Millisecond, WordOptions: s.Settings.WordOptions},
		players: map[string]*player{}, nextJoin: s.NextJoin, phase: s.Phase, deadline: s.Deadline,
		matchID: s.MatchID, drawOrder: slices.Clone(s.DrawOrder), drawn: map[string]bool{}, usedWords: map[string]bool{},
		roundNo: s.RoundNo, drawers: slices.Clone(s.Drawers), options: slices.Clone(s.Options), word: s.Word, drawStart: s.DrawStart,
		hintsGiven: s.HintsGiven, revealed: map[int]bool{}, correct: s.Correct, roundPoints: map[string]int{},
		matchStart: s.MatchStart, roundsFinished: s.RoundsFinished, seq: s.Seq, empty: s.Empty,
	}
	if s.ViewerWinner != nil {
		g.viewerWinner = *s.ViewerWinner
	}
	if len(g.drawers) == 0 && s.Drawer != "" {
		g.drawers = []string{s.Drawer}
	}
	for _, p := range s.Players {
		g.players[p.ID] = &player{id: p.ID, name: p.Name, joinOrder: p.JoinOrder, connected: p.Connected,
			leftAt: p.LeftAt, score: p.Score, guessed: p.Guessed, bucket: ratelimit.New(guessBurst, guessPerSecond)}
	}
	for _, id := range s.Drawn {
		g.drawn[id] = true
	}
	for _, w := range s.UsedWords {
		g.usedWords[w] = true
	}
	for _, i := range s.Revealed {
		g.revealed[i] = true
	}
	for k, v := range s.RoundPoints {
		g.roundPoints[k] = v
	}
	for _, b := range s.Canvas {
		cb := b.CanvasBatch
		cb.ReceivedAt = b.ReceivedAt
		g.canvas = append(g.canvas, cb)
		g.canvasPoints += len(cb.Points)
	}

	for _, ev := range events {
		if ev.Seq > s.Seq {
			if ev.Seq != g.seq+1 {
				return nil, fmt.Errorf("event log has a gap: seq %d after %d", ev.Seq, g.seq)
			}
			if err := g.apply(ev); err != nil {
				return nil, fmt.Errorf("apply event %d (%s): %w", ev.Seq, ev.Type, err)
			}
			g.seq = ev.Seq
		}
		g.log = append(g.log, protocol.Event{Seq: ev.Seq, Type: ev.Type, Data: ev.Data})
	}
	if len(g.log) > eventLogSize {
		g.log = g.log[len(g.log)-eventLogSize:]
	}
	return g, nil
}

// apply replays one event that happened after the snapshot. Only in-phase events can occur
// there; a phase event without a snapshot means the log and snapshot disagree.
func (g *Game) apply(ev StoredEvent) error {
	switch ev.Type {
	case "player_joined":
		var d struct {
			UserID string `json:"user_id"`
			Name   string `json:"name"`
		}
		if err := json.Unmarshal(ev.Data, &d); err != nil {
			return err
		}
		if p, ok := g.players[d.UserID]; ok {
			p.connected, p.leftAt, p.name = true, time.Time{}, d.Name
		} else {
			g.nextJoin++
			g.players[d.UserID] = &player{id: d.UserID, name: d.Name, joinOrder: g.nextJoin, connected: true,
				bucket: ratelimit.New(guessBurst, guessPerSecond)}
			if g.phase.Playing() {
				g.drawOrder = append(g.drawOrder, d.UserID)
			}
		}
		g.empty = time.Time{}
	case "player_left":
		var d struct {
			UserID string `json:"user_id"`
		}
		if err := json.Unmarshal(ev.Data, &d); err != nil {
			return err
		}
		if p, ok := g.players[d.UserID]; ok {
			p.connected, p.leftAt = false, ev.At
		}
		if g.Connected() == 0 {
			g.empty = ev.At
		}
	case "player_removed":
		var d struct {
			UserID string `json:"user_id"`
		}
		if err := json.Unmarshal(ev.Data, &d); err != nil {
			return err
		}
		delete(g.players, d.UserID)
	case "host_changed":
		var d struct {
			UserID string `json:"user_id"`
		}
		if err := json.Unmarshal(ev.Data, &d); err != nil {
			return err
		}
		g.host = d.UserID
	case "stroke_batch":
		var b CanvasBatch
		if err := json.Unmarshal(ev.Data, &b); err != nil {
			return err
		}
		b.ReceivedAt = ev.At
		g.canvas = append(g.canvas, b)
		g.canvasPoints += len(b.Points)
	case "stroke_undone":
		var d struct {
			StrokeID string `json:"stroke_id"`
			UserID   string `json:"user_id"`
		}
		if err := json.Unmarshal(ev.Data, &d); err != nil {
			return err
		}
		kept := g.canvas[:0]
		for _, b := range g.canvas {
			if b.StrokeID == d.StrokeID && b.UserID == d.UserID {
				g.canvasPoints -= len(b.Points)
				continue
			}
			kept = append(kept, b)
		}
		g.canvas = kept
	case "hint":
		var d struct {
			Mask string `json:"mask"`
		}
		if err := json.Unmarshal(ev.Data, &d); err != nil {
			return err
		}
		g.hintsGiven++
		for i, c := range d.Mask {
			if c != '_' {
				g.revealed[i] = true
			}
		}
	case "correct_guess":
		var d struct {
			UserID string `json:"user_id"`
			Order  int    `json:"order"`
		}
		if err := json.Unmarshal(ev.Data, &d); err != nil {
			return err
		}
		g.correct = d.Order
		points := Score(d.Order)
		if p, ok := g.players[d.UserID]; ok {
			p.score += points
			p.guessed = true
		}
		g.roundPoints[d.UserID] += points
		g.creditDrawers()
	case "viewer_correct":
		var d ViewerWinner
		if err := json.Unmarshal(ev.Data, &d); err != nil {
			return err
		}
		g.viewerWinner = d
	case "chat":
		// No state.
	default:
		if IsPhaseEvent(ev.Type) {
			return fmt.Errorf("phase event after the latest snapshot")
		}
		return fmt.Errorf("unknown event type")
	}
	return nil
}

// RecentEvents returns the in-memory events after seq, oldest first, or false when the buffer
// no longer holds all of them (the caller then sends a full welcome).
func (g *Game) RecentEvents(after int64) ([]protocol.Event, bool) {
	if after >= g.seq {
		return nil, true
	}
	if len(g.log) == 0 || g.log[0].Seq > after+1 {
		return nil, false
	}
	i := int(after + 1 - g.log[0].Seq)
	return append([]protocol.Event(nil), g.log[i:]...), true
}

// IsMember reports whether uid has a seat in the room (connected or within its grace).
func (g *Game) IsMember(uid string) bool {
	_, ok := g.players[uid]
	return ok
}

// String keeps snapshots readable in test failures without the canvas.
func (s Snapshot) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "room %s seq %d phase %s round %d drawers %v players %d strokes %d",
		s.RoomID, s.Seq, s.Phase, s.RoundNo, s.Drawers, len(s.Players), len(s.Canvas))
	return b.String()
}
