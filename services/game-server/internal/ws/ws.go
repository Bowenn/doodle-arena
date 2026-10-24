// Package ws serves the game WebSocket, GET /ws/{server_id}/rooms/{room_id} (ADR 0002). It
// checks the join ticket in the first message (never in the URL, which ends up in logs),
// then shuttles messages between the socket and the room's actor.
package ws

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Bowenn/doodle-arena/services/game-server/internal/protocol"
	"github.com/Bowenn/doodle-arena/services/game-server/internal/ratelimit"
	"github.com/Bowenn/doodle-arena/services/game-server/internal/room"
	"github.com/Bowenn/doodle-arena/services/game-server/internal/ticket"
	"github.com/coder/websocket"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// Timeouts and limits.
const (
	DefaultHelloTimeout = 10 * time.Second
	idleTimeout         = 60 * time.Second // the web client pings every 5 s
	writeTimeout        = 10 * time.Second
	sendQueue           = 256 // messages; a client this far behind is closed as slow (> room's 200-event replay)
	floodBurst          = 100 // messages per socket, before the per-player guess limit
	floodPerSecond      = 50
)

// Close codes from the WebSocket standard.
const (
	closeGoingAway   = websocket.StatusGoingAway       // 1001
	closeUnsupported = websocket.StatusUnsupportedData // 1003
	closePolicy      = websocket.StatusPolicyViolation // 1008
	closeInternal    = websocket.StatusInternalError   // 1011
	closeSlow        = websocket.StatusTryAgainLater   // 1013
)

// Rooms finds a room by id.
type Rooms interface {
	Room(id string) *room.Room
}

// Config wires a Handler.
type Config struct {
	ServerID     string
	Rooms        Rooms
	Verifier     *ticket.Verifier
	Redeemer     ticket.Redeemer
	Origins      []string // allowed Origin hosts, for example "localhost:8080"
	HelloTimeout time.Duration
	Log          *slog.Logger
	Meter        metric.Meter
}

// Handler serves game WebSockets.
type Handler struct {
	cfg         Config
	connections metric.Int64UpDownCounter
	slow        metric.Int64Counter
	sendLag     metric.Float64Histogram
	frames      *frameCounters
	tracer      trace.Tracer

	base   context.Context
	cancel context.CancelFunc
	mu     sync.Mutex // orders wg.Add against Shutdown's wg.Wait
	closed bool
	wg     sync.WaitGroup
}

// NewHandler returns the WebSocket handler.
func NewHandler(cfg Config) (*Handler, error) {
	if cfg.HelloTimeout == 0 {
		cfg.HelloTimeout = DefaultHelloTimeout
	}
	conns, err := cfg.Meter.Int64UpDownCounter("game.ws.connections",
		metric.WithDescription("Open game WebSockets"), metric.WithUnit("{connection}"))
	if err != nil {
		return nil, err
	}
	slow, err := cfg.Meter.Int64Counter("game.ws.slow_client_disconnects",
		metric.WithDescription("Sockets closed because their send queue filled up"), metric.WithUnit("{connection}"))
	if err != nil {
		return nil, err
	}
	// The room's broadcast ends when a message is queued; this is the rest of its way out.
	// When the CPU is pinned, writers wait to be scheduled and this grows while the broadcast
	// stays fast (task 5.3's AWS load test).
	sendLag, err := cfg.Meter.Float64Histogram("game.ws.send_lag",
		metric.WithDescription("Time from queueing a message to a socket until the socket write returns"),
		metric.WithUnit("ms"),
		metric.WithExplicitBucketBoundaries(0.1, 0.5, 1, 2.5, 5, 10, 25, 50, 100, 250, 500, 1000, 2500, 5000, 10000))
	if err != nil {
		return nil, err
	}
	frames, err := newFrameCounters(cfg.Meter)
	if err != nil {
		return nil, err
	}
	base, cancel := context.WithCancel(context.Background())
	return &Handler{cfg: cfg, connections: conns, slow: slow, sendLag: sendLag, frames: frames,
		tracer: otel.Tracer("game-server"), base: base, cancel: cancel}, nil
}

// frameCounters count a player socket's frames and bytes by direction (in, out), format (json,
// pb) and type (stroke, other): task 6.4's throughput, per format (ADR 0025).
type frameCounters struct {
	frames metric.Int64Counter
	bytes  metric.Int64Counter
}

func newFrameCounters(m metric.Meter) (*frameCounters, error) {
	frames, err := m.Int64Counter("game.ws.frames",
		metric.WithDescription("WebSocket frames on player sockets, by direction, format and type"), metric.WithUnit("{frame}"))
	if err != nil {
		return nil, err
	}
	bytes, err := m.Int64Counter("game.ws.bytes",
		metric.WithDescription("WebSocket payload bytes on player sockets, by direction, format and type"), metric.WithUnit("By"))
	if err != nil {
		return nil, err
	}
	return &frameCounters{frames: frames, bytes: bytes}, nil
}

func (f *frameCounters) add(direction string, binary, stroke bool, n int) {
	if f == nil {
		return
	}
	format, typ := "json", "other"
	if binary {
		format = "pb"
	}
	if stroke {
		typ = "stroke"
	}
	attrs := metric.WithAttributes(attribute.String("direction", direction), attribute.String("format", format), attribute.String("type", typ))
	f.frames.Add(context.Background(), 1, attrs)
	f.bytes.Add(context.Background(), int64(n), attrs)
}

// Shutdown stops accepting work and waits for every socket to finish (rooms close theirs
// with 1001 when their context ends). http.Server.Shutdown doesn't track hijacked
// connections, so this does.
func (h *Handler) Shutdown(ctx context.Context) error {
	h.mu.Lock()
	h.closed = true
	h.cancel()
	h.mu.Unlock()
	done := make(chan struct{})
	go func() { h.wg.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// ServeHTTP upgrades GET /ws/{server_id}/rooms/{room_id}.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.PathValue("server_id") != h.cfg.ServerID {
		// A stale route: the lobby's URL names another server.
		http.NotFound(w, r)
		return
	}
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		http.Error(w, "shutting down", http.StatusServiceUnavailable)
		return
	}
	h.wg.Add(1)
	h.mu.Unlock()
	defer h.wg.Done()

	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		// The gateway forwards Host without the port, so the library's same-host check would
		// reject every browser; the allowed origins are configured instead.
		OriginPatterns:  h.cfg.Origins,
		CompressionMode: websocket.CompressionDisabled,
		// A client that asks for it sends and gets stroke batches as binary frames (ADR 0025).
		Subprotocols: []string{protocol.StrokesSubprotocol},
	})
	if err != nil {
		return // Accept has written the HTTP error (for example 403 for a foreign origin)
	}
	h.connections.Add(h.base, 1)
	defer h.connections.Add(context.WithoutCancel(h.base), -1)
	c.SetReadLimit(protocol.MaxMessageBytes)

	ctx := otel.GetTextMapPropagator().Extract(h.base, propagation.HeaderCarrier(r.Header))
	h.serve(ctx, c, r.PathValue("room_id"))
}

func (h *Handler) serve(ctx context.Context, c *websocket.Conn, roomID string) {
	log := h.cfg.Log.With("room_id", roomID)
	spanCtx, span := h.tracer.Start(ctx, "game.ws.connect", trace.WithAttributes(attribute.String("room.id", roomID)))
	pc, rm, code, reason := h.hello(spanCtx, c, roomID, log)
	if pc == nil {
		span.SetStatus(codes.Error, reason)
		span.End()
		c.Close(code, reason)
		return
	}
	span.SetAttributes(attribute.String("user.id", pc.uid))
	span.End()
	log = log.With("user_id", pc.uid)

	// The connection's span (ended, but its ids still link log lines to the connect trace).
	h.readLoop(spanCtx, c, pc, rm, log)
	rm.Leave(pc)
	pc.closeWith(websocket.StatusNormalClosure, "")
	<-pc.writerDone
}

// hello reads the first message, checks the ticket and joins the room. On failure it
// returns a nil conn and the close code to use.
func (h *Handler) hello(ctx context.Context, c *websocket.Conn, roomID string, log *slog.Logger) (*Conn, *room.Room, websocket.StatusCode, string) {
	hello, code, reason := ReadHello(c, h.cfg.HelloTimeout)
	if code != 0 {
		return nil, nil, code, reason
	}

	t, err := h.cfg.Verifier.Verify(hello.Ticket, roomID)
	if err != nil {
		// The reason, never the ticket.
		log.WarnContext(ctx, "join ticket rejected", "reason", err.Error())
		return nil, nil, protocol.CloseBadTicket, "invalid ticket"
	}
	// The lobby only signs tickets for rooms it has placed; if this server doesn't hold the
	// room, it is moving (a takeover, or this server restarted): the client asks the lobby
	// again. 4004 would make it give up.
	rm := h.cfg.Rooms.Room(roomID)
	if rm == nil {
		return nil, nil, protocol.CloseRoomMoved, "room is not here; ask the lobby"
	}
	switch err := h.cfg.Redeemer.Redeem(ctx, t, rm.Token()); {
	case errors.Is(err, ticket.ErrNotOwner):
		return nil, nil, protocol.CloseRoomMoved, "room moved; ask the lobby"
	case errors.Is(err, ticket.ErrReused):
		log.WarnContext(ctx, "join ticket reused", "user_id", t.UserID)
		return nil, nil, protocol.CloseBadTicket, "ticket already used"
	case err != nil:
		log.ErrorContext(ctx, "join ticket not redeemed", "user_id", t.UserID, "err", err)
		return nil, nil, closeInternal, "try again"
	}

	pc := NewConn(h.base, c, t.UserID, func() {
		h.slow.Add(context.Background(), 1)
		log.WarnContext(ctx, "slow client disconnected", "user_id", t.UserID)
	}, func(lag time.Duration, _ time.Time) {
		h.sendLag.Record(context.Background(), float64(lag)/float64(time.Millisecond))
	})
	// Set before the room can send anything; the writer reads them after a queued message.
	pc.binary = c.Subprotocol() == protocol.StrokesSubprotocol
	pc.frames = h.frames
	switch err := rm.Join(ctx, pc, t.Name, hello.LastSeq); {
	case errors.Is(err, room.ErrRoomFull):
		pc.closeWith(protocol.CloseRoomFull, "room is full")
		<-pc.writerDone
		return nil, nil, protocol.CloseRoomFull, "room is full"
	case err != nil: // the room just stopped (closed, or taken over)
		pc.closeWith(protocol.CloseRoomMoved, "room moved; ask the lobby")
		<-pc.writerDone
		return nil, nil, protocol.CloseRoomMoved, "room moved; ask the lobby"
	}
	return pc, rm, 0, ""
}

// ReadHello reads a socket's first message, which must be hello within timeout. On failure it
// returns the close code (non-zero) and reason; it doesn't close the socket.
func ReadHello(c *websocket.Conn, timeout time.Duration) (protocol.Hello, websocket.StatusCode, string) {
	// A timer closes the socket with a proper code; a cancelled Read would only drop it.
	var timedOut atomic.Bool
	timer := time.AfterFunc(timeout, func() {
		timedOut.Store(true)
		_ = c.Close(protocol.CloseHelloTimeout, "hello timeout")
	})
	typ, data, err := c.Read(context.Background())
	timer.Stop()
	switch {
	case timedOut.Load():
		return protocol.Hello{}, protocol.CloseHelloTimeout, "hello timeout"
	case err != nil:
		return protocol.Hello{}, websocket.StatusNormalClosure, "read failed"
	case typ != websocket.MessageText:
		return protocol.Hello{}, closeUnsupported, "text messages only"
	}
	msg, err := protocol.DecodeClient(data)
	hello, ok := msg.(protocol.Hello)
	if err != nil || !ok {
		return protocol.Hello{}, protocol.CloseBadRequest, "the first message must be hello"
	}
	return hello, 0, ""
}

func (h *Handler) readLoop(ctx context.Context, c *websocket.Conn, pc *Conn, rm *room.Room, log *slog.Logger) {
	flood := ratelimit.New(floodBurst, floodPerSecond)
	for {
		// Not derived from pc.ctx: cancelling a Read makes the library drop the connection
		// abruptly, and the writer must be the one that closes it with the right code.
		readCtx, cancel := context.WithTimeout(context.Background(), idleTimeout)
		typ, data, err := c.Read(readCtx)
		cancel()
		if err != nil {
			return // closed by the client, by us, idle, or too big (1009)
		}
		binary := typ == websocket.MessageBinary
		if binary && !pc.binary {
			pc.closeWith(closeUnsupported, "text messages only")
			return
		}
		if !flood.Allow(time.Now()) {
			log.WarnContext(ctx, "socket flooding, closed")
			pc.closeWith(closePolicy, "too many messages")
			return
		}
		var msg protocol.ClientMsg
		if binary {
			msg, err = protocol.DecodeStrokeFrame(data)
		} else {
			msg, err = protocol.DecodeClient(data)
		}
		_, stroke := msg.(protocol.StrokeBatch)
		h.frames.add("in", binary, stroke, len(data))
		if err != nil {
			pc.Send(protocol.Marshal("error", protocol.ErrorData{Code: "bad_request", Message: err.Error()}))
			continue
		}
		switch m := msg.(type) {
		case protocol.Ping:
			pc.Send(protocol.Marshal("pong", map[string]int64{"t": m.T})) // no need to bother the room
		case protocol.Hello:
			pc.Send(protocol.Marshal("error", protocol.ErrorData{Code: "bad_request", Message: "already joined"}))
		default:
			if err := rm.Input(pc.ctx, pc, msg); err != nil {
				return // the room closed or the socket is going away
			}
		}
	}
}

// Conn is a socket as a room (or a relay, ADR 0017) sees it: Send queues without blocking, and
// one writer goroutine owns all writes and the close.
type Conn struct {
	uid        string
	ws         *websocket.Conn
	out        chan outMsg
	ctx        context.Context
	cancel     context.CancelFunc
	onSlow     func()
	onSent     func(lag time.Duration, origin time.Time) // after each write that succeeded
	once       sync.Once
	code       websocket.StatusCode
	reason     string
	writerDone chan struct{}
	binary     bool           // negotiated protocol.StrokesSubprotocol (ADR 0025)
	frames     *frameCounters // nil: not counted (relays' viewer sockets)
}

// outMsg is a queued message, when it was queued and, for a relay, when its event was
// committed (zero otherwise).
type outMsg struct {
	b      []byte
	queued time.Time
	origin time.Time
	binary bool // a binary frame (a stroke batch on a socket that negotiated it)
	stroke bool
}

// NewConn starts a socket's writer. onSlow runs when the socket is closed for a full queue,
// onSent after each successful write, with the time since it was queued and its origin.
func NewConn(base context.Context, c *websocket.Conn, uid string, onSlow func(), onSent func(lag time.Duration, origin time.Time)) *Conn {
	ctx, cancel := context.WithCancel(base)
	pc := &Conn{uid: uid, ws: c, out: make(chan outMsg, sendQueue), ctx: ctx, cancel: cancel,
		onSlow: onSlow, onSent: onSent, code: closeGoingAway, reason: "server shutting down", writerDone: make(chan struct{})}
	go pc.writer()
	return pc
}

func (pc *Conn) UserID() string { return pc.uid }

// Send implements room.Conn. It never blocks the room: a full queue means the client can't
// keep up, so it is disconnected (it can reconnect and get a fresh view).
func (pc *Conn) Send(b []byte) { pc.SendAt(b, time.Time{}) }

// SendAt queues like Send a message whose event was committed at origin: a relay measures its
// broadcast latency from there (ADR 0017).
func (pc *Conn) SendAt(b []byte, origin time.Time) {
	pc.queue(outMsg{b: b, queued: time.Now(), origin: origin})
}

// BinaryStrokes implements room.StrokeConn.
func (pc *Conn) BinaryStrokes() bool { return pc.binary }

// SendStroke implements room.StrokeConn: the binary frame if this socket negotiated it.
func (pc *Conn) SendStroke(json, bin []byte) {
	if pc.binary && bin != nil {
		pc.queue(outMsg{b: bin, queued: time.Now(), binary: true, stroke: true})
		return
	}
	pc.queue(outMsg{b: json, queued: time.Now(), stroke: true})
}

func (pc *Conn) queue(m outMsg) {
	select {
	case <-pc.ctx.Done():
	case pc.out <- m:
	default:
		pc.slowClose()
	}
}

// CloseWith closes the socket with code, unless it is already closing; it reports whether this
// call closed it.
func (pc *Conn) CloseWith(code websocket.StatusCode, reason string) bool {
	return pc.closeWith(code, reason)
}

// Wait returns once the writer has closed the socket.
func (pc *Conn) Wait() { <-pc.writerDone }

// Closing is done once the socket is closing.
func (pc *Conn) Closing() <-chan struct{} { return pc.ctx.Done() }

func (pc *Conn) slowClose() {
	if pc.closeWith(closeSlow, "too slow") {
		pc.onSlow()
	}
}

// Close implements room.Conn.
func (pc *Conn) Close(code int, reason string) {
	pc.closeWith(websocket.StatusCode(code), reason)
}

// closeWith records the first close reason and stops the writer, which closes the socket.
func (pc *Conn) closeWith(code websocket.StatusCode, reason string) bool {
	first := false
	pc.once.Do(func() {
		first = true
		pc.code, pc.reason = code, reason
		pc.cancel()
	})
	return first
}

func (pc *Conn) writer() {
	defer close(pc.writerDone)
	for {
		// A close wins over queued messages.
		if pc.ctx.Err() != nil {
			pc.once.Do(func() {})
			_ = pc.ws.Close(pc.code, pc.reason)
			return
		}
		select {
		case m := <-pc.out:
			// Not derived from pc.ctx: if a write's context is cancelled, the library drops
			// the connection without a close frame, and the client would never see the code.
			ctx, cancel := context.WithTimeout(context.Background(), writeTimeout)
			typ := websocket.MessageText
			if m.binary {
				typ = websocket.MessageBinary
			}
			err := pc.ws.Write(ctx, typ, m.b)
			cancel()
			switch {
			case errors.Is(err, context.DeadlineExceeded):
				pc.slowClose()
			case err != nil:
				pc.closeWith(websocket.StatusNormalClosure, "")
			default:
				// The wait in the queue (writers not scheduled, or behind a slow write) plus
				// the write itself.
				pc.onSent(time.Since(m.queued), m.origin)
				pc.frames.add("out", m.binary, m.stroke, len(m.b))
			}
		case <-pc.ctx.Done():
			// After a shutdown the base context ends first: the code stays "going away".
			pc.once.Do(func() {})
			_ = pc.ws.Close(pc.code, pc.reason)
			return
		}
	}
}
