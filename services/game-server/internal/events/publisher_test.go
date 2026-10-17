package events

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"log/slog"
	"slices"
	"sync"
	"testing"
	"time"

	eventsv1 "github.com/Bowenn/gap-project_private/proto/gen/go/doodle/events/v1"
	gamev1 "github.com/Bowenn/gap-project_private/proto/gen/go/doodle/game/v1"
	"github.com/Bowenn/gap-project_private/services/game-server/internal/protocol"
	"github.com/Bowenn/gap-project_private/services/game-server/internal/room"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/twmb/franz-go/pkg/kgo"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"google.golang.org/protobuf/proto"
)

var t0 = time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)

func batch(id, user string, at time.Duration, pts ...[]int) room.CanvasBatch {
	return room.CanvasBatch{UserID: user, ReceivedAt: t0.Add(at),
		StrokeBatch: protocol.StrokeBatch{StrokeID: id, Points: pts, Color: "#112233", Width: 4}}
}

func finishedRound() room.FinishedRound {
	return room.FinishedRound{
		MatchID: "m1", RoomID: "r1", RoundNo: 2, Word: "zebra", DrawerIDs: []string{"u1"}, Correct: 1,
		Canvas: []room.CanvasBatch{
			batch("s1", "u1", time.Second, []int{1, 2, 0}, []int{3, 4, 16}),
			batch("s2", "u1", 1500*time.Millisecond, []int{9, 9, 0}),
			batch("s1", "u1", 2*time.Second, []int{5, 6, 33}),
		},
		DrawStart: t0, EndedAt: t0.Add(30 * time.Second),
	}
}

func finishedMatch() room.FinishedMatch {
	return room.FinishedMatch{MatchID: "m1", RoomID: "r1", Mode: "classic", StartedAt: t0, EndedAt: t0.Add(time.Minute),
		Rounds: 2, Results: []room.PlayerResult{{UserID: "u2", Score: 100, Placement: 1}, {UserID: "u1", Score: 10, Placement: 2}}}
}

func TestStrokeLogGroupsBatchesIntoStrokes(t *testing.T) {
	l := StrokeLog(finishedRound())
	if l.GetMatchId() != "m1" || l.GetRoomId() != "r1" || l.GetRoundNo() != 2 || l.GetSchemaVersion() != 1 ||
		l.GetCanvasWidth() != 800 || l.GetCanvasHeight() != 600 || !l.GetRoundStartedAt().AsTime().Equal(t0) {
		t.Fatalf("header = %v", l)
	}
	var ids []string
	for _, s := range l.GetStrokes() {
		ids = append(ids, s.GetStrokeId())
	}
	if !slices.Equal(ids, []string{"s1", "s2"}) {
		t.Fatalf("strokes = %v, want s1 then s2 (first-seen order)", ids)
	}
	s1 := l.GetStrokes()[0]
	if len(s1.GetPoints()) != 3 || s1.GetPoints()[2].GetX() != 5 || s1.GetPoints()[2].GetTMs() != 33 {
		t.Fatalf("s1 points = %v, want both batches joined", s1.GetPoints())
	}
	if s1.GetServerReceivedMs() != 2000 || s1.GetColor() != "#112233" || s1.GetWidth() != 4 || s1.GetUserId() != "u1" {
		t.Fatalf("s1 = %v, want received at 2000 ms (its last batch)", s1)
	}
}

// fakeS3 stores objects; it fails the first failures uploads.
type fakeS3 struct {
	mu       sync.Mutex
	failures int
	calls    int
	objects  map[string][]byte
}

func (f *fakeS3) PutObject(_ context.Context, in *s3.PutObjectInput, _ ...func(*s3.Options)) (*s3.PutObjectOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.calls <= f.failures {
		return nil, errors.New("s3 unavailable")
	}
	body, _ := io.ReadAll(in.Body)
	if f.objects == nil {
		f.objects = map[string][]byte{}
	}
	f.objects[*in.Bucket+"/"+*in.Key] = body
	return &s3.PutObjectOutput{}, nil
}

// fakeKafka records produced records. block, when set, holds every ack until it is closed,
// like franz-go while no broker is reachable: the context doesn't cancel a held record.
type fakeKafka struct {
	mu       sync.Mutex
	records  []*kgo.Record
	block    chan struct{}
	err      error
	produced int // Produce calls
}

func (f *fakeKafka) Produce(_ context.Context, r *kgo.Record, promise func(*kgo.Record, error)) {
	f.mu.Lock()
	f.produced++
	f.mu.Unlock()
	go func() {
		if f.block != nil {
			<-f.block
		}
		f.mu.Lock()
		err := f.err
		if err == nil {
			f.records = append(f.records, r)
		}
		f.mu.Unlock()
		promise(r, err)
	}()
}

func (f *fakeKafka) all() []*kgo.Record {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.records)
}

type fixture struct {
	pub    *Publisher
	s3     *fakeS3
	kafka  *fakeKafka
	reader *sdkmetric.ManualReader
}

func newFixture(t *testing.T, s3f *fakeS3, k *fakeKafka, mod func(*Config)) *fixture {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	ids := 0
	cfg := Config{
		Kafka: k, S3: s3f, Bucket: "round-logs", Backoff: time.Millisecond, JobTimeout: 5 * time.Second,
		Log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		Meter: sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)).Meter("t"),
		NewID: func() string { ids++; return "ev" + string(rune('0'+ids)) },
	}
	if mod != nil {
		mod(&cfg)
	}
	p, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return &fixture{pub: p, s3: s3f, kafka: k, reader: reader}
}

// run starts the worker and stops it (draining) at the end of the test.
func (f *fixture) run(t *testing.T) {
	go f.pub.Run(context.Background())
	t.Cleanup(func() { f.pub.Close(); <-f.pub.Done() })
}

// drain publishes everything queued and waits for the worker to finish.
func (f *fixture) drain() {
	f.pub.Close()
	<-f.pub.Done()
}

// results returns game.events.published by "topic/result".
func (f *fixture) results(t *testing.T) map[string]int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := f.reader.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	got := map[string]int64{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "game.events.published" {
				continue
			}
			for _, dp := range m.Data.(metricdata.Sum[int64]).DataPoints {
				topic, _ := dp.Attributes.Value(attribute.Key("topic"))
				result, _ := dp.Attributes.Value(attribute.Key("result"))
				got[topic.AsString()+"/"+result.AsString()] = dp.Value
			}
		}
	}
	return got
}

func gunzipStrokeLog(t *testing.T, b []byte) *gamev1.StrokeLog {
	t.Helper()
	zr, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	var l gamev1.StrokeLog
	if err := proto.Unmarshal(raw, &l); err != nil {
		t.Fatal(err)
	}
	return &l
}

func TestPublishesRoundThenMatch(t *testing.T) {
	f := newFixture(t, &fakeS3{}, &fakeKafka{}, nil)
	f.pub.RoundFinished(context.Background(), finishedRound(), room.OutboxEntry{})
	f.pub.MatchFinished(context.Background(), finishedMatch(), room.OutboxEntry{})
	go f.pub.Run(context.Background())
	f.drain()

	obj, ok := f.s3.objects["round-logs/m1/2.binpb.gz"]
	if !ok {
		t.Fatalf("objects = %v", f.s3.objects)
	}
	if l := gunzipStrokeLog(t, obj); len(l.GetStrokes()) != 2 || l.GetMatchId() != "m1" {
		t.Fatalf("stored stroke log = %v", l)
	}

	recs := f.kafka.all()
	if len(recs) != 2 || recs[0].Topic != TopicRounds || recs[1].Topic != TopicMatches {
		t.Fatalf("records = %v, want the round then the match", recs)
	}
	for _, r := range recs {
		if string(r.Key) != "r1" {
			t.Fatalf("%s key = %q, want the room id", r.Topic, r.Key)
		}
	}
	var re eventsv1.RoundEvent
	if err := proto.Unmarshal(recs[0].Value, &re); err != nil {
		t.Fatal(err)
	}
	rf := re.GetRoundFinished()
	if rf.GetMeta().GetEventId() != "ev1" || rf.GetMeta().GetEventType() != "RoundFinished" || rf.GetMeta().GetProducer() != "game-server" ||
		rf.GetStrokeLogKey() != "m1/2.binpb.gz" || rf.GetStrokeCount() != 2 || rf.GetWord() != "zebra" ||
		!slices.Equal(rf.GetDrawerIds(), []string{"u1"}) || rf.GetRoundNo() != 2 || rf.GetCanvasWidth() != 800 ||
		!rf.GetEndedAt().AsTime().Equal(t0.Add(30*time.Second)) {
		t.Fatalf("RoundFinished = %v", rf)
	}
	var me eventsv1.MatchEvent
	if err := proto.Unmarshal(recs[1].Value, &me); err != nil {
		t.Fatal(err)
	}
	mf := me.GetMatchFinished()
	if mf.GetMeta().GetEventType() != "MatchFinished" || mf.GetMode() != gamev1.GameMode_GAME_MODE_CLASSIC ||
		len(mf.GetResults()) != 2 || mf.GetResults()[0].GetUserId() != "u2" || mf.GetResults()[0].GetPlacement() != 1 ||
		!mf.GetStartedAt().AsTime().Equal(t0) {
		t.Fatalf("MatchFinished = %v", mf)
	}
	if got := f.results(t); got[TopicRounds+"/success"] != 1 || got[TopicMatches+"/success"] != 1 {
		t.Fatalf("published = %v", got)
	}
}

func TestUploadIsRetriedBeforeTheRoundIsPublished(t *testing.T) {
	f := newFixture(t, &fakeS3{failures: 2}, &fakeKafka{}, nil)
	f.pub.RoundFinished(context.Background(), finishedRound(), room.OutboxEntry{})
	go f.pub.Run(context.Background())
	f.drain()
	if f.s3.calls != 3 || len(f.kafka.all()) != 1 {
		t.Fatalf("uploads = %d, records = %d; want 3 attempts, then one record", f.s3.calls, len(f.kafka.all()))
	}
}

func TestNoClaimCheckWithoutTheObject(t *testing.T) {
	f := newFixture(t, &fakeS3{failures: 1 << 30}, &fakeKafka{}, func(c *Config) { c.JobTimeout = 50 * time.Millisecond })
	f.pub.RoundFinished(context.Background(), finishedRound(), room.OutboxEntry{})
	go f.pub.Run(context.Background())
	f.drain()
	if n := len(f.kafka.all()); n != 0 {
		t.Fatalf("%d records published for a round whose stroke log never uploaded", n)
	}
	if got := f.results(t); got[TopicRounds+"/failed"] != 1 {
		t.Fatalf("published = %v, want one failed round", got)
	}
}

func TestProduceIsRetriedWithTheSameEventID(t *testing.T) {
	k := &fakeKafka{err: errors.New("broker down")}
	f := newFixture(t, &fakeS3{}, k, nil)
	f.pub.MatchFinished(context.Background(), finishedMatch(), room.OutboxEntry{})
	f.run(t)
	time.Sleep(20 * time.Millisecond) // a few failed attempts
	k.mu.Lock()
	k.err = nil
	k.mu.Unlock()
	deadline := time.Now().Add(5 * time.Second)
	for len(k.all()) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	recs := k.all()
	if len(recs) != 1 {
		t.Fatalf("records = %d", len(recs))
	}
	var me eventsv1.MatchEvent
	_ = proto.Unmarshal(recs[0].Value, &me)
	if me.GetMatchFinished().GetMeta().GetEventId() != "ev1" {
		t.Fatalf("event id = %q, want the one assigned when queued", me.GetMatchFinished().GetMeta().GetEventId())
	}
}

func TestFullQueueDropsInsteadOfBlocking(t *testing.T) {
	k := &fakeKafka{block: make(chan struct{})}
	f := newFixture(t, &fakeS3{}, k, func(c *Config) { c.QueueSize = 2 })
	f.run(t)
	f.pub.MatchFinished(context.Background(), finishedMatch(), room.OutboxEntry{}) // taken by the worker, blocked in produce
	time.Sleep(20 * time.Millisecond)
	done := make(chan struct{})
	go func() {
		for range 5 { // 2 fit in the queue, 3 are dropped
			f.pub.MatchFinished(context.Background(), finishedMatch(), room.OutboxEntry{})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("MatchFinished blocked on a full queue")
	}
	if got := f.results(t); got[TopicMatches+"/dropped"] != 3 {
		t.Fatalf("published = %v, want 3 dropped", got)
	}
	close(k.block)
	f.drain()
	if n := len(k.all()); n != 3 {
		t.Fatalf("records = %d, want the 3 that were queued", n)
	}
}

func TestShutdownDrainsTheQueueThenDropsLateWork(t *testing.T) {
	f := newFixture(t, &fakeS3{}, &fakeKafka{}, nil)
	for range 3 {
		f.pub.RoundFinished(context.Background(), finishedRound(), room.OutboxEntry{})
	}
	go f.pub.Run(context.Background())
	f.drain()
	if n := len(f.kafka.all()); n != 3 {
		t.Fatalf("records = %d, want every queued round published before Run returned", n)
	}
	f.pub.MatchFinished(context.Background(), finishedMatch(), room.OutboxEntry{})
	if got := f.results(t); got[TopicMatches+"/dropped"] != 1 {
		t.Fatalf("published = %v, want work after Close dropped", got)
	}
}

func TestKafkaOutageLongerThanTheDeadline(t *testing.T) {
	k := &fakeKafka{block: make(chan struct{})}
	defer close(k.block)
	f := newFixture(t, &fakeS3{}, k, func(c *Config) { c.JobTimeout = 50 * time.Millisecond })
	f.pub.MatchFinished(context.Background(), finishedMatch(), room.OutboxEntry{})
	f.pub.MatchFinished(context.Background(), finishedMatch(), room.OutboxEntry{})
	go f.pub.Run(context.Background())
	done := make(chan struct{})
	go func() { f.drain(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the worker hung on a record Kafka never acked")
	}
	if got := f.results(t); got[TopicMatches+"/failed"] != 2 {
		t.Fatalf("published = %v, want both failed at their deadline", got)
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.produced != 2 {
		t.Fatalf("Produce called %d times, want once per event (no retry after a timeout: the record may still be delivered)", k.produced)
	}
}

func TestCancelledRunDropsWhatIsLeft(t *testing.T) {
	k := &fakeKafka{block: make(chan struct{})}
	f := newFixture(t, &fakeS3{}, k, nil)
	f.pub.MatchFinished(context.Background(), finishedMatch(), room.OutboxEntry{})
	f.pub.MatchFinished(context.Background(), finishedMatch(), room.OutboxEntry{})
	ctx, cancel := context.WithCancel(context.Background())
	go f.pub.Run(ctx)
	f.pub.Close()
	time.Sleep(20 * time.Millisecond)
	cancel() // the shutdown deadline passed while Kafka hangs
	select {
	case <-f.pub.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("Run didn't return after its context ended")
	}
	if got := f.results(t); got[TopicMatches+"/failed"] != 1 || got[TopicMatches+"/dropped"] != 1 {
		t.Fatalf("published = %v, want the in-flight one failed and the queued one dropped", got)
	}
}
