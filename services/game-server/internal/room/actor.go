package room

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"sync/atomic"
	"time"

	"github.com/Bowenn/gap-project_private/services/game-server/internal/protocol"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// Conn is a player's socket as the room sees it.
type Conn interface {
	UserID() string
	// Send queues a message. It never blocks: a connection whose queue is full is closed as
	// a slow client, so one bad network can't stall the room.
	Send(msg []byte)
	Close(code int, reason string)
}

// Clock is time for the actor; tests use a fake one.
type Clock interface {
	Now() time.Time
	AfterFunc(d time.Duration, f func()) (stop func() bool)
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }
func (realClock) AfterFunc(d time.Duration, f func()) func() bool {
	return time.AfterFunc(d, f).Stop
}

// RealClock is the wall clock.
var RealClock Clock = realClock{}

// Sink takes finished rounds and matches out of the room (the event publisher). Both methods
// must return at once and never do I/O: the actor calls them inside its loop. ctx carries
// only the round's or match's end span; it is never cancelled with the room.
type Sink interface {
	RoundFinished(ctx context.Context, r FinishedRound)
	MatchFinished(ctx context.Context, m FinishedMatch)
}

type noSink struct{}

func (noSink) RoundFinished(context.Context, FinishedRound) {}
func (noSink) MatchFinished(context.Context, FinishedMatch) {}

// ErrClosed means the room has shut down.
var ErrClosed = errors.New("room closed")

// ErrMoved means another server took the room over (its lease and fencing token are newer):
// this server must stop the room at once (ADR 0007).
var ErrMoved = errors.New("room moved to another server")

// Durable stores a room's events and snapshots for its owner (package roomstore). Every call
// is checked against the owner's lease and fencing token: ErrMoved means the room is gone.
type Durable interface {
	Commit(ctx context.Context, events []StoredEvent, snap *Snapshot) error
	Close(ctx context.Context) error
}

// Restored is the state a new owner rebuilds a room from.
type Restored struct {
	Snapshot Snapshot
	Events   []StoredEvent
}

// maxReplay is the most events a resuming client gets replayed; further behind, it gets a
// full welcome. It stays well below the socket's send queue (256), so a replay can't make a
// client look slow.
const maxReplay = 200

// commitTimeout bounds one write to Redis; the room stops rather than wait longer.
const commitTimeout = 2 * time.Second

// Summary is what heartbeats report about a room.
type Summary struct {
	Players int // connected
	Seats   int // held, connected or not (Game.Seated)
	Playing bool
	Closed  bool
}

// Config creates a room.
type Config struct {
	ID       string
	Mode     string
	HostID   string
	Settings Settings
	Clock    Clock
	Rand     *rand.Rand // nil: a random seed
	Log      *slog.Logger
	Metrics  *Metrics
	Events   Sink    // nil: finished rounds and matches go nowhere
	Durable  Durable // nil: the room lives only in memory (tests)
	// Token is the fencing token of the lease the room was taken with (0 without Durable).
	// It never changes: a new owner builds a new Room.
	Token   int64
	Restore *Restored
}

// Inputs to the actor. Everything that touches a room goes through its inbox, in order.
type (
	joinIn struct {
		conn    Conn
		name    string
		lastSeq int64
		reply   chan error
		trace   context.Context // the connection's span, so the room's log lines link to it
	}
	leaveIn  struct{ conn Conn }
	clientIn struct {
		conn Conn
		msg  protocol.ClientMsg
	}
	tickIn   struct{}
	probeIn  struct{ done chan struct{} }
	fencedIn struct{} // the lease keeper found the lease taken over
)

// Room is one room's actor: a single goroutine owns the Game and every connection, so
// room state is never shared behind a lock (design doc, section 9).
type Room struct {
	id      string
	mode    string
	inbox   chan any
	done    chan struct{}
	clock   Clock
	game    *Game
	log     *slog.Logger
	metrics *Metrics
	events  Sink
	tracer  trace.Tracer

	conns     map[string]Conn            // actor only
	traces    map[string]context.Context // actor only: each connection's span, for its log lines
	armedFor  time.Time
	stopTimer func() bool
	summary   atomic.Pointer[Summary]

	durable     Durable
	token       int64
	fenced      atomic.Bool // the lease was lost: the room is stopping, or has stopped
	fresh       bool        // a new room: its first snapshot must be written before anything
	lastSnapSeq int64       // seq of the latest snapshot written
	moved       bool        // set by fencedIn
}

// New creates a room, or rebuilds one from cfg.Restore; start it with Run.
func New(cfg Config) (*Room, error) {
	rng := cfg.Rand
	if rng == nil {
		rng = rand.New(rand.NewPCG(rand.Uint64(), rand.Uint64()))
	}
	if cfg.Clock == nil {
		cfg.Clock = RealClock
	}
	if cfg.Events == nil {
		cfg.Events = noSink{}
	}
	r := &Room{
		id: cfg.ID, mode: cfg.Mode, inbox: make(chan any, 256), done: make(chan struct{}),
		clock: cfg.Clock, log: cfg.Log.With("room_id", cfg.ID), metrics: cfg.Metrics, events: cfg.Events,
		tracer: otel.Tracer("game-server"), conns: map[string]Conn{}, traces: map[string]context.Context{}, durable: cfg.Durable,
		token: cfg.Token,
	}
	if cfg.Restore != nil {
		g, err := RestoreGame(cfg.Restore.Snapshot, cfg.Restore.Events, rng, cfg.Clock.Now())
		if err != nil {
			return nil, err
		}
		r.game = g
		r.lastSnapSeq = cfg.Restore.Snapshot.Seq
	} else {
		r.game = NewGame(cfg.ID, cfg.HostID, cfg.Settings, rng, cfg.Clock.Now())
		r.fresh = true
	}
	r.summary.Store(&Summary{})
	return r, nil
}

// Fence tells the room that its lease was taken over. It never blocks: if the inbox is full,
// the lease keeper finds the lease lost again on its next round.
func (r *Room) Fence() {
	r.fenced.Store(true)
	select {
	case r.inbox <- fencedIn{}:
	default:
	}
}

// Fenced reports that the room lost its lease (Fence, or a write the store refused as moved).
// Safe from any goroutine.
func (r *Room) Fenced() bool { return r.fenced.Load() }

// ID is the room's id.
func (r *Room) ID() string { return r.id }

// Token is the fencing token of the room's lease; every write for the room (including ticket
// redemptions) is checked against it.
func (r *Room) Token() int64 { return r.token }

// Summary is the room's latest published state; safe from any goroutine.
func (r *Room) Summary() Summary { return *r.summary.Load() }

// Done is closed when the actor has stopped.
func (r *Room) Done() <-chan struct{} { return r.done }

// Join adds conn's player to the room. It returns once the actor has accepted the join; the
// replay is sent by then, and the welcome (or resumed) follows in the same step's flush.
func (r *Room) Join(ctx context.Context, conn Conn, name string, lastSeq int64) error {
	reply := make(chan error, 1)
	in := joinIn{conn: conn, name: name, lastSeq: lastSeq, reply: reply,
		trace: trace.ContextWithSpanContext(context.Background(), trace.SpanContextFromContext(ctx))}
	err := r.enqueue(ctx, in)
	if err == nil {
		select {
		case err = <-reply:
			return err // the actor counted a refused resume itself
		case <-r.done:
			err = ErrClosed
		case <-ctx.Done():
			err = ctx.Err()
		}
	}
	if lastSeq > 0 {
		r.metrics.reconnect(in.trace, "failed")
	}
	return err
}

// Input passes a client message to the room. It blocks while the inbox is full, which slows
// down only the sending socket.
func (r *Room) Input(ctx context.Context, conn Conn, msg protocol.ClientMsg) error {
	return r.enqueue(ctx, clientIn{conn: conn, msg: msg})
}

// Leave reports that conn has closed.
func (r *Room) Leave(conn Conn) {
	_ = r.enqueue(context.Background(), leaveIn{conn: conn})
}

func (r *Room) enqueue(ctx context.Context, in any) error {
	select {
	case r.inbox <- in:
		return nil
	case <-r.done:
		return ErrClosed
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Run is the actor loop. It returns when the room has been empty for EmptyTTL, or when ctx
// ends (shutdown), closing every socket.
func (r *Room) Run(ctx context.Context) {
	r.metrics.roomOpened(ctx, r.mode)
	defer r.metrics.roomClosed(context.WithoutCancel(ctx), r.mode)
	defer close(r.done)
	defer func() {
		if r.stopTimer != nil {
			r.stopTimer()
		}
		r.summary.Store(&Summary{Closed: true})
	}()
	if r.fresh && r.durable != nil {
		// The base every later event applies to: without it a new owner couldn't rebuild.
		snap := r.game.Snapshot()
		if err := r.commit(ctx, nil, &snap); err != nil {
			r.stop(err)
			return
		}
	}
	r.rearm()
	for {
		select {
		case <-ctx.Done():
			for _, c := range r.conns {
				c.Close(1001, "server shutting down")
			}
			r.log.Info("room stopped: shutdown")
			return
		case in := <-r.inbox:
			r.dispatch(ctx, in)
		}
		if r.moved {
			r.stop(ErrMoved)
			return
		}
		if err := r.flush(ctx); err != nil {
			r.stop(err)
			return
		}
		r.publish()
		if r.game.Closed() {
			if r.durable != nil {
				cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), commitTimeout)
				if err := r.durable.Close(cctx); err != nil {
					r.log.Warn("closed room not marked in Redis", "err", err)
				}
				cancel()
			}
			r.log.Info("room closed: empty")
			return
		}
		r.rearm()
	}
}

// stop ends the room after a failed write: another server took it (room_moved, 4010; clients
// find it through the lobby), or Redis is unreachable (1011; the lobby restores the room once
// it can). Nothing that wasn't stored was sent to anyone.
func (r *Room) stop(err error) {
	if errors.Is(err, ErrMoved) {
		r.fenced.Store(true)
		r.log.Warn("room taken over by another server: stopping", "match_id", r.game.MatchID())
		for _, c := range r.conns {
			c.Send(protocol.Marshal("room_moved", map[string]string{"reason": "takeover"}))
			c.Close(protocol.CloseRoomMoved, "room moved")
		}
		return
	}
	r.log.Error("room store unavailable: stopping", "err", err)
	for _, c := range r.conns {
		c.Close(1011, "try again")
	}
}

func (r *Room) commit(ctx context.Context, events []StoredEvent, snap *Snapshot) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), commitTimeout)
	defer cancel()
	if err := r.durable.Commit(ctx, events, snap); err != nil {
		return err
	}
	if snap != nil {
		r.lastSnapSeq = snap.Seq
	}
	return nil
}

func (r *Room) dispatch(ctx context.Context, in any) {
	now := r.clock.Now()
	switch in := in.(type) {
	case joinIn:
		uid := in.conn.UserID()
		old := r.conns[uid]
		// A member who says what it saw last gets only what it missed (design doc, 7.4),
		// if this room still holds those events; otherwise a full welcome.
		var replay []protocol.Event
		resume := false
		if in.lastSeq > 0 && in.lastSeq <= r.game.Seq() && r.game.IsMember(uid) {
			if evs, ok := r.game.RecentEvents(in.lastSeq); ok && len(evs) <= maxReplay {
				replay, resume = evs, true
			}
		}
		if err := r.game.Join(now, uid, in.name, resume); err != nil {
			if in.lastSeq > 0 {
				r.metrics.reconnect(ctx, "failed")
			}
			in.reply <- err
			return
		}
		if old != nil && old != in.conn {
			old.Close(protocol.CloseReplaced, "connected again elsewhere")
		}
		r.conns[uid] = in.conn
		r.traces[uid] = in.trace
		// The missed events go first; this step's own events follow in flush, in seq order.
		for _, ev := range replay {
			in.conn.Send(protocol.Marshal("event", ev))
		}
		switch {
		case resume:
			r.metrics.reconnect(ctx, "resumed")
		case in.lastSeq > 0:
			r.metrics.reconnect(ctx, "snapshot")
		}
		r.log.InfoContext(in.trace, "player joined", "user_id", uid, "replaced", old != nil && old != in.conn,
			"resumed", resume, "replayed", len(replay))
		in.reply <- nil
	case leaveIn:
		uid := in.conn.UserID()
		if r.conns[uid] != in.conn {
			return // an old, replaced socket
		}
		r.game.Leave(now, uid)
		r.log.InfoContext(r.traces[uid], "player left", "user_id", uid)
		delete(r.conns, uid)
		delete(r.traces, uid)
	case clientIn:
		uid := in.conn.UserID()
		if r.conns[uid] != in.conn {
			return
		}
		if res := r.game.Handle(now, uid, in.msg); res != "" {
			r.metrics.guess(ctx, res)
		}
	case tickIn:
		r.game.Advance(now)
	case probeIn:
		close(in.done)
	case fencedIn:
		r.moved = true
	}
}

// flush stores what the game produced, then delivers it (write, then broadcast: ADR 0007).
// If the store refuses, nothing is sent: a client never sees an event a new owner can't
// replay. Each shared event is marshalled once; a joiner skips events its welcome covers.
func (r *Room) flush(ctx context.Context) error {
	outs := r.game.TakeOut()
	welcomed := map[string]int64{}
	now := r.clock.Now()
	var stored []StoredEvent
	phase := false
	for i, o := range outs {
		switch o.Type {
		case "welcome":
			welcomed[o.To] = o.Seq
		case "event":
			ev := o.Data.(protocol.Event)
			data, err := json.Marshal(ev.Data)
			if err != nil {
				return fmt.Errorf("encode %s: %w", ev.Type, err)
			}
			ev.Data = json.RawMessage(data)
			outs[i].Data = ev
			stored = append(stored, StoredEvent{Seq: ev.Seq, Type: ev.Type, Data: data, At: now})
			phase = phase || IsPhaseEvent(ev.Type)
		}
	}
	if r.durable != nil && len(stored) > 0 {
		var snap *Snapshot
		if phase || r.game.Seq()-r.lastSnapSeq >= SnapshotEvery {
			s := r.game.Snapshot()
			snap = &s
		}
		if err := r.commit(ctx, stored, snap); err != nil {
			r.game.TakeFinished() // never published: the next owner has the state
			return err
		}
	}
	for _, o := range outs {
		if o.Type != "event" {
			if c := r.conns[o.To]; c != nil {
				c.Send(protocol.Marshal(o.Type, o.Data))
			}
			continue
		}
		ev := o.Data.(protocol.Event)
		start := time.Now()
		msg := protocol.Marshal("event", ev)
		for uid, c := range r.conns {
			if uid == o.Except {
				continue
			}
			if seq, ok := welcomed[uid]; ok && ev.Seq <= seq {
				continue
			}
			c.Send(msg)
		}
		r.metrics.broadcast(ctx, ev.Type, time.Since(start))
		r.lifecycle(ev)
	}
	for _, f := range r.game.TakeFinished() {
		r.finish(f)
	}
	return nil
}

// lifecycle logs and traces matches and rounds (never single strokes or guesses).
func (r *Room) lifecycle(ev protocol.Event) {
	g := r.game
	attrs := []attribute.KeyValue{attribute.String("room.id", r.id), attribute.String("match.id", g.MatchID())}
	switch ev.Type {
	case "match_started":
		ctx := r.span("game.match.start", attrs)
		r.log.InfoContext(ctx, "match started", "match_id", g.MatchID(), "players", g.Connected())
	case "round_started":
		r.span("game.round.start", append(attrs, attribute.Int("round.no", g.RoundNo())))
	case "match_ended":
		if g.roundsFinished == 0 {
			r.log.Info("match ended before any round finished; not published", "match_id", g.MatchID())
		}
	}
}

// finish logs and traces a finished round or match and hands it to the event publisher. The
// end span is the parent of the publisher's spans, so the Kafka traceparent leads back here.
func (r *Room) finish(f Finished) {
	switch {
	case f.Round != nil:
		fr := *f.Round
		ctx := r.span("game.round.end", []attribute.KeyValue{attribute.String("room.id", r.id),
			attribute.String("match.id", fr.MatchID), attribute.Int("round.no", fr.RoundNo)})
		// The word is logged only now that everyone has seen it.
		r.log.InfoContext(ctx, "round finished", "match_id", fr.MatchID, "round_no", fr.RoundNo, "word", fr.Word,
			"stroke_batches", len(fr.Canvas), "correct", fr.Correct, "user_id", fr.DrawerIDs[0])
		r.events.RoundFinished(ctx, fr)
	case f.Match != nil:
		fm := *f.Match
		fm.Mode = r.mode
		ctx := r.span("game.match.end", []attribute.KeyValue{attribute.String("room.id", r.id),
			attribute.String("match.id", fm.MatchID), attribute.Int("match.rounds", fm.Rounds)})
		r.log.InfoContext(ctx, "match finished", "match_id", fm.MatchID, "rounds", fm.Rounds, "players", len(fm.Results))
		r.events.MatchFinished(ctx, fm)
	}
}

// span records an instant span and returns a context that carries it.
func (r *Room) span(name string, attrs []attribute.KeyValue) context.Context {
	ctx, s := r.tracer.Start(context.Background(), name, trace.WithAttributes(attrs...))
	s.End()
	return ctx
}

func (r *Room) publish() {
	s := Summary{Players: r.game.Connected(), Seats: r.game.Seated(), Playing: r.game.Phase().Playing()}
	if *r.summary.Load() != s {
		r.summary.Store(&s)
	}
}

// rearm points the single timer at the game's next deadline. A tick only asks the game to
// fire what is due, so an early or stale tick does nothing.
func (r *Room) rearm() {
	next := r.game.NextDeadline()
	if next.Equal(r.armedFor) {
		return
	}
	if r.stopTimer != nil {
		r.stopTimer()
		r.stopTimer = nil
	}
	r.armedFor = next
	if next.IsZero() {
		return
	}
	r.stopTimer = r.clock.AfterFunc(max(0, next.Sub(r.clock.Now())), func() {
		select {
		case r.inbox <- tickIn{}:
		case <-r.done:
		}
	})
}

// sync waits until everything queued before it has been processed (for tests).
func (r *Room) sync(ctx context.Context) error {
	p := probeIn{done: make(chan struct{})}
	if err := r.enqueue(ctx, p); err != nil {
		return err
	}
	select {
	case <-p.done:
		return nil
	case <-r.done:
		return ErrClosed
	}
}
