# Doodle Arena: performance

All of the measured numbers on one page: the game under load and failover (task 5.3, then re-measured after its follow-ups) and the web app (task 5.4). The evidence log at the end of `docs/roadmap.md` stays the dated record of every run, with the full method; this file has the current numbers and how they changed. When a run adds a number, add a row to the evidence log and update the table here.

## How the numbers are made

| What | Command | Where to look |
|---|---|---|
| Load: up to 80 four-player rooms over 12 minutes, two drawers each sending a stroke batch every 50 ms | `make loadtest` (local), `GATEWAY=https://doodle-arena.icanfly.click make loadtest` (AWS) | Grafana, "Doodle Arena: load test" |
| Failover under load | `scripts/drills/failover.py kill` (local, `docker kill`), `stop` (AWS, `aws ecs stop-task`) at the 80-room plateau | The drill's own output; lobby and game-server logs |
| Lighthouse: 3 runs each of `/` and `/gallery`, mobile emulation, the median run | `make lighthouse` (the production web image, locally), `GATEWAY=https://doodle-arena.icanfly.click make lighthouse` (AWS) | The script's table; reports in `web/.lighthouseci/reports` |
| Real users' Core Web Vitals and WebSocket round trip | The web app reports them on its own (`web/src/rum.ts`) | Grafana, "Doodle Arena: service overview", row "Web app (real users)" |

The AWS stack (ADR 0003): 15 Fargate ARM64 services at 0.25 vCPU each, one t4g.large data host, load generated from Bowen's laptop through the ALB. Locally, one laptop runs the whole stack and k6.

## 1. The game under load (task 5.3)

Stroke fan-out is the time from a drawer's stroke batch until each other player in the room has it (k6 runs all four players of a room, so one clock measures it). Ping is a WebSocket `ping` until its `pong`.

| Run | 10 rooms | 40 rooms | 80 rooms | The limit |
|---|---|---|---|---|
| Local, first run | fan-out p95 4.8 ms | 9.6 ms | 125 ms | The laptop (k6 and every container on 10 cores) |
| AWS, first run | fan-out p50 17 ms, p95 24 ms; ping p50 9 ms | p50 22 ms, p95 450 ms; ping p95 94 ms | p95 at the 10 s cap; ping p95 9 s | A game server's 0.25 vCPU: 99 to 100% from about 25 rooms |
| AWS, after the follow-ups (#48 to #52) | p95 24 ms | p95 726 ms | seconds | Still the game servers' CPU, at about 40 rooms each |
| AWS, game servers at 1 vCPU, k6 on the GCP dev box (task 6.9, 2026-10-24) | | | fan-out p50 15 ms, p95 39 ms, p99 87 ms; ping p95 23 ms; input lag p95 7.3 ms, send lag p95 0.5 ms; 509 rounds, 1 room error | None reached: the 80-room ramp's top |

What the follow-ups changed and found:

- The first run's metrics couldn't see the bottleneck: `game.broadcast.duration` stayed at 0.1 ms throughout, because it ends when an event is queued. Two new metrics (#50) located it. `game.room.input_lag` (time an input waits in the room's inbox) p95 rose from 1 ms at 10 rooms to 773 ms at 40 and 6.7 s at 80. `game.ws.send_lag` stayed under 85 ms. So the room actors wait for CPU, not the socket writes.
- The SlowBroadcasts alert (`lag=input`, 250 ms over 5 minutes) fired in the AWS re-run, and its Discord message linked to Grafana on `localhost:3300` (#48).
- otel-lgtm under the ramp, locally: before #51, the Docker VM OOM-killed Tempo five times (up to 3.4 GiB each), because the game servers started a root trace for every room commit, so for every stroke. After #51 (no such traces, a memory limit in the collector, a stricter healthcheck), spans peaked at 234/s (about 2,000/s before) and Tempo used at most 383 MiB, with no OOM kill. In AWS, with the same guards on the data host (#52), otel-lgtm used 2 of 8 GB and never restarted, and the gateway's CPU peak dropped from 79% to 24%.
- Locally, with the game servers capped at 0.25 CPU like AWS, input lag p95 was under 1 ms up to 21 rooms and 0.9 to 2.4 s at 40.

The capacity, in one line: a game server at 0.25 vCPU holds about 20 to 40 four-player rooms of this traffic. At 1 vCPU (task 6.9), two servers hold the full 80-room ramp with stroke fan-out p95 39 ms, which is no change to code, only the task size. Measured locally at one CPU each with 50 ms batches, the ceiling is about 80 rooms with fan-out p95 under 250 ms (section 12).

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
| AWS, `stop` under the 80-room load, game servers at 1 vCPU (task 6.9, M6) | the drill's room moved to game-server-2; the lobby logged 47 rooms reassigned and a drain started and finished | the drill's players 16.1 s after `stop-task` (ECS's SIGTERM delay), disconnected for 0.1 to 0.2 s, no gap or duplicate; the load test's 164 reconnects resumed with p50 10.6 s, p95 20 s, behind the gateway's per-address limit (the reproduction below) | met |
| Dev box, `stop` under the 80-room load, the drill from the same address as k6 (2026-10-24) | 41 rooms in 6.1 s, all `reason=drain`, `result=moved` | k6's 160 reconnects resumed with p50 12.4 s, p95 20.3 s; the drill's players disconnected for 4.3 to 7.0 s; the gateway refused 457 joins and 171 WebSocket upgrades with 429, every one after the stop | met for the drain; the resumes measure the load generator's address |
| AWS, rolling redeploy, 80 rooms, the other services first, then one game server at a time | game-server-1: 47 rooms in 6.0 s; game-server-2: 74 rooms in 16.4 s; no drain hit its limit | the game-server phases: 30 room errors (429s from the retries, matches short of players); the gateway and lobby phase before them: 161 creates got 502 or 503 | met for the drains; see the note below |

ECS delivered SIGTERM about 18 s after `stop-task`, so the drill's own clock (32.6 s from the call) includes that delay. The lobby notices a dead server 6 to 7 s after its last heartbeat.

A redeploy (Stage 5 review) now replaces the other services first, then one game server at a time, so each drains into the other: no drain hit its limit. Two costs were left for Stage 6, and both are still open (2026-10-24):

- **Replacing the lobby gives 502s for a few seconds.** The gateway resolves the lobby's name at request time and caches it for 10 s (Cloud Map's record has a TTL too), so it keeps sending to the old task until that expires: 161 room creations failed in two minutes at 80 rooms. Nginx has no retry to another upstream (`proxy_next_upstream`) either; no change since.
- **One game server holds every room while the other is replaced.** At 0.25 vCPU that is two to four times its capacity (section 1): stroke fan-out p95 was 30.5 s and `ws_rtt` p95 14 s during those minutes. A third server, or a larger task for the duration of a deploy, would keep the survivor under its limit. Since task 6.9 each game server has 1 vCPU, four times the size measured here, but no deploy has been measured at that size and nothing adds a server during a deploy.

**Resumes after a drain under load measure the gateway's per-address limit, not the move.** Every k6 player comes from one address (the dev box's, or the Mac's Docker bridge locally), and the gateway allows one address 20 requests a second with a burst of 40 (`limit_req zone=api`), for the lobby's join and the WebSocket upgrade alike. A drained room's players all reconnect within the drain's few seconds, and each resume costs two requests: the 160 resumes of the reproduction needed about 320 at once. The rest got 429 and k6 backed off as clients do (design doc 7.4: 0.25 to 0.5 s, doubling up to 4 to 8 s), so with about four refusals each a player waited 8 to 15 s, which is the p50 of 10.6 to 12.4 s. The move itself is fast: the drain moved 41 rooms in 6.1 s, and in AWS the drill's own players, who connected from the Mac and so had a bucket of their own, were disconnected for 0.1 to 0.2 s. On the dev box the drill shared k6's address, and the same move cost its players 4.3 to 7.0 s. Real players each have their own address. A resume p50 that means something needs k6 spread over several addresses, or a load-test exemption in the gateway. The lobby answered no 5xx in either run.

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
| 2026-10-24 | AWS, with the browser's traces (task 6.9) | `/` and `/gallery` | 1.00 | 1.35 s | 0 | 0 ms | – | – |
| 2026-10-24 | Local production image, with the browser's traces | `/` and `/gallery` | 0.99 | 1.81 s | 0 | – | – | – |

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
| 2026-10-24, task 6.9 session 1 (305 resources: relays, Flink, their rules; a first try stopped at the data host's 60-rule limit) | 392 s (completing the stopped try) | 378 s, with the first backup | about an hour (load test, failover, the 5,000-viewer room, fire drills) |
| 2026-10-24, task 6.9 session 2 (283 resources) | 600 s, with a 185 s restore | 302 s after a forced backup failure that destroyed nothing (102 s) | 18 minutes |

Cost (Cost Explorer, 2026-10-20): about $0.30 to $0.80 per stack-hour, mostly the NAT gateway, then ECS, the ALB and EC2. Credits covered all of it so far; the domain ($15 plus tax) was the only charge. Cost Explorer lags up to a day, so the latest sessions may show more later.

## 5. Backup and restore (task 6.10)

What `aws-down` adds before the destroy and `aws-up` before the migrations (ADR 0016). The recovery point is the last completed `aws-down`.

| Where | Data | Dump (on the host) | Restore (load and count check) |
|---|---|---|---|
| Local, `make backup-check`, 2026-10-21 | 4 Postgres databases, 81,607 rows, 5.6 MB of dumps; MongoDB 6,978 replays, 2.5 GB in the collection, a 683 MB gzipped archive (load tests) | Postgres 0.5 to 1.0 s each; MongoDB 172 s | 42 s for all five |
| AWS, 2026-10-24 (task 6.9) | 4 Postgres databases (625 KB of dumps), MongoDB's replays (an 83 MB archive), 1,000 drawings (46 MB) | 57 s for every dump with its checksum check; the drawings copied in 12–13 s | 185 s for all five, every table's rows and every collection's documents matched; the whole `aws-up` 600 s |

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

In AWS (task 6.9, 2026-10-24): 5,000 viewers on one live room, from k6 on the GCP dev box (outside AWS, through the load balancer, connected at 9 a second), with two relays at 1 vCPU on Fargate:

| Viewers | Relays' p50 / p95 / p99 | Stream lag p99 | Relays' CPU | k6 |
|---|---|---|---|---|
| 5,000 (2,494 and 2,506 per relay) | 62 / 197 / **240 ms** (target 250: met) | 95 ms | 43–45% of a vCPU each, 52% at most (CloudWatch) | saturated: 96,000 messages a second, 28 GB in 14 minutes; viewer fan-out p95 6.5 s, 1,065 drops |

The relays measure until each write returns, and a viewer that reads late makes that write return late, so with k6 behind the p99 is pessimistic. The relays themselves had half their CPU spare.


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

The migration to partitions (V8) copied the 1.7 million entries in 17.8 s. The read replica (ADR 0020) runs in both composes; locally it replays within a second of the primary (0 bytes behind at rest). In AWS (task 6.9, session 1) its lag peaked at 0 s over 10 minutes of the 80-room load test. That load writes little to Postgres (a reward posting per finished match, about 235 matches), so it bounds the lag under game traffic, not under a burst of marketplace writes such as `make loadtest-tips`, which hasn't run in AWS.

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

## 11. The gallery cache (task 6.7b)

`make loadtest-gallery` on the GCP dev box (e2-standard-8), 2026-10-23, on the seeded data (300,000 approved drawings). The load mixes the gallery's first page (50%), its second or third page (20%), a Market lookup of 20 drawings (20%) and a drawing page (10%). The ids are a random sample of 2,000 seeded drawings, and a new drawing is approved every 2 seconds, so the gallery's generation moves 58 times in a run. k6 calls assets directly; assets runs two sync gunicorn workers (ADR 0024).

At 150 requests a second, which both setups can serve, for 2 minutes (18,000 requests each, no failures):

| | Cache off: p50 | p95 | p99 | Cache on: p50 | p95 | p99 |
|---|---|---|---|---|---|---|
| Gallery, first page | 9.7 ms | 253 ms | 534 ms | 4.7 ms | 16.7 ms | 228 ms |
| Gallery, next page | 10.0 ms | 168 ms | 526 ms | 9.4 ms | 20.3 ms | 100 ms |
| Market lookup (20 ids) | 11.0 ms | 199 ms | 547 ms | 5.1 ms | 18.8 ms | 204 ms |
| Drawing page | 8.9 ms | 241 ms | 535 ms | 3.9 ms | 17.1 ms | 227 ms |

- **Postgres reads:** assets' routed reads (`assets.db.reads`) fell from 17,508 to 2,834 in the run, from about 0.97 to 0.16 a request. Postgres's own count of rows read for the database, which includes the pipeline's and the indexer's background work, fell from 1.43 million to 0.88 million.
- **Hit ratios:** drawing entries 99.7% (69,490 hits, 205 misses). Gallery pages 77.6%: most misses are a next page whose cursor came from a generation an approval had just replaced, and they read Postgres. 16 requests waited for another request's fill, and none timed out.
- **Memory:** 5,903 drawing entries of about 600 bytes (about 3.5 MB) and 52 pages; Redis used 7 MB in all.

The ceiling:

| Offered | Cache off | Cache on |
|---|---|---|
| 200 a second | 186 served, p50 1.95 s (queued for the two workers), 1,317 iterations dropped | 199.5 served, p50 4–10 ms, p95 180–295 ms, 54 dropped; a second run at the edge served 183 |
| 300 a second | — | 287 served, p50 570 ms |
| 400 a second | — | 214 served, overloaded |

The cache moved the ceiling from about 186 to about 290 requests a second. More importantly, it took Postgres out of the path: what's left is CPU in assets' two workers, about 7 ms per cached request (179% CPU at 300 a second). That is the OpenTelemetry instrumentation, verifying the JWT and building JSON; locally, without telemetry, a cached request takes 1.8 ms, mostly waiting on Redis. More workers (`WEB_CONCURRENCY`) or instances now scale the gallery, because the shared stores are no longer the limit.

## 12. Stroke batching and wire formats (task 6.4)

`make experiment-strokes` on the GCP dev box (e2-standard-8), 2026-10-23, from branch `stage-6/6.4-experiments`. Each game server is limited to one CPU. 24 rooms (96 players, 48 drawers drawing 160 points a second) are held for 3 minutes, and every cell is the median of two runs (ADR 0025). k6 runs on the stack's network.

- **Ink delay:** from a point being drawn until another player has it (the batch's oldest point).
- **Fan-out:** from a batch being sent until another player has it.
- **Batches/s:** the server's own count of stroke batches received.

| Batch | Wire | Fan-out p50 / p95 | Ink delay p50 / p95 / p99 | Stroke KB/s in / out | Batches/s | Game-server CPU | CPU per 1,000 batches/s | k6 CPU | Redis cmds/s | Input / send lag p95 |
|---|---|---|---|---|---|---|---|---|---|---|
| 16 ms | JSON | 4 / 23 ms | 14 / 33 / 62 ms | 263 / 1,423 | 1,773 | 1.16 | 0.65 | 1.66 | 5,235 | 3.0 / 2.3 ms |
| 16 ms | protobuf | 13 / 76 | 22 / 86 / 141 | 70 / 353 | 1,445 | 0.97 | 0.67 | 3.04 | 4,291 | 3.8 / 2.3 |
| 33 ms | JSON | 3 / 15 | 30 / 42 / 68 | 224 / 1,144 | 1,204 | 0.91 | 0.76 | 1.32 | 3,570 | 1.7 / 1.9 |
| 33 ms | protobuf | 8 / 123 | 34 / 149 / 300 | 53 / 253 | 853 | 0.72 | 0.85 | 2.47 | 2,553 | 0.9 / 1.5 |
| 50 ms | JSON | 3 / 9 | 47 / 53 / 66 | 182 / 893 | 812 | 0.66 | 0.81 | 1.06 | 2,460 | 0.9 / 1.2 |
| 50 ms | protobuf | 4 / 36 | 48 / 79 / 126 | 57 / 264 | 737 | 0.66 | 0.89 | 2.29 | 2,254 | 0.3 / 0.9 |
| 100 ms | JSON | 2 / 7 | 96 / 101 / 108 | 138 / 635 | 411 | 0.38 | 0.92 | 0.70 | 1,330 | 0.7 / 0.9 |
| 100 ms | protobuf | 4 / 25 | 98 / 117 / 151 | 47 / 209 | 398 | 0.39 | 0.97 | 1.87 | 1,292 | 0.1 / 0.7 |

- **Batch interval:** the cost follows batches per second, since each is one Redis commit and a fan-out to three players. A shorter interval buys ink delay at a steep price; 50 ms stays the default.
- **Wire format:**
  - **Bytes:** protobuf frames are about 70% smaller.
  - **Server CPU:** the server spends slightly more per batch with them, because the room's event log stays JSON and each stroke is encoded twice. Decoding a protobuf batch is cheaper than the strict JSON decoder, but a batch's cost is mostly its Redis commit.
  - **k6:** protobuf's client-side numbers are k6's. Its JavaScript interpreter needs twice the CPU for binary frames and sends fewer batches as it falls behind.
  - **The browser:** in V8 both formats cost microseconds per message. Receiving one event takes 0.50 µs to decode and 0.67 µs with `JSON.parse`; sending a batch takes 2.4 µs to encode and 0.46 µs with `JSON.stringify` (`BENCH=1` in `web/src/game/strokeCodec.timing.test.ts`).
  - JSON stays the default, with protobuf opt-in (`?wire=pb`).

The ceiling at 50 ms batches, with rooms added 20 at a time, each held for 2 minutes:

| Rooms | 20 | 40 | 60 | 80 | 100 | 120 |
|---|---|---|---|---|---|---|
| JSON fan-out p95 | 7 ms | 22 | 75 | 184 | 269 | 360 |
| JSON batches/s taken in | 652 | 1,304 | 1,780 | 1,961 | 2,016 | 2,061 |
| JSON input lag p95 | 0.9 ms | 1.4 | 7.6 | 44 | 102 | 172 |
| Protobuf batches/s (k6 at 2.6–2.9 cores) | 590 | 832 | 884 | 897 | 926 | 859 |

With JSON, two one-CPU game servers keep the stroke fan-out p95 under 250 ms up to 80 rooms (320 players). From 80 rooms on, the servers take in about 2,000 batches a second however many rooms send them. For protobuf the limit was k6, not the servers, which stayed near 0.7 cores; finding its server ceiling would take a load client in Go.
