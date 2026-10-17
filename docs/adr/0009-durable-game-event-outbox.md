# ADR 0009: A durable outbox for the game server's events

- Status: Proposed
- Date: 2026-10-17
- Deciders: Bowen

## Context

At the end of a round the room actor hands a `FinishedRound` (with the canvas) to the event publisher, and at the end of a match a `FinishedMatch`. The publisher keeps them in a bounded in-memory queue (1024), uploads the round's stroke log to `round-logs`, then produces `RoundFinished` or `MatchFinished` to Kafka (design doc 4.4). The actor never waits for that I/O.

Three things lose an event today:

- **A crash or kill** of the game server loses everything still queued. The room itself survives (the next owner restores it from Redis), but its finished rounds and matches are never published.
- **A long Kafka or S3 outage** fills the queue, and new events are dropped (logged and counted) rather than blocking rooms.
- **A publish that fails** after its 2-minute deadline is counted as failed and not retried again.

Until now only the leaderboard consumed `MatchFinished`, and a missing match there costs a few points. Task 4.4 pays coin rewards from `MatchFinished`, and Stage 3 turns `RoundFinished` into tradable drawings, so a lost event becomes lost money or a lost item. The review on 2026-10-17 made this the next task, before 4.4.

Constraints that hold whatever is chosen:

- **Write, then broadcast** (ADR 0007): what players see was committed first. An event must be durable no later than the moment players see the round end, and must never exist for a round the next owner will play again.
- **The actor never waits for I/O.**
- **Only the lease holder writes a room's keys**, checked against the fencing token. Room keys share the hash tag `{id}`, so one script can touch all of them (and Redis Cluster stays possible).
- **Delivery stays at-least-once.** Consumers are already idempotent: the leaderboard by `match_id`, Stage 3 by round, 4.4 must be by `match_id`. A fixed `event_id` per event lets any consumer dedupe.
- No new datastore or technology: the game server owns Redis keys and a bucket, not a database.

## Options

### Option A: a per-room outbox stream, written in the fenced commit

The commit script that appends `round_ended` or `match_ended` also adds the finished payload (the `FinishedRound` or `FinishedMatch`, gzipped, with a fixed `event_id` and the trace context) to `room:{id}:outbox`. It is the same script call, under the same lease check, as the events players are about to see.

- The publisher works as today, but each job points at its outbox entry. It removes the entry (`XDEL`) only after S3 and Kafka confirm. A crash between the confirmation and the delete means a resend with the same `event_id`.
- Recovery:
  - **A new owner** re-queues whatever is in the room's outbox when it takes the room (the takeover already reads the room's keys).
  - **The owner itself** re-queues its rooms' entries older than a minute, so a publish that failed or was dropped is tried again for as long as the server holds the room.
  - **A room with entries left isn't closed:** the close script refuses, and the empty room retries later, keeping its lease. If its server dies, the lobby reassigns it, so a new owner publishes the entries.
- Pros:
  - The event is durable exactly when players see the round end, atomically with it, and only by the rightful owner. A fenced owner can't write one for a round the next owner will replay.
  - Recovery reuses the takeover path. Everything stays inside the room's hash slot, so it stays Cluster-safe.
- Cons:
  - The payload can be large: a round's canvas is up to 40,000 points, about 100 KB gzipped, written into Redis once per round and deleted moments later.
  - A room lives on while its outbox is stuck. A Kafka outage keeps empty rooms open, bounded by the outbox, not by players.
  - More moving parts: the re-queue loop, and close that can refuse.

### Option B: one global outbox stream with a consumer group

Every game server adds finished events to `game:outbox`, read through a Redis consumer group. Each server's publisher is a consumer: it acknowledges (`XACK`) after Kafka confirms, and the entries a crashed consumer left pending are claimed by another after 30 seconds (`XAUTOCLAIM`).

- Pros:
  - The textbook Redis Streams outbox, and recovery comes built in, independent of rooms and takeovers.
  - Simple to inspect: one stream, `XPENDING` shows what's stuck.
- Cons:
  - `game:outbox` isn't in the room's hash slot, so it can't be written in the fenced commit script (the script may only touch the keys it declares, and Cluster refuses cross-slot scripts).
  - Written right after the commit instead, it isn't atomic. A crash in between loses the event, the same flaw in a smaller window.
  - A fenced owner could add an event between its last good commit and noticing the takeover, for a round the next owner then plays again, with a different result.

### Option C: produce to Kafka before players see the round end

The actor commits, then waits for S3 and Kafka (or uses a Kafka transaction) before broadcasting `round_ended`.

- Pros: no outbox at all.
- Cons:
  - Breaks "the actor never waits for I/O": every room freezes while Kafka or S3 is slow, and a long outage stops every game.
  - A Kafka transaction doesn't cover the S3 upload.

### Option D: keep the in-memory queue, reconcile later

Keep today's publisher, and let the marketplace pay rewards from a periodic reconciliation against something the game server keeps (for example, a list of finished matches in Redis).

- Pros: no change to the game server's write path.
- Cons:
  - It moves the problem into a second, slower mechanism that the marketplace must trust, and still loses `RoundFinished`, so drawings.
  - It's the dual-write problem with extra steps.

## Decision

(Proposed: for Bowen to decide. The recommendation is Option A: it is the only option where the event is written atomically with the round's end, by the owner the fencing token allows.)

## Consequences

To be written when an option is accepted.
