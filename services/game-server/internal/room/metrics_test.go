package room

import (
	"context"
	"maps"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// The counters of rare events exist at 0 from the start. Prometheus reads a series that first
// appears with its first value as one sample, so increase() and rate() over it are 0: task 6.9's
// drain moved 47 rooms to a server that had never taken one over, and game.room.takeovers
// showed no increase.
func TestRareCountersStartAtZero(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	if _, err := NewMetrics(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)).Meter("t")); err != nil {
		t.Fatal(err)
	}
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	got := map[string]int64{}
	for _, sm := range rm.ScopeMetrics {
		for _, mt := range sm.Metrics {
			for _, dp := range mt.Data.(metricdata.Sum[int64]).DataPoints {
				name := mt.Name
				if v, ok := dp.Attributes.Value(attribute.Key("reason")); ok {
					name += "/" + v.AsString()
				}
				got[name] = dp.Value
			}
		}
	}
	want := map[string]int64{"game.room.takeovers": 0, "game.outbox.requeued/takeover": 0, "game.outbox.requeued/stale": 0}
	if !maps.Equal(got, want) {
		t.Fatalf("series before any event = %v, want %v", got, want)
	}
}
