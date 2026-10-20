package telemetry

import (
	"context"
	"testing"
	"time"

	"github.com/redis/go-redis/extra/redisotel/v9"
	"github.com/redis/go-redis/v9"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

// A room's commit runs outside any trace, once per stroke: its Redis call must not start a
// trace of its own. Inside a trace (a join, a round's end, a takeover), Redis calls stay traced,
// and other root spans (the room's own spans, heartbeats) are kept.
func TestSamplerDropsRedisCallsOutsideATrace(t *testing.T) {
	spans := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSampler(Sampler()), sdktrace.WithSpanProcessor(spans))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })

	// Nothing listens on port 1: each command fails fast, after redisotel has started its spans.
	rdb := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", DialTimeout: 100 * time.Millisecond, MaxRetries: -1})
	t.Cleanup(func() { _ = rdb.Close() })
	if err := redisotel.InstrumentTracing(rdb, redisotel.WithTracerProvider(tp)); err != nil {
		t.Fatal(err)
	}
	tracer := tp.Tracer("test")
	ctx := context.Background()

	rdb.Get(ctx, "outside")

	_, heartbeat := tracer.Start(ctx, "Heartbeat", trace.WithSpanKind(trace.SpanKindClient))
	heartbeat.End()
	roundCtx, round := tracer.Start(ctx, "game.round.end")
	rdb.Get(roundCtx, "inside")
	round.End()

	var got []string
	for _, s := range spans.Ended() {
		got = append(got, s.Name())
	}
	want := map[string]bool{"Heartbeat": true, "game.round.end": true, "get": true}
	for _, name := range got {
		if !want[name] {
			t.Errorf("span %q was kept, want only Heartbeat, game.round.end and the get inside it", name)
		}
		delete(want, name)
	}
	for name := range want {
		t.Errorf("span %q was dropped, want it kept (got %v)", name, got)
	}
	for _, s := range spans.Ended() {
		if s.Name() == "get" && !s.Parent().IsValid() {
			t.Errorf("the kept get span has no parent: the one outside a trace leaked through")
		}
	}
}
