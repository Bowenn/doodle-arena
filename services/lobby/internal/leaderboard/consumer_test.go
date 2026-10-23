package leaderboard

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/Bowenn/doodle-arena/libs/go/redistest"
	eventsv1 "github.com/Bowenn/doodle-arena/proto/gen/go/doodle/events/v1"
	"github.com/twmb/franz-go/pkg/kadm"
	"github.com/twmb/franz-go/pkg/kerr"
	"github.com/twmb/franz-go/pkg/kfake"
	"github.com/twmb/franz-go/pkg/kgo"
	"github.com/twmb/franz-go/plugin/kotel"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

// kafka is an in-memory Kafka (kfake) with the matches topic.
func kafka(t *testing.T) []string {
	t.Helper()
	c, err := kfake.NewCluster(kfake.NumBrokers(1), kfake.SeedTopics(1, Topic))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	return c.ListenAddrs()
}

func matchEvent(t *testing.T, id string, ended time.Time, results ...*eventsv1.PlayerResult) []byte {
	t.Helper()
	b, err := proto.Marshal(&eventsv1.MatchEvent{Event: &eventsv1.MatchEvent_MatchFinished{MatchFinished: &eventsv1.MatchFinished{
		Meta:    &eventsv1.EventMeta{EventId: "e-" + id, EventType: "MatchFinished", SchemaVersion: 1},
		MatchId: id, RoomId: "r1", EndedAt: timestamppb.New(ended), Results: results,
	}}})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// produce publishes the values in order, from ctx (so a span in it is the parent).
func produce(ctx context.Context, t *testing.T, brokers []string, values ...[]byte) {
	t.Helper()
	cl, err := kgo.NewClient(kgo.SeedBrokers(brokers...),
		kgo.WithHooks(kotel.NewKotel(kotel.WithTracer(kotel.NewTracer())).Hooks()...))
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()
	for _, v := range values {
		if err := cl.ProduceSync(ctx, &kgo.Record{Topic: Topic, Key: []byte("r1"), Value: v}).FirstErr(); err != nil {
			t.Fatal(err)
		}
	}
}

// committed returns the group's committed offset on partition 0, or -1.
func committed(t *testing.T, brokers []string) int64 {
	t.Helper()
	cl, err := kgo.NewClient(kgo.SeedBrokers(brokers...))
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()
	offs, err := kadm.NewClient(cl).FetchOffsets(t.Context(), Group)
	if errors.Is(err, kerr.GroupIDNotFound) {
		return -1 // the consumer hasn't joined yet
	}
	if err != nil {
		t.Fatal(err)
	}
	if o, ok := offs.Lookup(Topic, 0); ok {
		return o.At
	}
	return -1
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// start runs a consumer until the test ends.
func start(t *testing.T, brokers []string, store Applier) (stop func()) {
	t.Helper()
	c, err := NewConsumer(brokers, store, quiet, kgo.FetchMaxWait(100*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { c.Run(ctx); close(done) }()
	stop = sync.OnceFunc(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("Run did not return after cancel")
		}
	})
	t.Cleanup(stop)
	return stop
}

type telemetry struct {
	metrics *sdkmetric.ManualReader
	spans   *tracetest.SpanRecorder
}

func setupTelemetry() telemetry {
	tel := telemetry{metrics: sdkmetric.NewManualReader(), spans: tracetest.NewSpanRecorder()}
	otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(tel.metrics)))
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(tel.spans)))
	otel.SetTextMapPropagator(propagation.TraceContext{})
	return tel
}

func (tel telemetry) results(t *testing.T) map[string]int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := tel.metrics.Collect(t.Context(), &rm); err != nil {
		t.Fatal(err)
	}
	out := map[string]int64{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "lobby.leaderboard.matches" {
				continue
			}
			for _, dp := range m.Data.(metricdata.Sum[int64]).DataPoints {
				v, _ := dp.Attributes.Value(attribute.Key("result"))
				out[v.AsString()] += dp.Value
			}
		}
	}
	return out
}

func TestConsumerAppliesDedupesAndSkips(t *testing.T) {
	tel := setupTelemetry()
	rdb := redistest.New(t)
	brokers := kafka(t)

	m1 := matchEvent(t, "m1", friday, &eventsv1.PlayerResult{UserId: "alice", Score: 300, Placement: 1},
		&eventsv1.PlayerResult{UserId: "bob", Score: 100, Placement: 2})
	// A case this build doesn't know: field 2 of MatchEvent, as a newer producer would add.
	unknown := protowire.AppendBytes(protowire.AppendTag(nil, 2, protowire.BytesType), []byte{0x0a, 0x01, 'x'})
	values := [][]byte{
		m1,
		m1, // redelivered
		{}, // a MatchEvent with no case set
		unknown,
		{0xff, 0xff, 0xff}, // not a protobuf
		matchEvent(t, "m-no-user", friday, &eventsv1.PlayerResult{Score: 5}),
		matchEvent(t, "m2", friday.Add(7*24*time.Hour), &eventsv1.PlayerResult{UserId: "alice", Score: 40}),
	}
	produce(t.Context(), t, brokers, values...)

	start(t, brokers, NewStore(rdb))
	waitFor(t, "every offset to be committed", func() bool { return committed(t, brokers) == int64(len(values)) })

	if got := scores(t, rdb, "2026-W42"); got["alice"] != 300 || got["bob"] != 100 || len(got) != 2 {
		t.Errorf("2026-W42 = %v, want alice 300 and bob 100 (m1 counted once)", got)
	}
	if got := scores(t, rdb, "2026-W43"); got["alice"] != 40 {
		t.Errorf("2026-W43 = %v, want alice 40", got)
	}
	got := tel.results(t)
	want := map[string]int64{"applied": 2, "duplicate": 1, "skipped": 4}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("lobby.leaderboard.matches{result=%s} = %d, want %d (all: %v)", k, got[k], v, got)
		}
	}
}

func TestConsumerContinuesTheProducersTrace(t *testing.T) {
	tel := setupTelemetry()
	brokers := kafka(t)

	ctx, parent := otel.Tracer("test").Start(t.Context(), "game.publish_match")
	produce(ctx, t, brokers, matchEvent(t, "m1", friday, &eventsv1.PlayerResult{UserId: "alice", Score: 1}))
	parent.End()

	start(t, brokers, NewStore(redistest.New(t)))
	waitFor(t, "the offset to be committed", func() bool { return committed(t, brokers) == 1 })

	var found bool
	for _, s := range tel.spans.Ended() {
		if s.Name() != "lobby.leaderboard.apply" {
			continue
		}
		found = true
		if s.SpanContext().TraceID() != parent.SpanContext().TraceID() {
			t.Errorf("apply span trace %s, want the producer's %s", s.SpanContext().TraceID(), parent.SpanContext().TraceID())
		}
		if !s.Parent().IsValid() {
			t.Error("apply span has no parent")
		}
	}
	if !found {
		t.Fatal("no lobby.leaderboard.apply span")
	}
}

// flakyStore fails until healthy is set.
type flakyStore struct {
	mu      sync.Mutex
	healthy bool
	calls   int
	inner   Applier
}

func (f *flakyStore) Apply(ctx context.Context, m Match) (bool, error) {
	f.mu.Lock()
	f.calls++
	healthy := f.healthy
	f.mu.Unlock()
	if !healthy {
		return false, errors.New("redis is down")
	}
	return f.inner.Apply(ctx, m)
}

func (f *flakyStore) heal() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.healthy = true
}

func (f *flakyStore) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func TestRedisOutageNeitherSkipsNorCommits(t *testing.T) {
	setupTelemetry()
	rdb := redistest.New(t)
	brokers := kafka(t)
	produce(t.Context(), t, brokers, matchEvent(t, "m1", friday, &eventsv1.PlayerResult{UserId: "alice", Score: 300}))

	// The consumer retries while Redis is down, and shuts down without committing.
	down := &flakyStore{inner: NewStore(rdb)}
	stop := start(t, brokers, down)
	waitFor(t, "a few retries", func() bool { return down.callCount() >= 2 })
	stop()
	if got := committed(t, brokers); got != -1 {
		t.Fatalf("committed offset %d while the match was never written", got)
	}

	// The next consumer gets the same match, and once Redis is back it is counted.
	again := &flakyStore{inner: NewStore(rdb)}
	start(t, brokers, again)
	waitFor(t, "a retry", func() bool { return again.callCount() >= 1 })
	again.heal()
	waitFor(t, "the offset to be committed", func() bool { return committed(t, brokers) == 1 })
	if got := scores(t, rdb, "2026-W42")["alice"]; got != 300 {
		t.Errorf("alice = %v, want 300", got)
	}
}
