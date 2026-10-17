package room

import (
	"context"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"
)

// Metrics are the room metrics from docs/observability.md. IDs are never attributes.
type Metrics struct {
	roomsActive metric.Int64UpDownCounter
	broadcastMS metric.Float64Histogram
	guesses     metric.Int64Counter
	reconnects  metric.Int64Counter
	takeovers   metric.Int64Counter
	outboxStale metric.Int64Gauge
	requeued    metric.Int64Counter
}

// NewMetrics creates the instruments once per process.
func NewMetrics(m metric.Meter) (*Metrics, error) {
	var mt Metrics
	var err error
	if mt.roomsActive, err = m.Int64UpDownCounter("game.rooms.active",
		metric.WithDescription("Rooms with a running actor"), metric.WithUnit("{room}")); err != nil {
		return nil, err
	}
	if mt.broadcastMS, err = m.Float64Histogram("game.broadcast.duration",
		metric.WithDescription("Time to marshal a room event and queue it to every local socket (not the network write)"),
		metric.WithUnit("ms"),
		metric.WithExplicitBucketBoundaries(0.01, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 25, 50, 100, 250)); err != nil {
		return nil, err
	}
	if mt.guesses, err = m.Int64Counter("game.guesses",
		metric.WithDescription("Guesses by result: correct, wrong or rate_limited"), metric.WithUnit("{guess}")); err != nil {
		return nil, err
	}
	if mt.reconnects, err = m.Int64Counter("game.reconnects",
		metric.WithDescription("Reconnects by result: resumed, snapshot or failed"), metric.WithUnit("{reconnect}")); err != nil {
		return nil, err
	}
	if mt.takeovers, err = m.Int64Counter("game.room.takeovers",
		metric.WithDescription("Rooms this server took over from another owner"), metric.WithUnit("{room}")); err != nil {
		return nil, err
	}
	if mt.outboxStale, err = m.Int64Gauge("game.outbox.pending",
		metric.WithDescription("Outbox entries older than a minute (finished rounds and matches not yet in Kafka) in this server's rooms"),
		metric.WithUnit("{entry}")); err != nil {
		return nil, err
	}
	if mt.requeued, err = m.Int64Counter("game.outbox.requeued",
		metric.WithDescription("Outbox entries queued for publishing again, by reason: takeover or stale"),
		metric.WithUnit("{entry}")); err != nil {
		return nil, err
	}
	return &mt, nil
}

// Takeover counts a room this server restored from another owner's state.
func (m *Metrics) Takeover(ctx context.Context) { m.takeovers.Add(ctx, 1) }

// OutboxPending records how many outbox entries in this server's rooms are older than the
// stale threshold (ADR 0009).
func (m *Metrics) OutboxPending(ctx context.Context, n int) { m.outboxStale.Record(ctx, int64(n)) }

// OutboxRequeued counts entries queued again: reason is "takeover" or "stale".
func (m *Metrics) OutboxRequeued(ctx context.Context, reason string, n int) {
	if n > 0 {
		m.requeued.Add(ctx, int64(n), metric.WithAttributes(attribute.String("reason", reason)))
	}
}

// NoopMetrics records nothing (tests that don't look at metrics).
func NoopMetrics() *Metrics {
	m, _ := NewMetrics(noop.NewMeterProvider().Meter("noop"))
	return m
}

func (m *Metrics) roomOpened(ctx context.Context, mode string) {
	m.roomsActive.Add(ctx, 1, metric.WithAttributes(attribute.String("mode", mode)))
}

func (m *Metrics) roomClosed(ctx context.Context, mode string) {
	m.roomsActive.Add(ctx, -1, metric.WithAttributes(attribute.String("mode", mode)))
}

func (m *Metrics) broadcast(ctx context.Context, eventType string, d time.Duration) {
	m.broadcastMS.Record(ctx, float64(d)/float64(time.Millisecond), metric.WithAttributes(attribute.String("event_type", eventType)))
}

func (m *Metrics) guess(ctx context.Context, result string) {
	m.guesses.Add(ctx, 1, metric.WithAttributes(attribute.String("result", result)))
}

func (m *Metrics) reconnect(ctx context.Context, result string) {
	m.reconnects.Add(ctx, 1, metric.WithAttributes(attribute.String("result", result)))
}
