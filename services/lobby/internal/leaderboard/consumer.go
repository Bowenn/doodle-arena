package leaderboard

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	eventsv1 "github.com/Bowenn/gap-project_private/proto/gen/go/doodle/events/v1"
	"github.com/twmb/franz-go/pkg/kgo"
	"github.com/twmb/franz-go/plugin/kotel"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/protobuf/proto"
)

const (
	// Topic carries MatchEvent (design doc, section 6.2).
	Topic = "game.matches.v1"
	// Group is the lobby's consumer group. Lobby instances share it, so each match is
	// counted by one of them.
	Group = "lobby-leaderboard"

	commitTimeout = 5 * time.Second
	retryMin      = 200 * time.Millisecond
	retryMax      = 5 * time.Second
)

// Applier counts a match; *Store is the real one.
type Applier interface {
	Apply(ctx context.Context, m Match) (bool, error)
}

// Consumer reads MatchFinished events and adds them to the leaderboard. Delivery is at least
// once: an offset is committed only after its match is in Redis, and Apply ignores a match it
// has already counted.
type Consumer struct {
	client  *kgo.Client
	store   Applier
	log     *slog.Logger
	tracer  trace.Tracer
	matches metric.Int64Counter
}

// NewConsumer returns a consumer in group Group. It connects lazily, so the lobby starts (and
// serves rooms) while Kafka is down. The kotel hook continues each record's traceparent, so
// the apply span joins the trace of the match that produced the event. Extra options are for
// tests.
func NewConsumer(brokers []string, store Applier, log *slog.Logger, opts ...kgo.Opt) (*Consumer, error) {
	matches, err := otel.Meter("lobby").Int64Counter("lobby.leaderboard.matches",
		metric.WithDescription("MatchFinished events by result: applied, duplicate (counted before) or skipped (unknown, empty or malformed)"),
		metric.WithUnit("{match}"))
	if err != nil {
		return nil, fmt.Errorf("lobby.leaderboard.matches: %w", err)
	}
	opts = append([]kgo.Opt{
		kgo.SeedBrokers(brokers...),
		kgo.ConsumerGroup(Group),
		kgo.ConsumeTopics(Topic),
		// A new group starts at the oldest retained match, so none is missed.
		kgo.ConsumeResetOffset(kgo.NewOffset().AtStart()),
		// Commit by hand, after the Redis write.
		kgo.DisableAutoCommit(),
		// No rebalance between a poll and its commit, so a partition isn't handed to another
		// lobby while this one still works on it. Run allows it again after each commit.
		kgo.BlockRebalanceOnPoll(),
		kgo.WithHooks(kotel.NewKotel(kotel.WithTracer(kotel.NewTracer())).Hooks()...),
	}, opts...)
	client, err := kgo.NewClient(opts...)
	if err != nil {
		return nil, fmt.Errorf("kafka client: %w", err)
	}
	return &Consumer{client: client, store: store, log: log, tracer: otel.Tracer("lobby"), matches: matches}, nil
}

// Run consumes until ctx is cancelled, then leaves the group. Records it handled are
// committed even then; the rest are redelivered to the next consumer.
func (c *Consumer) Run(ctx context.Context) {
	defer c.client.CloseAllowingRebalance()
	for {
		fetches := c.client.PollFetches(ctx)
		if ctx.Err() != nil || fetches.IsClientClosed() {
			return
		}
		fetches.EachError(func(topic string, partition int32, err error) {
			c.log.WarnContext(ctx, "kafka fetch failed", "topic", topic, "partition", partition, "err", err)
		})
		var done []*kgo.Record
		stopped := false
		for it := fetches.RecordIter(); !it.Done(); {
			r := it.Next()
			if !c.handle(ctx, r) {
				stopped = true // cancelled while Redis was failing; r stays uncommitted
				break
			}
			done = append(done, r)
		}
		c.commit(done)
		c.client.AllowRebalance()
		if stopped {
			return
		}
	}
}

// commit stores the offsets of handled records. It doesn't use Run's context, so the work
// done before a shutdown is still committed. A failed commit only means redelivery, which
// Apply turns into duplicates.
func (c *Consumer) commit(records []*kgo.Record) {
	if len(records) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), commitTimeout)
	defer cancel()
	if err := c.client.CommitRecords(ctx, records...); err != nil {
		c.log.Warn("kafka commit failed, the records will be redelivered", "topic", Topic, "err", err)
	}
}

// handle applies one record. It returns false only if ctx was cancelled before the record
// could be applied. Records that can't be applied (an unknown event type, an empty wrapper,
// bytes that don't decode) are logged, counted and skipped, so they never block the
// partition (ADR 0004).
func (c *Consumer) handle(ctx context.Context, r *kgo.Record) bool {
	// The record's context holds kotel's receive span (a child of the producer's span); ctx
	// holds the cancellation.
	if r.Context != nil {
		ctx = trace.ContextWithSpan(ctx, trace.SpanFromContext(r.Context))
	}
	ctx, span := c.tracer.Start(ctx, "lobby.leaderboard.apply", trace.WithSpanKind(trace.SpanKindConsumer),
		trace.WithAttributes(
			attribute.String("messaging.destination.name", r.Topic),
			attribute.Int("messaging.destination.partition.id", int(r.Partition)),
			attribute.Int64("messaging.kafka.offset", r.Offset),
		))
	defer span.End()
	where := []any{"topic", r.Topic, "partition", r.Partition, "offset", r.Offset}

	m, err := decode(r.Value)
	if err != nil {
		c.log.WarnContext(ctx, "skipping match event", append(where, "reason", err.Error())...)
		span.SetAttributes(attribute.String("result", "skipped"))
		c.count(ctx, "skipped")
		return true
	}
	span.SetAttributes(attribute.String("match_id", m.ID))

	applied, err := c.applyWithRetry(ctx, m, where)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return false
	}
	result := "duplicate"
	if applied {
		result = "applied"
	}
	span.SetAttributes(attribute.String("result", result))
	c.count(ctx, result)
	msg := "match already counted, skipped"
	if applied {
		msg = "match counted"
	}
	c.log.InfoContext(ctx, msg, append(where, "match_id", m.ID, "week", Week(m.EndedAt), "players", len(m.Results))...)
	return true
}

// applyWithRetry retries until Redis takes the match or ctx is cancelled. A Redis outage
// pauses the leaderboard rather than skipping matches: the offset can't move past a match
// that wasn't counted.
func (c *Consumer) applyWithRetry(ctx context.Context, m Match, where []any) (bool, error) {
	wait := retryMin
	for {
		applied, err := c.store.Apply(ctx, m)
		if err == nil {
			return applied, nil
		}
		c.log.WarnContext(ctx, "leaderboard write failed, retrying", append(where, "match_id", m.ID, "err", err, "retry_in", wait.String())...)
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-time.After(wait):
		}
		wait = min(2*wait, retryMax)
	}
}

func (c *Consumer) count(ctx context.Context, result string) {
	c.matches.Add(ctx, 1, metric.WithAttributes(attribute.String("result", result)))
}

// Reasons decode refuses a record.
var (
	errMalformed = errors.New("malformed")
	errEmpty     = errors.New("empty event (no oneof case set)")
	errUnknown   = errors.New("unknown event type (a newer producer?)")
)

// decode turns a MatchEvent into a Match, or says why it can't be counted.
func decode(value []byte) (Match, error) {
	var ev eventsv1.MatchEvent
	if err := proto.Unmarshal(value, &ev); err != nil {
		return Match{}, fmt.Errorf("%w: %w", errMalformed, err)
	}
	mf := ev.GetMatchFinished()
	if mf == nil {
		// A case this build doesn't know arrives as unknown fields.
		if len(ev.ProtoReflect().GetUnknown()) > 0 {
			return Match{}, errUnknown
		}
		return Match{}, errEmpty
	}
	if mf.GetMatchId() == "" || !mf.GetEndedAt().IsValid() {
		return Match{}, fmt.Errorf("%w: match_finished needs match_id and ended_at", errMalformed)
	}
	m := Match{ID: mf.GetMatchId(), EndedAt: mf.GetEndedAt().AsTime(), Results: make([]Result, 0, len(mf.GetResults()))}
	for _, r := range mf.GetResults() {
		if r.GetUserId() == "" {
			return Match{}, fmt.Errorf("%w: a result has no user_id", errMalformed)
		}
		m.Results = append(m.Results, Result{UserID: r.GetUserId(), Score: int64(r.GetScore())})
	}
	return m, nil
}
