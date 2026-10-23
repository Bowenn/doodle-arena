# Doodle Arena: performance

All of the measured numbers on one page: the game under load and failover (task 5.3, then re-measured after its follow-ups) and the web app (task 5.4). The evidence log at the end of `docs/roadmap.md` stays the dated record of every run, with the full method; this file has the current numbers and how they changed. When a run adds a number, add a row to the evidence log and update the table here.

## How the numbers are made

| What | Command | Where to look |
|---|---|---|
| Load: up to 80 four-player rooms over 12 minutes, two drawers each sending a stroke batch every 50 ms | `make loadtest` (local), `GATEWAY=https://icanfly.click make loadtest` (AWS) | Grafana, "Doodle Arena: load test" |
| Failover under load | `scripts/drills/failover.py kill` (local, `docker kill`), `stop` (AWS, `aws ecs stop-task`) at the 80-room plateau | The drill's own output; lobby and game-server logs |
| Lighthouse: 3 runs each of `/` and `/gallery`, mobile emulation, the median run | `make lighthouse` (the production web image, locally), `GATEWAY=https://icanfly.click make lighthouse` (AWS) | The script's table; reports in `web/.lighthouseci/reports` |
| Real users' Core Web Vitals and WebSocket round trip | The web app reports them on its own (`web/src/rum.ts`) | Grafana, "Doodle Arena: service overview", row "Web app (real users)" |

The AWS stack (ADR 0003): 15 Fargate ARM64 services at 0.25 vCPU each, one t4g.large data host, load generated from Bowen's laptop through the ALB. Locally, one laptop runs the whole stack and k6.

## 1. The game under load (task 5.3)

Stroke fan-out is the time from a drawer's stroke batch until each other player in the room has it (k6 runs all four players of a room, so one clock measures it). Ping is a WebSocket `ping` until its `pong`.

| Run | 10 rooms | 40 rooms | 80 rooms | The limit |
|---|---|---|---|---|
| Local, first run | fan-out p95 4.8 ms | 9.6 ms | 125 ms | The laptop (k6 and every container on 10 cores) |
| AWS, first run | fan-out p50 17 ms, p95 24 ms; ping p50 9 ms | p50 22 ms, p95 450 ms; ping p95 94 ms | p95 at the 10 s cap; ping p95 9 s | A game server's 0.25 vCPU: 99 to 100% from about 25 rooms |
| AWS, after the follow-ups (#48 to #52) | p95 24 ms | p95 726 ms | seconds | Still the game servers' CPU, at about 40 rooms each |

What the follow-ups changed and found:

- The first run's metrics couldn't see the bottleneck: `game.broadcast.duration` stayed at 0.1 ms throughout, because it ends when an event is queued. Two new metrics (#50) located it. `game.room.input_lag` (time an input waits in the room's inbox) p95 rose from 1 ms at 10 rooms to 773 ms at 40 and 6.7 s at 80. `game.ws.send_lag` stayed under 85 ms. So the room actors wait for CPU, not the socket writes.
- The SlowBroadcasts alert (`lag=input`, 250 ms over 5 minutes) fired in the AWS re-run, and its Discord message linked to Grafana on `localhost:3300` (#48).
- otel-lgtm under the ramp, locally: before #51, the Docker VM OOM-killed Tempo five times (up to 3.4 GiB each), because the game servers started a root trace for every room commit, so for every stroke. After #51 (no such traces, a memory limit in the collector, a stricter healthcheck), spans peaked at 234/s (about 2,000/s before) and Tempo used at most 383 MiB, with no OOM kill. In AWS, with the same guards on the data host (#52), otel-lgtm used 2 of 8 GB and never restarted, and the gateway's CPU peak dropped from 79% to 24%.
- Locally, with the game servers capped at 0.25 CPU like AWS, input lag p95 was under 1 ms up to 21 rooms and 0.9 to 2.4 s at 40.

The capacity, in one line: a game server at 0.25 vCPU holds about 20 to 40 four-player rooms of this traffic. The next step is a larger task size or more servers (Stage 6), not code.

## 2. Failover under load

| Run | Rooms moved | Players back | Target |
|---|---|---|---|
| Local, `kill`, uncapped | 39 rooms | The drill's players in 8.1 to 8.3 s (6.4 s on an idle stack) | 10 s: met |
| Local, `kill`, 0.25 CPU, one room at a time | 39 rooms from +5.8 s to +20.4 s | 27.6 s | missed |
| Local, `kill`, 0.25 CPU, parallel reassignment (#49) | 62 rooms in one pass, +7.1 s to +10.6 s | 12.9 s | missed (a saturated server) |
| AWS, `stop`, first run | 41 rooms in 7 passes; the last 36 s after the stop | The drill's players gave up at 30 s | missed |
| AWS, `stop`, after #49 | 40 rooms in 3 passes, all within 17.8 s of the shutdown | 16 to 17.5 s after the shutdown, no gap or duplicate | missed (the surviving server was saturated) |
| Local, `stop` with the drain (5.5c), idle | 1 room, moved 2.0 s after SIGTERM | 2.1 s after the stop | met |
| Local, `stop` with the drain, 80 rooms, uncapped | 41 rooms in 3 passes within 14.4 s; the first pass 0.8 s after SIGTERM | 15.8 to 19.4 s after the stop (the rooms kept running until moved) | missed (one laptop runs both servers) |
| Local, `stop` with the drain, 80 rooms, 0.25 CPU | 41 rooms within 17.5 s | disconnected for 5.3 to 7.4 s (12.9 s for a capped kill) | met, for the disconnection |
| AWS, `stop` with the drain (5.5c), 80 rooms | 41 rooms in 2 passes within 6.8 s of the SIGTERM (which ECS sent about 18 s after `stop-task`) | disconnected for 0.5 to 2.0 s (16 to 17.5 s before the drain) | met |
| AWS, rolling redeploy (Stage 5 review), 80 rooms, both game servers in one phase with the gateway and lobby | game-server-1: 31 of 41 rooms moved before its 40 s drain limit, 10 failed over; game-server-2: 41 rooms in 5.1 s | 269 room errors, resume p95 55.7 s: every player reconnected through the new gateway at once, and creates got 502s while the lobby was replaced | missed |
| AWS, rolling redeploy, 80 rooms, the other services first, then one game server at a time | game-server-1: 47 rooms in 6.0 s; game-server-2: 74 rooms in 16.4 s; no drain hit its limit | the game-server phases: 30 room errors (429s from the retries, matches short of players); the gateway and lobby phase before them: 161 creates got 502 or 503 | met for the drains; see the note below |

ECS delivered SIGTERM about 18 s after `stop-task`, so the drill's own clock (32.6 s from the call) includes that delay. The lobby notices a dead server 6 to 7 s after its last heartbeat.

A redeploy (Stage 5 review) now replaces the other services first, then one game server at a time, so each drains into the other: no drain hit its limit. Two costs remain, both for Stage 6:

- **Replacing the lobby gives 502s for a few seconds.** The gateway resolves the lobby's name at request time and caches it for 10 s (Cloud Map's record has a TTL too), so it keeps sending to the old task until that expires: 161 room creations failed in two minutes at 80 rooms.
- **One game server holds every room while the other is replaced.** At 0.25 vCPU that is two to four times its capacity (section 1): stroke fan-out p95 was 30.5 s and `ws_rtt` p95 14 s during those minutes. A third server, or a larger task for the duration of a deploy, would keep the survivor under its limit.

With the drain (task 5.5c), a stop is no longer a failover. The server takes no new rooms and tells the lobby at once, and the lobby moves its rooms while they keep running. A drained room's players are cut off only for the reconnect after `room_moved`, so the drill also reports "disconnected for". The moves are still takeovers, so they are bounded by the surviving server's CPU, as before.

## 3. The web app (task 5.4)

### Bundle

| File | Raw | On the wire (nginx gzip level 6) |
|---|---|---|
| Main bundle (`index-*.js`) | 279 KB | 85 KB (102 KB at gzip's default level 1; 279 KB before 5.4, with no compression anywhere on the path) |
| Telemetry chunk (OpenTelemetry SDK and web-vitals, loaded after the page's load event) | 81 KB | 28 KB |
| CSS | 5.5 KB | 2.2 KB |

### Lighthouse

Lighthouse 12.6.1 (`@lhci/cli` 0.15.1), mobile emulation with simulated throttling (a mid-range phone on slow 4G), the median of 3 runs. Thresholds (`web/lighthouserc.json`): performance at least 0.9, LCP at most 2.5 s, CLS at most 0.1; TBT over 200 ms and accessibility under 0.9 only warn.

| Date | Target | Page | Performance | LCP | CLS | TBT | FCP | Accessibility |
|---|---|---|---|---|---|---|---|---|
| 2026-10-20 | Local production image | `/` | 0.99 | 1.66 s | 0 | 0 ms | 1.43 s | 1.00 |
| 2026-10-20 | Local production image | `/gallery` | 0.99 | 1.66 s | 0 | 0 ms | 1.43 s | 1.00 |
| 2026-10-20 | AWS (https://icanfly.click) | `/` | 1.00 | 1.21 s | 0 | 0 ms | 1.21 s | 1.00 |
| 2026-10-20 | AWS (https://icanfly.click) | `/gallery` | 1.00 | 1.21 s | 0 | 0 ms | 1.21 s | 1.00 |

The local run serves the image AWS runs, alone, so its API calls fail: it measures the bundle, caching and compression, not the API. AWS scores a little better than local, probably because the ALB speaks HTTP/2 (the local image HTTP/1.1), which Lighthouse's simulation rewards; the server answered in 13 ms. Lighthouse has no session, so on AWS both paths show the signed-out screen (the app checks `/api/auth/refresh`, gets 401 and renders the sign-in button): these numbers are the first visit's, not a signed-in gallery full of drawings. On the wire on AWS: main bundle 86 KB (gzip), telemetry chunk 24 KB, CSS 1.9 KB, all over h2.

### Real users

The web app reports `web.vitals.lcp`, `web.vitals.inp`, `web.vitals.cls` (each by `page`) and `web.ws.rtt` to the collector through the gateway (`docs/observability.md`, "Browser telemetry"). Checked end to end locally on 2026-10-20: a real browser's gallery and lobby vitals and a room's round trips (4 to 9 ms locally) reached Prometheus.

On AWS (2026-10-20, the signed-out screen; Prometheus on the data host):

| Page | Page loads | LCP | CLS | INP |
|---|---|---|---|---|
| `lobby` (`/`) | 6 LCP, 3 CLS, 1 INP | mean 74 ms | 0 | 8 ms |
| `gallery` | 6 LCP, 3 CLS, 1 INP | mean 65 ms | 0 | 8 ms |
| `market`, `leaderboard` | 1 INP each | – | – | under 25 ms |

- The LCP and CLS came from Lighthouse's own Chrome: its page loads run the app's RUM too, without throttling, so they report the real paint (about 70 ms on a fast laptop), while Lighthouse's own report gives the simulated slow-phone value (1.2 s). A real dashboard would want to drop those, for example by user agent at the gateway; with real users they are a small share.
- The INP came from clicks in the app's browser pane. That pane reports no LCP, because Chrome records none for a page loaded in a background view.
- The p75 panels read 375 ms for LCP and 18.75 ms for INP: every sample is in the first bucket (0 to 500 ms and 0 to 25 ms), and `histogram_quantile` interpolates inside it. The bucket bounds sit around Google's thresholds, so they say "good" correctly but can't tell 70 ms from 300 ms.
- `web.ws.rtt` wasn't measured on AWS: a room needs a Google sign-in, which only a human can do.

## 4. AWS sessions

| Session | Cold `make aws-up` | `make aws-down` | Up to down |
|---|---|---|---|
| 2026-10-19, task 5.3 | 505 s (208 resources) | 296 s | 28 minutes |
| 2026-10-19, the re-measure | 506 s | 315 s | 29 minutes |
| 2026-10-20, task 5.4 (209 resources: one more security-group rule) | 515 s | 357 s | 18 minutes |
| 2026-10-20, task 5.5a (230 resources: Kafka's passwords and TLS) | 515 s | 317 s | 18 minutes |
| 2026-10-20, task 5.5c (238 resources: Redis's passwords) | 530 s | 325 s | 30 minutes (with the load test) |

Cost (Cost Explorer, 2026-10-20): about $0.30 to $0.80 per stack-hour, mostly the NAT gateway, then ECS, the ALB and EC2. Credits covered all of it so far; the domain ($15 plus tax) was the only charge. Cost Explorer lags up to a day, so the latest sessions may show more later.

## 5. Backup and restore (task 6.10)

What `aws-down` adds before the destroy and `aws-up` before the migrations (ADR 0016). The recovery point is the last completed `aws-down`.

| Where | Data | Dump (on the host) | Restore (load and count check) |
|---|---|---|---|
| Local, `make backup-check`, 2026-10-21 | 4 Postgres databases, 81,607 rows, 5.6 MB of dumps; MongoDB 6,978 replays, 2.5 GB in the collection, a 683 MB gzipped archive (load tests) | Postgres 0.5 to 1.0 s each; MongoDB 172 s | 42 s for all five |
| AWS | Not measured yet: the first AWS session with task 6.10 records the backup's and the restore's time here | | |

MongoDB is most of it, and it grows with every match (a replay averages 360 KB). A full read for the dump pulls the whole collection through WiredTiger's cache, which is why MongoDB's cache is capped at 1 GB on both stacks (without the cap, the local `mongod` grew to 2.6 GB and was OOM-killed).

## 6. The live room (task 6.1c)

One live room with four players (two drawers, a stroke batch every 50 ms: about 40 events a second) and viewers through two relays (ADR 0017), from `make loadtest-live`. The target is p99 broadcast latency under 250 ms: the relays' `relay.broadcast.latency`, from the owner's commit until the write to a viewer's socket returns. "Viewer p99" is k6's `viewer_fanout`, from the drawer's send until k6 has the message, which also counts k6's own delay in reading.

Local runs on one laptop (Docker Desktop, 10 cores and 8 GB for the VM; the relays and the gateway uncapped), 2026-10-21, numbers over the hold, after every viewer connected:

| Viewers | k6 | Relays' p50 / p95 / p99 | Stream lag p99 | Viewer p99 | Gaps |
|---|---|---|---|---|---|
| 2,000 | in the stack's network | 12.6 / 71 / **214 ms** | 96 ms | 660 ms | 0 |
| 2,500 (best of four) | in the stack's network | 9.3 / – / 49 ms | 33 ms | 206 ms | 0 |
| 2,500 (the other three) | in the stack's network | 20–37 / 200–460 / 445–990 ms | 237–700 ms | 1.5–17 s | 0 |
| 2,650, then stopped | on the host | p99 at the 10 s bucket cap | about 2.5 s | p95 18 s | 0 |

What limits it here, in the order it was found:

- **Docker Desktop's proxy between macOS and its VM.** It saturated at about 2,000 viewers (80,000 messages a second). The players' own stroke fan-out, which never touches a relay, went from 4 ms to 1.45 s, while a relay's CPU profile showed under one core busy, half of it in write syscalls. With k6 in a container on the stack's network (as browsers reach the load balancer in AWS), the same load fits.
- **The load generator.** k6 needs 2 to 5 cores for 2,500 viewers and shares them with the stack, so past 2,000 viewers the relays' p99 depends on that contention. Near 3,000 viewers k6 itself falls behind: 120,000 messages a second outrun its JavaScript event loops, and its backlog grows until the OOM killer stops it (2.4 to 2.9 GB). Exporting k6's per-message samples over OTLP doubled its CPU, so the live test keeps them in k6's summary.
- **The relays** use under two cores each at 2,500 viewers, about 21,000 writes a second each. Most of their time is the per-viewer write: one goroutine wake-up and one syscall per viewer per event. Coalescing events per viewer is the obvious next step and belongs with task 6.4's batching experiments.

The 5,000-viewer number is measured in AWS (task 6.9), with k6 on a machine of its own and the relays on Fargate.


## 7. Data scale (task 6.6)

`make seed` writes about 5 million rows through the services' own rules (1.7 million ledger entries, 804,000 ledger transactions, 607,000 ownership events, 350,000 listings, 300,000 drawings, 516,000 outbox rows; 131 s). `make explain` runs each query three times with `EXPLAIN (ANALYZE, BUFFERS)` on that data; the numbers are medians, locally, 2026-10-22.

| Query | Before | After | What changed |
|---|---|---|---|
| A seller's active listings, generic plan | 48.4 ms (every active listing) | 0.01 ms | The statement holds only the filters given. With "`CAST($n AS ...) IS NULL OR col = $n`" for each filter, the plan the driver caches after five executions can't use an index |
| A seller's active listings, custom plan | 0.06 ms | 0.06 ms | Unchanged: a custom plan already folded the null filters away, which is why only the generic plan showed it |
| Ownership history; owned drawings | 0.32; 0.32 ms | 0.19; 0.15 ms | A covering index `(drawing_id, event_id DESC) INCLUDE (kind, to_owner)` replaced `(drawing_id)` |
| Reconciliation: ownership | 1,429 ms | 976 ms | The same index: index-only scans instead of heap reads |
| Reconciliation: ledger imbalance | 66 ms (whole ledger) | 34 ms (closed totals plus open months) | Monthly partitions and stored totals of closed months. The old full scan over the partitioned table took 153 ms |
| Reconciliation: accounts against balances | 474 ms (whole ledger) | 346 ms | The same; the old full scan took 702 ms on partitions |
| Reconciliation, the whole run | 2.4 to 3.1 s | 1.6 s | The first run after the migration closed 12 months in 16.5 s, once |
| A wallet's transactions | 0.95 to 4.1 ms | 1.6 ms | Partitioning costs this one: it reads one index per partition, because an account's entries span every month |
| Outbox retention (hourly delete) | 34.7 ms, a sequential scan | 30.1 ms | Deliberately no index: an hourly 30 ms scan is cheaper than an index every insert maintains |
| Every assets query (gallery, by author, by match, a drawing, review queue, outbox) | 0.01 to 0.10 ms | the same | Their existing indexes already fit |

Rejected, because `EXPLAIN` didn't use it: a ledger index `(account_id) INCLUDE (amount)` for the accounts check (the planner kept a parallel sequential scan, which reads less than an index over every row).

The migration to partitions (V8) copied the 1.7 million entries in 17.8 s. The read replica (ADR 0020) runs in both composes; locally it replays within a second of the primary (0 bytes behind at rest). Its lag and the primary's load under real traffic are measured in AWS in task 6.9.

## 8. Tips: the hot wallet (task 6.3)

Thousands of viewers tipping one drawer would all lock that drawer's balance row (ADR 0021). Tips go into 16 escrow accounts instead, and the round's `RoundFinished` pays the drawer once. Measured locally, 2026-10-22.

| Run | Throughput | p50 | p95 | p99 | Notes |
|---|---|---|---|---|---|
| `TipContentionTests`: 1,000 tippers, one drawer, 32 threads and connections, direct posting (each tip locks the drawer's row) | 361 tips/s | 44.8 ms | – | 521 ms | Testcontainers Postgres |
| The same, through the escrow | 987 tips/s | 24.2 ms | – | 147 ms | then one settlement of 38 ms |
| `make loadtest-tips`: 200 seeded users, 300 tips a second for a minute, straight to the marketplace | 298 tips/s (all offered) | 6.7 ms | 344 ms | 737 ms | 17,907 tips, none refused, 94 not sent in time (k6's dropped iterations); lock wait p99 23 ms |

After the k6 run, the drawer had all 17,907 coins 1.2 s after the `RoundFinished` was sent, paid in two settlements (5,000 tips each, then the rest).

### The marketplace JVM under the tips load

The k6 tail above isn't lock waits (p99 23 ms). It was first put down to the garbage collector. Taking it apart (2026-10-22, the same run repeated 16 times with the JVM's GC log and the connection pool's metrics) found two causes:

- **One long full collection per JVM.** Every marketplace JVM stopped once for 1.0 to 1.8 s, a minute or two into its first load. The GC log names it `Pause Full (CodeCache GC Threshold)`. Since JDK 20, once the JIT has added compiled code worth 15% of the 240 MB code cache (`SweeperThreshold`), the JVM collects to unload old code. Serial GC can only do that in a full collection with class unloading. With 98 MB of metaspace (Spring plus the OpenTelemetry agent), marking 66 MB of live objects took 1.1 s of CPU. On a 0.5 vCPU ECS task that would be about 3 s of wall time.
- **The connection pool's queue.** Each tip checks out a connection three times: the idempotency claim, the posting and the completion (ADR 0012). So 300 tips a second are 900 checkouts a second on 10 connections. While Postgres answers a checkout in 2 to 4 ms the pool is busy a fifth to a third of the time. When the shared Docker VM slows Postgres down to 7 ms or more, the pool is full and up to 191 requests wait for a connection. During runs, Postgres used 90 to 160% CPU, otel-lgtm up to 276% and Kafka up to 208%, all sharing 10 CPUs with about 40 containers and k6. Wait time follows Postgres' time per checkout. It doesn't follow GC time, which stayed under 7% of each run.

The JVM's settings changed (the marketplace's Dockerfile, so compose and ECS run the same):

| JVM | Collections per run | Full collections after startup | Longest pause |
|---|---|---|---|
| Before: Serial picked by ergonomics (1 GB is under the JVM's 1,792 MB "server" mark), heap 16 MB at start and at most 768 MB | about 166 young, 2.9 to 3.1 s in all | one per JVM, 0.95 to 1.8 s | 1.8 s |
| `-XX:+UseSerialGC`, heap fixed at 512 MB (`InitialRAMPercentage` = `MaxRAMPercentage` = 50) | 18 to 25 young, 1.2 to 1.5 s in all | still one: 1.40 and 1.45 s (CodeCache GC Threshold) | 1.45 s |
| The same, plus `-XX:SweeperThreshold=50` | about 28 young (p50 20 ms, p90 46 ms) | none in three runs; the three metaspace-threshold ones during startup take 40 to 68 ms | 95 ms |

- **Serial, not G1.** On ECS the task has 0.5 vCPU, which the JVM rounds to one processor. G1 would unload code concurrently, but its concurrent threads would share that one core with the requests, and its remembered sets cost heap.
- **The heap is fixed.** Without a fixed heap the JVM starts at 1/64 of memory and grows it only through full collections. Under load it collected a 23 MB young generation every 0.6 s.
- **Half of memory, not 75%.** The rest of the process (metaspace, code cache, threads, the agent) takes about 350 MB of the 1 GB, so a 768 MB heap would leave no room below the limit.
- **`SweeperThreshold=50`.** Warm-up never reaches 120 MB of new code: the code cache settles near 45 MB. A nearly full code cache still unloads old code, when 10% is free.

Latency with the final settings (`make loadtest-tips`, 300 tips a second for a minute, 10 connections): p95 150, 499 and 362 ms and p99 0.82, 2.29 and 0.82 s, against p95 685 and 979 ms with the old settings the same evening. So the 250 ms threshold is still missed in two of three runs, and what's left is the pool's queue. Twenty connections didn't settle it either: p95 61 to 770 ms with the new settings and 539 ms to 1.62 s with the old ones. More connections don't add capacity to a Postgres starved of CPU. Fewer checkouts per tip, and the same run on AWS's separate data host (task 6.9), are what's left.

## 9. Limited drops (task 6.8)

Many buyers racing for few editions at the same second (ADR 0022). The criterion is no overselling. Measured locally with Testcontainers (Postgres and Redis), 32 threads and connections, 2026-10-23:

| Run | Answers | Sold | Charged |
|---|---|---|---|
| `DropOversellTests`: 300 buyers for 20 editions, Redis honest | 20 sales, 280 `SOLD_OUT` | 20 | 20 |
| The same with Redis lying: every edition number handed out three times | 20 sales, 40 `DROP_CONFLICT` (Postgres's primary key refused them, their coins rolled back), 240 `SOLD_OUT` | 20 | 20 |
| The same burst through the Redis gate | 58 ms for all 300 | 20 | 20 |
| Through a Postgres-only counter (every buyer locks the drop's row, then counts) | 125 ms for all 300 | 20 | 20 |

The flash sale at scale (`make loadtest-drops`, task 6.8b) on the GCP dev box (e2-standard-8): 1,000 seeded buyers join the waiting room before the open, then all try to buy within 200 ms of it; 100 editions.

| Run | Requests | Sold | p50 | p95 | p99 |
|---|---|---|---|---|---|
| Before the fix (not-admitted buyers retried until their turn, though sold out) | 9,726 | exactly 100 | 87 ms | 1.08 s | 1.69 s |
| Sold out answered first, the JVM just rebuilt | 3,087 | exactly 100 | 989 ms | 2.14 s | 2.34 s |
| The same, warm JVM | 3,596 | exactly 100 | 128 ms | 1.23 s | 1.45 s |

Every run: 100 editions to 100 distinct buyers, 100 purchases charged, 1,000 coins to the authors, the ledger at 0, checked in Postgres. The tail is the burst itself: 1,000 requests inside 200 ms at one 1 GB marketplace, absorbed in about a second. More marketplace instances or a bigger one would shorten it; the inventory isn't the limit (the gate's scripts take well under a millisecond).

## 10. Search (task 6.7)

Measured on the GCP dev box (e2-standard-8), 2026-10-23, after `make seed`: 300,011 approved drawings and 50,001 active listings, Elasticsearch 9.5.5 with a 512 MB heap (ADR 0023).

| Step | Result |
|---|---|
| `POST /api/market/listings/republish` (the backfill) | 50,001 `ListingCreated` written to the marketplace's outbox in 12 s |
| The indexer applying them | 1,024 s, about 49 events a second: each record is one Postgres transaction, one document write and a synchronous offset commit |
| `reindex_search` (a new index, bulk, the alias swapped, the old index deleted) | 300,011 documents in 53.5 s, about 5,600 a second; 57.0 s during `search-check` |

`make search-bench`: each search 30 times after one warm-up, in the indexer's container. "With read-back" adds reading the 20 hits and their listings from Postgres.

| Search | Hits | Elasticsearch p50 | p95 | With read-back p50 | p95 |
|---|---|---|---|---|---|
| A word | 37,500 | 48.5 ms | 71.2 ms | 54.2 ms | 76.5 ms |
| Its first three letters (prefix) | 37,500 | 24.3 ms | 43.3 ms | 28.7 ms | 47.4 ms |
| The word with two letters swapped (typo) | 37,500 | 22.3 ms | 24.8 ms | 26.4 ms | 29.3 ms |
| Listed only, cheapest first | 50,001 | 17.0 ms | 56.8 ms | 22.2 ms | 62.9 ms |
| A word, listed, 10 to 500 coins | 5,729 | 17.0 ms | 24.4 ms | 23.0 ms | 29.2 ms |
| Everything, newest first | 300,011 | 40.7 ms | 53.6 ms | 44.8 ms | 58.6 ms |

The read-back from Postgres costs about 5 ms. The slowest searches are the ones that score or sort the most documents (the seed uses only eight words, so one word matches 37,500 drawings); a real word list spreads them out. The indexer's 49 a second is plenty for live traffic, where an event follows a sale or a listing; a backfill that has to be faster would batch records (one transaction and one bulk request per poll) instead of one each.
