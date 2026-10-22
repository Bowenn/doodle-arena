package relay

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	mrand "math/rand/v2"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Bowenn/gap-project_private/libs/go/redistest"
	"github.com/Bowenn/gap-project_private/services/game-server/internal/protocol"
	"github.com/Bowenn/gap-project_private/services/game-server/internal/room"
	"github.com/Bowenn/gap-project_private/services/game-server/internal/roomstore"
	"github.com/Bowenn/gap-project_private/services/game-server/internal/server"
	"github.com/Bowenn/gap-project_private/services/game-server/internal/ticket"
	"github.com/coder/websocket"
	"github.com/golang-jwt/jwt/v5"
	"github.com/redis/go-redis/v9"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func TestMain(m *testing.M) { redistest.Main(m) }

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

// owner plays a room the way its game server does: the game, then one commit per step through
// the real room store, with a snapshot at phase changes (design doc, section 9).
type owner struct {
	t     *testing.T
	g     *room.Game
	store *roomstore.Store
	id    string
	token int64
	now   time.Time
}

func newOwner(t *testing.T, store *roomstore.Store, roomID string) *owner {
	t.Helper()
	owned, err := store.Acquire(t.Context(), roomID, "gs-1")
	if err != nil {
		t.Fatal(err)
	}
	o := &owner{t: t, store: store, id: roomID, token: owned.Token, now: time.Now()}
	o.g = room.NewGame(roomID, "a", room.DefaultSettings, mrand.New(mrand.NewPCG(1, 2)), o.now)
	s := o.g.Snapshot() // a new room's first snapshot, as the actor writes it
	if err := store.Commit(t.Context(), roomID, "gs-1", o.token, nil, &s, nil); err != nil {
		t.Fatal(err)
	}
	return o
}

// step runs f on the game and commits what it produced.
func (o *owner) step(f func(g *room.Game, now time.Time)) {
	o.t.Helper()
	f(o.g, o.now)
	var evs []room.StoredEvent
	phase := false
	for _, out := range o.g.TakeOut() {
		if out.Type != "event" {
			continue
		}
		ev := out.Data.(protocol.Event)
		data, err := json.Marshal(ev.Data)
		if err != nil {
			o.t.Fatal(err)
		}
		evs = append(evs, room.StoredEvent{Seq: ev.Seq, Type: ev.Type, Data: data, At: time.Now()})
		phase = phase || room.IsPhaseEvent(ev.Type)
	}
	var snap *room.Snapshot
	if phase {
		s := o.g.Snapshot()
		snap = &s
	}
	// An owner renews its lease every 1.5 s; this one at every step, so a long test keeps it.
	if lost, err := o.store.Renew(o.t.Context(), "gs-1", []roomstore.Lease{{RoomID: o.id, Token: o.token}}); err != nil || len(lost) > 0 {
		o.t.Fatalf("lease: lost %v, err %v", lost, err)
	}
	if err := o.store.Commit(o.t.Context(), o.id, "gs-1", o.token, evs, snap, nil); err != nil {
		o.t.Fatal(err)
	}
}

func (o *owner) advance(d time.Duration) {
	o.now = o.now.Add(d)
	o.step(func(g *room.Game, now time.Time) { g.Advance(now) })
}

// stroke draws n points as uid and returns the stroke's id.
func (o *owner) stroke(uid string, n int) string {
	pts := make([][]int, n)
	for i := range pts {
		pts[i] = []int{i % 10000, i % 10000, i}
	}
	id := fmt.Sprintf("s%d", o.g.Seq())
	o.step(func(g *room.Game, now time.Time) {
		g.Handle(now, uid, protocol.StrokeBatch{StrokeID: id, Points: pts, Color: "#000000", Width: 5})
	})
	return id
}

func (o *owner) undo(uid, strokeID string) {
	o.step(func(g *room.Game, now time.Time) { g.Handle(now, uid, protocol.Undo{StrokeID: strokeID}) })
}

// drawing starts a match with players a and b and takes it to its first round's drawing phase.
// It returns the round's word and drawer.
func (o *owner) drawing() (word, drawer string) {
	o.step(func(g *room.Game, now time.Time) {
		_ = g.Join(now, "a", "Ada", false)
		_ = g.Join(now, "b", "Bo", false)
	})
	o.step(func(g *room.Game, now time.Time) { g.Handle(now, "a", protocol.Start{}) })
	o.advance(room.StartingFor + time.Millisecond) // the countdown: choosing
	drawer = o.g.Snapshot().Drawers[0]
	o.step(func(g *room.Game, now time.Time) { g.Handle(now, drawer, protocol.ChooseWord{Index: 0}) })
	s := o.g.Snapshot()
	if s.Phase != room.PhaseDrawing || s.Word == "" {
		o.t.Fatalf("phase %s, word %q: not drawing", s.Phase, s.Word)
	}
	return s.Word, drawer
}

type env struct {
	t      *testing.T
	rdb    redis.UniversalClient
	store  *roomstore.Store
	relay  *Relay
	srv    *httptest.Server
	key    ed25519.PrivateKey
	reader *sdkmetric.ManualReader
	pub    *recordingPublisher // the wrong guesses the relay sent to Kafka
}

func newEnv(t *testing.T) *env {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return newEnvOn(t, redistest.New(t), key)
}

// newEnvOn is another relay on the same Redis, trusting the same lobby key.
func newEnvOn(t *testing.T, rdb redis.UniversalClient, key ed25519.PrivateKey) *env {
	t.Helper()
	pub := key.Public().(ed25519.PublicKey)
	reader := sdkmetric.NewManualReader()
	var err error
	e := &env{t: t, rdb: rdb, store: roomstore.New(rdb), key: key, reader: reader, pub: &recordingPublisher{}}
	e.relay, err = New(Config{
		ServerID: "relay-1", Store: e.store, Verifier: ticket.NewVerifier([]ed25519.PublicKey{pub}, time.Now),
		Redeem: func(ctx context.Context, tk ticket.Ticket) error {
			return ticket.RedeemViewer(ctx, rdb, tk, time.Now())
		},
		Origins: []string{"localhost:8080"}, Log: quiet,
		Meter:     sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)).Meter("t"),
		Forwarder: e.store, Publisher: e.pub,
	})
	if err != nil {
		t.Fatal(err)
	}
	e.srv = httptest.NewServer(server.NewHandler(server.Info{Service: "game-server"}, e.relay))
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = e.relay.Shutdown(ctx)
		e.srv.Close()
	})
	return e
}

// ticketFor signs a ticket as the lobby does, for the audience aud.
func (e *env) ticketFor(uid, roomID, aud string) string {
	e.t.Helper()
	var id [16]byte
	_, _ = rand.Read(id[:])
	now := time.Now()
	tok := jwt.NewWithClaims(jwt.SigningMethodEdDSA, jwt.MapClaims{
		"iss": ticket.Issuer, "aud": []string{aud}, "sub": uid, "jti": hex.EncodeToString(id[:]),
		"iat": now.Unix(), "exp": now.Add(ticket.TTL).Unix(), "room_id": roomID, "name": "Viewer " + uid,
	})
	tok.Header["kid"] = ticket.Thumbprint(e.key.Public().(ed25519.PublicKey))
	s, err := tok.SignedString(e.key)
	if err != nil {
		e.t.Fatal(err)
	}
	return s
}

type viewer struct {
	t *testing.T
	c *websocket.Conn
}

type msg struct {
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
}

// watch connects a viewer and sends hello with lastSeq.
func (e *env) watch(uid, roomID, aud string, lastSeq int64) *viewer {
	e.t.Helper()
	c, _, err := websocket.Dial(e.t.Context(), "ws"+strings.TrimPrefix(e.srv.URL, "http")+"/ws/relay-1/rooms/"+roomID, nil)
	if err != nil {
		e.t.Fatal(err)
	}
	c.SetReadLimit(1 << 24)
	hello, _ := json.Marshal(map[string]any{"type": "hello", "data": map[string]any{"ticket": e.ticketFor(uid, roomID, aud), "last_seq": lastSeq}})
	if err := c.Write(e.t.Context(), websocket.MessageText, hello); err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(func() { _ = c.CloseNow() })
	return &viewer{t: e.t, c: c}
}

func (v *viewer) read() (msg, error) {
	ctx, cancel := context.WithTimeout(v.t.Context(), 5*time.Second)
	defer cancel()
	_, data, err := v.c.Read(ctx)
	if err != nil {
		return msg{}, err
	}
	var m msg
	if err := json.Unmarshal(data, &m); err != nil {
		v.t.Fatalf("not JSON: %s", data)
	}
	return m, nil
}

func (v *viewer) next() msg {
	v.t.Helper()
	m, err := v.read()
	if err != nil {
		v.t.Fatal(err)
	}
	return m
}

type event struct {
	Seq  int64           `json:"seq"`
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
}

// events reads messages until it has n events, and returns them with every raw message.
func (v *viewer) events(n int) ([]event, []string) {
	v.t.Helper()
	var evs []event
	var raw []string
	for len(evs) < n {
		m := v.next()
		raw = append(raw, string(m.Data))
		if m.Type != "event" {
			continue
		}
		var ev event
		_ = json.Unmarshal(m.Data, &ev)
		evs = append(evs, ev)
	}
	return evs, raw
}

func closeCode(err error) websocket.StatusCode { return websocket.CloseStatus(err) }

func (e *env) counter(name, key, value string) int64 {
	e.t.Helper()
	var rm metricdata.ResourceMetrics
	if err := e.reader.Collect(e.t.Context(), &rm); err != nil {
		e.t.Fatal(err)
	}
	var n int64
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if sum, ok := m.Data.(metricdata.Sum[int64]); ok && m.Name == name {
				for _, dp := range sum.DataPoints {
					if v, ok := dp.Attributes.Value(attribute.Key(key)); ok && v.AsString() == value {
						n += dp.Value
					}
				}
			}
		}
	}
	return n
}

func (e *env) histogramCount(name string) uint64 {
	e.t.Helper()
	var rm metricdata.ResourceMetrics
	if err := e.reader.Collect(e.t.Context(), &rm); err != nil {
		e.t.Fatal(err)
	}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if h, ok := m.Data.(metricdata.Histogram[float64]); ok && m.Name == name {
				var n uint64
				for _, dp := range h.DataPoints {
					n += dp.Count
				}
				return n
			}
		}
	}
	return 0
}

func TestViewerGetsAWelcomeThenEveryEventInOrder(t *testing.T) {
	e := newEnv(t)
	o := newOwner(t, e.store, "r1")
	o.step(func(g *room.Game, now time.Time) {
		_ = g.Join(now, "a", "Ada", false)
		_ = g.Join(now, "b", "Bo", false)
	})

	v := e.watch("v1", "r1", ticket.WatchAudience, 0)
	w := v.next()
	var welcome struct {
		UserID string `json:"user_id"`
		Seq    int64  `json:"seq"`
		Viewer bool   `json:"viewer"`
		View   struct {
			Players []struct {
				UserID    string `json:"user_id"`
				Connected bool   `json:"connected"`
			} `json:"players"`
		} `json:"view"`
	}
	if err := json.Unmarshal(w.Data, &welcome); err != nil || w.Type != "welcome" {
		t.Fatalf("first message = %s %s", w.Type, w.Data)
	}
	if welcome.UserID != "v1" || !welcome.Viewer || welcome.Seq != o.g.Seq() || len(welcome.View.Players) != 2 ||
		!welcome.View.Players[0].Connected {
		t.Fatalf("welcome = %+v, want both players connected, as the owner has them", welcome)
	}

	from := o.g.Seq()
	o.step(func(g *room.Game, now time.Time) { g.Handle(now, "a", protocol.Start{}) })
	o.advance(room.StartingFor + time.Millisecond)
	drawer := o.g.Snapshot().Drawers[0]
	o.step(func(g *room.Game, now time.Time) { g.Handle(now, drawer, protocol.ChooseWord{Index: 0}) })
	for range 20 {
		o.stroke(drawer, 3)
	}
	want := int(o.g.Seq() - from)
	if want < 23 {
		t.Fatalf("the owner produced %d events, want the match's start and 20 strokes", want)
	}
	evs, _ := v.events(want)
	for i, ev := range evs {
		if ev.Seq != from+int64(i)+1 {
			t.Fatalf("event %d has seq %d, want %d: out of order or missing", i, ev.Seq, from+int64(i)+1)
		}
	}
	// A sample is recorded once the relay's write returns, which can be just after the viewer has
	// read the event: wait for the last one instead of reading the count once.
	waitFor(t, func() bool { return e.histogramCount("relay.broadcast.latency") >= uint64(want) })
}

// The word never reaches a viewer while the round runs: not in a welcome built mid-round, not in
// any event before round_ended (design doc, section 6.4; .claude/rules/game-server.md).
func TestTheWordNeverReachesAViewerEarly(t *testing.T) {
	e := newEnv(t)
	o := newOwner(t, e.store, "r1")
	word, drawer := o.drawing()

	v := e.watch("v1", "r1", ticket.WatchAudience, 0)
	w := v.next()
	if w.Type != "welcome" || strings.Contains(string(w.Data), word) {
		t.Fatalf("welcome during drawing: %s", w.Data)
	}
	var welcome struct {
		View struct {
			Phase string `json:"phase"`
			Mask  string `json:"mask"`
			Word  string `json:"word"`
		} `json:"view"`
	}
	_ = json.Unmarshal(w.Data, &welcome)
	if welcome.View.Phase != string(room.PhaseDrawing) || welcome.View.Mask == "" || welcome.View.Word != "" {
		t.Fatalf("view = %+v", welcome.View)
	}

	from := o.g.Seq()
	o.stroke(drawer, 5)
	o.advance(room.DefaultSettings.Draw) // the round ends: now the word is public
	evs, raw := v.events(int(o.g.Seq() - from))
	for i, ev := range evs {
		if ev.Type == "round_ended" {
			if !strings.Contains(string(ev.Data), word) {
				t.Fatalf("round_ended without the word: %s", ev.Data)
			}
			return
		}
		if strings.Contains(raw[i], word) {
			t.Fatalf("%s before round_ended carries the word: %s", ev.Type, raw[i])
		}
	}
	t.Fatal("no round_ended")
}

func TestViewerResumesWithWhatItMissed(t *testing.T) {
	e := newEnv(t)
	o := newOwner(t, e.store, "r1")
	_, drawer := o.drawing()

	v := e.watch("v1", "r1", ticket.WatchAudience, 0)
	if w := v.next(); w.Type != "welcome" {
		t.Fatalf("first = %s", w.Type)
	}
	o.stroke(drawer, 2)
	evs, _ := v.events(1)
	seen := evs[0].Seq
	_ = v.c.Close(websocket.StatusNormalClosure, "")

	for range 3 {
		o.stroke(drawer, 2)
	}
	// Let the stream read them before the viewer comes back.
	waitFor(t, func() bool { return e.relay.streamSeq("r1") == o.g.Seq() })
	v2 := e.watch("v1", "r1", ticket.WatchAudience, seen)
	var got []int64
	for {
		m := v2.next()
		if m.Type == "welcome" {
			t.Fatal("a resume within the replay window got a full welcome")
		}
		if m.Type == "resumed" {
			var r struct {
				Seq int64 `json:"seq"`
			}
			_ = json.Unmarshal(m.Data, &r)
			if r.Seq != o.g.Seq() {
				t.Fatalf("resumed at %d, want %d", r.Seq, o.g.Seq())
			}
			break
		}
		var ev event
		_ = json.Unmarshal(m.Data, &ev)
		got = append(got, ev.Seq)
	}
	if len(got) != 3 || got[0] != seen+1 || got[2] != seen+3 {
		t.Fatalf("replayed %v, want the 3 after %d", got, seen)
	}
}

func TestTicketsForTheWrongRoleOrRoomAreRefused(t *testing.T) {
	e := newEnv(t)
	newOwner(t, e.store, "r1")
	for name, v := range map[string]*viewer{
		"a player's ticket":       e.watch("v1", "r1", ticket.Audience, 0),
		"a ticket for other room": e.watchWith("v1", "r1", e.ticketFor("v1", "r2", ticket.WatchAudience)),
	} {
		if _, err := v.read(); closeCode(err) != protocol.CloseBadTicket {
			t.Errorf("%s: err = %v, want close %d", name, err, protocol.CloseBadTicket)
		}
	}
	tk := e.ticketFor("v1", "r1", ticket.WatchAudience)
	if m := e.watchWith("v1", "r1", tk).next(); m.Type != "welcome" {
		t.Fatalf("first use: %s", m.Type)
	}
	if _, err := e.watchWith("v1", "r1", tk).read(); closeCode(err) != protocol.CloseBadTicket {
		t.Fatalf("second use: err = %v", err)
	}
}

func (e *env) watchWith(uid, roomID, tk string) *viewer {
	e.t.Helper()
	c, _, err := websocket.Dial(e.t.Context(), "ws"+strings.TrimPrefix(e.srv.URL, "http")+"/ws/relay-1/rooms/"+roomID, nil)
	if err != nil {
		e.t.Fatal(err)
	}
	hello, _ := json.Marshal(map[string]any{"type": "hello", "data": map[string]any{"ticket": tk}})
	_ = c.Write(e.t.Context(), websocket.MessageText, hello)
	e.t.Cleanup(func() { _ = c.CloseNow() })
	return &viewer{t: e.t, c: c}
}

func TestAnEndedRoomClosesItsViewers(t *testing.T) {
	old := checkEvery
	checkEvery = 50 * time.Millisecond
	t.Cleanup(func() { checkEvery = old })
	e := newEnv(t)
	o := newOwner(t, e.store, "r1")
	v := e.watch("v1", "r1", ticket.WatchAudience, 0)
	if m := v.next(); m.Type != "welcome" {
		t.Fatalf("first = %s", m.Type)
	}
	if err := e.store.Close(t.Context(), "r1", "gs-1", o.token); err != nil {
		t.Fatal(err)
	}
	for {
		_, err := v.read()
		if err == nil {
			continue
		}
		if closeCode(err) != websocket.StatusNormalClosure {
			t.Fatalf("err = %v, want a normal close", err)
		}
		break
	}
	if _, err := e.watch("v2", "r1", ticket.WatchAudience, 0).read(); closeCode(err) != protocol.CloseNoRoom {
		t.Fatalf("joining an ended room: err = %v, want close %d", err, protocol.CloseNoRoom)
	}
}

// A viewer that stops reading is closed as slow; the others get every event.
func TestASlowViewerDoesntHoldBackTheOthers(t *testing.T) {
	e := newEnv(t)
	o := newOwner(t, e.store, "r1")
	_, drawer := o.drawing()
	slow := e.watch("slow", "r1", ticket.WatchAudience, 0)
	fast := e.watch("fast", "r1", ticket.WatchAudience, 0)
	if m := fast.next(); m.Type != "welcome" {
		t.Fatalf("first = %s", m.Type)
	}
	waitFor(t, func() bool { v, _ := e.relay.Viewers(); return v == 2 })

	from := o.g.Seq()
	const n = 4000
	done := make(chan []event, 1)
	go func() {
		evs, _ := fast.events(n)
		done <- evs
	}()
	// 2,000 strokes of 512 points, each undone (a round holds 40,000 points): about 20 MB, far
	// more than a socket's buffers (macOS grows them to several MB) and its 256-message queue.
	for range n / 2 {
		o.undo(drawer, o.stroke(drawer, 512))
	}
	select {
	case evs := <-done:
		if evs[n-1].Seq != from+n {
			t.Fatalf("the fast viewer's last event is %d, want %d", evs[n-1].Seq, from+n)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("the fast viewer fell behind")
	}
	// The slow one was closed as slow. Its socket is too clogged for the close frame (1013) to
	// get through reliably, so the count tells, not the code.
	waitForWithin(t, 15*time.Second, func() bool { v, _ := e.relay.Viewers(); return v == 1 })
	if n := e.counter("relay.viewers.closed", "reason", "slow"); n != 1 {
		t.Fatalf("relay.viewers.closed{reason=slow} = %d, want 1", n)
	}
	_ = slow
}

// A stream that finds a gap (the stream was trimmed past this relay) reloads from the snapshot
// and gives its viewers a fresh welcome.
func TestAGapReloadsTheRoom(t *testing.T) {
	e := newEnv(t)
	o := newOwner(t, e.store, "r1")
	o.step(func(g *room.Game, now time.Time) { _ = g.Join(now, "a", "Ada", false) })
	st, err := e.relay.stream("r1")
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return e.relay.streamSeq("r1") == o.g.Seq() })
	// Hand the stream an event far ahead, as XREAD would after a trim.
	gap := room.StoredEvent{Seq: o.g.Seq() + 10, Type: "chat", Data: json.RawMessage(`{}`), At: time.Now()}
	reloaded := make(chan int64, 1)
	st.in <- inspectIn{f: func(s *stream) {
		s.apply(t.Context(), []room.StoredEvent{gap})
		reloaded <- s.seq
	}}
	if seq := <-reloaded; seq != o.g.Seq() {
		t.Fatalf("after the gap the stream is at %d, want the store's %d", seq, o.g.Seq())
	}
}

func waitFor(t *testing.T, ok func() bool) { t.Helper(); waitForWithin(t, 5*time.Second, ok) }

func waitForWithin(t *testing.T, d time.Duration, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// streamSeq is the last seq a room's stream has sent (it starts the stream if needed).
func (r *Relay) streamSeq(roomID string) int64 {
	st, err := r.stream(roomID)
	if err != nil {
		return -1
	}
	seq := make(chan int64, 1)
	select {
	case st.in <- inspectIn{f: func(s *stream) { seq <- s.seq }}:
		return <-seq
	case <-st.done:
		return -1
	}
}

// A viewer whose relay went away resumes on another one, which never sent it anything: the new
// stream preloads the room's last events, so the resume needs no full welcome.
func TestAViewerResumesOnAnotherRelay(t *testing.T) {
	e := newEnv(t)
	o := newOwner(t, e.store, "r1")
	_, drawer := o.drawing()
	v := e.watch("v1", "r1", ticket.WatchAudience, 0)
	if w := v.next(); w.Type != "welcome" {
		t.Fatalf("first = %s", w.Type)
	}
	o.stroke(drawer, 2)
	evs, _ := v.events(1)
	seen := evs[0].Seq
	_ = v.c.Close(websocket.StatusNormalClosure, "")
	for range 3 {
		o.stroke(drawer, 2)
	}

	other := newEnvOn(t, e.rdb, e.key)
	v2 := other.watch("v1", "r1", ticket.WatchAudience, seen)
	var got []int64
	for {
		m := v2.next()
		if m.Type == "welcome" {
			t.Fatal("the other relay sent a full welcome instead of a resume")
		}
		if m.Type == "resumed" {
			break
		}
		var ev event
		_ = json.Unmarshal(m.Data, &ev)
		got = append(got, ev.Seq)
	}
	if len(got) != 3 || got[0] != seen+1 {
		t.Fatalf("replayed %v, want the 3 after %d", got, seen)
	}
}
