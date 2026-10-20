package room

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Bowenn/gap-project_private/services/game-server/internal/protocol"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

func mustNew(t *testing.T, cfg Config) *Room {
	t.Helper()
	r, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// fakeClock fires timers only when the test advances it, synchronously, so the tick lands
// in the inbox before the test's next sync.
type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []*fakeTimer
}

type fakeTimer struct {
	at      time.Time
	f       func()
	stopped bool
}

func (c *fakeClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.now }

func (c *fakeClock) AfterFunc(d time.Duration, f func()) func() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := &fakeTimer{at: c.now.Add(d), f: f}
	c.timers = append(c.timers, t)
	return func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		was := !t.stopped
		t.stopped = true
		return was
	}
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	var due []func()
	for _, t := range c.timers {
		if !t.stopped && !t.at.After(c.now) {
			t.stopped = true
			due = append(due, t.f)
		}
	}
	c.mu.Unlock()
	for _, f := range due {
		f()
	}
}

// fakeConn records every message, decoded.
type fakeConn struct {
	uid    string
	mu     sync.Mutex
	msgs   []msg
	closed int
}

type msg struct {
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
	raw  []byte
}

func (c *fakeConn) UserID() string { return c.uid }

func (c *fakeConn) Send(b []byte) {
	var m msg
	if err := json.Unmarshal(b, &m); err != nil {
		panic(err)
	}
	m.raw = b
	c.mu.Lock()
	c.msgs = append(c.msgs, m)
	c.mu.Unlock()
}

func (c *fakeConn) Close(code int, _ string) {
	c.mu.Lock()
	c.closed = code
	c.mu.Unlock()
}

func (c *fakeConn) all() []msg {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.msgs)
}

// events returns the conn's shared events of a type (all types if typ is "").
func (c *fakeConn) events(typ string) []protocol.Event {
	var out []protocol.Event
	for _, m := range c.all() {
		if m.Type != "event" {
			continue
		}
		var ev struct {
			Seq  int64           `json:"seq"`
			Type string          `json:"type"`
			Data json.RawMessage `json:"data"`
		}
		_ = json.Unmarshal(m.Data, &ev)
		if typ == "" || ev.Type == typ {
			out = append(out, protocol.Event{Seq: ev.Seq, Type: ev.Type, Data: ev.Data})
		}
	}
	return out
}

func (c *fakeConn) private(typ string) []json.RawMessage {
	var out []json.RawMessage
	for _, m := range c.all() {
		if m.Type == typ {
			out = append(out, m.Data)
		}
	}
	return out
}

type harness struct {
	t     *testing.T
	room  *Room
	clock *fakeClock
	conns map[string]*fakeConn
}

// newHarness starts a room; opts change it before its actor starts.
func newHarness(t *testing.T, host string, s Settings, m *Metrics, opts ...func(*Room)) *harness {
	t.Helper()
	if m == nil {
		m = NoopMetrics()
	}
	clock := &fakeClock{now: t0}
	r := mustNew(t, Config{ID: "r1", Mode: "classic", HostID: host, Settings: s, Clock: clock,
		Rand: rand.New(rand.NewPCG(9, 9)), Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Metrics: m})
	for _, o := range opts {
		o(r)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go r.Run(ctx)
	t.Cleanup(func() { cancel(); <-r.Done() })
	return &harness{t: t, room: r, clock: clock, conns: map[string]*fakeConn{}}
}

func (h *harness) join(uid string) *fakeConn {
	h.t.Helper()
	c := &fakeConn{uid: uid}
	if err := h.room.Join(context.Background(), c, "Name "+uid, 0); err != nil {
		h.t.Fatalf("join %s: %v", uid, err)
	}
	h.conns[uid] = c
	return c
}

// resume reconnects uid on a new socket with the last seq it saw: a real resume (replay and
// resumed), not a welcome.
func (h *harness) resume(uid string, lastSeq int64) *fakeConn {
	h.t.Helper()
	c := &fakeConn{uid: uid}
	if err := h.room.Join(context.Background(), c, "Name "+uid, lastSeq); err != nil {
		h.t.Fatalf("resume %s: %v", uid, err)
	}
	h.conns[uid] = c
	return c
}

// lastSeq is the seq of the last shared event c received.
func lastSeq(c *fakeConn) int64 {
	evs := c.events("")
	if len(evs) == 0 {
		return 0
	}
	return evs[len(evs)-1].Seq
}

func (h *harness) input(uid string, m protocol.ClientMsg) {
	h.t.Helper()
	if err := h.room.Input(context.Background(), h.conns[uid], m); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) sync() {
	h.t.Helper()
	if err := h.room.sync(context.Background()); err != nil {
		h.t.Fatal(err)
	}
}

// advance lets the actor catch up (so its timer is armed), moves the clock, and waits for
// the resulting tick to be processed.
func (h *harness) advance(d time.Duration) {
	h.sync()
	h.clock.Advance(d)
	h.sync()
}

// startDrawing starts a match and has the first drawer choose; it returns drawer and word.
func (h *harness) startDrawing(host string) (string, string) {
	h.t.Helper()
	drawers, word := h.startDrawingAll(host)
	return drawers[0], word
}

// startDrawingAll is startDrawing for rooms that may have two drawers: it returns all of them.
func (h *harness) startDrawingAll(host string) ([]string, string) {
	h.t.Helper()
	h.input(host, protocol.Start{})
	h.advance(StartingFor)
	drawers := h.drawerIDs(host, "choosing")
	h.input(drawers[0], protocol.ChooseWord{Index: 0})
	h.sync()
	return drawers, h.word(drawers[0])
}

// drawerIDs reads drawer_ids from the latest event of a type that uid received.
func (h *harness) drawerIDs(uid, typ string) []string {
	h.t.Helper()
	evs := h.conns[uid].events(typ)
	if len(evs) == 0 {
		h.t.Fatalf("%s got no %s", uid, typ)
	}
	var c struct {
		DrawerIDs []string `json:"drawer_ids"`
	}
	_ = json.Unmarshal(evs[len(evs)-1].Data.(json.RawMessage), &c)
	return c.DrawerIDs
}

// word is the latest your_word uid received, or "".
func (h *harness) word(uid string) string {
	yw := h.conns[uid].private("your_word")
	if len(yw) == 0 {
		return ""
	}
	var w struct {
		Word string `json:"word"`
	}
	_ = json.Unmarshal(yw[len(yw)-1], &w)
	return w.Word
}

// M2 exit criterion: with concurrent correct guesses, exactly one is first, and every
// client sees the same order. With 8 players, two of them draw.
func TestConcurrentCorrectGuessesOneFirst(t *testing.T) {
	h := newHarness(t, "u0", DefaultSettings, nil)
	for i := range 8 {
		h.join(fmt.Sprintf("u%d", i))
	}
	drawers, word := h.startDrawingAll("u0")
	if len(drawers) != 2 {
		t.Fatalf("drawers = %v, want a pair", drawers)
	}

	var guessers []string
	for uid := range h.conns {
		if !slices.Contains(drawers, uid) {
			guessers = append(guessers, uid)
		}
	}
	start := make(chan struct{})
	var wg sync.WaitGroup
	for _, uid := range guessers {
		wg.Go(func() {
			<-start
			_ = h.room.Input(context.Background(), h.conns[uid], protocol.Guess{Text: word})
		})
	}
	close(start)
	wg.Wait()
	h.sync()

	type correct struct {
		Seq    int64
		UserID string `json:"user_id"`
		Order  int    `json:"order"`
	}
	var reference []correct
	for uid, c := range h.conns {
		var seen []correct
		for _, ev := range c.events("correct_guess") {
			var cg correct
			_ = json.Unmarshal(ev.Data.(json.RawMessage), &cg)
			cg.Seq = ev.Seq
			seen = append(seen, cg)
		}
		if reference == nil {
			reference = seen
		} else if !slices.Equal(seen, reference) {
			t.Fatalf("%s saw %v, others saw %v", uid, seen, reference)
		}
	}
	if len(reference) != 6 {
		t.Fatalf("correct guesses = %d, want 6", len(reference))
	}
	firsts := 0
	for i, cg := range reference {
		if cg.Order != i+1 {
			t.Fatalf("orders = %v", reference)
		}
		if cg.Order == 1 {
			firsts++
		}
	}
	if firsts != 1 {
		t.Fatalf("%d guessers were first", firsts)
	}
	// Everyone guessed, so the round ended, with the scores of the formula: each drawer gets
	// 10 points per correct guesser.
	ended := h.conns[drawers[0]].events("round_ended")
	if len(ended) != 1 {
		t.Fatal("round didn't end once everyone guessed")
	}
	var re struct {
		RoundPoints map[string]int `json:"round_points"`
	}
	_ = json.Unmarshal(ended[0].Data.(json.RawMessage), &re)
	if re.RoundPoints[drawers[0]] != 60 || re.RoundPoints[drawers[1]] != 60 ||
		re.RoundPoints[reference[0].UserID] != 100 || re.RoundPoints[reference[5].UserID] != 25 {
		t.Fatalf("round points = %v", re.RoundPoints)
	}
}

// containsWord looks for the word in every string value of a message except ids, colors
// and other machine fields, which are random hex or fixed names that could contain a short
// word by chance.
func containsWord(raw []byte, word string) bool {
	var v any
	_ = json.Unmarshal(raw, &v)
	var walk func(key string, v any) bool
	walk = func(key string, v any) bool {
		switch x := v.(type) {
		case map[string]any:
			for k, vv := range x {
				if walk(k, vv) {
					return true
				}
			}
		case []any:
			for _, vv := range x {
				if walk(key, vv) {
					return true
				}
			}
		case string:
			if key == "type" || key == "color" || strings.HasSuffix(key, "id") || strings.HasSuffix(key, "_ids") || key == "phase" {
				return false
			}
			return strings.Contains(Normalize(x), word)
		}
		return false
	}
	return walk("", v)
}

// M2 exit criterion: guessers never receive the word before the round ends — not by a
// late join, a reconnect, a chat that contains it, or the hints. Four players and a late
// joiner make a 5-player match with two rounds of two drawers, then one with a single drawer.
func TestGuessersNeverSeeWordBeforeRoundEnded(t *testing.T) {
	h := newHarness(t, "a", DefaultSettings, nil)
	// Every connection a guesser ever had, so replaced sockets are checked too.
	history := map[string][]*fakeConn{}
	for _, uid := range []string{"a", "b", "c", "d"} {
		history[uid] = []*fakeConn{h.join(uid)}
	}

	h.input("a", protocol.Start{})
	h.advance(StartingFor)
	for round := 1; ; round++ {
		marks := map[*fakeConn]int{}
		for _, cs := range history {
			for _, c := range cs {
				marks[c] = len(c.all())
			}
		}
		drawers := h.drawerIDs("a", "choosing")
		if want := []int{2, 2, 1}[min(round, 3)-1]; len(drawers) != want {
			t.Fatalf("round %d: drawers = %v, want %d", round, drawers, want)
		}
		h.input(drawers[0], protocol.ChooseWord{Index: 1})
		h.sync()
		var w struct{ Word string }
		w.Word = h.word(drawers[0])
		if w.Word == "" {
			t.Fatalf("round %d: drawer got no word", round)
		}
		for _, d := range drawers {
			if h.word(d) != w.Word {
				t.Fatalf("round %d: drawer %s got %q, want %q", round, d, h.word(d), w.Word)
			}
		}

		var guessers []string
		for uid := range h.conns {
			if !slices.Contains(drawers, uid) {
				guessers = append(guessers, uid)
			}
		}
		g1, g2 := guessers[0], guessers[1]
		h.sync()
		seen := lastSeq(h.conns[g1])
		h.input(g1, protocol.Guess{Text: "is it " + strings.ToUpper(w.Word) + "s?"}) // contains the word
		// g1 drops and resumes from before its own guess: the replay is checked too.
		h.room.Leave(h.conns[g1])
		h.sync()
		resumed := h.resume(g1, seen)
		history[g1] = append(history[g1], resumed)
		marks[resumed] = 0
		if round == 1 {
			e := h.join("e") // late joiner mid-round
			history["e"] = []*fakeConn{e}
			marks[e] = 0
		}
		reconnected := h.join(g2) // the same guesser on a new socket mid-round
		history[g2] = append(history[g2], reconnected)
		marks[reconnected] = 0
		h.advance(DefaultSettings.Draw * 75 / 100) // both hints
		if len(resumed.private("welcome")) != 0 || len(resumed.private("resumed")) != 1 {
			t.Fatalf("round %d: %s's reconnect with last_seq %d wasn't a resume", round, g1, seen)
		}
		h.advance(DefaultSettings.Draw) // time is up

		for uid, cs := range history {
			if slices.Contains(drawers, uid) {
				continue
			}
			for _, c := range cs {
				msgs := c.all()[marks[c]:]
				ended := false
				for _, m := range msgs {
					isEnd := m.Type == "event" && strings.Contains(string(m.Data), `"type":"round_ended"`)
					if !ended && !isEnd && containsWord(m.raw, w.Word) {
						t.Fatalf("round %d: %s received the word %q before round_ended: %s", round, uid, w.Word, m.raw)
					}
					ended = ended || isEnd
				}
				if c == h.conns[uid] && !ended {
					t.Fatalf("round %d: %s never got round_ended", round, uid)
				}
			}
		}
		h.advance(RevealFor)
		if len(h.conns["a"].events("match_ended")) > 0 {
			if round != 3 {
				t.Fatalf("match ended after %d rounds, want 3 (a+b, c+d, then the late joiner e)", round)
			}
			return
		}
	}
}

func TestReplacedSocket(t *testing.T) {
	h := newHarness(t, "a", DefaultSettings, nil)
	old := h.join("a")
	h.join("b")
	newer := h.join("a")
	if old.closed != protocol.CloseReplaced {
		t.Fatalf("old socket close code = %d", old.closed)
	}
	// The old socket's late messages and leave change nothing.
	_ = h.room.Input(context.Background(), old, protocol.Start{})
	h.room.Leave(old)
	h.sync()
	if len(h.conns["b"].events("player_left")) != 0 || len(newer.events("match_started")) != 0 {
		t.Fatal("a replaced socket still acted for its player")
	}
	if got := h.room.Summary(); got.Players != 2 || got.Seats != 2 || got.Playing {
		t.Fatalf("summary = %+v", got)
	}
	// A player who leaves keeps their seat through the rejoin grace (ADR 0008).
	h.room.Leave(h.conns["b"])
	h.sync()
	if got := h.room.Summary(); got.Players != 1 || got.Seats != 2 {
		t.Fatalf("after b left: summary = %+v, want 1 player in 2 seats", got)
	}
	h.join("b")
	h.input("a", protocol.Start{})
	h.sync()
	if !h.room.Summary().Playing {
		t.Fatal("summary not updated after start")
	}
}

// slowConn never drains: like the real connection, a full queue closes it.
type slowConn struct {
	fakeConn
	queued int
}

func (c *slowConn) Send(b []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.queued++
	if c.queued > 4 {
		c.closed = 1013
	}
}

func TestSlowClientDoesNotBlockTheRoom(t *testing.T) {
	h := newHarness(t, "a", DefaultSettings, nil)
	slow := &slowConn{fakeConn: fakeConn{uid: "s"}}
	if err := h.room.Join(context.Background(), slow, "S", 0); err != nil {
		t.Fatal(err)
	}
	fast := h.join("a")
	for i := range 20 {
		h.input("a", protocol.Guess{Text: fmt.Sprintf("hello %d", i)})
		h.advance(time.Second) // keep the guess bucket topped up
	}
	if n := len(fast.events("chat")); n != 20 {
		t.Fatalf("fast client got %d chats, want 20", n)
	}
	if slow.closed != 1013 {
		t.Fatal("slow client wasn't closed")
	}
}

func TestEmptyRoomStopsAndReportsClosed(t *testing.T) {
	h := newHarness(t, "a", DefaultSettings, nil)
	h.join("a")
	h.room.Leave(h.conns["a"])
	h.sync()
	h.clock.Advance(EmptyTTL)
	// The tick closes the room; nothing to sync with afterwards.
	select {
	case <-h.room.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("empty room didn't stop")
	}
	if !h.room.Summary().Closed {
		t.Fatal("summary doesn't say closed")
	}
	if err := h.room.Join(context.Background(), &fakeConn{uid: "b"}, "B", 0); err != ErrClosed {
		t.Fatalf("join after close: %v", err)
	}
}

func TestShutdownClosesSockets(t *testing.T) {
	clock := &fakeClock{now: t0}
	r := mustNew(t, Config{ID: "r1", Mode: "classic", Settings: DefaultSettings, Clock: clock,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Metrics: NoopMetrics()})
	ctx, cancel := context.WithCancel(context.Background())
	go r.Run(ctx)
	c := &fakeConn{uid: "a"}
	if err := r.Join(context.Background(), c, "A", 0); err != nil {
		t.Fatal(err)
	}
	cancel()
	<-r.Done()
	if c.closed != 1001 {
		t.Fatalf("close code = %d, want 1001", c.closed)
	}
}

// game.reconnects counts each resume by how it went, including the refused ones.
func TestReconnectMetrics(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	m, err := NewMetrics(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)).Meter("t"))
	if err != nil {
		t.Fatal(err)
	}
	s := DefaultSettings
	s.MaxPlayers = 2
	h := newHarness(t, "a", s, m)
	a := h.join("a")
	h.join("b")
	h.sync()
	evs := a.events("")
	lastSeen := evs[len(evs)-1].Seq

	// a comes back with what it saw: resumed.
	if err := h.room.Join(context.Background(), &fakeConn{uid: "a"}, "Name a", lastSeen); err != nil {
		t.Fatal(err)
	}
	// z claims a seq but holds no seat in a full room: refused, failed.
	if err := h.room.Join(context.Background(), &fakeConn{uid: "z"}, "Name z", lastSeen); !errors.Is(err, ErrRoomFull) {
		t.Fatalf("err = %v, want ErrRoomFull", err)
	}
	h.sync()

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	got := map[string]int64{}
	for _, sm := range rm.ScopeMetrics {
		for _, mt := range sm.Metrics {
			if mt.Name != "game.reconnects" {
				continue
			}
			for _, dp := range mt.Data.(metricdata.Sum[int64]).DataPoints {
				v, _ := dp.Attributes.Value(attribute.Key("result"))
				got[v.AsString()] = dp.Value
			}
		}
	}
	if got["resumed"] != 1 || got["failed"] != 1 || got["snapshot"] != 0 {
		t.Fatalf("reconnects = %v, want 1 resumed and 1 failed", got)
	}
}

func TestGuessMetrics(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	m, err := NewMetrics(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)).Meter("t"))
	if err != nil {
		t.Fatal(err)
	}
	h := newHarness(t, "a", DefaultSettings, m)
	h.join("a")
	h.join("b")
	h.join("c")
	drawer, word := h.startDrawing("a")
	guesser := "b"
	if drawer == "b" {
		guesser = "c"
	}
	h.input(guesser, protocol.Guess{Text: "nope"})
	h.input(guesser, protocol.Guess{Text: word})
	h.sync()

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	got := map[string]int64{}
	rooms := int64(-1)
	for _, sm := range rm.ScopeMetrics {
		for _, mt := range sm.Metrics {
			switch mt.Name {
			case "game.guesses":
				for _, dp := range mt.Data.(metricdata.Sum[int64]).DataPoints {
					v, _ := dp.Attributes.Value(attribute.Key("result"))
					got[v.AsString()] = dp.Value
				}
			case "game.rooms.active":
				rooms = mt.Data.(metricdata.Sum[int64]).DataPoints[0].Value
			}
		}
	}
	if got["correct"] != 1 || got["wrong"] != 1 || rooms != 1 {
		t.Fatalf("guesses = %v, rooms active = %d", got, rooms)
	}
}

// recordingSink records what the room hands to the publisher, with the span it came with.
type recordingSink struct {
	mu      sync.Mutex
	items   []any // FinishedRound or FinishedMatch
	entries []OutboxEntry
	spans   []trace.SpanContext
}

func (s *recordingSink) RoundFinished(ctx context.Context, r FinishedRound, e OutboxEntry) {
	s.add(ctx, r, e)
}
func (s *recordingSink) MatchFinished(ctx context.Context, m FinishedMatch, e OutboxEntry) {
	s.add(ctx, m, e)
}

func (s *recordingSink) add(ctx context.Context, item any, e OutboxEntry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.items = append(s.items, item)
	s.entries = append(s.entries, e)
	s.spans = append(s.spans, trace.SpanContextFromContext(ctx))
}

func TestFinishedRoundsAndMatchesReachTheSink(t *testing.T) {
	sink := &recordingSink{}
	spans := tracetest.NewSpanRecorder()
	h := newHarness(t, "a", DefaultSettings, nil, func(r *Room) {
		r.events = sink
		r.tracer = sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(spans)).Tracer("t")
	})
	h.join("a")
	h.join("b")
	drawer, word := h.startDrawing("a")
	h.input(drawer, stroke("s1", 2))
	guesser := map[string]string{"a": "b", "b": "a"}[drawer]
	h.input(guesser, protocol.Guess{Text: word}) // the only guesser: round 1 ends
	h.sync()
	h.room.Leave(h.conns[guesser]) // fewer than 2 players: the match ends
	h.sync()

	sink.mu.Lock()
	defer sink.mu.Unlock()
	if len(sink.items) != 2 {
		t.Fatalf("sink got %d items, want the round then the match", len(sink.items))
	}
	r, ok := sink.items[0].(FinishedRound)
	if !ok || r.RoundNo != 1 || r.Word != word || len(r.Canvas) != 1 || r.DrawerIDs[0] != drawer {
		t.Fatalf("round = %+v", sink.items[0])
	}
	m, ok := sink.items[1].(FinishedMatch)
	if !ok || m.MatchID != r.MatchID || m.Mode != "classic" || m.Rounds != 1 || len(m.Results) != 2 {
		t.Fatalf("match = %+v", sink.items[1])
	}
	// Each hand-off carries its end span, which becomes the parent of the publish spans.
	ended := map[string]trace.SpanContext{}
	for _, s := range spans.Ended() {
		ended[s.Name()] = s.SpanContext()
	}
	if !sink.spans[0].IsValid() || !sink.spans[0].Equal(ended["game.round.end"]) {
		t.Fatalf("round span = %v, want game.round.end %v", sink.spans[0], ended["game.round.end"])
	}
	if !sink.spans[1].IsValid() || !sink.spans[1].Equal(ended["game.match.end"]) {
		t.Fatalf("match span = %v, want game.match.end", sink.spans[1])
	}
}

// game.room.input_lag is how long a player's message waited for the actor: here 2 s on the
// fake clock while the actor wasn't running, as when it is stuck behind earlier steps.
func TestInputLagMeasuresTheWaitForTheActor(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	m, err := NewMetrics(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)).Meter("t"))
	if err != nil {
		t.Fatal(err)
	}
	clock := &fakeClock{now: t0}
	r := mustNew(t, Config{ID: "r1", Mode: "classic", HostID: "a", Settings: DefaultSettings, Clock: clock,
		Rand: rand.New(rand.NewPCG(9, 9)), Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Metrics: m})
	if err := r.Input(context.Background(), &fakeConn{uid: "a"}, protocol.Guess{Text: "apple"}); err != nil {
		t.Fatal(err)
	}
	clock.Advance(2 * time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	go r.Run(ctx)
	t.Cleanup(func() { cancel(); <-r.Done() })
	if err := r.sync(context.Background()); err != nil {
		t.Fatal(err)
	}

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	for _, sm := range rm.ScopeMetrics {
		for _, mt := range sm.Metrics {
			if mt.Name != "game.room.input_lag" {
				continue
			}
			if mt.Unit != "ms" {
				t.Fatalf("unit = %q, want ms", mt.Unit)
			}
			dps := mt.Data.(metricdata.Histogram[float64]).DataPoints
			if len(dps) != 1 || dps[0].Attributes.Len() != 0 {
				t.Fatalf("want one series without attributes, got %+v", dps)
			}
			if dps[0].Count != 1 || dps[0].Sum != 2000 {
				t.Fatalf("count = %d, sum = %v ms, want 1 input that waited 2000 ms", dps[0].Count, dps[0].Sum)
			}
			return
		}
	}
	t.Fatal("game.room.input_lag not recorded")
}
