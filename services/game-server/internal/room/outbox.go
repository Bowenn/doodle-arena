package room

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
