package telemetry

import (
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.24.0"
	"go.opentelemetry.io/otel/trace"
)

// Sampler is the trace sampler unless OTEL_TRACES_SAMPLER sets another: parent-based, keeping
// every root span except a Redis call outside any trace. A room commits every step (each stroke
// and guess) to Redis from its actor, which runs outside any trace, so tracing those calls
// would make one trace per stroke: about 97% of all spans under load, enough to run Tempo out
// of memory (docs/observability.md, section 4: rounds and room lifecycle events are traced,
// individual strokes are not). Redis calls inside a trace, such as a join's ticket redeem or a
// takeover's reads, are still traced.
func Sampler() sdktrace.Sampler {
	return sdktrace.ParentBased(rootSampler{})
}

// rootSampler decides for spans without a parent (ParentBased handles the rest).
type rootSampler struct{}

func (rootSampler) ShouldSample(p sdktrace.SamplingParameters) sdktrace.SamplingResult {
	ts := trace.SpanContextFromContext(p.ParentContext).TraceState()
	if p.Kind == trace.SpanKindClient {
		for _, a := range p.Attributes {
			// redisotel sets db.system when it starts the span.
			if a.Key == semconv.DBSystemKey && a.Value.AsString() == semconv.DBSystemRedis.Value.AsString() {
				return sdktrace.SamplingResult{Decision: sdktrace.Drop, Tracestate: ts}
			}
		}
	}
	return sdktrace.SamplingResult{Decision: sdktrace.RecordAndSample, Tracestate: ts}
}

func (rootSampler) Description() string { return "DropRootRedisCalls" }
