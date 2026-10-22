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
libs/go/         Go packages the Go services share: authn, logging, kafkaauth, and the test helpers kafkatest, redistest, tcretry
proto/           Protobuf: gRPC services and Kafka event schemas (shared contracts)
web/             TypeScript, React + Canvas
infra/           docker-compose, observability config, Terraform (Stage 5)
scripts/         Shell scripts: dev-up, seed, migrate, fake events, load helpers
loadtest/        k6 scenarios
docs/            Design doc, roadmap, ADRs, runbooks, interview notes
```

## Commands

Keep this list accurate:

- `make up` / `make down`: start or stop the local stack (`infra/docker-compose.yml`). The first `make up` creates `.env` with random local passwords. Two parts are opt-in compose profiles: `PROFILES=live make up` adds the relays (live rooms), `PROFILES=live,analytics make up` also Flink (about 1.8 GB). `make down`, `ps` and `logs` reach every profile.
- `make smoke`: checks every service's `/healthz` through the gateway (`http://localhost:8080/healthz/<service>`).
- `make telemetry-check`: one request with a known trace id must reach Tempo and Loki, and every service must export metrics (Grafana: `http://localhost:3000`).
- `make backup-check`: the AWS backup's dump and restore scripts (`infra/backup/`) against `make up`: dumps the local stores, restores into throwaway Postgres and MongoDB containers, and compares every table's rows (read-only for the stack; a few minutes with load-test replays in MongoDB).
- `make alerts-check`: Grafana loaded every dashboard, alert rule and the Discord contact point from `infra/observability/`, every panel query runs, and every rule evaluates without an error (CI's stack job runs it). Dashboards: `http://localhost:3000/dashboards`, folder Doodle Arena.
- `make loadtest` (k6, `loadtest/rooms.js`: up to 80 four-player rooms over 12 minutes) and `make loadtest-smoke` (2 rooms, a minute; CI's stack job). `GATEWAY=https://icanfly.click make loadtest` runs it against AWS (the stack's key comes from SSM). Watch the "Doodle Arena: load test" dashboard. In a worktree, set `DRILL_JWT_KEY` like the drills.
- `make loadtest-live` (k6, `loadtest/live.js`: one live room with four players and 2,000 viewers through the relays, connected at 9 a second within the gateway's per-address limit, then held 5 minutes; about 9 minutes; `VIEWERS`, `RATE` and `HOLD` override; locally k6 runs in a container on the stack's network, because Docker Desktop's proxy saturates first, and it shares the CPU with the stack and tops out near 3,000 viewers itself, so 5,000 is measured in AWS, task 6.9) and `make loadtest-live-smoke` (50 viewers; CI's stack job). The latency that counts is the relays' `relay.broadcast.latency` (game-server dashboard, Relays row).
- `make lighthouse`: Lighthouse (`@lhci/cli`, mobile, 3 runs of `/` and `/gallery`; `web/lighthouserc.json` asserts performance, LCP and CLS) against the production web image run locally; `GATEWAY=https://icanfly.click make lighthouse` audits AWS. Needs Google Chrome. Numbers go to `docs/performance.md`.
- `make pipeline-check`: one `RoundFinished` in (published as `dev-tools`), then an approved drawing, its image and its `DrawingApproved` out (CI's stack job runs it). Needs the AI stub to approve; in a worktree, set `ENV_FILE` to the main checkout's `.env`.
- `make data-acl-check`: Redis and SeaweedFS refuse each service another service's keys and buckets (one ACL user per key prefix, one S3 identity per bucket; CI's stack job runs it). In a worktree, set `ENV_FILE`.
- `make wash-check`: wash trading end to end with real sales (scripts/drills/wash_trading.py --restart): three drill users get match rewards, two drawings are minted for one of them, and listings and purchases take one round a round trip and the other a cycle, with Flink restarted in the middle; both WashTradeFlagged must come out (CI's stack job runs it). Needs `PROFILES=analytics make up` and uv; in a worktree, set `ENV_FILE` and `DRILL_JWT_KEY`.
- `make analytics-check`: viewer guesses in (as `dev-tools`), the Flink job's `TopGuesses` out (CI's stack job runs it). Needs `PROFILES=analytics make up` and Go; in a worktree, set `ENV_FILE`. Flink's web UI: `http://localhost:8081`.
- `make kafka-acl-check`: the broker refuses what each service must not do (ADR 0013): no credential, a wrong password, admin over the network, another service's topic or consumer group, creating a topic, changing retention (CI's stack job runs it). In a worktree, set `ENV_FILE` like pipeline-check.
- `docker compose -f infra/docker-compose.yml --env-file .env exec kafka kafka-admin <tool>.sh [args]`: a Kafka CLI tool as `admin`, the super user for humans, on the broker's localhost ADMIN listener (for example `kafka-acls.sh --list`). Every use raises KafkaAdminUsed, on purpose: say so in the team channel. In AWS, the same through a Session Manager shell on the data host (`sudo docker exec doodle-data-kafka-1 kafka-admin ...`).
- `make test` runs every service's tests with the host toolchains; `make test s=<service>` runs one.
- `make lint [s=<service>]` (`s=ci` lints the GitHub Actions workflows, `s=secrets` scans the git history with gitleaks, `s=observability` checks dashboards, alert rules and runbooks against `docs/observability.md`, `s=terraform` checks `infra/terraform/` offline: fmt, validate, tflint), `make logs s=<service>` (`s=game-server` follows both instances, `s=assets` the web process, the consumer, the Celery worker, the outbox relay and beat, `s=auth` the web process and its outbox relay, `s=analytics` Flink's JobManager and TaskManager), `make ps`.
- CI (`.github/workflows/`) runs the same `scripts/lint.sh` and `scripts/test.sh` per service, so `make lint s=<svc> && make test s=<svc>` predicts CI. Pin new actions to a full commit SHA with the version in a comment.
- AWS (ADR 0003, docs/setup.md "AWS"; each session costs money from `aws-up` to `aws-down`, so ask before running either): `make aws-bootstrap` (once, and after a change to the bootstrap root: state bucket, ECR, empty third-party SSM parameters, the backup bucket and the notifier's key), `make aws-push` (arm64 images tagged with the commit; clean tree), `DOMAIN=icanfly.click make aws-up` (restores the latest backup into a fresh stack, ADR 0016: `BACKUP_ID=<id>` picks one, `RESTORE=none` starts empty; open to everyone; `ALLOWED_CIDRS=<ip>/32` narrows it), `make aws-grafana` (Grafana on localhost:3300 and OTLP on localhost:14318 through Session Manager), `DOMAIN=icanfly.click make aws-check` (smoke, telemetry-check, alerts-check against AWS), `DOMAIN=icanfly.click make aws-down` (stops the writers, backs up the four Postgres databases, assets' MongoDB and the drawings to the backup bucket, then destroys the stack; a failed backup destroys nothing, and `SKIP_BACKUP=1` throws the session's data away; docs/runbooks/aws-backup.md). Needs `aws sso login --profile doodle-arena`.
- `make proto`: lint `proto/`, regenerate Go, Java and Python code into `proto/gen/`, and build it. `make test s=proto` checks without regenerating.
- `scripts/fake-events.sh [-match-id <id>] [-dry-run]`: publish a fake `RoundFinished` and `MatchFinished` (and upload the stroke log), as the `dev-tools` Kafka user from `.env` (`ENV_FILE` in a worktree). Read events back with `(cd tools/eventctl && go run . decode -topic <topic> -n <k>)`, which needs `KAFKA_DEV_TOOLS_PASSWORD` in its environment.
- `scripts/grant-role.sh <user_id> moderator`: give a local user a role (the user id is `id` in `/api/auth/me`); it reaches their next access token. Moderators open the Django admin at `http://localhost:8080/admin/` (Google sign-in through the web app's `/moderate`).
- `docker compose -f infra/docker-compose.yml --env-file .env exec assets python manage.py backfill_decision_events [--dry-run]`: write the missing `DrawingApproved`/`DrawingRejected` for drawings decided before the outbox existed (once, idempotent).
- `stripe listen --events checkout.session.completed,checkout.session.async_payment_succeeded --forward-to localhost:8080/api/market/stripe/webhook`: forward Stripe test-mode webhooks to the marketplace (needs the Stripe CLI and test keys in `.env`; docs/setup.md, "Stripe").
- `scripts/ai-mode.sh [approve|reject|review|fail] [delay_ms] [failure_rate]`: show or change the AI moderation stub's behaviour until the ai container restarts (for example `scripts/ai-mode.sh fail` to test the review-queue fallback).
- Drills against `make up` (they need `~/.docker/bin` on PATH; in a worktree, set `DRILL_JWT_KEY` to the main checkout's `secrets/jwt-private.pem` instead of copying it):
  - `uv run --project services/auth --with websockets python scripts/drills/failover.py kill|pause|stop [n]`: kill or freeze the room's game server mid-round and measure the resume; `stop` sends SIGTERM, so the server drains (task 5.5: `docker stop` locally, its ECS task in AWS with `GATEWAY` and `DRILL_JWT_PEM`, see the script), and it reports how long each player was disconnected.
  - `uv run --project services/auth python scripts/drills/mint_tokens.py [--roles player,moderator] <name>...`: dev access tokens for drills (keep them in shell variables; never write them to a file).
  - `uv run --project services/auth --with confluent-kafka --with 'protobuf>=7.36.2,<8' python scripts/drills/webhooks.py`: an API key, a healthy and a failing webhook endpoint on the `webhook-receiver` test service (admin on `localhost:9000`), a fake `MatchFinished`; reports the verified delivery, the failing one's 8 attempts, DEAD and its `DeliveryDead` (a few minutes).
  - `uv run --project services/auth --with confluent-kafka --with 'protobuf>=7.36.2,<8' python scripts/drills/credential_theft.py`: replays a rotated refresh token for a drill user and checks that the notifier disables their endpoint, refuses the old access token, and delivers again after Enable (needs `auth-relay`; in a worktree, also set `ENV_FILE` to the main checkout's `.env`).
  - `python3 scripts/drills/alerts.py [ledger|errors|kafka|wash]`: makes LedgerImbalance (a 1-coin balance mismatch on the `drill-alerts` account) and HighErrorRate (the AI stub in `fail` mode) fire, checks the notification on `webhook-receiver` (or look in Discord), and puts everything back (about 10 minutes). `kafka` (not in the default run): one `kafka-admin` call (KafkaAdminUsed) and wrong-password logins every 15 s (KafkaAuthFailures, about 6 minutes). `wash` (not in the default run): the wash-trading drill's real sales make WashTradingFlagged fire for both rules (it resolves 15 minutes later on its own).
  - `uv run --project services/auth --with websockets python scripts/drills/live_room.py [viewers]`: a live room (ADR 0017) with two bots and viewers on the relays (`relay-1`, `relay-2` with `PROFILES=live make up`); checks that viewers get every event in order, never the word before `round_ended`, resume on another relay, and that player and viewer tickets don't mix; prints how far viewers trail players. Viewers also guess (ADR 0019): wrong guesses come back as top guesses (with the analytics profile), every viewer guessing the word at once gets exactly one `viewer_correct`, and the drawer can't guess as a viewer.
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
- Record measured numbers (load tests, failover times, Lighthouse) in the evidence log at the end of `docs/roadmap.md`, and update the current numbers in `docs/performance.md`.

## Gotchas

- WebSocket auth uses a short-lived join ticket from the lobby, never a JWT in the URL, because URLs end up in logs.
- Nginx `limit_req` is a leaky bucket. The token bucket (per-player guess limit) lives in the game server.
- MinIO's community edition is no longer maintained. Use SeaweedFS for S3-compatible storage locally.
- Prefer official upstream Docker images (`postgres`, `redis`, `mongo`, `apache/kafka`, `grafana/otel-lgtm`, `chrislusf/seaweedfs`), pinned to a version tag. Distroless (the Go runtime) has no version tags, so it is pinned by digest.
- Run Go tests with `-race`: most game-server bugs are concurrency bugs.
- Local Postgres is published on host port 15432 (not 5432), so it doesn't clash with a Postgres installed on the host.
- SeaweedFS has no S3 lifecycle API. The 7-day expiry on `round-logs` is a SeaweedFS TTL (`fs.configure`, in `infra/seaweedfs/create-buckets.sh`); in AWS it becomes an S3 lifecycle rule.
- Alert rules, the contact point and dashboards are provisioned from `infra/observability/`; the UI can't save changes to them. Edit the files and restart otel-lgtm (dashboards reload on their own within 30 s). Grafana doesn't expand environment variables in alert rules, only in the contact point's settings.
- In AWS, secrets reach tasks only as environment variables: signing keys come from `JWT_PRIVATE_KEY_PEM`, `TICKET_PRIVATE_KEY_PEM` and `TICKET_PUBLIC_KEY_PEM` there, and from the `*_PATH` files locally. A task reads its SSM secrets when it starts: redeploy it after a change.
- The data host's IMDS hop limit is 1, so only otel-lgtm (on the host network) can use the instance role. Don't raise it: every bridged container could then read the data host's secrets.
- macOS runs scripts with bash 3.2: no `declare -A`, no `mapfile`.
- Go runtime images are distroless (no shell or curl), so the binary checks its own health: `/app healthcheck`.
- otel-lgtm has no memory limit. When the Docker VM runs out of memory, its OOM killer stops Tempo or the collector inside the container and Grafana keeps running; the healthcheck then turns unhealthy. Look with `docker run --rm --privileged --pid=host alpine dmesg | grep -i oom`, and restart it with `docker restart doodle-arena-otel-lgtm-1`.
- MongoDB's WiredTiger cache is capped at 1 GB in both compose files: by default it takes half the host (or Docker's VM), and a full read such as the backup's `mongodump` of every replay grew it to 2.6 GB and got it OOM-killed. The backup's Mongo tools run with `GOMEMLIMIT=256MiB` for the same reason (`infra/backup/lib.sh`).
- Loki (in otel-lgtm) stops accepting logs when its disk is over 90% full and answers "Ingester is shutting down". On CI, `stack.yml` frees runner disk first; locally, prune Docker if logs stop reaching Grafana.
- Refresh tokens rotate on every use, and a reused one revokes the whole family. Any client must refresh single-flight (see `web/src/auth.ts`); two concurrent refreshes log the user out.
- OTTL can't compare a slice to a list literal (`explicit_bounds != [..]` is always true) and doesn't parse an index on it: compare `Format("%v", [explicit_bounds])` with a string, as `infra/observability/otelcol/browser.yaml` does. Test a collector rule with crafted OTLP payloads and a debug exporter before trusting it.
- Kafka has a user per service and deny-by-default ACLs (ADR 0013). A new Kafka client, or a new topic for an existing one, needs its user and grants in `GRANTS` in `infra/kafka/init.sh` (and its password in `.env.example`, compose and `infra/terraform/stack/secrets.tf`), or the broker refuses it: look for `is Denied` in `make logs s=kafka`. Tests keep unauthenticated brokers: each client adds SASL only when `KAFKA_USERNAME` is set.
- Redis has a user per service and the default user off (`infra/redis/start.sh`): `redis-cli` needs `--user admin` with `REDISCLI_AUTH` set to `REDIS_ADMIN_PASSWORD`, and a service's new key prefix needs an ACL change. Celery's broker URL is `REDIS_URL`, never `CELERY_BROKER_URL`: Celery prefers that environment variable over its configuration, which drops the Redis user.
- SeaweedFS has an S3 identity per service (`infra/seaweedfs/entrypoint.sh`). `S3_ACCESS_KEY` and `S3_SECRET_KEY` in `.env` belong to `dev-tools` (the host tools), not to a service.
- Relays have their own Kafka user (`relay`: write `live.guesses.v1`, read `live.top-guesses.v1` without a group) and Redis user (`relay`: read `room:*`, write only `room:*:viewer-ticket:*` and `room:*:viewer-guesses`), ADR 0019. A relay change that writes anything else in Redis or Kafka needs an ACL change first.
- Only the lobby knows a room is live: it sends `live` with every `AllocateRoom`, moves included, and a live room's owner then reads its viewer guesses. A room allocated without it (an older lobby) still runs, but its viewers' correct guesses are never announced.
- Kafka has three super users: `broker` (the broker), `kafka-init` (the init job) and `admin` (humans only, no SCRAM credential, localhost-only through `kafka-admin`). The broker image (`infra/kafka/Dockerfile`) carries the OpenTelemetry agent; the AWS data host mounts the same files and jar into the official image (no buildx there).
- Kafka's data moved to `/var/lib/kafka/data/kraft` in task 5.5, so the broker formats a fresh storage with its first SCRAM credentials (`kafka-init` and `broker`); a volume from before keeps its old files unused (`docker volume rm doodle-arena_kafka-data` after `make down` reclaims them).
- Testcontainers needs Docker's socket. `scripts/test.sh` sets `DOCKER_HOST` from the Docker context, because Docker Desktop may not create `/var/run/docker.sock`.
- The whole stack with both profiles needs about 9 GB in the Docker VM: give Docker Desktop 11 GB or more (Settings, Resources). The JVMs are capped (Kafka `-Xmx384m`, marketplace `mem_limit: 1g`, Flink by its `*.memory.process.size`); otel-lgtm is not (see above).
- Flink's image runs Java 21 only, so analytics builds with a Java 21 Gradle toolchain and compiles the generated `doodle.events.v1` Java itself (`proto/gen/java` targets Java 25 for the marketplace). Flink 2.3 has no Kafka connector yet: stay on 2.2 until one ships.
- A new Flink JobManager starts the job from the newest completed checkpoint in `analytics-checkpoints` (ADR 0018; the job's `main` looks for it, since there's no HA). `ANALYTICS_RESTORE=none PROFILES=live,analytics make up` starts empty instead, and `ANALYTICS_RESTORE=<path>` starts from a savepoint. Top guesses' SQL state is dropped on restore (Flink renames SQL operators when the graph changes); never change the uids `guesses-source`, `sales-source` and `wash-trades`, or wash trading's 7 days of sales are dropped with it.
- Flink's counters are new series after every restart (a new `job_id`), and one that counts before its first export starts above 0, which `increase()` misses: count series that are new in the window with their whole value, as WashTradingFlagged does.
