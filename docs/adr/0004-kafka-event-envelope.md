# ADR 0004: How consumers tell event types apart on a Kafka topic

- Status: Accepted
- Date: 2026-10-15
- Deciders: Bowen

## Context

Kafka message values are protobuf bytes, and protobuf bytes don't say which message type they hold. Some topics carry more than one event type: `assets.drawings.v1` has `DrawingApproved` and `DrawingRejected`, and `market.listings.v1` has `ListingCreated` and `ListingCancelled`. `EventMeta` (with `event_type`) is field 1 of every event, but a consumer can't read it before it knows which message to decode. Contracts are checked by Buf (`buf lint`, `buf breaking`) in CI.

## Options

### Option A: one wrapper message per topic, with a `oneof`

For example, `DrawingEvent { oneof event { DrawingApproved drawing_approved = 1; DrawingRejected drawing_rejected = 2; } }`. Every message on a topic is that topic's wrapper.

- Pros: the contract states exactly what can appear on each topic. Buf's breaking-change check covers it. Consumers decode once and switch on the case, the same way in Go, Java and Python. Adding an event type is an additive change (a new `oneof` case).
- Cons: one extra message type per topic, and slightly deeper nesting in generated code.

### Option B: an `event_type` Kafka header

The value is the bare event, and the consumer picks a decoder from the header.

- Pros: slightly smaller messages; topics are flexible.
- Cons: the header lives outside the contract. A producer that forgets or misspells it fails only at consumption time, and Buf can't check it.

### Schema registry (considered separately)

A registry (Confluent or Apicurio) would add schema IDs to messages and enforce compatibility at produce time. That means one more stateful service to run locally and in AWS, for checks that `buf breaking` already does in CI.

## Decision

Option A, with no schema registry: Buf is the only schema tool.

- **Every topic has a wrapper, including topics that carry one event type today** (`game.rounds.v1` has `RoundEvent` wrapping `RoundFinished`). Consumer code then looks the same for every topic, and a second event type never changes a topic's message type.
- Wrappers are named `<Entity>Event` and listed in the event catalog in `docs/design-doc.md`. The `oneof` is always named `event`. Its case names are the snake_case event names.
- `EventMeta meta = 1` stays inside each event, not in the wrapper, so an event carries its metadata wherever it goes (an outbox row, a dead-letter queue, a log line).
- The Kafka value is the serialized wrapper. Trace context travels in the `traceparent` header, as before. No other header is needed to decode a message.
- **Consumers must tolerate a wrapper whose `oneof` is unset or holds an unknown case.** That is what an older consumer sees when a newer producer adds an event type. It logs a warning with the topic, partition and offset, counts it, commits the offset and moves on. It never crashes or blocks the partition.

## Consequences

- Schema evolution is enforced only in CI (`buf breaking` against `master`), not at produce time. A producer built from an unmerged branch could publish an incompatible message to a shared environment. PR review plus CI is the guard.
- Adding an event type means editing the wrapper too, in the same PR, which keeps the event catalog and the contract in sync.
- Tools such as `kcat` show raw bytes. Debugging uses the decode helper from task 1.2 (`fake-events` / the generated code), not plain text.
- Interview angle: how the system evolves schemas without a registry, the forward-compatibility rule for unknown cases, and when a registry would be worth it (many teams, many producers per topic, or external consumers).
