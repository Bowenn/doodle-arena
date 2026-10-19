# Doodle Arena: observability

Logging, monitoring, tracing and alerting for every service. `CLAUDE.md` has the short checklist; this file is the full spec.

## 1. Pipeline

```mermaid
flowchart LR
  svc["Services<br/>Go, Java, Python"] --> col["OpenTelemetry Collector"]
  webapp["Web app<br/>Web Vitals, errors"] --> col
  col --> prom["Prometheus<br/>metrics"]
  col --> tempo["Tempo<br/>traces"]
  col --> loki["Loki<br/>logs"]
  aws["AWS resources"] --> cw["CloudWatch"]
  prom --> graf["Grafana<br/>dashboards, alerts"]
  tempo --> graf
  loki --> graf
  cw --> graf
  graf --> discord["Discord channel"]
```

- Every service sends all three signals with the OpenTelemetry SDK, over OTLP, to one collector.
- The collector routes metrics to Prometheus, traces to Tempo and logs to Loki. Prometheus alone stores only metrics.
- In AWS, managed resources (load balancer, databases, containers) report to CloudWatch on their own. Grafana reads CloudWatch as a data source, so every signal is visible in one UI.
- Locally, `grafana/otel-lgtm` bundles the collector, Prometheus, Tempo, Loki and Grafana in one container. It is meant for development and demos. In AWS, either run the components separately or state clearly that the demo uses this image (ADR 0003).
- Every signal carries the resource attributes `service.name`, `service.version`, `service.instance.id` and `deployment.environment`.

### How each service is wired (task 1.5)

Configuration is the standard `OTEL_*` environment, set once in `infra/docker-compose.yml`: OTLP over HTTP to `http://otel-lgtm:4318`, `OTEL_SEMCONV_STABILITY_OPT_IN=http`, a 10-second metric interval, and per service `OTEL_SERVICE_NAME` plus `OTEL_RESOURCE_ATTRIBUTES`. Without `OTEL_EXPORTER_OTLP_ENDPOINT` (unit tests, CI jobs), nothing is exported and only the JSON logs are written. Every service starts only after otel-lgtm is healthy (all backends ready, Loki included): records sent while Loki is still starting wait in the collector's retry queue for minutes, which is what made the CI check fail on a cold stack.

| Service | How | Logs to Loki |
|---|---|---|
| Go (lobby, game-server, notifier) | `internal/telemetry` (SDK with OTLP exporters, Go runtime metrics), `otelhttp` around the handlers | `slog` fans out to the JSON handler and the `otelslog` bridge |
| Python (auth, ai, assets) | `telemetry.py` (SDK plus the FastAPI or Django, SQLAlchemy, psycopg and httpx instrumentations). Called at startup, or in gunicorn's `post_fork` and Celery's `worker_process_init` for assets (the Celery instrumentation carries trace context from the sender to the task). Not `opentelemetry-instrument`, because the services' own logging config replaces root handlers and gunicorn forks after the agent starts its threads | an OTLP handler next to stdout in the logging config |
| Java (marketplace) | OpenTelemetry Java agent, pinned and checksum-verified in the Dockerfile | the agent exports logback records; trace ids come from the MDC |
| Gateway (Nginx) | `ngx_otel_module`: a server span per proxied API request, propagated upstream | no: its JSON access log (with `trace_id`) stays in stdout |

`service.instance.id` names one process, not one container: gunicorn's workers and Celery's pool processes append their own suffix (`assets-1-w1`, `assets-worker-1-p0`), because each exports its own cumulative counters, and under one id Prometheus sees a single series that keeps resetting.

Health checks and the Vite dev-server assets are not traced, except in marketplace: the Java agent has no simple path exclusion, so its container health check produces a small trace every few seconds until tail sampling arrives (Stage 6). Browser telemetry (Web Vitals, errors, fetch spans) comes with task 5.4. `make telemetry-check` proves the pipeline end to end: a request with a known trace id must show up in Tempo (gateway and auth spans) and in Loki (an auth log line with that `trace_id`), and every service must export metrics. CI runs it in `stack.yml`.

## 2. Logging

Services write one JSON object per line to stdout:

```json
{"timestamp":"2026-10-20T14:03:12.481Z","level":"info","service":"marketplace","version":"0.4.1","trace_id":"4bf92f3577b34da6a3ce929d0e0e4736","span_id":"00f067aa0ba902b7","msg":"purchase completed","drawing_id":"d_81f2","user_id":"u_19ac","listing_id":"l_77e0","latency_ms":38}
```

- Required fields: `timestamp`, `level`, `service`, `version`, `trace_id`, `span_id`, `msg`. `trace_id` and `span_id` are there whenever the line belongs to a span: a request, a gRPC call, a Kafka record, or a room's own span (a round ending, a match starting, a player's connection). Lines outside any trace (startup, shutdown, a room closing on its own) carry no trace ids.
- A few lines aren't JSON: the gateway's nginx error log (rate-limit and upstream warnings), the JVM and OpenTelemetry agent banners at marketplace start, and the Vite dev server behind `web`. Loki keeps them as plain text.
- Domain fields where relevant: `room_id`, `match_id`, `user_id`, `drawing_id`, `listing_id`, `endpoint_id`.
- Levels: `debug` (off by default), `info` (state changes), `warn` (handled problems such as retries), `error` (needs attention).
- With the `trace_id`, Grafana can jump from a log line to its trace and back.

Never log:

- Secrets, tokens, API keys, Stripe payloads that contain secrets, or email addresses.
- Individual strokes. Log once per round instead (`round finished`, with the stroke count).

The same goes for span attributes, which end up in Tempo. Query strings are the usual leak: auth replaces every query value in its server spans with `REDACTED` (`server_request_hook` in `services/auth/app/telemetry.py`), and the gateway doesn't trace `/api/auth/google/callback`, whose query holds the OAuth code and state (its span would record them as `http.target`). It sends no trace context there either, so that request's trace starts at auth.

Logs are not the audit trail. They can be sampled and they expire. The history of money and ownership lives in the marketplace's Postgres tables, as data that is never dropped.

## 3. Metrics

Every service gets RED metrics (rate, errors and duration) from OpenTelemetry's HTTP and gRPC instrumentation. The domain metrics below are added by hand. Names follow OpenTelemetry style; Prometheus converts dots to underscores and adds unit suffixes.

Every service sets `OTEL_SEMCONV_STABILITY_OPT_IN=http`, so the Go, Java and Python instrumentation all emit the stable HTTP metric names (`http.server.request.duration` and friends). Without it, the languages disagree on names and units, and one service-overview dashboard can't cover all of them.

Never put IDs such as `room_id`, `user_id` or `drawing_id` in metric attributes. Prometheus stores one time series per combination of attribute values, so thousands of rooms would create thousands of series. IDs belong in logs and traces.

| Metric | Type | Attributes | Service |
|---|---|---|---|
| `game.ws.connections` | UpDownCounter | – | game-server |
| `game.rooms.active` | UpDownCounter | `mode` | game-server |
| `game.broadcast.duration` | Histogram (ms) | `event_type` (time to marshal an event and queue it to every local socket, after the step's commit to Redis; it doesn't include the network write) | game-server |
| `game.guesses` | Counter | `result` = correct, wrong, rate_limited | game-server |
| `game.reconnects` | Counter | `result` = resumed (missed events replayed), snapshot (a full `welcome`), failed (a join with `last_seq` that the room refused or never reached, for example a full room or one that just stopped) | game-server |
| `game.room.takeovers` | Counter | – (rooms this server restored from another owner's state) | game-server |
| `game.ws.slow_client_disconnects` | Counter | – | game-server |
| `game.events.published` | Counter | `topic`, `result` = success, failed (retried until the 2-minute deadline), dropped (queue full, or shutting down). A failed or dropped event stays in its room's outbox and is queued again | game-server |
| `game.outbox.pending` | Gauge | – (outbox entries older than a minute in this server's rooms: finished rounds and matches not yet in Kafka; ADR 0009) | game-server |
| `game.outbox.requeued` | Counter | `reason` = takeover (left by the previous owner), stale (this server's, failed or dropped) | game-server |
| `game.events.queued` | UpDownCounter | – (finished rounds and matches waiting for the publisher) | game-server |
| `game.events.publish.duration` | Histogram (ms) | `topic` (for rounds it includes the stroke-log upload; retries included) | game-server |
| `auth.logins` | Counter | `result` = success, failed | auth |
| `auth.refreshes` | Counter | `result` = rotated, reuse_detected, expired, invalid | auth |
| `lobby.allocations` | Counter | `result` = success, no_servers, exhausted (every tried server full or draining), error | lobby |
| `lobby.reassignments` | Counter | `result` = moved, ended (the game server answered NOT_FOUND), failed (retried on the next pass) | lobby |
| `lobby.servers.healthy` | Gauge | – (game servers whose heartbeat arrived in the last 6 s, draining ones included) | lobby |
| `lobby.leaderboard.matches` | Counter | `result` = applied, duplicate (the match was counted before: a redelivery), skipped (an unset or unknown `oneof` case, or a malformed message) | lobby |
| `assets.pipeline.duration` | Histogram (ms) | `step` = fetch, render, upload, moderate | assets |
| `assets.jobs.oldest_age` | Gauge (s) | `queue` | assets (pool process 0 of each worker; from a `sent_at` header on every task message, 0 when the queue is empty; a missing series means no worker is running) |
| `assets.admin.sign_ins` | Counter | `result` = ok, forbidden, unauthenticated | assets |
| `assets.rounds.received` | Counter | `result` = created, duplicate, empty, skipped | assets (consumer) |
| `assets.pipeline.results` | Counter | `result` = prepared, skipped, failed | assets |
| `assets.pipeline.requeued` | Counter | – | assets (sweeper) |
| `assets.outbox.lag` | Gauge (s) | – (age of the oldest outbox event Kafka hasn't confirmed; 0 when drained) | assets (relay) |
| `assets.events.published` | Counter | `result` = success, failed (left in the outbox and retried) | assets (relay) |
| `assets.moderation` | Counter | `result` = approved, rejected, retry, manual | assets (`manual`: sent to the review queue, after the last failed call, a refusal, or an unsure AI) |
| `ai.moderation.requests` | Counter | `decision` = approve, reject, review, unavailable, invalid, abandoned (the caller left before the answer) | ai |
| `market.purchases` | Counter | `result` = success, conflict (the listing was sold or cancelled: a lost race), insufficient_funds | marketplace |
| `market.idempotency.replays` | Counter | `operation` = create_listing, purchase, checkout (requests answered from a stored response) | marketplace |
| `market.stripe.webhooks` | Counter | `result` = processed (coins credited), duplicate (the event, or another event for the same session, was handled before), ignored (an unpaid or unrelated event), unknown_session (a paid session the marketplace didn't create, or for another user), invalid_signature | marketplace |
| `market.db.lock_wait.duration` | Histogram (ms) | `operation` = ledger_post (the balance rows), purchase (the listing row, where concurrent buyers queue) | marketplace |
| `market.outbox.lag` | Gauge (s) | – (age of the oldest outbox event Kafka hasn't acknowledged; 0 when drained) | marketplace |
| `market.ledger.imbalance` | Gauge (coins) | – | marketplace |
| `market.ledger.mismatched_accounts` | Gauge | – | marketplace |
| `market.ownership.violations` | Gauge | – | marketplace |
| `market.matches.received` | Counter | `result` = paid, duplicate (the match was paid before: a redelivery), ineligible (fewer than two players), skipped (undecodable, or an unset or unknown event), failed (an event that breaks its contract, or a record Postgres refuses as data) | marketplace |
| `market.drawings.received` | Counter | `result` = minted, duplicate (the drawing was minted before: a redelivery), rejected (a `DrawingRejected`, nothing to do), skipped (undecodable, or an unset or unknown event), failed (an event that breaks its contract, or a record Postgres refuses as data) | marketplace |
| `notifier.deliveries` | Counter | `result` = success, retry (failed, will try again), dead (failed for the last time) | notifier |
| `notifier.delivery.duration` | Histogram (ms) | – (from sending to the answer or the failure) | notifier |
| `notifier.dlq.size` | Gauge | – (deliveries that went DEAD in the last 24 hours, test pings aside; for dashboards: a dead delivery is usually a developer's server being down, so no alert pages on it) | notifier |
| `notifier.queue.oldest_age` | Gauge (s) | – (how long the oldest due delivery of a live, enabled endpoint has waited for a worker; 0 when none is due. It grows when the notifier falls behind, not when a developer's server fails) | notifier |
| `notifier.events` | Counter | `result` = queued, no_subscribers, duplicate (queued before: a redelivery; or a revocation not newer than the watermark), revoked (a `CredentialsRevoked` applied), skipped (unreadable) | notifier |
| `auth.api_keys.verifications` | Counter | `result` = valid, invalid, revoked | auth |
| `auth.events.published` | Counter | `result` = success (confirmed by Kafka), failed (left to retry) | auth (relay) |
| `auth.outbox.lag` | Gauge (s) | – (age of the oldest outbox event Kafka hasn't confirmed; 0 when drained) | auth (relay) |
| `notifier.endpoints.disabled` | Counter | – (endpoints disabled because auth revoked their owner's credentials) | notifier |
| `authn.jwks.fetches` | Counter | `reason` = max_age (the cached set was 10 minutes old, or older), unknown_kid; `result` = success, failed (the last good set stays in use for up to an hour) | lobby, notifier, assets (web) |
| `web.vitals.lcp`, `web.vitals.inp`, `web.vitals.cls` | Histogram | `page` | web |
| `web.ws.rtt` | Histogram (ms) | – | web |

Kafka consumer lag per consumer group comes from the collector's Kafka metrics receiver (or a Kafka exporter).

Dashboards are JSON files in `infra/observability/dashboards/`, provisioned into Grafana:

1. Service overview: RED metrics for every service.
2. Game server: connections, rooms, broadcast latency, reconnects, takeovers.
3. Marketplace: purchases and conflicts, lock waits, Stripe webhooks, outbox lag, the two invariants.
4. Async pipeline: Kafka lag, Celery job age, webhook deliveries and the dead-letter queue.
5. Load test: the panels used while k6 runs, so screenshots can go in the README.

## 4. Tracing

- Propagate W3C trace context over HTTP, gRPC and Kafka message headers (`traceparent`). Without the Kafka header, a trace breaks at every async step.
- The showcase trace follows a purchase across languages: gateway, marketplace (Java), Postgres, the outbox, Kafka, notifier (Go), and the outgoing webhook.
- The gateway creates the root span with `ngx_otel_module`, which ships in the official `nginx` image's `-otel` variant. It starts a trace or continues the browser's `traceparent`.
- Span names: domain spans are `<service>.<operation>`, for example `market.purchase`, `lobby.quickplay` or `game.round.end` (the game server's prefix is `game`). HTTP, gRPC and database spans keep the names their instrumentation gives them, which follow the OpenTelemetry conventions, such as `POST /api/lobby/rooms/{room_id}/join`.
- Span attributes for ids use dots: `room.id`, `match.id`, `user.id`, `round.no` (log fields keep `room_id` and the like). Ids are fine on spans, never on metrics.
- Sampling:
  - Keep every trace for purchases, Stripe webhooks and rewards.
  - Sample game traffic. Trace rounds and room lifecycle events, not individual strokes.
  - Stage 6: tail sampling in the collector keeps every trace that errored or was slow.

## 5. Alerting

Principles:

- Alert on what players feel and on broken invariants, not on every possible cause.
- Every alert links to a runbook in `docs/runbooks/` (use `docs/runbooks/TEMPLATE.md`).
- Grafana alerting sends alerts to a Discord channel. `critical` means fix now; `warning` means look today.

Starting rules (tune the thresholds after the first load test):

| Alert | Condition | Severity |
|---|---|---|
| LedgerImbalance | `market.ledger.imbalance != 0` or `market.ledger.mismatched_accounts > 0` | critical |
| ReconciliationStale | `market.ledger.imbalance` missing for 5 minutes (the job stopped or can't read the database; the two invariant alerts above are blind) | warning |
| OutboxStuck | `game.outbox.pending > 0` for 10 minutes (Kafka or S3 down, or an entry that can't be published; rewards and drawings are waiting) | warning |
| OwnershipViolation | `market.ownership.violations > 0` | critical |
| SlowBroadcasts | p99 of `game.broadcast.duration` above 150 ms for 5 minutes | warning |
| HighErrorRate | Server errors above 1% of requests for 5 minutes, per service | warning |
| ConsumerLagGrowing | Kafka lag growing for 10 minutes for any consumer group | warning |
| AuthOutboxStuck | `auth.outbox.lag` above 60 s for 5 minutes (Kafka down, or `auth-relay` stopped: a credential revocation isn't reaching the notifier, so a thief's webhooks keep running) | critical |
| MarketOutboxStuck | `market.outbox.lag` above 60 s for 5 minutes (Kafka down, or the relay failing; sales and listings aren't reaching the notifier) | warning |
| DrawingEventsStuck | `assets.outbox.lag` above 60 s for 5 minutes (Kafka down, or the relay stopped; the marketplace isn't minting approved drawings) | warning |
| DrawingsFailing | `assets.pipeline.results{result=failed}` above 0 for 10 minutes (stroke logs missing or unreadable: an S3 outage longer than 7 days, or a game-server bug) | warning |
| JobsBacklog | `assets.jobs.oldest_age` above 300 s for 5 minutes, or the series missing for 5 minutes (no worker) | warning |
| WebhooksFailing | `notifier.deliveries{result!=success}` above half of all attempts for 15 minutes, across many endpoints (our sender or network is broken, not one developer's server) | warning |
| WebhookQueueStuck | `notifier.queue.oldest_age` above 300 s for 5 minutes (workers stopped or Postgres slow) | warning |
| StripeSignatureFailures | More than 5 invalid signatures in 10 minutes (a wrong secret or forged requests) | warning |
| RefreshTokenReuse | `auth.refreshes{result=reuse_detected}` above its usual rate for 10 minutes (stolen tokens, or a client that refreshes concurrently) | warning |
| GameServerDown | `lobby.servers.healthy` below the expected count for 1 minute | critical |
| AwsBudget | Spending above the budget (AWS Budgets, or a CloudWatch billing alarm; billing metrics live in `us-east-1`) | critical |

The two invariant alerts come from the marketplace's reconciliation job, which checks the ledger and ownership every minute and exports the results as gauges. `market.ledger.imbalance` is the sum of every ledger entry, in coins; `market.ledger.mismatched_accounts` counts accounts whose balance isn't the sum of their entries, user balances below zero, and accounts without a balance row. When no run has succeeded for 3 minutes, all three gauges (with `market.ownership.violations` below) stop reporting, so a dead job shows as a missing series (ReconciliationStale) instead of a reassuring 0. `market.ownership.violations` counts drawings whose ownership row and provenance disagree (design doc, section 8). After a redeploy, the old container's last value stays visible for Prometheus's 5-minute lookback as a second series with its own `host_name`; an alert in the first minutes after a deploy should be checked against the new container's series.

Each run is a `market.reconcile` span (attributes `ledger.imbalance`, `ledger.mismatched_accounts` and `ownership.violations`) under the Java agent's span for the scheduled method, with one log line: `ledger reconciled` at info, or `invariants broken` at error with up to 20 `mismatched_account_ids` and `violating_drawing_ids`.

A purchase is a `market.purchase` span (`listing.id`, `drawing.id`, `sale.id`, `user.id`; `purchase.result` for a refused one) inside the HTTP server span, with the SQL in lock order below it. The relay sends each outbox row under a `market.outbox.publish` span whose parent is the trace context stored with the row, so the agent's `market.sales.v1 publish` span and every consumer of the event join the purchase's trace.

Starting a checkout is a `market.checkout` span (`user.id`, `coin_pack.id`, `stripe.session_id`) around the Stripe API call; a webhook is a `market.stripe.webhook` span (`stripe.event_type`, `stripe.event_id`, `stripe.result`). The webhook starts a new trace (Stripe sends no `traceparent`); its `CoinsCredited` is published under it. Logs carry the session id and `user_id`, never the payload or any key.

A webhook's trace continues the event's: the notifier's `notifier.queue` span (under kotel's consumer span) stores its trace context on the delivery row, and every attempt, however much later, is a `notifier.deliver` span under it (`delivery.id`, `endpoint.id`, `event.type`, `delivery.attempt`, `http.response.status_code`, `result`). Outgoing webhooks carry no `traceparent` (trace ids stay inside). Logs carry `endpoint_id`, `delivery_id` and the event id, never secrets, keys or response bodies; an API key appears only as its prefix. A `DeliveryDead` is published by a background relay in a trace of its own; its `delivery_id` leads to the delivery's attempts and their trace.

A credential theft is one trace (ADR 0015): auth's `POST /api/auth/refresh`, its SQL (the revocation and the outbox insert), the relay's `auth.users.v1 publish` (parented by the trace context stored with the row), then the notifier's `notifier.revoke` span (`user.id`, `event.id`, `endpoints.disabled`, `result`) under kotel's consumer span. Logs carry `user_id` and the event id, never tokens.

Paying a match is a `market.reward` span (`match.id`, `reward.players`, `reward.coins`, `reward.duplicate`) under the agent's consumer span for `game.matches.v1`, so the trace starts at the game server's `game.match.end`; its `CoinsCredited` events are published under that trace by the relay.

Minting a drawing is a `market.mint` span (`drawing.id`, `match.id`, `mint.duplicate`) under the agent's Kafka consumer span, which continues the assets relay's `traceparent`: one trace runs from the game server's `game.round.end` to the mint.

## 6. Release monitoring

- `service.version` on every signal lets dashboards compare the old and new versions during a rollout.
- CI adds a deploy marker (a Grafana annotation) for every release.
- During a rolling deploy of game servers, watch `game.reconnects`. A spike means connections aren't being drained properly.
- Automatic rollback is a later improvement.

## 7. When each part is built

- Stage 1: the stack runs, every service exports traces, metrics and logs over OTLP, and logs carry `trace_id` and `span_id` (task 1.5).
- Stages 2–4: a task isn't done until its metrics from section 3 exist.
- Stage 5: dashboards, alert rules, runbooks, the Discord contact point and the AWS budget alarm.
- Stage 6: SLOs, tail sampling, deploy markers. The kill test doubles as a fire drill: the right alert must fire, and its runbook must fix the problem.
