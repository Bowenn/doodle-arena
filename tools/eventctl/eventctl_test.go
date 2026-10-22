package main

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/twmb/franz-go/pkg/kgo"
	"google.golang.org/protobuf/proto"

	eventsv1 "github.com/Bowenn/gap-project_private/proto/gen/go/doodle/events/v1"
	gamev1 "github.com/Bowenn/gap-project_private/proto/gen/go/doodle/game/v1"
)

var update = flag.Bool("update", false, "rewrite fixtures/golden/*.binpb")

var fixedNow = time.Date(2026, 10, 20, 14, 3, 12, 0, time.UTC)

func counterIDs() func() string {
	n := 0
	return func() string {
		n++
		return strings.Repeat(string(rune('0'+n)), 8) + "-0000-4000-8000-000000000000"
	}
}

func TestBuildBatchIsConsistent(t *testing.T) {
	b, err := buildBatch("m_fixed", fixedNow, counterIDs())
	if err != nil {
		t.Fatal(err)
	}

	rf := b.round.GetRoundFinished()
	mf := b.match.GetMatchFinished()
	if rf.GetMatchId() != "m_fixed" || mf.GetMatchId() != "m_fixed" || b.strokeLog.GetMatchId() != "m_fixed" {
		t.Errorf("match ids differ: round=%q match=%q log=%q", rf.GetMatchId(), mf.GetMatchId(), b.strokeLog.GetMatchId())
	}
	if want := "m_fixed/1.binpb.gz"; rf.GetStrokeLogKey() != want || b.strokeLogKey != want {
		t.Errorf("stroke log key = %q, want %q", rf.GetStrokeLogKey(), want)
	}
	if got, want := rf.GetStrokeCount(), uint32(len(b.strokeLog.GetStrokes())); got != want || got == 0 {
		t.Errorf("stroke_count = %d, log has %d strokes", got, want)
	}
	if !rf.GetEndedAt().AsTime().Equal(fixedNow) {
		t.Errorf("ended_at = %v, want %v", rf.GetEndedAt().AsTime(), fixedNow)
	}

	metas := []*eventsv1.EventMeta{rf.GetMeta(), mf.GetMeta()}
	types := []string{"RoundFinished", "MatchFinished"}
	for i, m := range metas {
		if m.GetEventType() != types[i] || m.GetProducer() != producer || m.GetSchemaVersion() != 1 {
			t.Errorf("meta %d = %v", i, m)
		}
	}
	if metas[0].GetEventId() == metas[1].GetEventId() {
		t.Error("both events got the same event_id")
	}
}

func TestSetMeta(t *testing.T) {
	w := &eventsv1.DrawingEvent{Event: &eventsv1.DrawingEvent_DrawingRejected{DrawingRejected: &eventsv1.DrawingRejected{DrawingId: "d_1"}}}
	meta := &eventsv1.EventMeta{EventId: "e_1", EventType: "wrong", Producer: "assets"}

	if err := setMeta(w, meta); err != nil {
		t.Fatal(err)
	}

	got := w.GetDrawingRejected().GetMeta()
	if got.GetEventId() != "e_1" || got.GetEventType() != "DrawingRejected" {
		t.Errorf("meta = %v", got)
	}
	if meta.GetEventType() != "wrong" {
		t.Error("setMeta modified the caller's EventMeta")
	}
	if err := setMeta(&eventsv1.DrawingEvent{}, meta); err != errNoEvent {
		t.Errorf("unset oneof: err = %v, want errNoEvent", err)
	}
}

func TestTopicKeys(t *testing.T) {
	tests := []struct {
		topic   string
		wrapper proto.Message
		want    string
	}{
		{"game.rounds.v1", &eventsv1.RoundEvent{Event: &eventsv1.RoundEvent_RoundFinished{RoundFinished: &eventsv1.RoundFinished{RoomId: "r_1"}}}, "r_1"},
		{"game.matches.v1", &eventsv1.MatchEvent{Event: &eventsv1.MatchEvent_MatchFinished{MatchFinished: &eventsv1.MatchFinished{RoomId: "r_2"}}}, "r_2"},
		{"assets.drawings.v1", &eventsv1.DrawingEvent{Event: &eventsv1.DrawingEvent_DrawingApproved{DrawingApproved: &eventsv1.DrawingApproved{DrawingId: "d_1"}}}, "d_1"},
		{"market.listings.v1", &eventsv1.ListingEvent{Event: &eventsv1.ListingEvent_ListingCancelled{ListingCancelled: &eventsv1.ListingCancelled{DrawingId: "d_2"}}}, "d_2"},
		{"market.sales.v1", &eventsv1.SaleEvent{Event: &eventsv1.SaleEvent_SaleCompleted{SaleCompleted: &eventsv1.SaleCompleted{DrawingId: "d_3"}}}, "d_3"},
		{"market.wallet.v1", &eventsv1.WalletEvent{Event: &eventsv1.WalletEvent_CoinsCredited{CoinsCredited: &eventsv1.CoinsCredited{UserId: "u_1"}}}, "u_1"},
		{"notifier.dlq.v1", &eventsv1.DeadLetterEvent{Event: &eventsv1.DeadLetterEvent_DeliveryDead{DeliveryDead: &eventsv1.DeliveryDead{EndpointId: "ep_1"}}}, "ep_1"},
		{"live.guesses.v1", &eventsv1.ViewerGuessEvent{Event: &eventsv1.ViewerGuessEvent_ViewerGuess{ViewerGuess: &eventsv1.ViewerGuess{RoomId: "r_3"}}}, "r_3"},
		{"live.top-guesses.v1", &eventsv1.TopGuessesEvent{Event: &eventsv1.TopGuessesEvent_TopGuesses{TopGuesses: &eventsv1.TopGuesses{RoomId: "r_4"}}}, "r_4"},
	}
	if len(tests) != len(topics) {
		t.Fatalf("test covers %d topics, registry has %d", len(tests), len(topics))
	}
	for _, tt := range tests {
		t.Run(tt.topic, func(t *testing.T) {
			tp := topics[tt.topic]
			if got, err := tp.key(tt.wrapper); err != nil || got != tt.want {
				t.Errorf("key = %q, %v; want %q", got, err, tt.want)
			}
			if _, err := tp.key(tp.newWrapper()); err == nil {
				t.Error("empty wrapper: want an error")
			}
		})
	}
}

func TestRecordsCarryKeysAndTraceparent(t *testing.T) {
	b, err := buildBatch("m_fixed", fixedNow, counterIDs())
	if err != nil {
		t.Fatal(err)
	}
	records, err := recordsFor(b, newTraceparent)
	if err != nil {
		t.Fatal(err)
	}

	wantTopics := []string{"game.rounds.v1", "game.matches.v1"}
	for i, r := range records {
		if r.Topic != wantTopics[i] || string(r.Key) != "r_demo" {
			t.Errorf("record %d: topic=%s key=%s", i, r.Topic, r.Key)
		}
		if got := headerValue(r, "traceparent"); !regexp.MustCompile(`^00-[0-9a-f]{32}-[0-9a-f]{16}-01$`).MatchString(got) {
			t.Errorf("record %d: traceparent %q is not W3C trace context", i, got)
		}
		w := topics[r.Topic].newWrapper()
		if err := proto.Unmarshal(r.Value, w); err != nil {
			t.Fatalf("record %d does not decode as its topic's wrapper: %v", i, err)
		}
		if _, err := eventOf(w); err != nil {
			t.Errorf("record %d: %v", i, err)
		}
	}
}

func TestNewUUIDIsVersion4(t *testing.T) {
	re := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	if a, b := newUUID(), newUUID(); !re.MatchString(a) || a == b {
		t.Errorf("newUUID() = %q, %q", a, b)
	}
}

func TestStrokeLogGzipRoundTrip(t *testing.T) {
	b, err := buildBatch("m_fixed", fixedNow, counterIDs())
	if err != nil {
		t.Fatal(err)
	}
	gz, err := gzipStrokeLog(b.strokeLog)
	if err != nil {
		t.Fatal(err)
	}
	got, err := gunzipStrokeLog(gz)
	if err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(got, b.strokeLog) {
		t.Error("stroke log changed in the gzip round trip")
	}
}

func TestDescribeRecordSkipsUnknownCase(t *testing.T) {
	// Field 99 stands in for an event type added by a newer producer.
	r := &kgo.Record{Topic: "assets.drawings.v1", Value: []byte{0x9a, 0x06, 0x00}}
	if got := describeRecord(topics[r.Topic], r); !strings.Contains(got, "unknown or unset event case") {
		t.Errorf("describeRecord = %q", got)
	}
}

// TestGoldenFiles pins the wire bytes of each fixture. proto/gen/python's tests decode the
// same files, which checks that Go-produced bytes decode correctly in Python.
// Regenerate after changing a fixture: go test -run TestGoldenFiles -update
func TestGoldenFiles(t *testing.T) {
	golden := map[string]proto.Message{
		"round_finished": &eventsv1.RoundEvent{},
		"match_finished": &eventsv1.MatchEvent{},
		"stroke_log":     &gamev1.StrokeLog{},
	}
	for name, m := range golden {
		t.Run(name, func(t *testing.T) {
			if err := loadFixture(name, m); err != nil {
				t.Fatal(err)
			}
			got, err := proto.MarshalOptions{Deterministic: true}.Marshal(m)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join("fixtures", "golden", name+".binpb")
			if *update {
				if err := os.WriteFile(path, got, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("%v (run with -update to create it)", err)
			}
			if !bytes.Equal(got, want) {
				t.Errorf("%s differs from the fixture; run with -update if the change is intended", path)
			}
		})
	}
}

func headerValue(r *kgo.Record, key string) string {
	for _, h := range r.Headers {
		if h.Key == key {
			return string(h.Value)
		}
	}
	return ""
}

func TestGuessRecordsAreViewerGuessesOfOneRoom(t *testing.T) {
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	records, err := guessRecords("r_9", 2, []string{"Dog!", "cat"}, now, func() string { return "id" })
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 2 {
		t.Fatalf("%d records", len(records))
	}
	var e eventsv1.ViewerGuessEvent
	if err := proto.Unmarshal(records[0].Value, &e); err != nil {
		t.Fatal(err)
	}
	g := e.GetViewerGuess()
	if string(records[0].Key) != "r_9" || records[0].Topic != "live.guesses.v1" {
		t.Fatalf("record %s key %s", records[0].Topic, records[0].Key)
	}
	// Normalized as the relays will publish them (design doc 6.4).
	if g.GetText() != "dog" || g.GetRoundNo() != 2 || !g.GetGuessedAt().AsTime().Equal(now) || g.GetMeta().GetEventType() != "ViewerGuess" {
		t.Fatalf("guess = %v", g)
	}
	if _, err := guessRecords("r_9", 1, []string{"!!!"}, now, func() string { return "id" }); err == nil {
		t.Fatal("a guess with no letters: want an error")
	}
}
