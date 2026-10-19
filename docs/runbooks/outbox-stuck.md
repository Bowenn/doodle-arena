# Runbook: OutboxStuck, MarketOutboxStuck, DrawingEventsStuck, AuthOutboxStuck

- Severity: AuthOutboxStuck is critical; the others are warnings
- Dashboard: [Async pipeline, Outbox lag](http://localhost:3000/d/da-async-pipeline), [Game server, Outbox](http://localhost:3000/d/da-game-server)

## What it means

Four services write events to an outbox in the same transaction as their state change, and a relay publishes them to Kafka (ADRs 0009, 0010, 0012, 0015). The alert means events are being written but not published:

| Alert | Outbox | Relay | What waits |
|---|---|---|---|
| OutboxStuck | each room's Redis stream `room:{id}:outbox` | the game server's publisher | `RoundFinished`, `MatchFinished`: drawings and rewards |
| DrawingEventsStuck | `drawings_outboxevent` (assets' Postgres) | `assets-relay` | `DrawingApproved`/`Rejected`: minting |
| MarketOutboxStuck | `outbox` (marketplace's Postgres) | the relay inside marketplace | sales, listings, coin credits: webhooks |
| AuthOutboxStuck | `outbox` (auth's Postgres) | `auth-relay` | `CredentialsRevoked`: while it waits, a thief's webhook endpoints keep running |

Nothing is lost: the events stay in the outbox and go out once the relay recovers. The cost is delay, and for AuthOutboxStuck, a security window.

## Check

1. Is Kafka healthy? `make ps`; `docker compose -f infra/docker-compose.yml --env-file .env exec kafka /opt/kafka/bin/kafka-broker-api-versions.sh --bootstrap-server localhost:9092`. If Kafka is down, every outbox alert fires together.
2. Is the relay running? `make logs s=auth` (web and `auth-relay`), `make logs s=assets` (includes `assets-relay`), `make logs s=marketplace`, `make logs s=game-server`. Look for `publish` warnings and their errors.
3. Is one event poisoning the queue? A relay that publishes in order can stall behind one record Kafka refuses (too large, a deleted topic). The relay's warn log names the event id.
4. For the game server: S3 (SeaweedFS) must be up too, because a round's stroke log is uploaded before its `RoundFinished` is published.

## Fix

- Kafka down: bring it back (`docker compose -f infra/docker-compose.yml --env-file .env up -d kafka`); the relays retry on their own.
- Relay stopped: restart it (`auth-relay`, `assets-relay`, `marketplace` or the game server). Publishing is idempotent and consumers dedupe on `event_id`, so a republish after a crash is safe.
- AuthOutboxStuck lasting more than a few minutes: a user's credentials were revoked but their webhooks still run. Until the relay recovers, find the user in auth's outbox (`SELECT * FROM outbox WHERE published_at IS NULL`) and tell them; the notifier disables their endpoints as soon as the event arrives.
- A poison record: fix the cause (topic, size); never delete an outbox row by hand without writing down which event it was.

## Afterwards

Note how long the outbox was stuck and how many events waited. If it was Kafka, check ConsumerLagGrowing cleared too.
