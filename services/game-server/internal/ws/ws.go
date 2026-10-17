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

	"github.com/Bowenn/gap-project_private/services/game-server/internal/protocol"
	"github.com/Bowenn/gap-project_private/services/game-server/internal/ratelimit"
	"github.com/Bowenn/gap-project_private/services/game-server/internal/room"
	"github.com/Bowenn/gap-project_private/services/game-server/internal/ticket"
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
	idleTimeout         = 60 * time.Second // clients ping every 15 s
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
	base, cancel := context.WithCancel(context.Background())
	return &Handler{cfg: cfg, connections: conns, slow: slow, tracer: otel.Tracer("game-server"), base: base, cancel: cancel}, nil
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
func (h *Handler) hello(ctx context.Context, c *websocket.Conn, roomID string, log *slog.Logger) (*playerConn, *room.Room, websocket.StatusCode, string) {
	// A timer closes the socket with a proper code; a cancelled Read would only drop it.
	var timedOut atomic.Bool
	timer := time.AfterFunc(h.cfg.HelloTimeout, func() {
		timedOut.Store(true)
		_ = c.Close(protocol.CloseHelloTimeout, "hello timeout")
	})
	typ, data, err := c.Read(context.Background())
	timer.Stop()
	switch {
	case timedOut.Load():
		return nil, nil, protocol.CloseHelloTimeout, "hello timeout"
	case err != nil:
		return nil, nil, websocket.StatusNormalClosure, "read failed"
	case typ != websocket.MessageText:
		return nil, nil, closeUnsupported, "text messages only"
	}
	msg, err := protocol.DecodeClient(data)
	hello, ok := msg.(protocol.Hello)
	if err != nil || !ok {
		return nil, nil, protocol.CloseBadRequest, "the first message must be hello"
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

	pc := newPlayerConn(h.base, c, t.UserID, func() {
		h.slow.Add(context.Background(), 1)
		log.WarnContext(ctx, "slow client disconnected", "user_id", t.UserID)
	})
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

func (h *Handler) readLoop(ctx context.Context, c *websocket.Conn, pc *playerConn, rm *room.Room, log *slog.Logger) {
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
		if typ != websocket.MessageText {
			pc.closeWith(closeUnsupported, "text messages only")
			return
		}
		if !flood.Allow(time.Now()) {
			log.WarnContext(ctx, "socket flooding, closed")
			pc.closeWith(closePolicy, "too many messages")
			return
		}
		msg, err := protocol.DecodeClient(data)
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

// playerConn is a socket as the room sees it: Send queues without blocking, and one writer
// goroutine owns all writes and the close.
type playerConn struct {
	uid        string
	ws         *websocket.Conn
	out        chan []byte
	ctx        context.Context
	cancel     context.CancelFunc
	onSlow     func()
	once       sync.Once
	code       websocket.StatusCode
	reason     string
	writerDone chan struct{}
}

func newPlayerConn(base context.Context, c *websocket.Conn, uid string, onSlow func()) *playerConn {
	ctx, cancel := context.WithCancel(base)
	pc := &playerConn{uid: uid, ws: c, out: make(chan []byte, sendQueue), ctx: ctx, cancel: cancel,
		onSlow: onSlow, code: closeGoingAway, reason: "server shutting down", writerDone: make(chan struct{})}
	go pc.writer()
	return pc
}

func (pc *playerConn) UserID() string { return pc.uid }

// Send implements room.Conn. It never blocks the room: a full queue means the client can't
// keep up, so it is disconnected (it can reconnect and get a fresh view).
func (pc *playerConn) Send(b []byte) {
	select {
	case <-pc.ctx.Done():
	case pc.out <- b:
	default:
		pc.slowClose()
	}
}

func (pc *playerConn) slowClose() {
	if pc.closeWith(closeSlow, "too slow") {
		pc.onSlow()
	}
}

// Close implements room.Conn.
func (pc *playerConn) Close(code int, reason string) {
	pc.closeWith(websocket.StatusCode(code), reason)
}

// closeWith records the first close reason and stops the writer, which closes the socket.
func (pc *playerConn) closeWith(code websocket.StatusCode, reason string) bool {
	first := false
	pc.once.Do(func() {
		first = true
		pc.code, pc.reason = code, reason
		pc.cancel()
	})
	return first
}

func (pc *playerConn) writer() {
	defer close(pc.writerDone)
	for {
		// A close wins over queued messages.
		if pc.ctx.Err() != nil {
			pc.once.Do(func() {})
			_ = pc.ws.Close(pc.code, pc.reason)
			return
		}
		select {
		case b := <-pc.out:
			// Not derived from pc.ctx: if a write's context is cancelled, the library drops
			// the connection without a close frame, and the client would never see the code.
			ctx, cancel := context.WithTimeout(context.Background(), writeTimeout)
			err := pc.ws.Write(ctx, websocket.MessageText, b)
			cancel()
			if errors.Is(err, context.DeadlineExceeded) {
				pc.slowClose()
			} else if err != nil {
				pc.closeWith(websocket.StatusNormalClosure, "")
			}
		case <-pc.ctx.Done():
			// After a shutdown the base context ends first: the code stays "going away".
			pc.once.Do(func() {})
			_ = pc.ws.Close(pc.code, pc.reason)
			return
		}
	}
}
