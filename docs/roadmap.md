# Doodle Arena: roadmap

How to use this file:

- Stages run roughly in order. After Stage 1, two tracks run in parallel.
- Each task has an ID, such as 2.3. Start a task in Claude Code with `/milestone 2.3`.
- Each stage ends in a milestone (M1 to M6) with exit criteria. A milestone is reached when every criterion is checked.
- Tick tasks (`[x]`) as they finish, and record measured numbers in the evidence log at the end.
- The estimates assume agents write most of the code. Correctness tests and measurements take longer than code; plan for that.

## Overview

| Stage | Goal | Track | Estimate | Depends on | Milestone |
|---|---|---|---|---|---|
| 1. Foundations | Repo, local stack, contracts, auth, CI, telemetry | Both | Day 1 | – | M1 |
| 2. Game core | A playable, resilient classic mode | A | Days 2–3 | 1 | M2 |
| 3. Drawing lifecycle | Rounds become stored, moderated drawings | B | Day 4 | 1 | M3 |
| 4. Economy | Ledger, marketplace, Stripe, webhooks | B, plus A for the notifier | Days 5–6 | 1, 3 | M4 |
| 5. Production readiness | Observable, deployed, first numbers | Both | Day 7 | 2, 3, 4 | M5 (MVP) |
| 6. Scale and depth | Live-stream room, data scale, search, flash sales | A and B | Week 2 | 5 | M6 |
| 7. Extras | Optional keywords | Either | Any time after M5 | 5 | – |

Tracks:

- Track A: Bowen (Go services, web app).
- Track B: Will (Java services), once he joins. Until then Bowen covers Track B as well. The Python services (auth, assets, ai) belong to Bowen.
- Track B doesn't wait for Stage 2. Until the game server publishes real events, `scripts/fake-events.sh` (built in task 1.2) publishes `RoundFinished` and `MatchFinished` from fixtures.

## Stage 1: Foundations (both tracks, Day 1)

- [x] **1.1 Repo skeleton and local stack**
  - Monorepo layout from `CLAUDE.md`, plus `.gitignore`, `.env.example`, a `Makefile` and `scripts/dev-up.sh`.
  - Docker Compose with Postgres (four databases, one user each), Redis, MongoDB, Kafka in KRaft mode, SeaweedFS with the buckets `round-logs` and `drawings`, and `grafana/otel-lgtm`.
  - A skeleton for every week-1 service, each in its own container with a `/healthz` endpoint.
- [x] **1.2 Contracts**
  - Buf setup. `doodle.events.v1` with `EventMeta` and every week-1 event, plus `doodle.game.v1` and `doodle.lobby.v1`.
  - `make proto` generates code for Go, Java and Python. `proto/README.md` explains where it lands.
  - A script that creates the Kafka topics (for example 6 partitions each) at startup.
  - `scripts/fake-events.sh` publishes sample `RoundFinished` and `MatchFinished` events from fixtures.
- [ ] **1.3 Auth and gateway**
  - Auth: Google OAuth with PKCE and `state`, the users table (Alembic), JWT signing and JWKS, refresh-token rotation, `/me`, and pytest tests.
  - Gateway: every route, WebSocket upgrade headers, coarse `limit_req`.
  - Web: a login page and a "logged in as" header.
  - ADR 0001 is decided (2026-10-15): each service verifies RS256 JWTs itself with `aud = doodle-api`, and the Django admin bridges Google sign-in into a session.
- [ ] **1.4 CI**
  - A GitHub Actions workflow per service with path filters: lint, test and Docker build.
  - `buf lint` and `buf breaking` for `proto/`.
- [ ] **1.5 Observability baseline**
  - Every skeleton exports traces, metrics and logs to the collector, using the log format in `docs/observability.md`.
  - Grafana is reachable locally.
- [ ] **1.6 Docs**
  - These kickoff docs are committed, and a README explains how to run the stack.

**M1 exit criteria**

- [x] `make up` starts everything, and every service's `/healthz` returns 200 through the gateway (`make smoke`).
- [ ] Google login works end to end, `/api/auth/me` returns the user, and the refresh-rotation test passes.
- [ ] `make proto` generates code for all three languages, and CI is green for every service.
- [ ] The trace and logs of one request are visible together in Grafana.
- [x] ADR 0001 is Accepted.

## Stage 2: Game core (Track A, Days 2–3)

- [ ] **2.1 Lobby**: rooms, the game-server registry (gRPC `Heartbeat`), `AllocateRoom`, join tickets, the room list.
- [ ] **2.2 Game server core**: the room actor, WebSocket handling with tickets, the round state machine, word lists, scoring, hints, the per-player token bucket, and the JSON protocol from the design doc.
- [ ] **2.3 Web room UI**: canvas with batched strokes, guess box, scores and timer.
- [ ] **2.4 Events out**: the stroke log upload (claim check), `RoundFinished` and `MatchFinished`.
- [ ] **2.5 Reliability**: the Redis Stream event log with explicit `seq` IDs, snapshots, reconnect with resume, the lease and fencing token through Lua, and failover through the lobby, with two game-server instances in Compose. The humans decide ADR 0002 first.
- [ ] **2.6 Shared canvas and leaderboard**: two drawers per round with own-stroke undo; a weekly leaderboard built from `MatchFinished`.

**M2 exit criteria**

- [ ] A full match plays end to end with three or more browsers.
- [ ] Test: with concurrent correct guesses, exactly one is first, and every client sees the same order.
- [ ] Test: guessers never receive the word before the round ends.
- [ ] Test: a client that disconnects mid-round resumes without missing events.
- [ ] Test: after a takeover, the old owner's writes are rejected by the fencing check.
- [ ] Killing a game server mid-round moves the room to the other instance. Recovery time is recorded in the evidence log.
- [ ] `RoundFinished` and `MatchFinished` appear in Kafka with trace context.

## Stage 3: Drawing lifecycle (Track B, Day 4)

- [ ] **3.1 Assets skeleton**: Django project, `Drawing` model and migrations, Django admin with a moderation queue, Celery with a Redis broker.
- [ ] **3.2 Pipeline**: consume `RoundFinished`, fetch the stroke log (claim check), store the replay in MongoDB, render the PNG and thumbnail, and upload them to `drawings` under SHA-256 keys.
- [ ] **3.3 Moderation**: the AI service stub (`POST /v1/moderate`), called with a timeout and retried with backoff and jitter, with the manual review queue as the fallback.
- [ ] **3.4 Events and API**: publish `DrawingApproved` and `DrawingRejected`; REST endpoints for drawings, presigned image URLs and replays.
- [ ] **3.5 Web gallery**: a gallery and a replay viewer.

**M3 exit criteria**

- [ ] A finished round, or a fake event, becomes an approved drawing that shows up in the gallery.
- [ ] Test: replaying the same `RoundFinished` creates no duplicate drawing.
- [ ] Test: identical images are stored only once.
- [ ] Test: when the AI stub is slow or failing, the call is retried, and after the last attempt the drawing lands in the review queue.

## Stage 4: Economy (Track B, plus Track A for 4.7; Days 5–6)

- [ ] **4.1 Ledger core**: accounts, balances, transactions and entries (Flyway), system accounts, and the reconciliation job with its metrics.
- [ ] **4.2 Ownership**: mint from `DrawingApproved`, record authors, serve the ownership history.
- [ ] **4.3 Listings and purchase**: create and cancel listings, the purchase transaction, `Idempotency-Key` handling, the outbox publisher.
- [ ] **4.4 Rewards**: pay round rewards from `MatchFinished`.
- [ ] **4.5 Stripe coin packs**: Checkout, webhook signature verification, dedupe and crediting; local testing with the Stripe CLI.
- [ ] **4.6 Royalties**: the royalty split and its rounding rule.
- [ ] **4.7 Notifier and API keys** (Track A): API keys in auth; endpoint registration, signing, retries, the dead-letter queue and the delivery log in the notifier.
- [ ] **4.8 Web**: wallet, buying coins, the marketplace, "my drawings", and developer settings.

**M4 exit criteria**

- [ ] Test: 50 concurrent buyers on one listing produce exactly one new owner and 49 responses of `409`, and the ledger still sums to zero.
- [ ] Test: replaying a purchase request, a `MatchFinished` event and a Stripe webhook each has no extra effect.
- [ ] Test: concurrent transfers in opposite directions finish without a deadlock.
- [ ] Test: the royalty rounding cases pass.
- [ ] Buying a coin pack with a Stripe test card credits the wallet exactly once.
- [ ] A webhook to a failing endpoint is retried with backoff and ends in the dead-letter queue. A healthy test receiver verifies the signature.
- [ ] The reconciliation metrics show 0 imbalance and 0 ownership violations.

## Stage 5: Production readiness (both tracks, Day 7)

- [ ] **5.1 Dashboards and alerts**: dashboards as code (a service overview plus one per deep part), alert rules, a Discord contact point, and runbooks in `docs/runbooks/`.
- [ ] **5.2 AWS deployment**: the humans decide ADR 0003 first. Then Terraform, both S3 buckets, CloudWatch, the billing alarm and secrets.
- [ ] **5.3 First measurements**: a k6 load test with many small rooms, and a kill test of a game server mid-round. Record the numbers.
- [ ] **5.4 Frontend performance**: Lighthouse CI and Core Web Vitals reporting.
- [ ] **5.5 Hardening**: one deliberate refactor of agent-written code, fixing flaky tests, and a README with the architecture diagram and how to run everything.

**M5 exit criteria (MVP)**

- [ ] The stack runs in AWS. Traces, metrics and logs are visible in Grafana, and AWS resources are visible in CloudWatch.
- [ ] In a test, at least the ledger-imbalance alert and the error-rate alert fire to Discord.
- [ ] The baseline load-test and kill-test numbers are in the evidence log.
- [ ] Someone new can understand the system from the README in under five minutes.

## Stage 6: Scale and depth (week 2)

Track A:

- [ ] **6.1 Live-stream room**: a relay role that reads room streams and fans them out to viewers who watch and guess.
- [ ] **6.2 Viewer guesses**: viewer guesses go to `live.guesses.v1`, and top guesses come back from Flink. Write an ADR on how "first correct viewer" works across many relays.
- [ ] **6.3 Tipping** (with Track B): viewers tip the drawer with coins. Write an ADR on handling the hot wallet, for example batched or sharded ledger entries.
- [ ] **6.4 Experiments**: stroke batch intervals (16, 33, 50 and 100 ms) and JSON versus binary messages. Record latency and throughput.

Track B:

- [ ] **6.5 Flink jobs**: top guesses per live room; wash-trading detection.
- [ ] **6.6 Data scale**: seed millions of rows; `EXPLAIN ANALYZE` before and after adding indexes; a read replica for read-heavy endpoints with read-your-writes handling; monthly partitions for `ledger_entries`.
- [ ] **6.7 Search**: Elasticsearch for drawings and listings.
- [ ] **6.8 Limited drops**: N editions released at a set time, with a waiting room, atomic inventory and rate limits.

Shared:

- [ ] **6.9 Final measurements**: load and fault-injection runs in AWS, SLOs, tail sampling, deploy markers, and an alert fire drill with the runbooks.

**M6 exit criteria**

- [ ] One live room holds N thousand viewers with p99 broadcast latency under the target. Both numbers are recorded.
- [ ] Failover under load works, and the recovery time is recorded.
- [ ] Both Flink jobs run and have dashboards.
- [ ] The before and after numbers from query tuning are recorded.
- [ ] A flash-sale load test shows no overselling.
- [ ] In the fire drill, the right alert fires and its runbook fixes the problem.

## Stage 7: Extras (optional, any order after M5)

| Extra | Keywords it adds |
|---|---|
| Auctions, including a live in-room auction after a vote | Real-time systems, concurrency |
| Full co-op mode with per-player undo | Real-time collaboration, consistency models (CRDTs versus server ordering) |
| Ranked mode: Elo or MMR, seasons, idempotent season rewards | Matchmaking, batch jobs |
| pHash plagiarism detection | Search and retrieval |
| AI guesser, "close!" hints and an eval set | AI integration, computer vision, embeddings, LLM evaluation |
| MCP server, so an AI agent can play or browse the market | MCP, AI agents |
| A/B test of the shop layout or hint timing | Experimentation |
| Lambda thumbnails triggered by S3 uploads | Serverless, AWS Lambda |
| AI service on GCP Cloud Run; drawing backups to Azure Blob | GCP, Azure, distributed storage |
| Jenkins deployment pipeline | Jenkins |
| Kubernetes (EKS, or kind locally) | Kubernetes |

## Evidence log

Record every number you might put on a resume or mention in an interview.

| Date | What was measured | Result | How it was measured | Link (dashboard, PR, screenshot) |
|---|---|---|---|---|
| | | | | |
