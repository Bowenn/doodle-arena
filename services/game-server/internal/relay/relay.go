// Package relay is the game server's relay role (ADR 0017): it streams live rooms to viewers.
//
// A relay never owns a room and never writes one. For each room it has viewers for, one stream
// goroutine owns everything about that room on this relay:
//
//   - the room's latest snapshot and the events after it, from which a viewer's welcome is built;
//   - the last frames, for viewers that resume;
//   - the viewers.
//
// A reader goroutine per room follows the room's Redis stream with XREAD BLOCK and hands events
// to its stream. The stream encodes each event once and queues it to every viewer's bounded send
// queue, so a slow viewer is closed (and resumes) instead of holding back the others. The room's
// owner is unaware of all this, and a takeover changes nothing here: the keys are the same
// whoever owns the room.
package relay

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Bowenn/doodle-arena/services/game-server/internal/protocol"
	"github.com/Bowenn/doodle-arena/services/game-server/internal/ratelimit"
	"github.com/Bowenn/doodle-arena/services/game-server/internal/room"
	"github.com/Bowenn/doodle-arena/services/game-server/internal/roomstore"
	"github.com/Bowenn/doodle-arena/services/game-server/internal/ticket"
	"github.com/Bowenn/doodle-arena/services/game-server/internal/ws"
	"github.com/coder/websocket"
	"github.com/twmb/franz-go/pkg/kgo"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// Timing and limits.
const (
	// replayFrames is how many recent events a stream keeps for resuming viewers, as a room does.
	replayFrames = 200
	tailBlock    = 5 * time.Second
	tailCount    = 512
	// maxBuffered is how many events after its snapshot a stream keeps before reading the
	// snapshot again (the owner writes one every 200 events).
	maxBuffered = 400
	// linger keeps a room's stream for a while after its last viewer leaves, so viewers
	// reconnecting don't restart it.
	linger     = 30 * time.Second
	retryAfter = 500 * time.Millisecond
	// Viewers ping and guess (ADR 0019); guesses also have their own bucket (guesses.go).
	floodBurst     = 20
	floodPerSecond = 5
	idleTimeout    = 60 * time.Second
)

// checkEvery is how often a stream looks for its room's tombstone and its linger (a variable
// for tests).
var checkEvery = 10 * time.Second

// Store reads rooms from Redis, without a lease (roomstore.Store).
type Store interface {
	Snapshot(ctx context.Context, roomID string) (room.Snapshot, error)
	EventsAfter(ctx context.Context, roomID string, after int64) ([]room.StoredEvent, error)
	Tail(ctx context.Context, roomID string, after int64, block time.Duration, count int64) ([]room.StoredEvent, error)
	Recent(ctx context.Context, roomID string, n int64) ([]room.StoredEvent, error)
	Closed(ctx context.Context, roomID string) (bool, error)
}

// Config wires a Relay.
type Config struct {
	ServerID string
	Store    Store
	Verifier *ticket.Verifier
	// Redeem marks a viewer's ticket used (ticket.RedeemViewer).
	Redeem       func(ctx context.Context, t ticket.Ticket) error
	Origins      []string // allowed Origin hosts
	HelloTimeout time.Duration
	Log          *slog.Logger
	Meter        metric.Meter
	// Forwarder passes viewers' correct guesses to their rooms' owners (ADR 0019);
	// roomstore.Store. nil: correct guesses are only answered.
	Forwarder Forwarder
	// Kafka is the relay's client (its own user, relay): wrong guesses out, top guesses in.
	// nil: neither (tests).
	Kafka *kgo.Client
	// Publisher overrides the Kafka publisher (tests).
	Publisher Publisher
}

// Relay serves viewers' WebSockets and runs a stream per watched room.
type Relay struct {
	cfg     Config
	metrics *metrics
	tracer  trace.Tracer

	base   context.Context // ends at Shutdown: streams close their viewers with 1001
	cancel context.CancelFunc

	// The map of streams is relay bookkeeping, not room state: each room's state belongs to
	// its stream goroutine.
	mu       sync.Mutex
	streams  map[string]*stream
	closed   bool
	sockets  sync.WaitGroup
	running  sync.WaitGroup // stream goroutines
	draining atomic.Bool
	viewers  atomic.Int64
	forwards chan forward // correct guesses waiting for Redis (guesses.go)
}

// New returns a relay.
func New(cfg Config) (*Relay, error) {
	if cfg.HelloTimeout == 0 {
		cfg.HelloTimeout = ws.DefaultHelloTimeout
	}
	m, err := newMetrics(cfg.Meter)
	if err != nil {
		return nil, err
	}
	if cfg.Publisher == nil && cfg.Kafka != nil {
		cfg.Publisher = kafkaPublisher{client: cfg.Kafka, metrics: m}
	}
	base, cancel := context.WithCancel(context.Background())
	r := &Relay{cfg: cfg, metrics: m, tracer: otel.Tracer("game-server"), base: base, cancel: cancel,
		streams: map[string]*stream{}, forwards: make(chan forward, forwardQueue)}
	if cfg.Forwarder != nil {
		r.running.Add(1)
		go func() { defer r.running.Done(); r.forwardGuesses(base) }()
	}
	return r, nil
}

// Drain makes heartbeats report this relay as draining, so the lobby sends it no new viewers.
func (r *Relay) Drain() { r.draining.Store(true) }

// Shutdown closes every viewer with 1001 (they ask the lobby for another relay and resume
// there) and waits for the sockets and streams to finish.
func (r *Relay) Shutdown(ctx context.Context) error {
	r.mu.Lock()
	r.closed = true
	r.cancel()
	r.mu.Unlock()
	done := make(chan struct{})
	go func() { r.sockets.Wait(); r.running.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Watched is one room this relay streams and its viewers here, for the heartbeat.
type Watched struct {
	RoomID  string
	Viewers uint32
}

// Viewers reports the relay's viewers, in total and per room, for its heartbeat.
func (r *Relay) Viewers() (total uint32, rooms []Watched) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for id, s := range r.streams {
		if n := s.viewers.Load(); n > 0 {
			rooms = append(rooms, Watched{RoomID: id, Viewers: uint32(n)})
		}
	}
	return uint32(r.viewers.Load()), rooms
}

// Draining reports whether Drain was called.
func (r *Relay) Draining() bool { return r.draining.Load() }

// ServeHTTP upgrades GET /ws/{server_id}/rooms/{room_id} for a viewer.
func (r *Relay) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if req.PathValue("server_id") != r.cfg.ServerID {
		http.NotFound(w, req)
		return
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		http.Error(w, "shutting down", http.StatusServiceUnavailable)
		return
	}
	r.sockets.Add(1)
	r.mu.Unlock()
	defer r.sockets.Done()

	c, err := websocket.Accept(w, req, &websocket.AcceptOptions{
		OriginPatterns: r.cfg.Origins, CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		return
	}
	c.SetReadLimit(protocol.MaxMessageBytes)
	ctx := otel.GetTextMapPropagator().Extract(r.base, propagation.HeaderCarrier(req.Header))
	r.serve(ctx, c, req.PathValue("room_id"))
}

func (r *Relay) serve(ctx context.Context, c *websocket.Conn, roomID string) {
	log := r.cfg.Log.With("room_id", roomID)
	ctx, span := r.tracer.Start(ctx, "game.ws.connect", trace.WithAttributes(
		attribute.String("room.id", roomID), attribute.String("ws.role", "viewer")))
	conn, st, name, code, reason := r.hello(ctx, c, roomID, log)
	if conn == nil {
		span.SetStatus(codes.Error, reason)
		span.End()
		_ = c.Close(code, reason)
		return
	}
	span.SetAttributes(attribute.String("user.id", conn.UserID()))
	span.End()

	r.readLoop(ctx, c, conn, st, name, log)
	st.leave(conn)
	conn.CloseWith(websocket.StatusNormalClosure, "")
	conn.Wait()
}

func (r *Relay) hello(ctx context.Context, c *websocket.Conn, roomID string, log *slog.Logger) (*ws.Conn, *stream, string, websocket.StatusCode, string) {
	hello, code, reason := ws.ReadHello(c, r.cfg.HelloTimeout)
	if code != 0 {
		return nil, nil, "", code, reason
	}
	t, err := r.cfg.Verifier.VerifyViewer(hello.Ticket, roomID)
	if err != nil {
		log.WarnContext(ctx, "viewer ticket rejected", "reason", err.Error())
		return nil, nil, "", protocol.CloseBadTicket, "invalid ticket"
	}
	switch err := r.cfg.Redeem(ctx, t); {
	case errors.Is(err, ticket.ErrReused):
		log.WarnContext(ctx, "viewer ticket reused", "user_id", t.UserID)
		return nil, nil, "", protocol.CloseBadTicket, "ticket already used"
	case err != nil:
		log.ErrorContext(ctx, "viewer ticket not redeemed", "user_id", t.UserID, "err", err)
		return nil, nil, "", websocket.StatusInternalError, "try again"
	}
	conn := ws.NewConn(r.base, c, t.UserID, func() {
		r.metrics.closed(context.Background(), "slow")
		log.WarnContext(ctx, "slow viewer disconnected", "user_id", t.UserID)
	}, func(_ time.Duration, origin time.Time) {
		if !origin.IsZero() {
			r.metrics.latency(context.Background(), time.Since(origin))
		}
	})
	for {
		st, err := r.stream(roomID)
		if err != nil { // shutting down
			conn.CloseWith(websocket.StatusGoingAway, "relay shutting down")
			conn.Wait()
			return nil, nil, "", websocket.StatusGoingAway, "relay shutting down"
		}
		switch err := st.join(ctx, conn, hello.LastSeq); {
		case errors.Is(err, errStreamGone):
			continue // it stopped as we arrived: start a new one
		case errors.Is(err, roomstore.ErrClosed), errors.Is(err, roomstore.ErrNotFound):
			conn.CloseWith(protocol.CloseNoRoom, "the room has ended")
			conn.Wait()
			return nil, nil, "", protocol.CloseNoRoom, "the room has ended"
		case err != nil:
			log.ErrorContext(ctx, "viewer couldn't join the stream", "user_id", t.UserID, "err", err)
			conn.CloseWith(websocket.StatusInternalError, "try again")
			conn.Wait()
			return nil, nil, "", websocket.StatusInternalError, "try again"
		}
		log.DebugContext(ctx, "viewer joined", "user_id", t.UserID, "last_seq", hello.LastSeq)
		return conn, st, t.Name, 0, ""
	}
}

// readLoop answers pings and passes guesses to the room's stream (ADR 0019). Everything else a
// viewer sends is refused: viewers watch and guess, nothing more.
func (r *Relay) readLoop(ctx context.Context, c *websocket.Conn, conn *ws.Conn, st *stream, name string, log *slog.Logger) {
	flood := ratelimit.New(floodBurst, floodPerSecond)
	guesses := ratelimit.New(guessBurst, guessPerSecond)
	for {
		readCtx, cancel := context.WithTimeout(context.Background(), idleTimeout)
		typ, data, err := c.Read(readCtx)
		cancel()
		if err != nil {
			return
		}
		if typ != websocket.MessageText {
			conn.CloseWith(websocket.StatusUnsupportedData, "text messages only")
			return
		}
		if !flood.Allow(time.Now()) {
			log.WarnContext(ctx, "viewer socket flooding, closed", "user_id", conn.UserID())
			conn.CloseWith(websocket.StatusPolicyViolation, "too many messages")
			return
		}
		msg, err := protocol.DecodeClient(data)
		if err != nil {
			conn.Send(protocol.Marshal("error", protocol.ErrorData{Code: "bad_request", Message: err.Error()}))
			continue
		}
		switch m := msg.(type) {
		case protocol.Ping:
			conn.Send(protocol.Marshal("pong", map[string]int64{"t": m.T}))
		case protocol.Guess:
			if !guesses.Allow(time.Now()) {
				r.metrics.viewerGuess(ctx, guessRateLimited)
				conn.Send(protocol.Marshal("rate_limited", map[string]any{}))
				continue
			}
			st.guess(conn, name, m.Text)
		default:
			conn.Send(protocol.Marshal("error", protocol.ErrorData{Code: "viewer", Message: "viewers can only watch and guess"}))
		}
	}
}

var errShuttingDown = errors.New("relay shutting down")

// stream returns the room's stream, starting one if there is none.
func (r *Relay) stream(roomID string) (*stream, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil, errShuttingDown
	}
	if s, ok := r.streams[roomID]; ok {
		return s, nil
	}
	s := &stream{relay: r, id: roomID, in: make(chan any), done: make(chan struct{}),
		conns: map[*ws.Conn]struct{}{}, guessed: map[string]string{}, log: r.cfg.Log.With("room_id", roomID)}
	r.streams[roomID] = s
	r.running.Add(1)
	go s.run(r.base)
	return s, nil
}

// remove drops a stream that is stopping, so the next viewer starts a new one.
func (r *Relay) remove(s *stream) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.streams[s.id] == s {
		delete(r.streams, s.id)
	}
}

// ---------------------------------------------------------------- one room's stream

var errStreamGone = errors.New("stream stopped")

type joinIn struct {
	conn    *ws.Conn
	lastSeq int64
	reply   chan error
}

type leaveIn struct{ conn *ws.Conn }

// inspectIn runs f inside the stream's goroutine, for tests.
type inspectIn struct{ f func(*stream) }

type frame struct {
	seq int64
	b   []byte
}

// stream is one room on this relay. Everything below `viewers` is owned by run's goroutine.
type stream struct {
	relay *Relay
	id    string
	in    chan any // unbuffered: a send succeeds only while run still receives
	done  chan struct{}
	log   *slog.Logger
	// viewers is read by heartbeats; run is its only writer.
	viewers atomic.Int64

	conns   map[*ws.Conn]struct{}
	snap    room.Snapshot
	events  []room.StoredEvent // after snap.Seq, in order
	seq     int64              // the last event sent to viewers
	recent  []frame
	version int64 // changes with every event or snapshot: the cached welcome's key
	cached  struct {
		version int64
		seq     int64
		view    json.RawMessage
	}
	idleSince time.Time

	// Viewer guesses (ADR 0019), per round: a round is its match and number (roundKey).
	guessed map[string]string // user id -> the round they guessed right (by user: a reconnect keeps it)
	winner  string            // the round whose viewer_correct this stream has seen
	top     []byte            // the round's latest top_guesses frame, for viewers who join
	topEnd  int64             // its window's end
}

// join adds a viewer. It returns errStreamGone if the stream stopped in the meantime.
func (s *stream) join(ctx context.Context, conn *ws.Conn, lastSeq int64) error {
	j := joinIn{conn: conn, lastSeq: lastSeq, reply: make(chan error, 1)}
	select {
	case s.in <- j:
		return <-j.reply
	case <-s.done:
		return errStreamGone
	case <-ctx.Done():
		return ctx.Err()
	}
}

// leave removes a viewer (a stream that already stopped has none).
func (s *stream) leave(conn *ws.Conn) {
	select {
	case s.in <- leaveIn{conn: conn}:
	case <-s.done:
	}
}

func (s *stream) run(ctx context.Context) {
	defer s.relay.running.Done()
	m := s.relay.metrics
	m.rooms.Add(context.Background(), 1)
	defer m.rooms.Add(context.Background(), -1)
	defer func() {
		s.relay.remove(s)
		close(s.done)
	}()

	if err := s.load(ctx, "start"); err != nil {
		// Answer whoever is waiting (the viewer that started this stream), then stop.
		s.refuseJoins(ctx, err)
		return
	}
	batches := make(chan []room.StoredEvent)
	readCtx, stopReading := context.WithCancel(ctx)
	defer stopReading()
	go s.read(readCtx, s.seq, batches)

	ticker := time.NewTicker(checkEvery)
	defer ticker.Stop()
	s.idleSince = time.Now()
	for {
		select {
		case in := <-s.in:
			switch in := in.(type) {
			case joinIn:
				s.add(in.conn, in.lastSeq)
				in.reply <- nil
			case leaveIn:
				s.drop(in.conn)
			case inspectIn:
				in.f(s)
			case guessIn:
				s.onGuess(ctx, in)
			case topIn:
				s.onTop(ctx, in.top)
			}
		case evs := <-batches:
			s.apply(ctx, evs)
		case <-ticker.C:
			if len(s.conns) == 0 && time.Since(s.idleSince) > linger {
				return
			}
			if closed, err := s.relay.cfg.Store.Closed(ctx, s.id); err == nil && closed {
				s.log.InfoContext(ctx, "watched room ended", "viewers", len(s.conns))
				s.closeAll("room_ended", websocket.StatusNormalClosure, "the room has ended")
				return
			}
		case <-ctx.Done():
			s.closeAll("shutdown", websocket.StatusGoingAway, "relay shutting down")
			return
		}
	}
}

// refuseJoins answers joins with err for a moment, so the viewer that started a stream that
// can't load learns why instead of waiting.
func (s *stream) refuseJoins(ctx context.Context, err error) {
	timeout := time.NewTimer(time.Second)
	defer timeout.Stop()
	for {
		select {
		case in := <-s.in:
			if j, ok := in.(joinIn); ok {
				j.reply <- err
			}
		case <-timeout.C:
			return
		case <-ctx.Done():
			return
		}
	}
}

// load reads the room afresh: the latest snapshot and every event after it.
func (s *stream) load(ctx context.Context, reason string) (err error) {
	ctx, span := s.relay.tracer.Start(ctx, "relay.stream.load", trace.WithAttributes(
		attribute.String("room.id", s.id), attribute.String("reason", reason)))
	defer func() {
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
		}
		span.End()
	}()
	s.relay.metrics.rebuild(ctx, reason)
	snap, err := s.relay.cfg.Store.Snapshot(ctx, s.id)
	if err != nil {
		return err
	}
	evs, err := s.relay.cfg.Store.EventsAfter(ctx, s.id, snap.Seq)
	if err != nil {
		return err
	}
	s.snap, s.events, s.seq, s.recent = snap, nil, snap.Seq, nil
	for _, ev := range evs {
		if ev.Seq != s.seq+1 {
			return fmt.Errorf("room %s: event log has a gap at seq %d after %d", s.id, ev.Seq, s.seq)
		}
		s.events = append(s.events, ev)
		s.seq = ev.Seq
	}
	// The last frames too, so a viewer whose relay stopped can resume on this one without a full
	// welcome, even though this relay never sent them.
	recent, err := s.relay.cfg.Store.Recent(ctx, s.id, replayFrames)
	if err != nil {
		return err
	}
	for _, ev := range recent {
		if ev.Seq > s.seq {
			break // committed after the read above: the stream's reader delivers it
		}
		if n := len(s.recent); n > 0 && ev.Seq != s.recent[n-1].seq+1 {
			s.recent = nil // only an unbroken run up to s.seq can be replayed
		}
		s.recent = append(s.recent, frame{seq: ev.Seq, b: protocol.Marshal("event", protocol.Event{Seq: ev.Seq, Type: ev.Type, Data: ev.Data})})
	}
	if n := len(s.recent); n > 0 && s.recent[n-1].seq != s.seq {
		s.recent = nil
	}
	s.version++
	span.SetAttributes(attribute.Int64("seq", s.seq), attribute.Int("events", len(s.events)))
	s.log.InfoContext(ctx, "streaming room", "reason", reason, "seq", s.seq, "snapshot_seq", snap.Seq)
	return nil
}

// read follows the room's stream from after and hands each batch of events to run.
func (s *stream) read(ctx context.Context, after int64, out chan<- []room.StoredEvent) {
	failing := false
	for ctx.Err() == nil {
		evs, err := s.relay.cfg.Store.Tail(ctx, s.id, after, tailBlock, tailCount)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			if !failing {
				s.log.WarnContext(ctx, "reading the room's stream failed", "err", err)
				failing = true
			}
			select {
			case <-time.After(retryAfter):
			case <-ctx.Done():
				return
			}
			continue
		}
		failing = false
		if len(evs) == 0 {
			continue
		}
		after = evs[len(evs)-1].Seq
		select {
		case out <- evs:
		case <-ctx.Done():
			return
		}
	}
}

// apply sends new events to every viewer, in seq order.
func (s *stream) apply(ctx context.Context, evs []room.StoredEvent) {
	now := time.Now()
	for _, ev := range evs {
		if ev.Seq <= s.seq {
			continue // already loaded
		}
		if ev.Seq != s.seq+1 {
			// The stream was trimmed past this relay (it fell behind): start over from the
			// snapshot, and give every viewer a fresh welcome.
			s.log.WarnContext(ctx, "gap in the room's stream: reloading", "seq", ev.Seq, "after", s.seq)
			if err := s.load(ctx, "gap"); err != nil {
				s.log.ErrorContext(ctx, "reloading the room failed", "err", err)
				s.closeAll("error", websocket.StatusInternalError, "try again")
				return
			}
			for c := range s.conns {
				s.welcome(c)
			}
			continue
		}
		s.relay.metrics.streamLag(ctx, now.Sub(ev.At))
		s.seq = ev.Seq
		if ev.Seq > s.snap.Seq {
			s.events = append(s.events, ev)
		}
		s.version++
		b := protocol.Marshal("event", protocol.Event{Seq: ev.Seq, Type: ev.Type, Data: ev.Data})
		s.recent = append(s.recent, frame{seq: ev.Seq, b: b})
		if len(s.recent) > replayFrames {
			s.recent = s.recent[len(s.recent)-replayFrames:]
		}
		for c := range s.conns {
			c.SendAt(b, ev.At)
		}
		// A phase event comes with a snapshot in the same commit; so does every 200th event.
		if room.IsPhaseEvent(ev.Type) || len(s.events) > maxBuffered {
			s.refreshSnapshot(ctx)
		}
		s.onEvent(ev)
	}
}

// refreshSnapshot reads the room's snapshot again and drops the events it covers.
func (s *stream) refreshSnapshot(ctx context.Context) {
	snap, err := s.relay.cfg.Store.Snapshot(ctx, s.id)
	if err != nil {
		s.log.WarnContext(ctx, "reading the room's snapshot failed", "err", err)
		return
	}
	if snap.Seq < s.snap.Seq {
		return
	}
	s.snap = snap
	i := 0
	for i < len(s.events) && s.events[i].Seq <= snap.Seq {
		i++
	}
	s.events = s.events[i:]
	s.version++
}

func (s *stream) add(c *ws.Conn, lastSeq int64) {
	s.conns[c] = struct{}{}
	s.viewers.Add(1)
	s.relay.viewers.Add(1)
	s.relay.metrics.viewers.Add(context.Background(), 1)
	// A viewer who says what it saw last gets only what it missed, if this relay still holds
	// it; otherwise a full welcome (as players, design doc 7.4).
	if lastSeq > 0 && lastSeq <= s.seq && len(s.recent) > 0 && lastSeq >= s.recent[0].seq-1 {
		for _, f := range s.recent {
			if f.seq > lastSeq {
				c.Send(f.b)
			}
		}
		c.Send(protocol.Marshal("resumed", map[string]int64{"seq": s.seq}))
	} else {
		s.welcome(c)
	}
	if s.top != nil {
		c.Send(s.top)
	}
}

func (s *stream) drop(c *ws.Conn) {
	if _, ok := s.conns[c]; !ok {
		return
	}
	delete(s.conns, c)
	s.viewers.Add(-1)
	s.relay.viewers.Add(-1)
	s.relay.metrics.viewers.Add(context.Background(), -1)
	if len(s.conns) == 0 {
		s.idleSince = time.Now()
	}
}

// welcome sends a viewer the room as it is now. The view is built once per change and shared
// by every viewer who joins until the next one: it has nothing personal in it.
func (s *stream) welcome(c *ws.Conn) {
	if s.cached.view == nil || s.cached.version != s.version {
		view, seq, err := room.ViewAt(s.snap, s.events)
		if err != nil {
			s.log.Error("building a viewer's welcome failed", "err", err)
			c.CloseWith(websocket.StatusInternalError, "try again")
			return
		}
		b, err := json.Marshal(view)
		if err != nil {
			s.log.Error("encoding a viewer's welcome failed", "err", err)
			c.CloseWith(websocket.StatusInternalError, "try again")
			return
		}
		s.cached.version, s.cached.seq, s.cached.view = s.version, seq, b
	}
	c.Send(protocol.Marshal("welcome", map[string]any{
		"user_id": c.UserID(), "view": s.cached.view, "seq": s.cached.seq, "viewer": true,
	}))
}

func (s *stream) closeAll(reason string, code websocket.StatusCode, text string) {
	for c := range s.conns {
		if c.CloseWith(code, text) {
			s.relay.metrics.closed(context.Background(), reason)
		}
		s.drop(c)
	}
}
