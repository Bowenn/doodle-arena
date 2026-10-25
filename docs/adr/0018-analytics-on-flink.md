# ADR 0018: The analytics service on Flink

- Status: Accepted (2026-10-21)
- Date: 2026-10-21
- Deciders: Bowen (covering Will's track until he joins)

## Context

Task 6.5 builds the analytics service, which runs on Apache Flink (design doc 4.9). It has two jobs:

- **Top guesses** reads viewer guesses from `live.guesses.v1` and writes each live room's most common wrong guesses over a sliding window to `live.top-guesses.v1`, which the relays show their viewers (task 6.2).
- **Wash trading** reads `market.sales.v1` (`SaleCompleted`: drawing, seller, buyer, price, time) and flags accounts that trade among themselves to inflate prices or launder coins. The flags go to a dashboard and an alert, an M6 criterion ("Both Flink jobs run and have dashboards").

The design doc names Flink and the topics, but not how Flink runs, where its state lives, what each output promises, or what counts as wash trading. The choices are below. Each also changes the local stack, AWS (ADR 0003) and the Kafka ACL table (ADR 0013).

## Options

### 1. How Flink runs

- **A. Application mode, one Flink application with both jobs.** A custom image from the official `flink` image (Java 21) with the job jar. It runs as one JobManager and one TaskManager container locally, and the same two on ECS in AWS. The application submits both jobs.
  - Pros: real Flink (JobManager, TaskManagers, checkpoints, the web UI) and one deploy unit. It is how Flink is run on Kubernetes or ECS.
  - Cons: two more containers locally (about 1.5 GB), and two jobs share one deploy.
- **B. A session cluster plus a submit step.** A long-running JobManager and TaskManager, with jars submitted over the REST API by a one-shot container.
  - Pros: jobs can be redeployed one at a time.
  - Cons: the deployed state is "whatever was submitted last", which is harder to reproduce, and needs one more moving part.
- **C. Flink embedded in a plain Java process** (a MiniCluster).
  - Pros: one small container.
  - Cons: not how Flink runs anywhere real, so little interview value.
- **AWS for each:** Flink containers on ECS (as the other services), or Amazon Managed Service for Apache Flink. Managed is less to run but a different deployment model and cost line. Either way it is task 6.9's to deploy.

### 2. State, checkpoints and delivery

- **A. Checkpoints to object storage, at-least-once outputs.**
  - Checkpoints every 30 s with the HashMap state backend go to a bucket of analytics' own: `analytics-checkpoints` in SeaweedFS with an S3 identity of its own (task 5.5b), S3 in AWS.
  - Kafka sinks are at-least-once. Both outputs tolerate a repeat: a `TopGuesses` replaces the room's previous list, and a flag carries a deterministic `flag_id` that consumers dedupe on.
- **B. The same, with exactly-once Kafka sinks.** Transactional producers need ACLs on a `transactional.id` prefix (ADR 0013), and consumers must read committed only.
  - Pros: no duplicates downstream.
  - Cons: output appears only at each checkpoint, so top guesses would lag by up to 30 s, which defeats a live list.
- **C. No checkpoints.**
  - Pros: simplest.
  - Cons: a restart loses the windows and the wash-trading history, and the jobs restart from the latest offsets.

### 3. What counts as wash trading (job 2)

Flags are keyed by the pair of accounts, never the room or user as a metric attribute. Three rules, in order of cost:

- **R1, round trip.** The same drawing goes from A to B and back to A within 7 days.
- **R2, pair churn.** A and B trade with each other (either direction, any drawings) 3 or more times within 24 hours.
- **R3, cycle.** A drawing passes A → B → C → A within 7 days, so that no two accounts trade directly. It needs a per-drawing path in state, or `MATCH_RECOGNIZE`.

### 4. API style

- **A. Flink SQL for top guesses, the DataStream API for wash trading.** Top-N over a hopping window is a few lines of SQL (a window TVF with `ROW_NUMBER`). Wash trading needs keyed state and timers (`KeyedProcessFunction`).
  - Pros: shows both APIs, each where it fits.
  - Cons: two styles in one service.
- **B. The DataStream API for both.**
  - Pros: one style, full control.
  - Cons: more code for top-N.
- **C. SQL for both.** R3 maps to `MATCH_RECOGNIZE`.
  - Pros: the least code.
  - Cons: state and timers are hidden, which makes the job harder to explain and test.

## Decision

Bowen chose:

1. **A.** Application mode with both jobs in one Flink application, on ECS in AWS (task 6.9).
2. **A.** Checkpoints to the object store, at-least-once Kafka sinks.
3. **R1 and R3.** Round trips and cycles of three or more hops, both within 7 days. R2 (pair churn) is left out.
4. **A.** Flink SQL for top guesses, the DataStream API for wash trading.

The details, as built in task 6.5:

- **Versions.** Flink 2.2.1 on the official `flink:2.2.1-java21` image, with the Kafka connector `5.0.0-2.2`. Flink 2.3.0 is out, but no Kafka connector supports it yet.
- **One job.** The application runs as one Flink job, `analytics`, holding both pipelines, so they share a checkpoint. The job jar runs in application mode, with one JobManager and one TaskManager container.
- **Java.** It is built with a Java 21 Gradle toolchain to match the image. It compiles only the `doodle.events.v1` classes of the generated code itself, so `proto/gen/java` stays on Java 25 for the marketplace.
- **Top guesses.**
  - Event time is the relay's `guessed_at`, with 2 seconds of out-of-orderness.
  - Windows are 30 seconds long and slide every 2 seconds (a hopping window TVF), grouped by room and round, keeping the top 5 per window with `ROW_NUMBER`.
  - Each window's list becomes one `TopGuesses`, which replaces the room's previous one.
- **Checkpoints.** Every 30 seconds to the `analytics-checkpoints` bucket: through the Presto S3 filesystem (`s3p://`) to SeaweedFS with analytics' own S3 identity locally, and since task 6.9 through the Hadoop one (`s3a://`) to S3 with the ECS task role in AWS, because Presto falls back to an EC2 instance profile, which Fargate tasks don't have. The HashMap state backend is used.
- **Wash trading** (task 6.5b, decided by Bowen on 2026-10-21):
  - The job keeps, per drawing (keyed by `drawing_id`, the key of `market.sales.v1`), the sales of the last 7 days by event time (`completed_at`). Event-time timers drop each sale 7 days after it happened.
  - One check covers both rules. A sale whose buyer sold this drawing within the last 7 days closes a loop. Two hops (A to B to A) is R1, three or more is R3.
  - Each loop becomes one `WashTradeFlagged` on `analytics.flags.v1`, with a `flag_id` derived from the rule and the sale ids.
- **Restarts keep the state.** Checkpoints are retained when the job stops, and the job has a fixed id. A new JobManager starts from the newest completed checkpoint in the bucket, so a container restart (or a new ECS task) keeps the 7-day history. Without that, a JobManager without high availability would start every job empty. ZooKeeper or Kubernetes HA would be another component to run. Replaying 7 days of sales would fire the alert again after each restart.
- **Tested with real sales.** The end-to-end check creates its sales through the marketplace's own API: a drawing minted from a fake round, coins from a fake match's rewards, then listings and purchases. `dev-tools` gets no write grant on `market.sales.v1`, where a forged sale would reach the notifier's webhooks.

## Consequences

- **Kafka.**
  - analytics has its own SCRAM user and grants (`infra/kafka/init.sh`): read `live.guesses.v1` and `market.sales.v1` with group `analytics`, write `live.top-guesses.v1` and, from task 6.5b, `analytics.flags.v1`.
  - Flink keeps its Kafka offsets in its checkpoints and only commits them to the group for lag metrics.
  - `dev-tools` may also write `live.guesses.v1` locally, for `make analytics-check` and until the relays publish guesses (task 6.2).
- **Memory.** About 1.8 GB more in the local stack (JobManager 768 MB, because the SQL planner runs there in application mode; TaskManager 1 GB; set by Flink's own `*.memory.process.size`). It runs only with `PROFILES=analytics make up`; CI's stack job turns it on.
- **At-least-once.** A restart replays from the last checkpoint, so a window can be emitted twice. A `TopGuesses` replaces the last one, and a wash-trading flag will carry a deterministic id.
- **Restarts without HA.** Flink restores a checkpoint when the job restarts inside a running JobManager (a failed task or TaskManager). A new JobManager has no high-availability store, so on its own it would start the job fresh from the offsets committed to the group. Task 6.5b's entrypoint starts it from the newest retained checkpoint instead (see above). A checkpoint that can't be restored, for example after a change to the job's graph, needs a human: the runbook says how to start empty.
- **Telemetry.**
  - Flink's own OpenTelemetry metric reporter sends job metrics (checkpoints, records, restarts) and the jobs' counters over OTLP.
  - Logs are JSON through log4j's JSON template layout.
  - A trace doesn't follow an event through a window: windows mix many guesses, so there is no single parent.
- **What to watch:** checkpoint duration and failures, the `analytics` group's lag, and late guesses dropped by the watermark.
