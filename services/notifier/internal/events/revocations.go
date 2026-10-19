package events

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/twmb/franz-go/pkg/kgo"
	"github.com/twmb/franz-go/plugin/kotel"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/protobuf/proto"

	eventsv1 "github.com/Bowenn/gap-project_private/proto/gen/go/doodle/events/v1"
)

const (
	// UsersTopic carries auth's user events (CredentialsRevoked, ADR 0015).
	UsersTopic = "auth.users.v1"
	// RevocationGroup reads UsersTopic from the earliest offset, apart from Group (which starts
	// at the newest): a revocation published before the notifier first subscribed must still
	// disable the endpoints. Revocations are few and idempotent, so reading them all is cheap.
	RevocationGroup = "notifier-auth"
)

// Revoker applies a CredentialsRevoked; *store.Store is the real one.
type Revoker interface {
	RevokeCredentials(ctx context.Context, ownerID string, revokedAt time.Time) (disabled int, fresh bool, err error)
}

// RevocationConsumer disables a user's endpoints when auth revokes their credentials. The offset
// is committed after the store commits; a redelivered event changes nothing (the store keeps the
// newest revocation per user).
type RevocationConsumer struct {
	client   *kgo.Client
	revoker  Revoker
	log      *slog.Logger
	tracer   trace.Tracer
	events   metric.Int64Counter
	disabled metric.Int64Counter
}

// NewRevocationConsumer joins RevocationGroup on UsersTopic. Like NewConsumer, it connects lazily.
func NewRevocationConsumer(brokers []string, revoker Revoker, log *slog.Logger, opts ...kgo.Opt) (*RevocationConsumer, error) {
	meter := otel.Meter("notifier")
	events, err := meter.Int64Counter("notifier.events",
		metric.WithDescription("Events read from Kafka, by result: queued (deliveries created), no_subscribers, duplicate (queued before) or skipped (unreadable)"),
		metric.WithUnit("{event}"))
	if err != nil {
		return nil, fmt.Errorf("notifier.events: %w", err)
	}
	disabled, err := meter.Int64Counter("notifier.endpoints.disabled",
		metric.WithDescription("Endpoints disabled because auth revoked their owner's credentials"),
		metric.WithUnit("{endpoint}"))
	if err != nil {
		return nil, fmt.Errorf("notifier.endpoints.disabled: %w", err)
	}
	opts = append([]kgo.Opt{
		kgo.SeedBrokers(brokers...),
		kgo.ConsumerGroup(RevocationGroup),
		kgo.ConsumeTopics(UsersTopic),
		kgo.ConsumeResetOffset(kgo.NewOffset().AtStart()),
		kgo.DisableAutoCommit(),
		kgo.BlockRebalanceOnPoll(),
		kgo.WithHooks(kotel.NewKotel(kotel.WithTracer(kotel.NewTracer())).Hooks()...),
	}, opts...)
	client, err := kgo.NewClient(opts...)
	if err != nil {
		return nil, fmt.Errorf("kafka client: %w", err)
	}
	return &RevocationConsumer{client: client, revoker: revoker, log: log, tracer: otel.Tracer("notifier"),
		events: events, disabled: disabled}, nil
}

// Run consumes until ctx is cancelled.
func (c *RevocationConsumer) Run(ctx context.Context) {
	consume(ctx, c.client, c.log, c.handle)
}

// DecodeRevocation reads a UserEvent; only CredentialsRevoked is handled (ADR 0004: unknown cases
// are skipped).
func DecodeRevocation(value []byte) (*eventsv1.CredentialsRevoked, error) {
	var ev eventsv1.UserEvent
	if err := proto.Unmarshal(value, &ev); err != nil {
		return nil, fmt.Errorf("unreadable UserEvent: %w", err)
	}
	cr := ev.GetCredentialsRevoked()
	switch {
	case cr == nil:
		return nil, errors.New("not a CredentialsRevoked")
	case cr.GetUserId() == "" || cr.GetRevokedAt() == nil:
		return nil, errors.New("CredentialsRevoked without user_id or revoked_at")
	}
	return cr, nil
}

func (c *RevocationConsumer) handle(ctx context.Context, r *kgo.Record) bool {
	if r.Context != nil {
		ctx = trace.ContextWithSpan(ctx, trace.SpanFromContext(r.Context))
	}
	ctx, span := c.tracer.Start(ctx, "notifier.revoke", trace.WithSpanKind(trace.SpanKindConsumer),
		trace.WithAttributes(
			attribute.String("messaging.destination.name", r.Topic),
			attribute.Int("messaging.destination.partition.id", int(r.Partition)),
			attribute.Int64("messaging.kafka.offset", r.Offset),
		))
	defer span.End()
	where := []any{"topic", r.Topic, "partition", r.Partition, "offset", r.Offset}

	cr, err := DecodeRevocation(r.Value)
	if err != nil {
		c.log.WarnContext(ctx, "skipping event", append(where, "reason", err.Error())...)
		c.count(ctx, span, "skipped")
		return true
	}
	user := cr.GetUserId()
	span.SetAttributes(attribute.String("user.id", user), attribute.String("event.id", cr.GetMeta().GetEventId()))
	for wait := retryMin; ; wait = min(2*wait, retryMax) {
		n, fresh, err := c.revoker.RevokeCredentials(ctx, user, cr.GetRevokedAt().AsTime())
		if err == nil {
			span.SetAttributes(attribute.Int("endpoints.disabled", n))
			if !fresh {
				c.count(ctx, span, "duplicate")
				return true
			}
			c.count(ctx, span, "revoked")
			c.disabled.Add(ctx, int64(n))
			c.log.InfoContext(ctx, "credentials revoked: endpoints disabled",
				append(where, "user_id", user, "event_id", cr.GetMeta().GetEventId(), "endpoints", n, "reason", cr.GetReason())...)
			return true
		}
		// Postgres is down: wait rather than skip, so the endpoints are always disabled.
		c.log.WarnContext(ctx, "revocation failed, retrying", append(where, "user_id", user, "err", err, "retry_in", wait.String())...)
		select {
		case <-ctx.Done():
			span.RecordError(ctx.Err())
			span.SetStatus(codes.Error, "cancelled")
			return false
		case <-time.After(wait):
		}
	}
}

func (c *RevocationConsumer) count(ctx context.Context, span trace.Span, result string) {
	span.SetAttributes(attribute.String("result", result))
	c.events.Add(ctx, 1, metric.WithAttributes(attribute.String("result", result)))
}
