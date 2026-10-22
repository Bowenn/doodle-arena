package relay

import (
	"context"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// metrics are the relay's instruments (docs/observability.md, "relay.*"). No room or user id is
// ever an attribute.
type metrics struct {
	viewers   metric.Int64UpDownCounter
	rooms     metric.Int64UpDownCounter
	broadcast metric.Float64Histogram
	lag       metric.Float64Histogram
	closes    metric.Int64Counter
	rebuilds  metric.Int64Counter
	guesses   metric.Int64Counter
	publishes metric.Int64Counter
	forwards  metric.Int64Counter
	tops      metric.Int64Counter
}

// Latency buckets in ms, from a local fan-out to a viewer far behind.
var latencyBuckets = []float64{1, 2.5, 5, 10, 25, 50, 100, 250, 500, 1000, 2500, 5000, 10000}

func newMetrics(m metric.Meter) (*metrics, error) {
	var out metrics
	var err error
	if out.viewers, err = m.Int64UpDownCounter("relay.viewers",
		metric.WithDescription("Viewers connected to this relay"), metric.WithUnit("{viewer}")); err != nil {
		return nil, err
	}
	if out.rooms, err = m.Int64UpDownCounter("relay.rooms",
		metric.WithDescription("Rooms this relay streams"), metric.WithUnit("{room}")); err != nil {
		return nil, err
	}
	if out.broadcast, err = m.Float64Histogram("relay.broadcast.latency",
		metric.WithDescription("Time from the owner's commit of an event until the write to a viewer's socket returns"),
		metric.WithUnit("ms"), metric.WithExplicitBucketBoundaries(latencyBuckets...)); err != nil {
		return nil, err
	}
	if out.lag, err = m.Float64Histogram("relay.stream.lag",
		metric.WithDescription("Time from the owner's commit of an event until this relay read it from the stream"),
		metric.WithUnit("ms"), metric.WithExplicitBucketBoundaries(latencyBuckets...)); err != nil {
		return nil, err
	}
	if out.closes, err = m.Int64Counter("relay.viewers.closed",
		metric.WithDescription("Viewers the relay disconnected, by reason: slow, room_ended, shutdown or error"),
		metric.WithUnit("{viewer}")); err != nil {
		return nil, err
	}
	if out.rebuilds, err = m.Int64Counter("relay.stream.rebuilds",
		metric.WithDescription("Times a room's stream was loaded from its snapshot, by reason: start or gap"),
		metric.WithUnit("{load}")); err != nil {
		return nil, err
	}
	// Viewer guesses (ADR 0019).
	if out.guesses, err = m.Int64Counter("relay.viewer_guesses",
		metric.WithDescription("Viewers' guesses by result: correct, wrong, close, not_drawing, rate_limited or already_guessed"),
		metric.WithUnit("{guess}")); err != nil {
		return nil, err
	}
	if out.publishes, err = m.Int64Counter("relay.guesses.published",
		metric.WithDescription("Wrong viewer guesses sent to live.guesses.v1, by result: success, failed or dropped (the client's buffer was full)"),
		metric.WithUnit("{guess}")); err != nil {
		return nil, err
	}
	if out.forwards, err = m.Int64Counter("relay.correct_guesses.forwarded",
		metric.WithDescription("Correct viewer guesses handed to the room's owner through Redis, by result: success, failed or dropped (queue full)"),
		metric.WithUnit("{guess}")); err != nil {
		return nil, err
	}
	if out.tops, err = m.Int64Counter("relay.top_guesses.received",
		metric.WithDescription("Top-guesses lists read from live.top-guesses.v1, by result: shown, stale, not_watched, dropped or unreadable"),
		metric.WithUnit("{list}")); err != nil {
		return nil, err
	}
	return &out, nil
}

func ms(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }

func (m *metrics) latency(ctx context.Context, d time.Duration)   { m.broadcast.Record(ctx, ms(d)) }
func (m *metrics) streamLag(ctx context.Context, d time.Duration) { m.lag.Record(ctx, ms(d)) }

func (m *metrics) closed(ctx context.Context, reason string) {
	m.closes.Add(ctx, 1, metric.WithAttributes(attribute.String("reason", reason)))
}

func (m *metrics) rebuild(ctx context.Context, reason string) {
	m.rebuilds.Add(ctx, 1, metric.WithAttributes(attribute.String("reason", reason)))
}

func (m *metrics) viewerGuess(ctx context.Context, result string) {
	m.guesses.Add(ctx, 1, metric.WithAttributes(attribute.String("result", result)))
}

func (m *metrics) published(ctx context.Context, result string) {
	m.publishes.Add(ctx, 1, metric.WithAttributes(attribute.String("result", result)))
}

func (m *metrics) forwarded(ctx context.Context, result string) {
	m.forwards.Add(ctx, 1, metric.WithAttributes(attribute.String("result", result)))
}

func (m *metrics) topGuesses(ctx context.Context, result string) {
	m.tops.Add(ctx, 1, metric.WithAttributes(attribute.String("result", result)))
}
