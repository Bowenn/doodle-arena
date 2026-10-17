// Package events takes finished rounds and matches out of the game server: it uploads each
// round's stroke log to the round-logs bucket and publishes RoundFinished (with the object key,
// the claim-check pattern) and MatchFinished to Kafka (design doc, sections 4.4 and 6.2).
//
// Room actors must never wait for I/O, so the Publisher takes work through a bounded queue
// and one worker goroutine does the uploads and produces. The queue is only the hand-off: every
// event is already in its room's outbox in Redis (ADR 0009), and the worker deletes it there
// (Ack) once Kafka has it. When the queue is full, a job is dropped instead of blocking the
// rooms, and a failed job is given up after its deadline; either way the entry stays in the
// outbox and is queued again (Requeue) by the owner's scan or by a new owner after a takeover.
package events

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Bowenn/gap-project_private/services/game-server/internal/room"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/google/uuid"
	"github.com/twmb/franz-go/pkg/kgo"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/protobuf/proto"
)

// Producer is the part of *kgo.Client the publisher uses.
type Producer interface {
	Produce(ctx context.Context, r *kgo.Record, promise func(*kgo.Record, error))
}

// ObjectStore is the part of *s3.Client the publisher uses.
type ObjectStore interface {
	PutObject(ctx context.Context, in *s3.PutObjectInput, opts ...func(*s3.Options)) (*s3.PutObjectOutput, error)
}

// Acker deletes a published entry from its room's outbox (roomstore.Store).
type Acker interface {
	Ack(ctx context.Context, roomID, entryID string) error
}

// Defaults for Config.
const (
	DefaultQueueSize  = 1024
	DefaultJobTimeout = 2 * time.Minute
	attemptTimeout    = 15 * time.Second
	firstBackoff      = 500 * time.Millisecond
	maxBackoff        = 10 * time.Second
)

// Config creates a Publisher.
type Config struct {
	Kafka      Producer
	S3         ObjectStore
	Bucket     string
	QueueSize  int           // finished rounds and matches waiting to be published; 0: DefaultQueueSize
	JobTimeout time.Duration // retries stop after this per event; 0: DefaultJobTimeout
	Backoff    time.Duration // first wait between retries, doubling up to 10 s; 0: 500 ms
	Log        *slog.Logger
	Meter      metric.Meter
	NewID      func() string // event ids for entries without one (tests); nil: random UUIDs
	Outbox     Acker         // deletes published entries; nil: nothing to delete (no Redis)
}

type job struct {
	ctx     context.Context // carries the end span from the room; never cancelled
	eventID string          // fixed when the job is queued, so every retry reuses it
	entry   room.OutboxEntry
	round   *room.FinishedRound
	match   *room.FinishedMatch
}

func (j job) topic() string {
	if j.round != nil {
		return TopicRounds
	}
	return TopicMatches
}

// Publisher is a room.Sink that publishes on its own goroutine (Run).
type Publisher struct {
	cfg     Config
	log     *slog.Logger
	metrics *metrics
	tracer  trace.Tracer
	queue   chan job
	closing chan struct{}
	closed  atomic.Bool
	done    chan struct{}

	mu       sync.Mutex
	inflight map[string]bool // outbox entry IDs queued or being published
}

var _ room.Sink = (*Publisher)(nil)

// New creates a Publisher; start it with Run.
func New(cfg Config) (*Publisher, error) {
	if cfg.QueueSize <= 0 {
		cfg.QueueSize = DefaultQueueSize
	}
	if cfg.JobTimeout <= 0 {
		cfg.JobTimeout = DefaultJobTimeout
	}
	if cfg.Backoff <= 0 {
		cfg.Backoff = firstBackoff
	}
	if cfg.NewID == nil {
		cfg.NewID = uuid.NewString
	}
	m, err := newMetrics(cfg.Meter)
	if err != nil {
		return nil, fmt.Errorf("events metrics: %w", err)
	}
	return &Publisher{
		cfg: cfg, log: cfg.Log, metrics: m, tracer: otel.Tracer("game-server"),
		queue: make(chan job, cfg.QueueSize), closing: make(chan struct{}), done: make(chan struct{}),
		inflight: map[string]bool{},
	}, nil
}

// RoundFinished queues a finished round, with the event_id its outbox entry fixed. It never
// blocks: the room actor calls it.
func (p *Publisher) RoundFinished(ctx context.Context, r room.FinishedRound, e room.OutboxEntry) {
	p.enqueue(job{ctx: context.WithoutCancel(ctx), eventID: p.eventID(e), entry: e, round: &r},
		"room_id", r.RoomID, "match_id", r.MatchID, "round_no", r.RoundNo)
}

// MatchFinished queues a finished match. It never blocks.
func (p *Publisher) MatchFinished(ctx context.Context, m room.FinishedMatch, e room.OutboxEntry) {
	p.enqueue(job{ctx: context.WithoutCancel(ctx), eventID: p.eventID(e), entry: e, match: &m},
		"room_id", m.RoomID, "match_id", m.MatchID)
}

// Requeue queues outbox entries again: ones a previous owner left, or this server's that
// failed or were dropped. Entries already queued or being published are skipped, so a scan
// never doubles the work. It never blocks, and returns how many it queued.
func (p *Publisher) Requeue(entries []room.OutboxEntry) int {
	n := 0
	for _, e := range entries {
		if p.isInflight(e.ID) {
			continue
		}
		f, ctx, err := room.DecodeOutbox(e)
		if err != nil {
			// A bug, not an outage: it stays in the outbox, where someone can look at it.
			p.log.Error("outbox entry can't be decoded", "room_id", e.RoomID, "entry_id", e.ID,
				"event_id", e.EventID, "err", err)
			continue
		}
		j := job{ctx: ctx, eventID: p.eventID(e), entry: e, round: f.Round, match: f.Match}
		if p.enqueue(j, "room_id", e.RoomID, "entry_id", e.ID) {
			n++
		}
	}
	return n
}

func (p *Publisher) isInflight(entryID string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return entryID != "" && p.inflight[entryID]
}

func (p *Publisher) setInflight(entryID string, on bool) {
	if entryID == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if on {
		p.inflight[entryID] = true
	} else {
		delete(p.inflight, entryID)
	}
}

// eventID is the entry's, so every delivery of one event has the same id.
func (p *Publisher) eventID(e room.OutboxEntry) string {
	if e.EventID != "" {
		return e.EventID
	}
	return p.cfg.NewID()
}

func (p *Publisher) enqueue(j job, logAttrs ...any) bool {
	if !p.closed.Load() {
		p.setInflight(j.entry.ID, true)
		select {
		case p.queue <- j:
			p.metrics.queued.Add(j.ctx, 1)
			return true
		default:
			p.setInflight(j.entry.ID, false)
		}
	}
	// Blocking would freeze the room for every player. The event isn't lost: it stays in the
	// room's outbox and is queued again by the next scan.
	p.metrics.result(j.ctx, j.topic(), resultDropped)
	p.log.ErrorContext(j.ctx, "event dropped: publish queue full or shutting down",
		append(logAttrs, "topic", j.topic(), "event_id", j.eventID)...)
	return false
}

// Run publishes queued work, one event at a time, in the order it was queued, so a room's
// last RoundFinished goes out before its MatchFinished. After Close it publishes what is
// still queued and returns. Cancelling ctx aborts the work in progress and drops the rest.
func (p *Publisher) Run(ctx context.Context) {
	defer close(p.done)
	for {
		select {
		case j := <-p.queue:
			p.handle(ctx, j)
		case <-ctx.Done():
			p.Close() // drop what is queued (handle sees the cancelled context)
		case <-p.closing:
			for {
				select {
				case j := <-p.queue:
					p.handle(ctx, j)
				default:
					return
				}
			}
		}
	}
}

// Close stops taking work; Run drains the queue and returns. Call it after the rooms stop.
func (p *Publisher) Close() {
	if p.closed.CompareAndSwap(false, true) {
		close(p.closing)
	}
}

// Done is closed when Run has returned.
func (p *Publisher) Done() <-chan struct{} { return p.done }

func (p *Publisher) handle(runCtx context.Context, j job) {
	p.metrics.queued.Add(j.ctx, -1)
	defer p.setInflight(j.entry.ID, false)
	topic := j.topic()
	if runCtx.Err() != nil {
		p.metrics.result(j.ctx, topic, resultDropped)
		p.log.ErrorContext(j.ctx, "event dropped: shutdown deadline", "topic", topic, "event_id", j.eventID)
		return
	}
	// The job's span from the room, with the run's cancellation and the job's deadline.
	ctx := trace.ContextWithSpanContext(runCtx, trace.SpanContextFromContext(j.ctx))
	ctx, cancel := context.WithTimeout(ctx, p.cfg.JobTimeout)
	defer cancel()

	start := time.Now()
	var err error
	if j.round != nil {
		err = p.publishRound(ctx, j.eventID, *j.round)
	} else {
		err = p.publishMatch(ctx, j.eventID, *j.match)
	}
	p.metrics.took(ctx, topic, time.Since(start))
	if err != nil {
		p.metrics.result(ctx, topic, resultFailed)
		return
	}
	p.metrics.result(ctx, topic, resultSuccess)
	p.ack(ctx, j)
}

// ack deletes the published entry from its room's outbox. If that fails, the entry is
// published again later with the same event_id, which consumers absorb.
func (p *Publisher) ack(ctx context.Context, j job) {
	if p.cfg.Outbox == nil || j.entry.ID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), attemptTimeout)
	defer cancel()
	if err := p.cfg.Outbox.Ack(ctx, j.entry.RoomID, j.entry.ID); err != nil {
		p.log.WarnContext(ctx, "published event not removed from the outbox; it will be sent again",
			"room_id", j.entry.RoomID, "entry_id", j.entry.ID, "event_id", j.eventID, "err", err)
	}
}

func (p *Publisher) publishRound(ctx context.Context, eventID string, r room.FinishedRound) (err error) {
	ctx, span := p.tracer.Start(ctx, "game.round.publish", trace.WithAttributes(
		attribute.String("room.id", r.RoomID), attribute.String("match.id", r.MatchID), attribute.Int("round.no", r.RoundNo)))
	defer endSpan(span, &err)
	log := p.log.With("room_id", r.RoomID, "match_id", r.MatchID, "round_no", r.RoundNo, "event_id", eventID)

	strokeLog := StrokeLog(r)
	body, err := gzipProto(strokeLog)
	if err != nil {
		log.ErrorContext(ctx, "stroke log not built", "err", err)
		return err
	}
	key := StrokeLogKey(r.MatchID, r.RoundNo)
	// Upload first: RoundFinished must never point at an object that doesn't exist. The key
	// is deterministic, so uploading again after a timeout overwrites the same bytes.
	err = p.retry(ctx, log, "upload stroke log", attemptTimeout, func(ctx context.Context) error {
		_, err := p.cfg.S3.PutObject(ctx, &s3.PutObjectInput{
			Bucket: aws.String(p.cfg.Bucket), Key: aws.String(key), Body: bytes.NewReader(body),
			ContentType: aws.String(strokeLogMIMEType),
		})
		return err
	})
	if err != nil {
		log.ErrorContext(ctx, "round not published: stroke log upload failed", "err", err, "stroke_log_key", key)
		return err
	}
	ev := RoundEvent(eventID, r, key, len(strokeLog.GetStrokes()))
	if err := p.produce(ctx, log, TopicRounds, r.RoomID, ev); err != nil {
		log.ErrorContext(ctx, "round not published: produce failed", "err", err, "stroke_log_key", key)
		return err
	}
	log.InfoContext(ctx, "round published", "stroke_log_key", key, "stroke_count", len(strokeLog.GetStrokes()),
		"stroke_log_bytes", len(body))
	return nil
}

func (p *Publisher) publishMatch(ctx context.Context, eventID string, m room.FinishedMatch) (err error) {
	ctx, span := p.tracer.Start(ctx, "game.match.publish", trace.WithAttributes(
		attribute.String("room.id", m.RoomID), attribute.String("match.id", m.MatchID)))
	defer endSpan(span, &err)
	log := p.log.With("room_id", m.RoomID, "match_id", m.MatchID, "event_id", eventID)

	if err := p.produce(ctx, log, TopicMatches, m.RoomID, MatchEvent(eventID, m)); err != nil {
		log.ErrorContext(ctx, "match not published: produce failed", "err", err)
		return err
	}
	log.InfoContext(ctx, "match published", "players", len(m.Results), "rounds", m.Rounds)
	return nil
}

// produce sends one wrapper message keyed by room id and waits for the broker's ack or the
// job's deadline, whichever comes first.
//
// franz-go retries a buffered record on its own, in order, and its idempotent producer keeps
// those retries from writing duplicates. It fails a record only when that is safe (the record
// was never sent, or the broker answered), so a failure is retried here with a fresh record
// (the tracing hook writes traceparent into each record it buffers). While Kafka is
// unreachable a record can wait past any deadline, so the publisher stops waiting at the
// job's deadline and counts the event as failed; franz-go may still deliver it later. A
// definite failure retried here, or a late delivery, can publish an event twice
// (at-least-once): consumers deduplicate on event_id or the natural key.
func (p *Publisher) produce(ctx context.Context, log *slog.Logger, topic, key string, wrapper proto.Message) error {
	value, err := proto.Marshal(wrapper)
	if err != nil {
		return fmt.Errorf("marshal %s: %w", topic, err)
	}
	return p.retry(ctx, log, "produce to "+topic, 0, func(ctx context.Context) error {
		acked := make(chan error, 1)
		p.cfg.Kafka.Produce(ctx, &kgo.Record{Topic: topic, Key: []byte(key), Value: value},
			func(_ *kgo.Record, err error) { acked <- err })
		select {
		case err := <-acked:
			return err
		case <-ctx.Done():
			return fmt.Errorf("no ack before the deadline (the record stays buffered): %w", ctx.Err())
		}
	})
}

// retry runs f until it succeeds or ctx ends, backing off between attempts. With perAttempt
// set, each attempt gets its own deadline.
func (p *Publisher) retry(ctx context.Context, log *slog.Logger, what string, perAttempt time.Duration, f func(context.Context) error) error {
	wait := p.cfg.Backoff
	for attempt := 1; ; attempt++ {
		actx, cancel := ctx, context.CancelFunc(func() {})
		if perAttempt > 0 {
			actx, cancel = context.WithTimeout(ctx, perAttempt)
		}
		err := f(actx)
		cancel()
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return fmt.Errorf("%s: gave up after %d attempts: %w", what, attempt, errors.Join(err, ctx.Err()))
		}
		log.WarnContext(ctx, "retrying", "op", what, "attempt", attempt, "err", err, "wait_ms", wait.Milliseconds())
		select {
		case <-time.After(wait):
		case <-ctx.Done():
			return fmt.Errorf("%s: gave up after %d attempts: %w", what, attempt, errors.Join(err, ctx.Err()))
		}
		wait = min(2*wait, maxBackoff)
	}
}

func endSpan(span trace.Span, err *error) {
	if *err != nil {
		span.RecordError(*err)
		span.SetStatus(codes.Error, (*err).Error())
	}
	span.End()
}
