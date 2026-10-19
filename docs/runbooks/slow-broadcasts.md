# Runbook: SlowBroadcasts

- Severity: warning
- Dashboard: [Game server, Broadcast latency](http://localhost:3000/d/da-game-server)

## What it means

The p99 of `game.broadcast.duration` has stayed above 150 ms for 5 minutes. It measures marshalling an event and queueing it to every local socket, after the step's commit to Redis. Players see strokes and guesses late, and fast guessers lose to slow broadcasts.

## Check

1. Is it one server or both? Split the panel by `service_instance_id` (Explore).
2. Load: WebSocket connections and active rooms on the same dashboard. A load test (task 5.3) is the usual cause.
3. CPU and garbage collection: the Go runtime metrics for `service_name="game-server"` (`go_*`), and the container's CPU (`docker stats`).
4. Slow clients: `game.ws.slow_client_disconnects` rising means sockets are full; queueing to them shouldn't block, so a rise together with slow broadcasts points at the server, not the network.
5. Redis latency: a slow commit happens before the broadcast and isn't in this metric, but a Redis that is slow often comes with CPU pressure on the same host.

## Fix

- Overload on one server: drain it (the lobby stops placing new rooms on a draining server) and let new rooms go to the other.
- Overload on all servers: add a game server (in AWS, scale the service).
- A regression after a deploy: roll back and compare `service.version` on the dashboard.

## Afterwards

Record the load at which it happened in the evidence log; it's a data point for the load-test baseline.
