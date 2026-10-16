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
	return &mt, nil
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
