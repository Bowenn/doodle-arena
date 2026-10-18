# ADR 0014: Webhook delivery: who gets what, the retry queue, secrets and local testing

- Status: Accepted
- Date: 2026-10-18
- Deciders: Bowen

## Context

Task 4.7 builds the notifier (design doc 4.7): developers register webhook endpoints with an API key, and the notifier delivers `SaleCompleted` and `CoinsCredited` as signed POSTs, retries failures with full-jitter backoff (base 2 s, cap 30 minutes, 8 attempts), and sends what never succeeds to `notifier.dlq.v1`. The design doc fixed the signature, the retry policy and the SSRF rules. Four questions were open:

1. Which events an endpoint receives. The design doc said "developers subscribe to event types", not whose events. `CoinsCredited` includes Stripe purchases.
2. Where retries are scheduled.
3. How the per-endpoint signing secret is stored, given that the notifier needs its plaintext to sign.
4. How to test locally, when the test receiver runs in Docker on a private address the SSRF guard refuses.

## Options

### 1. Event scope

- **A. Only the key owner's events:** an endpoint gets events that involve its owner (seller, buyer or royalty recipient of a sale; the credited user). Pros: no player's trades or purchases leak to other developers. Cons: no public feed; a marketplace-wide integration would need a separate, deliberate design.
- **B. Every event of the subscribed types:** a public feed. Pros: simple fan-out. Cons: exposes every player's activity and Stripe purchases to anyone with an API key.

### 2. Retry scheduling

- **A. A Postgres table as the queue:** one row per (endpoint, event) with `status` and `next_attempt_at`; workers claim due rows `FOR UPDATE SKIP LOCKED`. Pros: durable; the delivery log is the same table; the same pattern as the outboxes. Cons: polling.
- **B. Kafka retry topics:** a delay topic per backoff tier. Pros: event-driven. Cons: eight tiers of topics, delays tied to the topic layout, and the log still needs a table.
- **C. A Redis sorted set:** due times as scores. Pros: fast. Cons: a second store for queue state that must agree with the Postgres log.

### 3. Secrets at rest

- **A. Encrypted (AES-256-GCM, key from the environment):** a database copy alone can't forge webhooks.
- **B. Plaintext:** simplest; anyone who reads the database can sign as Doodle Arena.

### 4. Local SSRF testing

- **A. A hostname allow-list (`NOTIFIER_ALLOWED_PRIVATE_HOSTS`):** exact names that may resolve to private addresses and use plain http; empty in production. Pros: the guard stays on everywhere else. Cons: one more setting.
- **B. A switch that turns the guard off locally:** simpler, but the guard isn't exercised locally and such a switch is easy to leave on.

## Decision

1A, 2A, 3A, 4A.

- An event's users are the seller, the buyer and the royalty recipients of a `SaleCompleted`, and the credited user of a `CoinsCredited`; each gets the event on their own live endpoints subscribed to its type.
- `deliveries` is the queue and the log: `UNIQUE (endpoint_id, source_event_id)` makes a redelivered Kafka record a no-op; a claim pushes `next_attempt_at` forward by a 60-second lease, so a crashed worker's delivery comes back; `delivery_attempts` keeps each try. A DEAD row is its own outbox: `dead_published_at` is set once Kafka acknowledged its `DeliveryDead`, whose `event_id` is derived from the delivery id so a resend dedupes.
- The secret is `whsec_` + 32 random bytes, shown once. It is sealed with AES-256-GCM under SHA-256(`NOTIFIER_SECRET_KEY`) (random text of 32+ characters: `make up` generates it locally, Secrets Manager in AWS), with a fresh nonce and the endpoint id as associated data, so a ciphertext copied onto another endpoint doesn't open.
- Only the compose service name `webhook-receiver` is allow-listed locally. The guard resolves the host at send time, refuses loopback, private, carrier-grade NAT, link-local (including 169.254.169.254), unique-local, multicast, reserved and NAT64 ranges (judging IPv4-mapped addresses by their IPv4 address, and stripping IPv6 zones first), dials the address it checked, and never follows redirects.

## Consequences

- No developer can watch another player's account through webhooks; a public market feed, if ever wanted, is a new decision.
- Delivery survives restarts and outages of everything but Postgres; a Postgres outage pauses delivery (the consumer waits instead of skipping). Polling every 500 ms costs a cheap indexed query.
- Losing `NOTIFIER_SECRET_KEY` makes every endpoint's secret unreadable: endpoints must then be registered again. Rotating the key needs a re-encryption step that doesn't exist yet.
- Webhooks are at least once: receivers dedupe on `Doodle-Event-Id`.
