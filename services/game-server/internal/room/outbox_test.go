package room

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/Bowenn/gap-project_private/services/game-server/internal/protocol"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

func TestOutboxEntryRoundTrips(t *testing.T) {
	ctx, span := sdktrace.NewTracerProvider().Tracer("t").Start(context.Background(), "game.round.end")
	span.End()
	at := time.Date(2026, 10, 17, 12, 0, 0, 123_000_000, time.UTC)
	round := FinishedRound{MatchID: "m1", RoomID: "r1", RoundNo: 2, Word: "cat", DrawerIDs: []string{"u1", "u2"}, Correct: 3,
		Canvas: []CanvasBatch{{UserID: "u1", StrokeBatch: protocol.StrokeBatch{StrokeID: "s1", Points: [][]int{{1, 2, 0}, {3, 4, 16}},
			Color: "#112233", Width: 40, Done: true}, ReceivedAt: at}},
		DrawStart: at.Add(-time.Minute), EndedAt: at}

	e, err := EncodeOutbox(ctx, "r1", "ev-1", Finished{Round: &round})
	if err != nil {
		t.Fatal(err)
	}
	if e.Kind != OutboxRound || e.EventID != "ev-1" || e.RoomID != "r1" || e.Traceparent == "" {
		t.Fatalf("entry = %+v", e)
	}
	f, got, err := DecodeOutbox(e)
	if err != nil {
		t.Fatal(err)
	}
	r := f.Round
	if r == nil || r.Word != "cat" || r.RoundNo != 2 || !slices.Equal(r.DrawerIDs, round.DrawerIDs) || r.Correct != 3 ||
		!r.EndedAt.Equal(at) || len(r.Canvas) != 1 || r.Canvas[0].StrokeID != "s1" || len(r.Canvas[0].Points) != 2 {
		t.Fatalf("decoded round = %+v", r)
	}
	// The server receive time is in the stroke log, so it must survive (CanvasBatch drops it in JSON).
	if !r.Canvas[0].ReceivedAt.Equal(at) {
		t.Fatalf("received_at = %v, want %v", r.Canvas[0].ReceivedAt, at)
	}
	if sc := trace.SpanContextFromContext(got); sc.TraceID() != span.SpanContext().TraceID() || sc.SpanID() != span.SpanContext().SpanID() {
		t.Fatal("the decoded entry doesn't continue the end span's trace")
	}

	match := FinishedMatch{MatchID: "m1", RoomID: "r1", Mode: "classic", Rounds: 3, EndedAt: at,
		Results: []PlayerResult{{UserID: "u1", Score: 120, Placement: 1}}}
	e, err = EncodeOutbox(context.Background(), "r1", "ev-2", Finished{Match: &match})
	if err != nil {
		t.Fatal(err)
	}
	f, _, err = DecodeOutbox(e)
	if err != nil || f.Match == nil || f.Match.Mode != "classic" || f.Match.Rounds != 3 || f.Match.Results[0] != match.Results[0] {
		t.Fatalf("decoded match = %+v, err %v", f.Match, err)
	}
}

// playToMatchEnd plays one round of two players to its end and lets the match end too.
func playToMatchEnd(h *harness) {
	h.join("a")
	h.join("b")
	drawer, word := h.startDrawing("a")
	h.input(drawer, stroke("s1", 2))
	guesser := map[string]string{"a": "b", "b": "a"}[drawer]
	h.input(guesser, protocol.Guess{Text: word}) // the only guesser: round 1 ends
	h.sync()
	h.room.Leave(h.conns[guesser]) // fewer than 2 players: the match ends
	h.sync()
}

// ADR 0009: each finished round or match is in the outbox from the same commit as the event
// that ends it, and the publisher gets the committed entry.
func TestFinishedRoundsGoIntoTheOutboxWithTheirCommit(t *testing.T) {
	d := &fakeDurable{}
	sink := &recordingSink{}
	h := newHarness(t, "a", DefaultSettings, nil, withDurable(d), func(r *Room) { r.events = sink })
	playToMatchEnd(h)

	together := map[string]bool{}
	for _, c := range d.commits {
		for _, pair := range [][2]string{{"round_ended", "outbox:round"}, {"match_ended", "outbox:match"}} {
			if slices.Contains(c, pair[0]) && slices.Contains(c, pair[1]) {
				together[pair[0]] = true
			}
			if slices.Contains(c, pair[1]) && !slices.Contains(c, pair[0]) {
				t.Fatalf("an outbox entry was committed without its event: %v", c)
			}
		}
	}
	if !together["round_ended"] || !together["match_ended"] {
		t.Fatalf("commits = %v, want each outbox entry with its ending event", d.commits)
	}
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if len(sink.entries) != 2 || len(d.outbox) != 2 {
		t.Fatalf("sink got %d entries, outbox has %d; want 2 and 2", len(sink.entries), len(d.outbox))
	}
	for i, e := range sink.entries {
		if e.ID != d.outbox[i].ID || e.ID == "" || e.EventID == "" || e.EventID != d.outbox[i].EventID {
			t.Fatalf("entry %d handed to the publisher = %+v, committed %+v", i, e, d.outbox[i])
		}
	}
	if sink.entries[0].EventID == sink.entries[1].EventID {
		t.Fatal("the round and the match share an event_id")
	}
	f, _, err := DecodeOutbox(d.outbox[0])
	if err != nil || f.Round == nil || f.Round.Word != sink.items[0].(FinishedRound).Word {
		t.Fatalf("the committed round = %+v (err %v), want what the sink got", f.Round, err)
	}
}

// A room whose outbox still has entries isn't closed: it stays, empty and owned, and closes
// once they are published (ADR 0009).
func TestEmptyRoomWaitsForItsOutbox(t *testing.T) {
	d := &fakeDurable{}
	h := newHarness(t, "a", DefaultSettings, nil, withDurable(d))
	playToMatchEnd(h)
	for uid, c := range h.conns {
		if uid != "" {
			h.room.Leave(c)
		}
	}
	h.advance(EmptyTTL)
	if d.closed || h.room.Summary().Closed {
		t.Fatal("the room closed with 2 unpublished entries")
	}
	select {
	case <-h.room.Done():
		t.Fatal("the room stopped with unpublished entries")
	default:
	}

	for _, e := range slices.Clone(d.outbox) {
		d.ack(e.ID)
	}
	h.sync()
	h.clock.Advance(EmptyTTL)
	select {
	case <-h.room.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the room didn't close once its outbox was empty")
	}
	if !d.closed {
		t.Fatal("the store wasn't told to close the room")
	}
}
