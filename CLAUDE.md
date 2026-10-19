# Doodle Arena

<!-- Maintainer note (stripped before Claude reads this file): keep this file under ~150 lines.
Procedures belong in .claude/skills/, area-specific rules in .claude/rules/, long specs in docs/. -->

Doodle Arena is a real-time multiplayer draw-and-guess game. Each finished drawing becomes a unique item that players can trade for coins in a player marketplace.

It is a portfolio project for backend internship interviews. Every technology was chosen to cover a skill from real job postings, so depth, correctness and explainability matter more than features or polish.

## People and ownership

- Bowen: Go services (`lobby`, `game-server`, `notifier`), the web app, local infra.
- Will: Java services (`marketplace`, `analytics`).
- Python services (`auth`, `assets`, `ai`): Bowen.
- Until Will joins, Bowen covers every service, including Will's.
- Both may run sessions in parallel on different tracks. Stay inside the services of the current task. Ask before editing another track's service or a shared contract in `proto/`.

## Where things are documented

- `docs/design-doc.md`: architecture, services, data ownership, contracts, key flows, decision log. Read the relevant sections before starting any task.
- `docs/roadmap.md`: stages, tasks and milestone exit criteria. Work on one task at a time.
- `docs/observability.md`: log format, metric names, tracing rules, alerts.
- `docs/keyword-coverage.md`: which interview skill each part of the system covers.
- `docs/adr/`: decisions. An ADR with status Proposed is undecided: present the options to a human instead of deciding.
- `docs/interview-notes.md`: talking points collected after each task.
- `docs/setup.md`: tools, accounts and secrets the humans set up.

## Repo layout

```text
services/
  gateway/       Nginx config: reverse proxy, TLS, WebSocket upgrade
  auth/          Python, FastAPI: Google OAuth 2.0, JWT, API keys
  lobby/         Go: rooms, game-server registry, room allocation, leaderboard
  game-server/   Go: rounds, guessing, per-room event log, reconnect, failover
  assets/        Python, Django + Celery: render, store, moderate, list drawings; replays
  marketplace/   Java, Spring Boot: coin ledger, ownership, listings, Stripe, royalties
  notifier/      Go: signed outgoing webhooks with retries and a dead-letter queue
  ai/            Python, FastAPI: moderation (stub first), guesser and hints later
  analytics/     Java, Flink (Stage 6): live top guesses, wash-trading detection
proto/           Protobuf: gRPC services and Kafka event schemas (shared contracts)
web/             TypeScript, React + Canvas
infra/           docker-compose, observability config, Terraform (Stage 5)
scripts/         Shell scripts: dev-up, seed, migrate, fake events, load helpers
loadtest/        k6 scenarios
docs/            Design doc, roadmap, ADRs, runbooks, interview notes
```

## Commands

Keep this list accurate:

- `make up` / `make down`: start or stop the local stack (`infra/docker-compose.yml`). The first `make up` creates `.env` with random local passwords.
- `make smoke`: checks every service's `/healthz` through the gateway (`http://localhost:8080/healthz/<service>`).
- `make telemetry-check`: one request with a known trace id must reach Tempo and Loki, and every service must export metrics (Grafana: `http://localhost:3000`).
- `make pipeline-check`: one `RoundFinished` in, then an approved drawing, its image and its `DrawingApproved` out (CI's stack job runs it). Needs the AI stub to approve; in a worktree, set `ENV_FILE` to the main checkout's `.env`.
- `make test` runs every service's tests with the host toolchains; `make test s=<service>` runs one.
- `make lint [s=<service>]` (`s=ci` lints the GitHub Actions workflows, `s=secrets` scans the git history with gitleaks), `make logs s=<service>` (`s=game-server` follows both instances, `s=assets` the web process, the consumer, the Celery worker, the outbox relay and beat, `s=auth` the web process and its outbox relay), `make ps`.
- CI (`.github/workflows/`) runs the same `scripts/lint.sh` and `scripts/test.sh` per service, so `make lint s=<svc> && make test s=<svc>` predicts CI. Pin new actions to a full commit SHA with the version in a comment.
- `make proto`: lint `proto/`, regenerate Go, Java and Python code into `proto/gen/`, and build it. `make test s=proto` checks without regenerating.
- `scripts/fake-events.sh [-match-id <id>] [-dry-run]`: publish a fake `RoundFinished` and `MatchFinished` (and upload the stroke log). Read events back with `(cd tools/eventctl && go run . decode -topic <topic> -n <k>)`.
- `scripts/grant-role.sh <user_id> moderator`: give a local user a role (the user id is `id` in `/api/auth/me`); it reaches their next access token. Moderators open the Django admin at `http://localhost:8080/admin/` (Google sign-in through the web app's `/moderate`).
- `docker compose -f infra/docker-compose.yml --env-file .env exec assets python manage.py backfill_decision_events [--dry-run]`: write the missing `DrawingApproved`/`DrawingRejected` for drawings decided before the outbox existed (once, idempotent).
- `stripe listen --events checkout.session.completed,checkout.session.async_payment_succeeded --forward-to localhost:8080/api/market/stripe/webhook`: forward Stripe test-mode webhooks to the marketplace (needs the Stripe CLI and test keys in `.env`; docs/setup.md, "Stripe").
- `scripts/ai-mode.sh [approve|reject|review|fail] [delay_ms] [failure_rate]`: show or change the AI moderation stub's behaviour until the ai container restarts (for example `scripts/ai-mode.sh fail` to test the review-queue fallback).
- Drills against `make up` (they need `~/.docker/bin` on PATH; in a worktree, set `DRILL_JWT_KEY` to the main checkout's `secrets/jwt-private.pem` instead of copying it):
  - `uv run --project services/auth --with websockets python scripts/drills/failover.py kill|pause [n]`: kill or freeze the room's game server mid-round and measure the resume.
  - `uv run --project services/auth python scripts/drills/mint_tokens.py [--roles player,moderator] <name>...`: dev access tokens for drills (keep them in shell variables; never write them to a file).
  - `uv run --project services/auth --with confluent-kafka --with 'protobuf>=7.36.2,<8' python scripts/drills/webhooks.py`: an API key, a healthy and a failing webhook endpoint on the `webhook-receiver` test service (admin on `localhost:9000`), a fake `MatchFinished`; reports the verified delivery, the failing one's 8 attempts, DEAD and its `DeliveryDead` (a few minutes).
  - `node scripts/drills/web-seats.mts [room_id] [--seats n]` (Node 24): extra players running the web app's own client; with no room id, a bot-only match that exits 0 when every seat agrees.
- Planned: `make seed` (later). Don't use it before it exists.

## Ground rules

1. Humans own the architecture. If a task needs a decision the design doc doesn't cover (a new service or datastore, a different technology, a contract change), stop and ask. Record each decision as an ADR (use `/adr`).
2. Technology choices are deliberate. Never swap one out to make a task easier, for example Kafka for direct HTTP calls, Flink for a plain consumer, or Postgres for SQLite.
3. Contracts first: change `proto/` before the code that uses it. Changes are additive; never reuse or renumber a field. Regenerate with `make proto`.
4. Database per service. A service never reads or writes another service's database, bucket or Redis key prefix. Data crosses services through Kafka events, gRPC or REST. The one deliberate exception is the claim check: assets reads the stroke logs the game server puts in `round-logs` (design doc, D13).
5. Simple, readable code beats clever code. Bugs are acceptable outside the critical paths below, but the critical paths must be correct and tested.
6. Work in small steps on a branch named `stage-<n>/<task-id>-<short-name>`. Make one commit per working step, using Conventional Commits in English (for example `feat(marketplace): add purchase transaction`).
7. Write all code, comments, docs and commit messages in English.

## Correctness rules (critical paths)

- Money is integer coins (`BIGINT`), never floats. The entries of every ledger transaction sum to zero.
- A purchase moves coins and ownership in one Postgres transaction. Coins are never debited without the transfer.
- Every money-moving operation has an idempotency key protected by a `UNIQUE` constraint. A replay is a no-op that returns the original result.
- Kafka delivery is at-least-once, so every consumer is idempotent.
- The game server is authoritative for timing, scoring, answer checking and event order. Never trust client timestamps. Never send the secret word to guessers before the round ends.
- Only the current lease holder writes a room's state, and every write is checked against its fencing token.

## Telemetry (every service, every feature)

- Use the OpenTelemetry SDK and export OTLP to the collector (`OTEL_EXPORTER_OTLP_ENDPOINT`).
- Write JSON logs to stdout with `timestamp`, `level`, `service`, `version`, `trace_id`, `span_id` and `msg`, plus `room_id`, `match_id`, `user_id` or `drawing_id` where relevant.
- Never log secrets, tokens, API keys, email addresses or individual strokes.
- Propagate trace context over HTTP, gRPC and Kafka message headers.
- Use the metric names in `docs/observability.md`. IDs such as `room_id` or `user_id` are never metric attributes.

## Definition of done (every task)

- It runs in `make up`, `make test` passes (including the tests named in the roadmap's exit criteria), and CI is green.
- The telemetry rules above are met, and the task's metrics exist.
- Docs are updated when behavior, contracts or commands change.
- Finish with a summary for the humans: what changed, how to verify it, and the main design decisions and trade-offs in plain English. Append 3–5 talking points to `docs/interview-notes.md`.

## Workflow

- Start each task with `/milestone <task-id>`, for example `/milestone 2.3`. It plans first and waits for approval before writing code.
- Use plan mode for anything that touches marketplace money paths, room ownership in the game server, or `proto/`.
- When two designs are both reasonable, present both with their trade-offs and ask.
- Record measured numbers (load tests, failover times) in the evidence log at the end of `docs/roadmap.md`.

## Gotchas

- WebSocket auth uses a short-lived join ticket from the lobby, never a JWT in the URL, because URLs end up in logs.
- Nginx `limit_req` is a leaky bucket. The token bucket (per-player guess limit) lives in the game server.
- MinIO's community edition is no longer maintained. Use SeaweedFS for S3-compatible storage locally.
- Prefer official upstream Docker images (`postgres`, `redis`, `mongo`, `apache/kafka`, `grafana/otel-lgtm`, `chrislusf/seaweedfs`), pinned to a version tag. Distroless (the Go runtime) has no version tags, so it is pinned by digest.
- Run Go tests with `-race`: most game-server bugs are concurrency bugs.
- Local Postgres is published on host port 15432 (not 5432), so it doesn't clash with a Postgres installed on the host.
- SeaweedFS has no S3 lifecycle API. The 7-day expiry on `round-logs` is a SeaweedFS TTL (`fs.configure`, in `infra/seaweedfs/create-buckets.sh`); in AWS it becomes an S3 lifecycle rule.
- Go runtime images are distroless (no shell or curl), so the binary checks its own health: `/app healthcheck`.
- Loki (in otel-lgtm) stops accepting logs when its disk is over 90% full and answers "Ingester is shutting down". On CI, `stack.yml` frees runner disk first; locally, prune Docker if logs stop reaching Grafana.
- Refresh tokens rotate on every use, and a reused one revokes the whole family. Any client must refresh single-flight (see `web/src/auth.ts`); two concurrent refreshes log the user out.
- Testcontainers needs Docker's socket. `scripts/test.sh` sets `DOCKER_HOST` from the Docker context, because Docker Desktop may not create `/var/run/docker.sock`.
