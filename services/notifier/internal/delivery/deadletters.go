package delivery

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	eventsv1 "github.com/Bowenn/gap-project_private/proto/gen/go/doodle/events/v1"
	"github.com/google/uuid"
	"github.com/twmb/franz-go/pkg/kgo"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/Bowenn/gap-project_private/services/notifier/internal/store"
	"github.com/Bowenn/gap-project_private/services/notifier/internal/webhook"
)

// DLQTopic carries DeliveryDead (design doc 6.2).
const DLQTopic = "notifier.dlq.v1"

// deadLetterNamespace makes a DeliveryDead's event_id a function of its delivery id, so a resend
// after a crash carries the same id and consumers can dedupe.
var deadLetterNamespace = uuid.MustParse("6f1d8e1c-3b0a-4f6e-9d27-0b1c2d3e4f50")

// DeadLetters publishes a DeliveryDead for every DEAD delivery and marks it once Kafka
// acknowledged it: the deliveries table is its own outbox, so a dead delivery is never lost on
// its way to the dead-letter queue.
type DeadLetters struct {
	store  *store.Store
	client *kgo.Client
	log    *slog.Logger
	every  time.Duration
}

// NewDeadLetters registers the notifier.dlq.size gauge (deliveries that went DEAD in the last
// 24 hours, so an alert on it clears by itself once nothing new dies). Test pings are left out
// of both the gauge and the topic: they carry no event, and a developer testing a broken
// endpoint is not news.
func NewDeadLetters(st *store.Store, client *kgo.Client, log *slog.Logger, every time.Duration) (*DeadLetters, error) {
	_, err := otel.Meter("notifier").Int64ObservableGauge("notifier.dlq.size",
		metric.WithDescription("Deliveries that went DEAD in the last 24 hours"),
		metric.WithUnit("{delivery}"),
		metric.WithInt64Callback(func(ctx context.Context, o metric.Int64Observer) error {
			ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
			defer cancel()
			n, err := st.DeadSince(ctx, time.Now().Add(-24*time.Hour))
			if err != nil {
				return nil // no value rather than a wrong one
			}
			o.Observe(n)
			return nil
		}))
	if err != nil {
		return nil, fmt.Errorf("notifier.dlq.size: %w", err)
	}
	return &DeadLetters{store: st, client: client, log: log, every: every}, nil
}

// Run publishes until ctx is cancelled.
func (d *DeadLetters) Run(ctx context.Context) {
	for {
		if n, err := d.PublishOnce(ctx); err != nil && ctx.Err() == nil {
			d.log.WarnContext(ctx, "publishing dead letters failed, retrying", "err", err)
		} else if n > 0 {
			d.log.InfoContext(ctx, "dead letters published", "count", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(d.every):
		}
	}
}

// PublishOnce sends the waiting dead letters and returns how many Kafka acknowledged.
func (d *DeadLetters) PublishOnce(ctx context.Context) (int, error) {
	dead, err := d.store.UnpublishedDead(ctx, 100)
	if err != nil || len(dead) == 0 {
		return 0, err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	records := make([]*kgo.Record, len(dead))
	for i, x := range dead {
		ev := &eventsv1.DeliveryDead{
			Meta: &eventsv1.EventMeta{
				EventId:       uuid.NewSHA1(deadLetterNamespace, x.ID[:]).String(),
				EventType:     "DeliveryDead",
				SchemaVersion: 1,
				OccurredAt:    timestamppb.New(*x.FinishedAt),
				Producer:      "notifier",
			},
			DeliveryId: x.ID.String(), EndpointId: x.EndpointID.String(),
			SourceEventId: x.SourceEventID, SourceEventType: webhook.SourceEventType(x.EventType), Attempts: uint32(x.Attempts),
			FirstAttemptAt: timestamppb.New(*x.FirstAttemptAt), DeadAt: timestamppb.New(*x.FinishedAt),
		}
		if x.LastStatusCode != nil {
			ev.LastStatusCode = int32(*x.LastStatusCode)
		}
		if x.LastError != nil {
			ev.LastError = *x.LastError
		}
		value, err := proto.Marshal(&eventsv1.DeadLetterEvent{Event: &eventsv1.DeadLetterEvent_DeliveryDead{DeliveryDead: ev}})
		if err != nil {
			return 0, err
		}
		records[i] = &kgo.Record{Topic: DLQTopic, Key: []byte(x.EndpointID.String()), Value: value}
	}
	results := d.client.ProduceSync(ctx, records...)
	var acked []uuid.UUID
	for i, r := range results {
		if r.Err == nil {
			acked = append(acked, dead[i].ID)
		}
	}
	if err := d.store.MarkDeadPublished(ctx, acked); err != nil {
		return 0, err
	}
	if len(acked) < len(dead) {
		return len(acked), fmt.Errorf("%d of %d dead letters not acknowledged: %w", len(dead)-len(acked), len(dead), results.FirstErr())
	}
	return len(acked), nil
}
