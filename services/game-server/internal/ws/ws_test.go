package ws

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Bowenn/gap-project_private/services/game-server/internal/protocol"
	"github.com/Bowenn/gap-project_private/services/game-server/internal/room"
	"github.com/Bowenn/gap-project_private/services/game-server/internal/ticket"
	"github.com/coder/websocket"
	"github.com/golang-jwt/jwt/v5"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

type roomTable map[string]*room.Room

func (t roomTable) Room(id string) *room.Room { return t[id] }

// memRedeemer is single use in memory, for the room's lease with token lease (the room's
// fencing token, like Redis); err makes it fail like a Redis outage.
type memRedeemer struct {
	mu    sync.Mutex
	used  map[string]bool
	lease int64
	err   error
}

func (m *memRedeemer) Redeem(_ context.Context, t ticket.Ticket, token int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return m.err
	}
	if token != m.lease {
		return ticket.ErrNotOwner
	}
	if m.used[t.ID] {
		return ticket.ErrReused
	}
	m.used[t.ID] = true
	return nil
}

type env struct {
	t        *testing.T
	srv      *httptest.Server
	h        *Handler
	key      ed25519.PrivateKey
	redeemer *memRedeemer
	cancel   context.CancelFunc
	rooms    roomTable
}

func newEnv(t *testing.T, maxPlayers int, hello time.Duration) *env {
	t.Helper()
	return newEnvWithMeter(t, maxPlayers, hello, noop.NewMeterProvider().Meter("t"))
}

func newEnvWithMeter(t *testing.T, maxPlayers int, hello time.Duration, meter metric.Meter) *env {
	t.Helper()
	pub, key, _ := ed25519.GenerateKey(rand.Reader)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx, cancel := context.WithCancel(context.Background())
	s := room.DefaultSettings
	s.MaxPlayers = maxPlayers
	r, err := room.New(room.Config{ID: "r1", Mode: "classic", HostID: "u1", Settings: s, Log: log, Metrics: room.NoopMetrics(), Token: roomToken})
	if err != nil {
		t.Fatal(err)
	}
	go r.Run(ctx)
	rooms := roomTable{"r1": r}
	red := &memRedeemer{used: map[string]bool{}, lease: roomToken}
	h, err := NewHandler(Config{
		ServerID: "gs-1", Rooms: rooms, Verifier: ticket.NewVerifier([]ed25519.PublicKey{pub}, time.Now),
		Redeemer: red, Origins: []string{"localhost:8080"}, HelloTimeout: hello, Log: log,
		Meter: meter,
	})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.Handle("GET /ws/{server_id}/rooms/{room_id}", h)
	srv := httptest.NewServer(mux)
	e := &env{t: t, srv: srv, h: h, key: key, redeemer: red, cancel: cancel, rooms: rooms}
	t.Cleanup(func() {
		cancel()
		<-r.Done()
		_ = h.Shutdown(context.Background())
		srv.Close()
	})
	return e
}

// sign makes a ticket as the lobby does.
func (e *env) sign(uid, roomID, jti string) string {
	tok := jwt.NewWithClaims(jwt.SigningMethodEdDSA, jwt.MapClaims{
		"iss": ticket.Issuer, "aud": ticket.Audience, "sub": uid, "jti": jti, "room_id": roomID, "name": "Name " + uid,
		"iat": time.Now().Unix(), "exp": time.Now().Add(ticket.TTL).Unix(),
	})
	tok.Header["kid"] = ticket.Thumbprint(e.key.Public().(ed25519.PublicKey))
	s, err := tok.SignedString(e.key)
	if err != nil {
		e.t.Fatal(err)
	}
	return s
}

func (e *env) dial(path string, opts *websocket.DialOptions) *websocket.Conn {
	e.t.Helper()
	c, _, err := websocket.Dial(e.t.Context(), "ws"+strings.TrimPrefix(e.srv.URL, "http")+path, opts)
	if err != nil {
		e.t.Fatalf("dial %s: %v", path, err)
	}
	c.SetReadLimit(1 << 20)
	e.t.Cleanup(func() { c.CloseNow() })
	return c
}

func write(t *testing.T, c *websocket.Conn, typ string, data any) {
	t.Helper()
	if err := c.Write(t.Context(), websocket.MessageText, protocol.Marshal(typ, data)); err != nil {
		t.Fatal(err)
	}
}

// next reads messages until one of type typ arrives.
func next(t *testing.T, c *websocket.Conn, typ string) json.RawMessage {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	for {
		_, b, err := c.Read(ctx)
		if err != nil {
			t.Fatalf("waiting for %s: %v", typ, err)
		}
		var m struct {
			Type string          `json:"type"`
			Data json.RawMessage `json:"data"`
		}
		_ = json.Unmarshal(b, &m)
		if m.Type == typ {
			return m.Data
		}
	}
}

// closeCode reads until the server closes the socket and returns the close code.
func closeCode(t *testing.T, c *websocket.Conn) websocket.StatusCode {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	for {
		if _, _, err := c.Read(ctx); err != nil {
			return websocket.CloseStatus(err)
		}
	}
}

func (e *env) join(uid, jti string) *websocket.Conn {
	e.t.Helper()
	c := e.dial("/ws/gs-1/rooms/r1", nil)
	write(e.t, c, "hello", map[string]any{"ticket": e.sign(uid, "r1", jti)})
	next(e.t, c, "welcome")
	return c
}

func TestHelloThenWelcomeAndPing(t *testing.T) {
	e := newEnv(t, 8, time.Second)
	c := e.join("u1", "j1")
	write(t, c, "ping", map[string]any{"t": 42})
	if got := string(next(t, c, "pong")); got != `{"t":42}` {
		t.Fatalf("pong = %s", got)
	}
	// Another player joining shows up as an event, with the name from the ticket.
	e.join("u2", "j2")
	ev := next(t, c, "event")
	if !strings.Contains(string(ev), `"player_joined"`) || !strings.Contains(string(ev), `"Name u2"`) {
		t.Fatalf("event = %s", ev)
	}
}

func TestTicketChecks(t *testing.T) {
	e := newEnv(t, 8, time.Second)
	e.join("u1", "j1")

	tests := []struct {
		name   string
		ticket string
		want   websocket.StatusCode
	}{
		{"reused ticket", e.sign("u1", "r1", "j1"), protocol.CloseBadTicket},
		{"ticket for another room", e.sign("u1", "r2", "j3"), protocol.CloseBadTicket},
		{"garbage", "nope", protocol.CloseBadTicket},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := e.dial("/ws/gs-1/rooms/r1", nil)
			write(t, c, "hello", map[string]any{"ticket": tt.ticket})
			if got := closeCode(t, c); got != tt.want {
				t.Fatalf("close = %d, want %d", got, tt.want)
			}
		})
	}
	if e.redeemer.used["j3"] {
		t.Fatal("an invalid ticket was redeemed")
	}
}

// The room was taken with this fencing token.
const roomToken = 7

// A ticket is redeemed against the room's own fencing token: once the lease carries a newer
// one (a takeover), the join is refused as moved, and the client asks the lobby again.
func TestRedeemChecksTheRoomsFencingToken(t *testing.T) {
	e := newEnv(t, 8, time.Second)
	e.join("u1", "j1")
	e.redeemer.mu.Lock()
	e.redeemer.lease = roomToken + 1
	e.redeemer.mu.Unlock()

	c := e.dial("/ws/gs-1/rooms/r1", nil)
	write(t, c, "hello", map[string]any{"ticket": e.sign("u2", "r1", "j2")})
	if got := closeCode(t, c); got != protocol.CloseRoomMoved {
		t.Fatalf("close = %d, want %d (room moved)", got, protocol.CloseRoomMoved)
	}
	if e.redeemer.used["j2"] {
		t.Fatal("a ticket was redeemed against a lease this room no longer holds")
	}
}

func TestUnknownRoomAndServer(t *testing.T) {
	e := newEnv(t, 8, time.Second)
	c := e.dial("/ws/gs-1/rooms/r9", nil)
	write(t, c, "hello", map[string]any{"ticket": e.sign("u1", "r9", "j1")})
	if got := closeCode(t, c); got != protocol.CloseRoomMoved {
		t.Fatalf("close = %d", got)
	}
	resp, err := http.Get(e.srv.URL + "/ws/gs-2/rooms/r1")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("other server's path: %d", resp.StatusCode)
	}
}

func TestOriginCheck(t *testing.T) {
	e := newEnv(t, 8, time.Second)
	_, resp, err := websocket.Dial(t.Context(), "ws"+strings.TrimPrefix(e.srv.URL, "http")+"/ws/gs-1/rooms/r1",
		&websocket.DialOptions{HTTPHeader: http.Header{"Origin": {"http://evil.example"}}})
	if err == nil || resp == nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("foreign origin: err = %v", err)
	}
	// Through the gateway: Host has no port, Origin does.
	c := e.dial("/ws/gs-1/rooms/r1", &websocket.DialOptions{Host: "localhost", HTTPHeader: http.Header{"Origin": {"http://localhost:8080"}}})
	c.CloseNow()
}

func TestHelloTimeoutAndBadFrames(t *testing.T) {
	e := newEnv(t, 8, 100*time.Millisecond)
	c := e.dial("/ws/gs-1/rooms/r1", nil)
	if got := closeCode(t, c); got != protocol.CloseHelloTimeout {
		t.Fatalf("no hello: close = %d", got)
	}

	c = e.join("u1", "j1")
	if err := c.Write(t.Context(), websocket.MessageBinary, []byte{1, 2, 3}); err != nil {
		t.Fatal(err)
	}
	if got := closeCode(t, c); got != websocket.StatusUnsupportedData {
		t.Fatalf("binary: close = %d", got)
	}

	c = e.join("u1", "j2")
	big := `{"type":"guess","data":{"text":"` + strings.Repeat("a", protocol.MaxMessageBytes) + `"}}`
	_ = c.Write(t.Context(), websocket.MessageText, []byte(big))
	if got := closeCode(t, c); got != websocket.StatusMessageTooBig {
		t.Fatalf("65 KB: close = %d", got)
	}

	c = e.join("u1", "j3")
	_ = c.Write(t.Context(), websocket.MessageText, []byte(`{"type":"teleport"}`))
	if got := string(next(t, c, "error")); !strings.Contains(got, "bad_request") {
		t.Fatalf("unknown type: %s", got)
	}
}

func TestRoomFullAndRedisDown(t *testing.T) {
	e := newEnv(t, 1, time.Second)
	e.join("u1", "j1")
	c := e.dial("/ws/gs-1/rooms/r1", nil)
	write(t, c, "hello", map[string]any{"ticket": e.sign("u2", "r1", "j2")})
	if got := closeCode(t, c); got != protocol.CloseRoomFull {
		t.Fatalf("full: close = %d", got)
	}

	e.redeemer.mu.Lock()
	e.redeemer.err = errors.Join(ticket.ErrUnavailable, errors.New("connection refused"))
	e.redeemer.mu.Unlock()
	c = e.dial("/ws/gs-1/rooms/r1", nil)
	write(t, c, "hello", map[string]any{"ticket": e.sign("u1", "r1", "j3")})
	if got := closeCode(t, c); got != websocket.StatusInternalError {
		t.Fatalf("redis down: close = %d", got)
	}
}

func TestReconnectReplacesTheOldSocket(t *testing.T) {
	e := newEnv(t, 8, time.Second)
	old := e.join("u1", "j1")
	e.join("u1", "j2")
	if got := closeCode(t, old); got != protocol.CloseReplaced {
		t.Fatalf("old socket: close = %d", got)
	}
}

func TestShutdownClosesWithGoingAway(t *testing.T) {
	e := newEnv(t, 8, time.Second)
	c := e.join("u1", "j1")
	e.cancel() // the room's context, as main cancels on SIGTERM
	if got := closeCode(t, c); got != websocket.StatusGoingAway {
		t.Fatalf("close = %d, want 1001", got)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := e.h.Shutdown(ctx); err != nil {
		t.Fatalf("handler didn't drain: %v", err)
	}
}

// Closing a socket while messages are still being written must still send the close code:
// cancelling a write's context makes the library drop the connection without a close frame.
func TestCloseDuringWritesKeepsTheCloseCode(t *testing.T) {
	accepted := make(chan *Conn, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		pc := NewConn(context.Background(), c, "u1", func() {}, func(time.Duration, time.Time) {})
		accepted <- pc
		<-pc.writerDone
	}))
	defer srv.Close()

	for i := range 20 {
		c, _, err := websocket.Dial(t.Context(), "ws"+strings.TrimPrefix(srv.URL, "http"), nil)
		if err != nil {
			t.Fatal(err)
		}
		c.SetReadLimit(1 << 22)
		pc := <-accepted
		big := []byte(`{"type":"x","data":"` + strings.Repeat("a", 512<<10) + `"}`)
		for range 8 {
			pc.Send(big)
		}
		pc.Close(protocol.CloseReplaced, "connected again elsewhere")
		if got := closeCode(t, c); got != protocol.CloseReplaced {
			t.Fatalf("attempt %d: close = %d, want %d", i, got, protocol.CloseReplaced)
		}
		c.CloseNow()
	}
}

// sendLag reads game.ws.send_lag from the reader.
func sendLag(t *testing.T, reader *sdkmetric.ManualReader) (metricdata.Metrics, metricdata.HistogramDataPoint[float64]) {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(t.Context(), &rm); err != nil {
		t.Fatal(err)
	}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name == "game.ws.send_lag" {
				dps := m.Data.(metricdata.Histogram[float64]).DataPoints
				if len(dps) != 1 {
					t.Fatalf("send_lag has %d series, want 1 (no attributes)", len(dps))
				}
				return m, dps[0]
			}
		}
	}
	t.Fatal("game.ws.send_lag not recorded")
	return metricdata.Metrics{}, metricdata.HistogramDataPoint[float64]{}
}

// Every message written to a socket records its send lag, in milliseconds and without
// attributes (ids never go on metrics).
func TestSendLagIsRecordedForEveryWrite(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	meter := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)).Meter("t")
	e := newEnvWithMeter(t, 8, time.Second, meter)
	c := e.join("u1", "j1") // welcome
	write(t, c, "ping", map[string]any{"t": 1})
	next(t, c, "pong")

	// The writer records a lag after its write returns, so the client can read the pong
	// before the pong's lag is recorded: wait for it.
	m, dp := sendLag(t, reader)
	for deadline := time.Now().Add(2 * time.Second); dp.Count < 2 && time.Now().Before(deadline); {
		time.Sleep(5 * time.Millisecond)
		m, dp = sendLag(t, reader)
	}
	if m.Unit != "ms" {
		t.Fatalf("unit = %q, want ms", m.Unit)
	}
	if dp.Attributes.Len() != 0 {
		t.Fatalf("attributes = %v, want none", dp.Attributes.ToSlice())
	}
	if dp.Count < 2 {
		t.Fatalf("count = %d, want at least 2 (welcome and pong)", dp.Count)
	}
}

// lagConn is a Conn on a real socket that records its send lags.
type lagConn struct {
	pc     *Conn
	client *websocket.Conn
	mu     sync.Mutex
	lags   []time.Duration
}

func newLagConn(t *testing.T) *lagConn {
	t.Helper()
	lc := &lagConn{}
	accepted := make(chan *Conn, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		pc := NewConn(context.Background(), c, "u1", func() {}, func(lag time.Duration, _ time.Time) {
			lc.mu.Lock()
			lc.lags = append(lc.lags, lag)
			lc.mu.Unlock()
		})
		accepted <- pc
		<-pc.writerDone
	}))
	t.Cleanup(srv.Close)
	c, _, err := websocket.Dial(t.Context(), "ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	c.SetReadLimit(1 << 22)
	t.Cleanup(func() { c.CloseNow() })
	lc.pc, lc.client = <-accepted, c
	return lc
}

func (lc *lagConn) recorded() []time.Duration {
	<-lc.pc.writerDone
	lc.mu.Lock()
	defer lc.mu.Unlock()
	return append([]time.Duration(nil), lc.lags...)
}

// More than the kernel's socket buffers hold on loopback, so the writer blocks until the
// client reads, as it does behind a slow network or when the CPU is pinned.
const (
	bigMessages = 8
	bigSize     = 1 << 20
)

var bigMessage = []byte(`{"type":"x","data":"` + strings.Repeat("a", bigSize) + `"}`)

// The lag runs from Send, not from the write: messages that wait in the queue behind a
// blocked write carry the wait. That is what game.broadcast.duration can't see.
func TestSendLagIncludesTheWaitInTheQueue(t *testing.T) {
	lc := newLagConn(t)
	const hold = 300 * time.Millisecond
	for range bigMessages {
		lc.pc.Send(bigMessage)
	}
	time.Sleep(hold) // the client isn't reading yet
	for i := range bigMessages {
		if _, _, err := lc.client.Read(t.Context()); err != nil {
			t.Fatalf("message %d: %v", i, err)
		}
	}
	lc.pc.Close(int(websocket.StatusNormalClosure), "")
	closeCode(t, lc.client) // answers the close handshake
	lags := lc.recorded()
	if len(lags) != bigMessages {
		t.Fatalf("%d lags recorded, want %d", len(lags), bigMessages)
	}
	if last := lags[len(lags)-1]; last < hold {
		t.Fatalf("the last message's lag = %v, want at least the %v it waited", last, hold)
	}
}

// A message that is never written (the socket closed first) records nothing, so a socket
// closed as slow can't drag the lag towards its 10-second write timeout.
func TestSendLagSkipsMessagesNeverWritten(t *testing.T) {
	lc := newLagConn(t)
	for range bigMessages {
		lc.pc.Send(bigMessage)
	}
	lc.pc.Close(int(protocol.CloseReplaced), "connected again elsewhere")
	lc.pc.Send(bigMessage) // after the close: dropped
	received := 0
	for {
		if _, _, err := lc.client.Read(t.Context()); err != nil {
			if got := websocket.CloseStatus(err); got != protocol.CloseReplaced {
				t.Fatalf("close = %d, want %d", got, protocol.CloseReplaced)
			}
			break
		}
		received++
	}
	lags := lc.recorded()
	if received >= bigMessages {
		t.Fatalf("received all %d messages: the close didn't win over the queue", received)
	}
	if len(lags) != received {
		t.Fatalf("%d lags recorded for %d messages written", len(lags), received)
	}
}

// What the metric adds per message and socket: a clock read and one Record, from many
// writer goroutines into one series.
func BenchmarkSendLagRecord(b *testing.B) {
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(sdkmetric.NewManualReader()))
	h, err := NewHandler(Config{Meter: mp.Meter("b")})
	if err != nil {
		b.Fatal(err)
	}
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			queued := time.Now()
			h.sendLag.Record(context.Background(), float64(time.Since(queued))/float64(time.Millisecond))
		}
	})
}
