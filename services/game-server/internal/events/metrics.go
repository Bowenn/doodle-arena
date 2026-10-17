package events

import (
	"context"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// Publish results, the result attribute of game.events.published.
const (
	resultSuccess = "success"
	resultFailed  = "failed"  // retried until the job's deadline, then given up
	resultDropped = "dropped" // the queue was full, or the server was shutting down
)

type metrics struct {
	published metric.Int64Counter
	queued    metric.Int64UpDownCounter
	duration  metric.Float64Histogram
}

func newMetrics(m metric.Meter) (*metrics, error) {
	var mt metrics
	var err error
	if mt.published, err = m.Int64Counter("game.events.published",
		metric.WithDescription("Round and match events by topic and result: success, failed or dropped"),
		metric.WithUnit("{event}")); err != nil {
		return nil, err
	}
	if mt.queued, err = m.Int64UpDownCounter("game.events.queued",
		metric.WithDescription("Finished rounds and matches waiting for the publisher"), metric.WithUnit("{event}")); err != nil {
		return nil, err
	}
	if mt.duration, err = m.Float64Histogram("game.events.publish.duration",
		metric.WithDescription("Time to publish an event, including the stroke-log upload and retries"),
		metric.WithUnit("ms"),
		metric.WithExplicitBucketBoundaries(5, 10, 25, 50, 100, 250, 500, 1000, 2500, 5000, 10000, 30000, 60000, 120000)); err != nil {
		return nil, err
	}
	return &mt, nil
}

func (m *metrics) result(ctx context.Context, topic, result string) {
	m.published.Add(ctx, 1, metric.WithAttributes(attribute.String("topic", topic), attribute.String("result", result)))
}

func (m *metrics) took(ctx context.Context, topic string, d time.Duration) {
	m.duration.Record(ctx, float64(d)/float64(time.Millisecond), metric.WithAttributes(attribute.String("topic", topic)))
}
