package room

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"go.opentelemetry.io/otel/propagation"
)

// OutboxEntry is one finished round or match waiting in the room's outbox (ADR 0009). The room
// writes it in the same fenced commit as the round_ended or match_ended event, the publisher
// deletes it once Kafka has it, and a new owner publishes whatever is left.
type OutboxEntry struct {
	ID          string // the entry's stream ID, set when it is committed
	RoomID      string
	EventID     string // the event's event_id: the same on every delivery
	Kind        string // OutboxRound or OutboxMatch
	Payload     []byte // the FinishedRound or FinishedMatch, as gzipped JSON
	Traceparent string // the end span's W3C trace context, so the publish continues its trace
}

// Kinds of outbox entries.
const (
	OutboxRound = "round"
	OutboxMatch = "match"
)

// ErrOutboxPending means the room can't close yet: its outbox still has entries.
var ErrOutboxPending = errors.New("room outbox not empty")

// outboxRound is a FinishedRound as the outbox stores it. CanvasBatch leaves ReceivedAt out of
// JSON (clients don't get it), but the stroke log needs it, so the canvas goes in as
// StoredBatch, like the snapshot's. The outer Canvas shadows the embedded one.
type outboxRound struct {
	FinishedRound
	Canvas []StoredBatch
}

// EncodeOutbox builds the outbox entry of a finished round or match: CPU only, so the actor can
// do it before its commit. ctx carries the end span, recorded as the entry's traceparent.
func EncodeOutbox(ctx context.Context, roomID, eventID string, f Finished) (OutboxEntry, error) {
	e := OutboxEntry{RoomID: roomID, EventID: eventID}
	var v any
	switch {
	case f.Round != nil:
		p := outboxRound{FinishedRound: *f.Round}
		for _, b := range f.Round.Canvas {
			p.Canvas = append(p.Canvas, StoredBatch{CanvasBatch: b, ReceivedAt: b.ReceivedAt})
		}
		e.Kind, v = OutboxRound, p
	case f.Match != nil:
		e.Kind, v = OutboxMatch, *f.Match
	default:
		return OutboxEntry{}, errors.New("encode outbox: nothing finished")
	}
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if err := json.NewEncoder(zw).Encode(v); err != nil {
		return OutboxEntry{}, fmt.Errorf("encode outbox %s: %w", e.Kind, err)
	}
	if err := zw.Close(); err != nil {
		return OutboxEntry{}, fmt.Errorf("compress outbox %s: %w", e.Kind, err)
	}
	e.Payload = buf.Bytes()
	carrier := propagation.MapCarrier{}
	propagation.TraceContext{}.Inject(ctx, carrier)
	e.Traceparent = carrier["traceparent"]
	return e, nil
}

// DecodeOutbox is EncodeOutbox's inverse: the finished round or match, and a context that
// carries the end span as its remote parent.
func DecodeOutbox(e OutboxEntry) (Finished, context.Context, error) {
	zr, err := gzip.NewReader(bytes.NewReader(e.Payload))
	if err != nil {
		return Finished{}, nil, fmt.Errorf("decode outbox entry %s: %w", e.ID, err)
	}
	raw, err := io.ReadAll(zr)
	if err != nil {
		return Finished{}, nil, fmt.Errorf("decode outbox entry %s: %w", e.ID, err)
	}
	var f Finished
	switch e.Kind {
	case OutboxRound:
		var p outboxRound
		if err := json.Unmarshal(raw, &p); err != nil {
			return Finished{}, nil, fmt.Errorf("decode outbox round %s: %w", e.ID, err)
		}
		r := p.FinishedRound
		r.Canvas = nil
		for _, b := range p.Canvas {
			cb := b.CanvasBatch
			cb.ReceivedAt = b.ReceivedAt
			r.Canvas = append(r.Canvas, cb)
		}
		f.Round = &r
	case OutboxMatch:
		var m FinishedMatch
		if err := json.Unmarshal(raw, &m); err != nil {
			return Finished{}, nil, fmt.Errorf("decode outbox match %s: %w", e.ID, err)
		}
		f.Match = &m
	default:
		return Finished{}, nil, fmt.Errorf("decode outbox entry %s: unknown kind %q", e.ID, e.Kind)
	}
	ctx := propagation.TraceContext{}.Extract(context.Background(), propagation.MapCarrier{"traceparent": e.Traceparent})
	return f, ctx, nil
}
