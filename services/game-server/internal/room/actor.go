package room

import (
	"context"
	"errors"
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

// Summary is what heartbeats report about a room.
type Summary struct {
	Players int
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
	Events   Sink // nil: finished rounds and matches go nowhere
}

// Inputs to the actor. Everything that touches a room goes through its inbox, in order.
type (
	joinIn struct {
		conn    Conn
		name    string
		lastSeq int64
		reply   chan error
	}
	leaveIn  struct{ conn Conn }
	clientIn struct {
		conn Conn
		msg  protocol.ClientMsg
	}
	tickIn  struct{}
	probeIn struct{ done chan struct{} }
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

	conns     map[string]Conn // actor only
	armedFor  time.Time
	stopTimer func() bool
	summary   atomic.Pointer[Summary]
}

// New creates a room; start it with Run.
func New(cfg Config) *Room {
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
		clock: cfg.Clock, game: NewGame(cfg.ID, cfg.HostID, cfg.Settings, rng, cfg.Clock.Now()),
		log: cfg.Log.With("room_id", cfg.ID), metrics: cfg.Metrics, events: cfg.Events, tracer: otel.Tracer("game-server"),
		conns: map[string]Conn{},
	}
	r.summary.Store(&Summary{})
	return r
}

// ID is the room's id.
func (r *Room) ID() string { return r.id }

// Summary is the room's latest published state; safe from any goroutine.
func (r *Room) Summary() Summary { return *r.summary.Load() }

// Done is closed when the actor has stopped.
func (r *Room) Done() <-chan struct{} { return r.done }

// Join adds conn's player to the room and waits until the welcome is queued.
func (r *Room) Join(ctx context.Context, conn Conn, name string, lastSeq int64) error {
	reply := make(chan error, 1)
	if err := r.enqueue(ctx, joinIn{conn: conn, name: name, lastSeq: lastSeq, reply: reply}); err != nil {
		return err
	}
	select {
	case err := <-reply:
		return err
	case <-r.done:
		return ErrClosed
	case <-ctx.Done():
		return ctx.Err()
	}
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
		r.flush(ctx)
		r.publish()
		if r.game.Closed() {
			r.log.Info("room closed: empty")
			return
		}
		r.rearm()
	}
}

func (r *Room) dispatch(ctx context.Context, in any) {
	now := r.clock.Now()
	switch in := in.(type) {
	case joinIn:
		uid := in.conn.UserID()
		old := r.conns[uid]
		if err := r.game.Join(now, uid, in.name); err != nil {
			in.reply <- err
			return
		}
		if old != nil && old != in.conn {
			old.Close(protocol.CloseReplaced, "connected again elsewhere")
		}
		r.conns[uid] = in.conn
		if in.lastSeq > 0 {
			r.metrics.reconnect(ctx, "snapshot") // 2.2 always resends a full view
		}
		r.log.Info("player joined", "user_id", uid, "replaced", old != nil && old != in.conn)
		in.reply <- nil
	case leaveIn:
		uid := in.conn.UserID()
		if r.conns[uid] != in.conn {
			return // an old, replaced socket
		}
		delete(r.conns, uid)
		r.game.Leave(now, uid)
		r.log.Info("player left", "user_id", uid)
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
	}
}

// flush delivers what the game produced. Each shared event is marshalled once and queued to
// every socket; a joiner skips events its welcome already covers.
func (r *Room) flush(ctx context.Context) {
	outs := r.game.TakeOut()
	welcomed := map[string]int64{}
	for _, o := range outs {
		if o.Type == "welcome" {
			welcomed[o.To] = o.Seq
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
}

// lifecycle logs and traces matches and rounds (never single strokes or guesses).
func (r *Room) lifecycle(ev protocol.Event) {
	g := r.game
	attrs := []attribute.KeyValue{attribute.String("room.id", r.id), attribute.String("match.id", g.MatchID())}
	switch ev.Type {
	case "match_started":
		r.log.Info("match started", "match_id", g.MatchID(), "players", g.Connected())
		r.span("game.match.start", attrs)
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
		// The word is logged only now that everyone has seen it.
		r.log.Info("round finished", "match_id", fr.MatchID, "round_no", fr.RoundNo, "word", fr.Word,
			"stroke_batches", len(fr.Canvas), "correct", fr.Correct, "user_id", fr.DrawerIDs[0])
		ctx := r.span("game.round.end", []attribute.KeyValue{attribute.String("room.id", r.id),
			attribute.String("match.id", fr.MatchID), attribute.Int("round.no", fr.RoundNo)})
		r.events.RoundFinished(ctx, fr)
	case f.Match != nil:
		fm := *f.Match
		r.log.Info("match finished", "match_id", fm.MatchID, "rounds", fm.Rounds, "players", len(fm.Results))
		ctx := r.span("game.match.end", []attribute.KeyValue{attribute.String("room.id", r.id),
			attribute.String("match.id", fm.MatchID), attribute.Int("match.rounds", fm.Rounds)})
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
	s := Summary{Players: r.game.Connected(), Playing: r.game.Phase().Playing()}
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
