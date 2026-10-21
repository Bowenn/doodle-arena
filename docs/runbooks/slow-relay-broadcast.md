# Runbook: SlowRelayBroadcast

- Severity: warning
- Dashboard: Game server (`da-game-server`), row "Relays: live rooms", panel "Viewer broadcast latency"

## What it means

SlowRelayBroadcast fires when the p99 of `relay.broadcast.latency` has been above 250 ms for 5 minutes while more than 100 viewers watch. That is the time from a room owner's commit of an event until a relay's write to a viewer's socket (ADR 0017). Live-room viewers see the drawing and the guesses late. The players in the room aren't affected: relays only read the room.

## Check

1. **Viewer broadcast latency.** If stream lag p99 is high too, the delay is before the relay: Redis is slow, or the relay can't read fast enough. If only broadcast latency is high, the fan-out is behind: the relay's CPU, or the viewers' network.
2. **Viewers and watched rooms.** One relay holding most viewers means the lobby's placement isn't spreading them. The lobby picks the relay with the fewest viewers. Heartbeats are 2 s apart, so a burst of joins can land on one relay.
3. **Viewers closed and stream reloads.**
   - Many `slow` closes mean viewers can't keep up (a mobile network, or the relay's send queues backing up).
   - `gap` reloads mean a relay fell behind the stream's trim and reloaded the room.
4. **Logs:** `{service_name="game-server", service_instance_id=~"relay-.*"}` in Loki, for "slow viewer disconnected", "gap in the room's stream" and "reading the room's stream failed".
5. **CPU:** `docker stats` locally. In AWS, the relays' ECS CPU in CloudWatch.

## Fix

- **One relay is overloaded:** add a relay. Locally, a third `relay-N` service in `infra/docker-compose.yml` and in the gateway's `$ws_upstream` map. New viewers go to the new relay. Existing ones stay until they reconnect.
- **CPU pinned on every relay:** more relays, or more CPU per relay. A load test should say which (`make loadtest-live`, task 6.1c).
- **Redis slow (stream lag high):** see the Redis panels and `INFO commandstats` (`redis-cli --user admin`). Look for a long command blocking the event loop.
- **A relay is stuck:** restart it (`docker compose ... restart relay-1`; in AWS, `update-service --force-new-deployment`). It drains through the lobby, and its viewers resume on the other relay with their last seq.

## Afterwards

Record the latency, the viewer count and the cause in the evidence log (docs/roadmap.md). If the fix was capacity, update the measured viewers-per-relay figure in docs/performance.md.
