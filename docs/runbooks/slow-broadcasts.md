# Runbook: SlowBroadcasts

- Severity: warning
- Dashboard: [Game server, Send lag](http://localhost:3000/d/da-game-server)

## What it means

The p95 of `game.ws.send_lag` has stayed above 250 ms for 5 minutes, while the game servers were writing more than 10 messages a second. Send lag runs from the moment the room queues a message to a socket until the socket write returns, so one in twenty strokes, guesses and pongs reaches players a quarter of a second late or worse, and fast guessers lose to slow sockets.

Two causes look the same here: the server can't schedule its writer goroutines (CPU pinned: every socket falls behind), or clients can't take the data (a slow network: their writes block and their queues grow). `game.broadcast.duration` doesn't help with either; it ends when a message is queued.

## Check

1. One server or both? Split the Send lag panel by `service_instance_id` (Explore).
2. CPU: the Scheduler latency panel (`go_schedule_duration_seconds`). A p99 above a few milliseconds means goroutines wait for a CPU, so the server is the cause. Confirm with ECS CPU in AWS (the load-test dashboard, or CloudWatch `AWS/ECS` `CPUUtilization`) or `docker stats` locally. Task 5.3 measured a 0.25-vCPU game server pinned at about 20 four-player rooms.
3. Load: WebSocket connections and active rooms on the same dashboard. A load test is the usual cause.
4. Slow clients: if scheduler latency is low but send lag is high, the writes themselves are slow. `game.ws.slow_client_disconnects` rising (a queue filled up or a write took 10 s) points at clients' networks or the gateway, not the server.
5. The room actor: `game.broadcast.duration` p99 above a few milliseconds means the actor itself is slow to queue (huge rooms, or garbage collection: `go_memory_allocated_bytes_total`).

## Fix

- Overload on one server: drain it (the lobby stops placing new rooms on a draining server) and let new rooms go to the other.
- Overload on all servers: add a game server, or give the tasks more CPU (in AWS, scale the service or raise the task size in Terraform).
- Slow clients only: nothing to fix on the server; they are closed as too slow at 256 queued messages and resume. Check the gateway's CPU if many clients are affected at once.
- A regression after a deploy: roll back and compare `service.version` on the dashboard.

## Afterwards

Record the load at which it happened in the evidence log; it's a data point for the load-test baseline and for sizing `MAX_ROOMS` (design doc, "Measured capacity").
