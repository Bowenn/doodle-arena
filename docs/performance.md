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

ECS delivered SIGTERM about 18 s after `stop-task`, so the drill's own clock (32.6 s from the call) includes that delay. The lobby notices a dead server 6 to 7 s after its last heartbeat.

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
| | AWS (https://icanfly.click) | `/`, `/gallery` | Not measured yet: needs an AWS session | | | | | |

The local run serves the image AWS runs, alone, so its API calls fail: it measures the bundle, caching and compression, not the API. Only AWS gives the real numbers.

### Real users

The web app reports `web.vitals.lcp`, `web.vitals.inp`, `web.vitals.cls` (each by `page`) and `web.ws.rtt` to the collector through the gateway (`docs/observability.md`, "Browser telemetry"). Checked end to end locally on 2026-10-20: a real browser's gallery and lobby vitals and a room's round trips (4 to 9 ms locally) reached Prometheus. Real AWS numbers come with the next AWS session.

## 4. AWS sessions

| Session | Cold `make aws-up` | `make aws-down` | Up to down |
|---|---|---|---|
| 2026-10-19, task 5.3 | 505 s (208 resources) | 296 s | 28 minutes |
| 2026-10-19, the re-measure | 506 s | 315 s | 29 minutes |

The cost per session comes from Cost Explorer, whose data lags about a day.
