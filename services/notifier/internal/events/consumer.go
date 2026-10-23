package events

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/twmb/franz-go/pkg/kgo"
	"github.com/twmb/franz-go/plugin/kotel"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	"github.com/Bowenn/doodle-arena/services/notifier/internal/store"
	"github.com/Bowenn/doodle-arena/services/notifier/internal/webhook"
)

const (
	// Group is the notifier's consumer group; instances share it.
	Group = "notifier"

	commitTimeout = 5 * time.Second
	retryMin      = 200 * time.Millisecond
	retryMax      = 5 * time.Second
)

// Queuer stores deliveries; *store.Store is the real one.
type Queuer interface {
	Subscribers(ctx context.Context, ownerIDs []string, eventType string) ([]store.Endpoint, error)
	InsertDeliveries(ctx context.Context, ds []store.Delivery) (int, error)
}

// Consumer reads sales and credits and queues a delivery per subscribed endpoint. Delivery is at
// least once: an offset is committed only after its deliveries are stored, and the
// (endpoint, event) unique key makes a redelivered record queue nothing new.
type Consumer struct {
	client *kgo.Client
	queue  Queuer
	log    *slog.Logger
	tracer trace.Tracer
	events metric.Int64Counter
}

// NewConsumer joins group Group on both topics. It connects lazily, so the notifier starts (and
// serves its API) while Kafka is down. The kotel hook continues each record's traceparent, so
// the queueing span joins the trace of the sale that produced the event.
func NewConsumer(brokers []string, queue Queuer, log *slog.Logger, opts ...kgo.Opt) (*Consumer, error) {
	counter, err := otel.Meter("notifier").Int64Counter("notifier.events",
		metric.WithDescription("Events read from Kafka, by result: queued (deliveries created), no_subscribers, duplicate (queued before) or skipped (unreadable)"),
		metric.WithUnit("{event}"))
	if err != nil {
		return nil, fmt.Errorf("notifier.events: %w", err)
	}
	opts = append([]kgo.Opt{
		kgo.SeedBrokers(brokers...),
		kgo.ConsumerGroup(Group),
		kgo.ConsumeTopics(SalesTopic, WalletTopic),
		// A new group starts at the newest offset: endpoints receive events from after they
		// were registered, not the marketplace's whole history.
		kgo.ConsumeResetOffset(kgo.NewOffset().AtEnd()),
		kgo.DisableAutoCommit(),
		kgo.BlockRebalanceOnPoll(),
		kgo.WithHooks(kotel.NewKotel(kotel.WithTracer(kotel.NewTracer())).Hooks()...),
	}, opts...)
	client, err := kgo.NewClient(opts...)
	if err != nil {
		return nil, fmt.Errorf("kafka client: %w", err)
	}
	return &Consumer{client: client, queue: queue, log: log, tracer: otel.Tracer("notifier"), events: counter}, nil
}

// Run consumes until ctx is cancelled, then leaves the group. Records it handled are committed
// even then; the rest are redelivered to the next consumer.
func (c *Consumer) Run(ctx context.Context) {
	consume(ctx, c.client, c.log, c.handle)
}

// consume polls, hands each record to handle in order and commits the handled ones, until ctx is
// cancelled or handle returns false (cancelled mid-record; that record stays uncommitted).
func consume(ctx context.Context, client *kgo.Client, log *slog.Logger, handle func(context.Context, *kgo.Record) bool) {
	defer client.CloseAllowingRebalance()
	for {
		fetches := client.PollFetches(ctx)
		if ctx.Err() != nil || fetches.IsClientClosed() {
			return
		}
		fetches.EachError(func(topic string, partition int32, err error) {
			log.WarnContext(ctx, "kafka fetch failed", "topic", topic, "partition", partition, "err", err)
		})
		var done []*kgo.Record
		stopped := false
		for it := fetches.RecordIter(); !it.Done(); {
			r := it.Next()
			if !handle(ctx, r) {
				stopped = true // cancelled while Postgres was failing; r stays uncommitted
				break
			}
			done = append(done, r)
		}
		commit(client, log, done)
		client.AllowRebalance()
		if stopped {
			return
		}
	}
}

func commit(client *kgo.Client, log *slog.Logger, records []*kgo.Record) {
	if len(records) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), commitTimeout)
	defer cancel()
	if err := client.CommitRecords(ctx, records...); err != nil {
		log.Warn("kafka commit failed, the records will be redelivered", "err", err)
	}
}

// handle queues one record's deliveries. It returns false only if ctx was cancelled first.
func (c *Consumer) handle(ctx context.Context, r *kgo.Record) bool {
	if r.Context != nil {
		ctx = trace.ContextWithSpan(ctx, trace.SpanFromContext(r.Context))
	}
	ctx, span := c.tracer.Start(ctx, "notifier.queue", trace.WithSpanKind(trace.SpanKindConsumer),
		trace.WithAttributes(
			attribute.String("messaging.destination.name", r.Topic),
			attribute.Int("messaging.destination.partition.id", int(r.Partition)),
			attribute.Int64("messaging.kafka.offset", r.Offset),
		))
	defer span.End()
	where := []any{"topic", r.Topic, "partition", r.Partition, "offset", r.Offset}

	ev, err := Decode(r.Topic, r.Value)
	if err != nil {
		c.log.WarnContext(ctx, "skipping event", append(where, "reason", err.Error())...)
		c.count(ctx, span, "skipped")
		return true
	}
	span.SetAttributes(attribute.String("event.id", ev.ID), attribute.String("event.type", ev.Type))

	for wait := retryMin; ; wait = min(2*wait, retryMax) {
		result, n, err := c.queueEvent(ctx, ev)
		if err == nil {
			c.count(ctx, span, result)
			if n > 0 {
				c.log.InfoContext(ctx, "deliveries queued", append(where, "event_id", ev.ID, "event_type", ev.Type, "deliveries", n)...)
			}
			return true
		}
		// Postgres is down: wait rather than skip, so no subscribed endpoint misses the event.
		c.log.WarnContext(ctx, "queueing failed, retrying", append(where, "event_id", ev.ID, "err", err, "retry_in", wait.String())...)
		select {
		case <-ctx.Done():
			span.RecordError(ctx.Err())
			span.SetStatus(codes.Error, "cancelled")
			return false
		case <-time.After(wait):
		}
	}
}

func (c *Consumer) queueEvent(ctx context.Context, ev Event) (string, int, error) {
	endpoints, err := c.queue.Subscribers(ctx, ev.Users, ev.Type)
	if err != nil {
		return "", 0, err
	}
	if len(endpoints) == 0 {
		return "no_subscribers", 0, nil
	}
	body, err := webhook.Body(ev.ID, ev.Type, ev.Created, ev.Data)
	if err != nil {
		return "", 0, err
	}
	// Every attempt, however much later, continues this trace (and so the sale's).
	carrier := propagation.MapCarrier{}
	otel.GetTextMapPropagator().Inject(ctx, carrier)
	ds := make([]store.Delivery, len(endpoints))
	for i, e := range endpoints {
		ds[i] = store.Delivery{ID: uuid.New(), EndpointID: e.ID, SourceEventID: ev.ID, EventType: ev.Type, Payload: body,
			Traceparent: carrier.Get("traceparent")}
	}
	n, err := c.queue.InsertDeliveries(ctx, ds)
	if err != nil {
		return "", 0, err
	}
	if n == 0 {
		return "duplicate", 0, nil
	}
	return "queued", n, nil
}

func (c *Consumer) count(ctx context.Context, span trace.Span, result string) {
	span.SetAttributes(attribute.String("result", result))
	c.events.Add(ctx, 1, metric.WithAttributes(attribute.String("result", result)))
}
