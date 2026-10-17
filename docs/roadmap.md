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
- [x] **1.3 Auth and gateway**
  - Auth: Google OAuth with PKCE and `state`, the users table (Alembic), JWT signing and JWKS, refresh-token rotation, `/me`, and pytest tests.
  - Gateway: every route, WebSocket upgrade headers, coarse `limit_req`.
  - Web: a login page and a "logged in as" header.
  - ADR 0001 is decided (2026-10-15): each service verifies RS256 JWTs itself with `aud = doodle-api`, and the Django admin bridges Google sign-in into a session.
- [x] **1.4 CI**
  - A GitHub Actions workflow per service with path filters: lint, test and Docker build.
  - `buf lint` and `buf breaking` for `proto/`.
- [x] **1.5 Observability baseline**
  - Every skeleton exports traces, metrics and logs to the collector, using the log format in `docs/observability.md`.
  - Grafana is reachable locally.
- [x] **1.6 Docs**
  - These kickoff docs are committed, and a README explains how to run the stack.

**M1 exit criteria**

- [x] `make up` starts everything, and every service's `/healthz` returns 200 through the gateway (`make smoke`).
- [x] Google login works end to end, `/api/auth/me` returns the user, and the refresh-rotation test passes (`tests/test_refresh.py::test_refresh_rotation`; live sign-in checked 2026-10-16).
- [x] `make proto` generates code for all three languages, and CI is green for every service (PR #4: 21/21 checks, 2026-10-16; the stale-code check was shown to fail on a hand-edited file).
- [x] The trace and logs of one request are visible together in Grafana (`make telemetry-check`; checked in Grafana Explore on 2026-10-16: gateway → auth trace, "Related logs" opens the auth log line).
- [x] ADR 0001 is Accepted.

## Stage 2: Game core (Track A, Days 2–3)

- [x] **2.1 Lobby**: rooms, the game-server registry (gRPC `Heartbeat`), `AllocateRoom`, join tickets, the room list.
  - ADR 0005 is decided (2026-10-16): Ed25519 join tickets, verify-only public keys on game servers, single-use through Redis `SET NX`.
  - Game servers send heartbeats and accept rooms through a stub `AllocateRoom`; room actors and ticket checks come in 2.2. Shared JWT fixtures (`fixtures/jwt/`) are used by auth and the lobby.
- [x] **2.2 Game server core**: the room actor, WebSocket handling with tickets, the round state machine, word lists, scoring, hints, the per-player token bucket, and the JSON protocol from the design doc.
  - ADR 0002 is decided (2026-10-16): direct URLs `/ws/<server_id>/rooms/<room_id>`, mapped by the gateway. Access tokens and tickets carry the display name; `AllocateRoom` names the host.
- [x] **2.3 Web room UI**: canvas with batched strokes, guess box, scores and timer.
  - Lobby page and `/rooms/<id>` invite links; a pure reducer over server messages; strokes batched every 50 ms (`VITE_STROKE_BATCH_MS`, `?batch_ms=`) with widths in normalized units; reconnect with a fresh ticket, `last_seq` and jittered backoff. Checked live on 2026-10-16 with one signed-in browser and two scripted players, through two full matches.
- [x] **2.4 Events out**: the stroke log upload (claim check), `RoundFinished` and `MatchFinished`.
  - Decided 2026-10-16: a bounded in-memory queue that drops new events when full (2.5's Redis Stream can become the durable outbox), a fixed 4:3 canvas (800 x 600), and every round published but no match without a finished round.
- [x] **2.5 Reliability**: the Redis Stream event log with explicit `seq` IDs, snapshots, reconnect with resume, the lease and fencing token through Lua, and failover through the lobby, with two game-server instances in Compose. The humans decide ADR 0002 first.
  - ADR 0007 is decided (2026-10-16): forced takeover with fencing tokens. Write-then-broadcast, snapshots at phase changes plus in-phase events on restore, resume by replay, and a lobby reassigner. Drill: `scripts/drills/failover.py kill|pause`.
- [ ] **2.6 Shared canvas and leaderboard**: two drawers per round with own-stroke undo; a weekly leaderboard built from `MatchFinished`.
  - 2.6b, the weekly leaderboard, is done (decided 2026-10-16: rank by the sum of match scores per ISO week, names from a lobby name cache, no contract change). The lobby consumes `game.matches.v1` as group `lobby-leaderboard`, counts each match once with one Lua script (`SET NX` on the `match_id`, then `ZINCRBY`), and serves `GET /api/lobby/leaderboard`; the web app has a `/leaderboard` page. 2.6a (the shared canvas) is a separate PR.

**M2 exit criteria**

- [ ] A full match plays end to end with three or more browsers.
- [x] Test: with concurrent correct guesses, exactly one is first, and every client sees the same order (`TestConcurrentCorrectGuessesOneFirst` in `services/game-server/internal/room`, 50 runs with `-race`).
- [x] Test: guessers never receive the word before the round ends (`TestGuessersNeverSeeWordBeforeRoundEnded`: late joiner, reconnect, chat containing the word, hints; fails when a leak is planted).
- [x] Test: a client that disconnects mid-round resumes without missing events (`TestResumeReplaysExactlyTheMissedEvents` in `services/game-server/internal/room`; across a takeover, `TestRestoredRoomResumesItsPlayers` and the failover drill).
- [x] Test: after a takeover, the old owner's writes are rejected by the fencing check (`TestTakeoverFencesTheOldOwner` in `internal/roomstore`, `TestTakeoverAcrossServers` in `internal/control`, and the zombie drill).
- [x] Killing a game server mid-round moves the room to the other instance. Recovery time is recorded in the evidence log (5.1–7.8 s, target 10 s).
- [x] `RoundFinished` and `MatchFinished` appear in Kafka with trace context (`TestPublishThroughKafkaAndSeaweedFS` in `services/game-server/internal/events` against real `apache/kafka` and SeaweedFS; and a match played through the gateway, decoded with `eventctl decode`, with the full trace in Tempo).

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
- [ ] **5.2 AWS deployment**: the humans decide ADR 0003 first. Then Terraform, both S3 buckets, CloudWatch, the billing alarm and secrets, and security groups that open the internal gRPC port only between the lobby and game servers (ADR 0006).
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
| 2026-10-16 | End-to-end telemetry: one request through the gateway | Trace with gateway and auth spans in Tempo; auth log line with the same trace_id in Loki; all 7 services export metrics | `make telemetry-check` (also runs in CI's stack job), then Grafana Explore: Tempo trace → Related logs | Task 1.5 PR |
| 2026-10-16 | Dead game server detection (registry) | Stopped `game-server-1` dropped out of the registry 4 s after `docker compose stop` (TTL 6 s after the last 2 s heartbeat); `lobby.servers.healthy` went from 2 to 1 | `docker compose stop game-server-1`, polling `EXISTS lobby:server:game-server-1`, then Prometheus via Grafana | Task 2.1 PR |
| 2026-10-16 | Events out after a played match | 3 `RoundFinished` and 1 `MatchFinished`, keyed by room id, with `traceparent`. Tempo shows `game.round.end` → `game.round.publish` → `S3.PutObject` and `game.rounds.v1 publish`. Event published 25–95 ms after the round ended (the first after a start took about 1.2 s to connect to Kafka) | 3-player match over WebSockets through the gateway, `eventctl decode`, the stroke log fetched from SeaweedFS and decoded with `buf convert`, Tempo API | Task 2.4 PR |
| 2026-10-16 | Kafka outage during a match | Kafka stopped for 24 s while a full match (3 rounds) ran: the match played normally, and all 4 events were published 7 s after Kafka restarted, in order, nothing dropped | `docker compose stop kafka`, the e2e match, `start kafka`, game-server logs and `game.events.published` | Task 2.4 PR |
| 2026-10-16 | Game-server registration at cold start | First successful heartbeat about 4 s after start (8 s before capping gRPC's reconnect backoff at 2 s) | Container logs: `grpc listening` to `heartbeat recovered` | Task 2.1 PR |
| 2026-10-16 | Browser WebSocket round-trip time (ping/pong) | 2–6 ms on one machine through the gateway | The room page's ping (every 15 s then, every 5 s since the liveness check), shown next to the connection status | Task 2.3 PR |
| 2026-10-16 | Failover: `docker kill` of the game server that owns a room mid-drawing | All 3 players resumed on the other server 5.1–7.8 s after the kill (7 runs; median 6.5 s), by replay (no full `welcome`), with 0 missed or duplicate events; target 10 s | `scripts/drills/failover.py kill` against `make up` | Task 2.5 PR |
| 2026-10-16 | Zombie: `docker pause` of the owner for 9 s, then `unpause` | The lobby moved the room after its heartbeats stopped; on waking, the old owner's lease renewal failed the fencing check within 0.2 s, it sent `room_moved`, and all 3 players resumed on the new owner with 0 missed or duplicate events | `scripts/drills/failover.py pause`, plus its fencing log lines | Task 2.5 PR |
| 2026-10-16 | Web client liveness: `docker pause` of the room's owner for 30 s | Both clients noticed the silent socket about 8 s after the pause (no answer within 5 s of a ping) and resumed on the other server 8.4–10.4 s after the pause (3 runs), by replay, with no out-of-order events, long before the zombie woke up. Before, they stayed stuck until the unpause | The web app's `RoomConnection` run in Node against `make up` (2 players, a match running), token minted with the local dev key, `docker pause` / `unpause` | stage-2/web-client-liveness PR |
| 2026-10-16 | Leaderboard: replayed `MatchFinished` and catch-up | The same match published twice (`fake-events.sh -match-id`) was counted once: offset 2 `applied`, offset 3 `duplicate`, `u_bob` 100 points, not 200. On its first start the new group read the topic's 24 earlier matches in about 60 ms. The `lobby.leaderboard.apply` span continues the game server's trace: `game.match.end` → `game.match.publish` → `game.matches.v1 publish` → `game.matches.v1 receive` → `lobby.leaderboard.apply` → Redis `evalsha` | `make up` with the lobby rebuilt, `fake-events.sh` twice, `redis-cli ZSCORE`, lobby logs, the Tempo API, `lobby_leaderboard_matches_total` in Prometheus, and the page in the browser | Task 2.6b PR |
| 2026-10-16 | Game-server registration at cold start, DNS part | Before: the first successful heartbeat came 0.32–0.38 s after `grpc listening` in 6 of 10 runs and 6.0–8.0 s in 4 of 10 (every beat failed with `DeadlineExceeded` until gRPC's `_grpc_config.<host>` TXT lookup ended; Go's resolver gives up on it after 5 s). After (`grpc.WithDisableServiceConfig()`): 3–18 ms in 10 of 10 runs, with no failed beat. The roughly 4 s recorded in task 2.1 also included the lobby starting up | The game-server image (before and after) on `doodle-arena_default`, pointed at a stub `RegistryService` under a fresh network alias per run (so the upstream had nothing cached); container logs from `grpc listening` to the stub's first heartbeat; `TestFirstHeartbeatDoesNotWaitForATXTLookup` | stage-2/fix-heartbeat-cold-start PR |
