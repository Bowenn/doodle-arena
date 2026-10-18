# ADR 0010: Publishing drawing decisions through an outbox and a relay

- Status: Accepted
- Date: 2026-10-18
- Deciders: Bowen

## Context

A moderation decision changes a drawing's status in the assets Postgres database, and the marketplace mints the drawing from `DrawingApproved` on `assets.drawings.v1` (design doc 7.6). Writing the database and then producing to Kafka is a dual write: a crash or an outage between the two leaves an approved drawing that is never minted, or an event for a decision that rolled back. Task 3.4 needs the event to exist exactly when the decision commits.

Constraints that hold whatever is chosen:

- Delivery to Kafka is at least once (the project's rule); consumers dedupe on `event_id` or `drawing_id`, and the marketplace already does with `INSERT ... ON CONFLICT DO NOTHING`.
- No new datastore or technology (no Debezium or CDC connector).
- Events of one drawing stay in order (one decision per drawing today, but keyed by `drawing_id` so it stays true later).

## Options

### Option A: a transactional outbox table and a dedicated relay process

`decide()` writes the event to `drawings_outboxevent` in the same transaction as the status change. A separate process (`assets-relay`, `manage.py relay_outbox`) polls every 200 ms, locks unpublished rows in id order with `FOR UPDATE SKIP LOCKED`, produces them with an idempotent producer (acks=all) and stamps `published_at` after Kafka confirms.

- Pros: the textbook transactional outbox; easy to explain, scale (SKIP LOCKED keeps relays apart) and alert on (`assets.outbox.lag`); Kafka trouble never touches the request or the Celery workers.
- Cons: one more container; polling costs a small query every 200 ms while idle; a crash after the ack and before the stamp republishes (at least once).

### Option B: an outbox written in the transaction, published by Celery

`decide()` writes the row and schedules a publish task on commit; Celery beat sweeps unpublished rows every few seconds.

- Pros: no new process.
- Cons: publishing then depends on the Celery broker too; ordering is looser (tasks run on any pool process); every pool process holds a Kafka producer.

### Option C: like A, woken by Postgres LISTEN/NOTIFY

- Pros: lower latency and no idle polling.
- Cons: a NOTIFY sent while the relay is down is lost, so it still needs the poll as a backstop; more moving parts for a latency the marketplace doesn't need.

## Decision

Option A, decided by Bowen on 2026-10-18 (task 3.4): the decision and its event commit together or not at all, and the relay is the one place that talks to Kafka. Measured: 15–112 ms from the decision's commit to Kafka's acknowledgement.

## Consequences

- An approval needs the drawing's image, so the event always carries `sha256`, `image_key` and `thumb_key`.
- Published rows are kept 7 days (debugging, replays) and then purged.
- Drawings decided before the outbox existed get their events from `manage.py backfill_decision_events`, once.
- The relay counts as failed every row Kafka hasn't confirmed by the stamp; a confirmation that arrives later only means that event goes out twice.
- Watch `assets.outbox.lag` (alert DrawingEventsStuck, Stage 5).
