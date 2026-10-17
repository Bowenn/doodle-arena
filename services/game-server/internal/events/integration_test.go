package events

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	eventsv1 "github.com/Bowenn/gap-project_private/proto/gen/go/doodle/events/v1"
	"github.com/Bowenn/gap-project_private/services/game-server/internal/kafkatest"
	"github.com/Bowenn/gap-project_private/services/game-server/internal/room"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/twmb/franz-go/pkg/kgo"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric/noop"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/protobuf/proto"
)

// TestPublishThroughKafkaAndSeaweedFS is the M2 criterion "RoundFinished and MatchFinished
// appear in Kafka with trace context", against the real broker and object store: the stroke
// log is stored under the event's key, and each record's traceparent continues the room's
// end span.
func TestPublishThroughKafkaAndSeaweedFS(t *testing.T) {
	if testing.Short() {
		t.Skip("starts Kafka and SeaweedFS containers")
	}
	brokers := kafkatest.Kafka(t)
	kafkatest.CreateTopics(t, brokers, TopicRounds, TopicMatches)

	// The production setup: a global tracer provider and the W3C propagator (internal/telemetry),
	// set before the clients are built: otelaws captures the global provider when the S3 client
	// is created, so a client built first would keep the provider of an earlier run (-count=N).
	spans := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(spans))
	prevTP, prevProp := otel.GetTracerProvider(), otel.GetTextMapPropagator()
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.TraceContext{})
	t.Cleanup(func() { otel.SetTracerProvider(prevTP); otel.SetTextMapPropagator(prevProp) })

	store := NewS3Client(S3Config{Endpoint: kafkatest.SeaweedFS(t, "testkey", "testsecret"), Region: "us-east-1",
		AccessKey: "testkey", SecretKey: "testsecret"})
	createBucket(t, store, "round-logs")

	kafka, err := NewKafkaClient([]string{brokers})
	if err != nil {
		t.Fatal(err)
	}
	defer kafka.Close()
	pub, err := New(Config{Kafka: kafka, S3: store, Bucket: "round-logs",
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Meter: noop.NewMeterProvider().Meter("t")})
	if err != nil {
		t.Fatal(err)
	}
	go pub.Run(context.Background())

	roundCtx, roundSpan := tp.Tracer("t").Start(context.Background(), "game.round.end")
	roundSpan.End()
	matchCtx, matchSpan := tp.Tracer("t").Start(context.Background(), "game.match.end")
	matchSpan.End()
	// The round comes back from an outbox, as a new owner sends what the old one left (ADR
	// 0009): its trace continues through the traceparent stored in the entry.
	r := finishedRound()
	entry, err := room.EncodeOutbox(roundCtx, "r1", "evt-round", room.Finished{Round: &r})
	if err != nil {
		t.Fatal(err)
	}
	entry.ID = "1-0"
	if n := pub.Requeue([]room.OutboxEntry{entry}); n != 1 {
		t.Fatalf("requeued %d, want 1", n)
	}
	pub.MatchFinished(matchCtx, finishedMatch(), room.OutboxEntry{ID: "1-1", RoomID: "r1", EventID: "evt-match"})
	pub.Close()
	select {
	case <-pub.Done():
	case <-time.After(time.Minute):
		t.Fatal("publisher didn't finish")
	}

	// The claim check: the object exists under the key the event names.
	obj, err := store.GetObject(t.Context(), &s3.GetObjectInput{Bucket: aws.String("round-logs"), Key: aws.String("m1/2.binpb.gz")})
	if err != nil {
		t.Fatalf("get stroke log: %v", err)
	}
	body, _ := io.ReadAll(obj.Body)
	_ = obj.Body.Close()
	if l := gunzipStrokeLog(t, body); len(l.GetStrokes()) != 2 || l.GetRoundNo() != 2 {
		t.Fatalf("stored stroke log = %v", l)
	}

	rounds := consume(t, brokers, TopicRounds)
	var re eventsv1.RoundEvent
	if err := proto.Unmarshal(rounds.Value, &re); err != nil {
		t.Fatal(err)
	}
	if string(rounds.Key) != "r1" || re.GetRoundFinished().GetStrokeLogKey() != "m1/2.binpb.gz" {
		t.Fatalf("round record key %q, event %v", rounds.Key, re.GetRoundFinished())
	}
	matches := consume(t, brokers, TopicMatches)
	var me eventsv1.MatchEvent
	if err := proto.Unmarshal(matches.Value, &me); err != nil {
		t.Fatal(err)
	}
	if string(matches.Key) != "r1" || len(me.GetMatchFinished().GetResults()) != 2 {
		t.Fatalf("match record key %q, event %v", matches.Key, me.GetMatchFinished())
	}

	// traceparent: room end span -> publish span -> Kafka producer span (the header's parent).
	ended := map[string]sdktrace.ReadOnlySpan{}
	for _, s := range spans.Ended() {
		ended[s.Name()] = s
	}
	for _, c := range []struct {
		rec                    *kgo.Record
		end, publish, producer string
	}{
		{rounds, "game.round.end", "game.round.publish", TopicRounds + " publish"},
		{matches, "game.match.end", "game.match.publish", TopicMatches + " publish"},
	} {
		sc := headerSpan(t, c.rec)
		root, pubSpan, prodSpan := ended[c.end], ended[c.publish], ended[c.producer]
		if root == nil || pubSpan == nil || prodSpan == nil {
			t.Fatalf("%s: missing spans; got %v", c.rec.Topic, names(spans.Ended()))
		}
		if sc.TraceID() != root.SpanContext().TraceID() {
			t.Fatalf("%s: traceparent trace %s, want the room's %s", c.rec.Topic, sc.TraceID(), root.SpanContext().TraceID())
		}
		if sc.SpanID() != prodSpan.SpanContext().SpanID() || prodSpan.Parent().SpanID() != pubSpan.SpanContext().SpanID() ||
			pubSpan.Parent().SpanID() != root.SpanContext().SpanID() {
			t.Fatalf("%s: span chain broken", c.rec.Topic)
		}
	}
	// The upload is traced too, inside the round's publish span.
	if s3span := ended["S3.PutObject"]; s3span == nil || s3span.Parent().SpanID() != ended["game.round.publish"].SpanContext().SpanID() {
		t.Fatalf("no S3.PutObject span under game.round.publish; got %v", names(spans.Ended()))
	}
}

func createBucket(t *testing.T, store *s3.Client, bucket string) {
	t.Helper()
	// The S3 port opens before the gateway can create buckets; retry briefly.
	deadline := time.Now().Add(30 * time.Second)
	for {
		_, err := store.CreateBucket(t.Context(), &s3.CreateBucketInput{Bucket: aws.String(bucket)})
		if err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("create bucket: %v", err)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// consume returns the first record of a topic.
func consume(t *testing.T, brokers, topic string) *kgo.Record {
	t.Helper()
	cl, err := kgo.NewClient(kgo.SeedBrokers(brokers), kgo.ConsumeTopics(topic), kgo.ConsumeResetOffset(kgo.NewOffset().AtStart()))
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	for {
		fs := cl.PollFetches(ctx)
		if err := ctx.Err(); err != nil {
			t.Fatalf("no record on %s: %v", topic, err)
		}
		fs.EachError(func(_ string, _ int32, err error) {
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Logf("fetch %s: %v", topic, err)
			}
		})
		if recs := fs.Records(); len(recs) > 0 {
			return recs[0]
		}
	}
}

func headerSpan(t *testing.T, r *kgo.Record) trace.SpanContext {
	t.Helper()
	carrier := propagation.MapCarrier{}
	for _, h := range r.Headers {
		carrier[h.Key] = string(h.Value)
	}
	if carrier["traceparent"] == "" {
		t.Fatalf("%s record has no traceparent header (headers %v)", r.Topic, carrier)
	}
	sc := trace.SpanContextFromContext(propagation.TraceContext{}.Extract(context.Background(), carrier))
	if !sc.IsValid() {
		t.Fatalf("%s: bad traceparent %q", r.Topic, carrier["traceparent"])
	}
	return sc
}

func names(spans []sdktrace.ReadOnlySpan) []string {
	var out []string
	for _, s := range spans {
		out = append(out, s.Name())
	}
	return out
}
