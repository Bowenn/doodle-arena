# Runbook: SlowBroadcasts

- Severity: warning
- Dashboard: [Game server, Input and send lag](http://localhost:3000/d/da-game-server)

## What it means

The p95 of a game server lag has stayed above 250 ms for 5 minutes, while the game servers were writing more than 10 messages a second. The alert's `lag` label says which:

- `input`: `game.room.input_lag`, a player's message (a stroke, a guess, a chat line) waiting in its room's inbox before the room's actor starts on it. Each step ends with a Redis commit, so a slow step delays every input behind it, and a socket whose reader is blocked on a full inbox can't even read its pings.
- `send`: `game.ws.send_lag`, a message waiting in a socket's send queue until its write returns.

Either way one in twenty strokes, guesses and pongs reaches players late or worse, and fast guessers lose to slow servers. `game.broadcast.duration` doesn't help: it ends when a message is queued.

## Check

1. One server or both? Split the Input and send lag panel by `service_instance_id` (it shows the p95 per instance).
2. CPU: the Scheduler latency panel (`go_schedule_duration_seconds`). A p99 above a few milliseconds means goroutines wait for a CPU, so the server is the cause. Confirm with ECS CPU in AWS (the load-test dashboard, or CloudWatch `AWS/ECS` `CPUUtilization`) or `docker stats` locally. Task 5.3 measured a 0.25-vCPU game server pinned at about 20 four-player rooms.
3. Load: WebSocket connections and active rooms on the same dashboard. A load test is the usual cause.
4. Input lag high but the CPU idle: the steps themselves are slow, which usually means Redis. Check the Redis container's (or host's) CPU and latency (`redis-cli --latency`).
5. Send lag high but scheduler latency low: the writes themselves are slow. `game.ws.slow_client_disconnects` rising (a queue filled up or a write took 10 s) points at clients' networks or the gateway, not the server.
6. The room actor: `game.broadcast.duration` p99 above a few milliseconds means the actor itself is slow to queue (huge rooms, or garbage collection: `go_memory_allocated_bytes_total`).

## Fix

- Overload on one server: drain it (the lobby stops placing new rooms on a draining server) and let new rooms go to the other.
- Overload on all servers: add a game server, or give the tasks more CPU (in AWS, scale the service or raise the task size in Terraform).
- Redis slow: see its host's CPU and memory first; the game servers can't go faster than their commits.
- Slow clients only: nothing to fix on the server; they are closed as too slow at 256 queued messages and resume. Check the gateway's CPU if many clients are affected at once.
- A regression after a deploy: roll back and compare `service.version` on the dashboard.

## Afterwards

Record the load at which it happened in the evidence log; it's a data point for the load-test baseline and for sizing `MAX_ROOMS` (design doc, "Measured capacity").
