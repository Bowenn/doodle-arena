package room

import (
	"context"
	"encoding/json"
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
	r := New(Config{ID: "r1", Mode: "classic", HostID: host, Settings: s, Clock: clock,
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
	h.input(host, protocol.Start{})
	h.advance(StartingFor)
	ch := h.conns[host].events("choosing")
	var c struct {
		DrawerIDs []string `json:"drawer_ids"`
	}
	_ = json.Unmarshal(ch[len(ch)-1].Data.(json.RawMessage), &c)
	drawer := c.DrawerIDs[0]
	h.input(drawer, protocol.ChooseWord{Index: 0})
	h.sync()
	yw := h.conns[drawer].private("your_word")
	var w struct {
		Word string `json:"word"`
	}
	_ = json.Unmarshal(yw[len(yw)-1], &w)
	return drawer, w.Word
}

// M2 exit criterion: with concurrent correct guesses, exactly one is first, and every
// client sees the same order.
func TestConcurrentCorrectGuessesOneFirst(t *testing.T) {
	h := newHarness(t, "u0", DefaultSettings, nil)
	for i := range 8 {
		h.join(fmt.Sprintf("u%d", i))
	}
	drawer, word := h.startDrawing("u0")

	var guessers []string
	for uid := range h.conns {
		if uid != drawer {
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
	if len(reference) != 7 {
		t.Fatalf("correct guesses = %d, want 7", len(reference))
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
	// Everyone guessed, so the round ended, with the scores of the formula.
	ended := h.conns[drawer].events("round_ended")
	if len(ended) != 1 {
		t.Fatal("round didn't end once everyone guessed")
	}
	var re struct {
		RoundPoints map[string]int `json:"round_points"`
	}
	_ = json.Unmarshal(ended[0].Data.(json.RawMessage), &re)
	if re.RoundPoints[drawer] != 70 || re.RoundPoints[reference[0].UserID] != 100 || re.RoundPoints[reference[6].UserID] != 20 {
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
// late join, a reconnect, a chat that contains it, or the hints.
func TestGuessersNeverSeeWordBeforeRoundEnded(t *testing.T) {
	h := newHarness(t, "a", DefaultSettings, nil)
	h.join("a")
	h.join("b")
	h.join("c")
	// Every connection a guesser ever had, so replaced sockets are checked too.
	history := map[string][]*fakeConn{"a": {h.conns["a"]}, "b": {h.conns["b"]}, "c": {h.conns["c"]}}

	h.input("a", protocol.Start{})
	h.advance(StartingFor)
	for round := 1; ; round++ {
		marks := map[*fakeConn]int{}
		for _, cs := range history {
			for _, c := range cs {
				marks[c] = len(c.all())
			}
		}
		var ch struct {
			DrawerIDs []string `json:"drawer_ids"`
		}
		evs := h.conns["a"].events("choosing")
		_ = json.Unmarshal(evs[len(evs)-1].Data.(json.RawMessage), &ch)
		drawer := ch.DrawerIDs[0]
		h.input(drawer, protocol.ChooseWord{Index: 1})
		h.sync()
		yw := h.conns[drawer].private("your_word")
		var w struct{ Word string }
		_ = json.Unmarshal(yw[len(yw)-1], &w)
		if w.Word == "" {
			t.Fatalf("round %d: drawer got no word", round)
		}

		var guessers []string
		for uid := range h.conns {
			if uid != drawer {
				guessers = append(guessers, uid)
			}
		}
		g1, g2 := guessers[0], guessers[1]
		h.input(g1, protocol.Guess{Text: "is it " + strings.ToUpper(w.Word) + "s?"}) // contains the word
		if round == 1 {
			d := h.join("d") // late joiner mid-round
			history["d"] = []*fakeConn{d}
			marks[d] = 0
		}
		reconnected := h.join(g2) // the same guesser on a new socket mid-round
		history[g2] = append(history[g2], reconnected)
		marks[reconnected] = 0
		h.advance(DefaultSettings.Draw * 75 / 100) // both hints
		h.advance(DefaultSettings.Draw)            // time is up

		for uid, cs := range history {
			if uid == drawer {
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
			if round != 4 {
				t.Fatalf("match ended after %d rounds, want 4 (a, b, c and the late joiner d)", round)
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
	if got := h.room.Summary(); got.Players != 2 || got.Playing {
		t.Fatalf("summary = %+v", got)
	}
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
	r := New(Config{ID: "r1", Mode: "classic", Settings: DefaultSettings, Clock: clock,
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
	mu    sync.Mutex
	items []any // FinishedRound or FinishedMatch
	spans []trace.SpanContext
}

func (s *recordingSink) RoundFinished(ctx context.Context, r FinishedRound) { s.add(ctx, r) }
func (s *recordingSink) MatchFinished(ctx context.Context, m FinishedMatch) { s.add(ctx, m) }

func (s *recordingSink) add(ctx context.Context, item any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.items = append(s.items, item)
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
	if !ok || m.MatchID != r.MatchID || m.Rounds != 1 || len(m.Results) != 2 {
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
