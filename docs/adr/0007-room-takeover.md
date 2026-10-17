# ADR 0007: How a game server takes over a room whose old owner may still be alive

- Status: Accepted
- Date: 2026-10-16
- Deciders: Bowen

## Context

Each room has exactly one owner: the game server holding its lease in Redis (`room:{id}:lease`, 5-second TTL, renewed every 1.5 seconds). Writes to the room's keys carry a fencing token (`room:{id}:fence`), checked by a Lua script. The lobby decides where rooms live, and reassigns a server's rooms when its heartbeats stop for 6 seconds (design doc, sections 4.3 and 7.5).

The hard case is a partition. Game server A loses contact with the lobby, but can still reach Redis and its players. The lobby thinks A is dead and calls `AllocateRoom` on server B. A keeps renewing its leases, so they never expire on their own. The question is how B takes the room, and what A's players see in the meantime.

Two rules hold whichever option is chosen:

- **Write, then broadcast.** A room's actor appends each step's events to the room's stream, through the script that checks the token, before it sends them to any socket. A client never sees an event the next owner can't replay.
- **Take, then read.** A new owner takes the lease first, then reads the snapshot and the stream. Writes the old owner made before that are visible; writes after it fail the token check.

## Options

### Option A: force the takeover

`AllocateRoom` always claims the room. B increments the fence (its token), overwrites the lease with its own id and token, then loads the snapshot and the events after it. A notices at its next write or lease renewal (within about 1.5 seconds), stops the room, sends `room_moved` and closes the sockets. Clients ask the lobby for the new URL and a ticket, and resume on B from `last_seq`.

- Pros: recovery is lobby detection (6 s) plus about 2 s to reconnect, inside the 10-second target. The lobby stays the single authority on placement. Safety doesn't depend on A behaving well: a zombie is stopped by the fencing check.
- Cons: if only the lobby's link to A is broken while A is healthy, rooms move anyway (a few seconds of reconnecting, nothing lost). Only the lobby may call `AllocateRoom` (ADR 0006 keeps that port inside the private network).

### Option B: wait for the lease to expire

A stops renewing its leases after losing the lobby for, say, 4 seconds; B polls until the lease expires and then takes it.

- Pros: a room never leaves a server that still holds its lease; no forced writes.
- Cons: adds up to 5 seconds (11–13 s total, over the target). Correctness depends on A's clock and on A actually stepping down, which is the zombie case that fencing tokens exist for, so fencing is needed anyway. B needs a poll loop.

### Option C: wait, then force

B waits up to one lease period, then forces. When A really died, its lease has already expired by the time the lobby notices, so this only adds delay in the partition case.

## Decision

Option A: forced takeover with fencing tokens. It is the standard lease-plus-fencing design: forcing is safe precisely because the token makes the old owner harmless, so there is nothing to gain from waiting.

- B takes the lease with `INCR room:{id}:fence` and `SET room:{id}:lease <server_id>:<token> PX 5000` in one script, then reads the snapshot and the events after it.
- Every write by an owner (event appends, snapshots, lease renewals, ticket redemptions) checks `lease == <its server_id>:<its token>`. A failed check means the room has moved: the owner stops it at once, sends each socket `room_moved {reason: "takeover"}` and closes it with 4010.
- The fence counter only goes up and never expires, so an old token can never become valid again.
- What A's players see: they keep playing on A for at most about 1.5 seconds; then `room_moved`, a lobby call, and a resume on B with the events they missed. An input A received but couldn't commit in that window (a guess, a stroke) is lost; the player sends it again.

## Consequences

- Recovery time is dominated by the lobby's 6-second detection. Measure it with the kill drill and record it in the evidence log.
- Every actor step that produces shared events costs one Redis round trip before anything is broadcast; `game.broadcast.duration` now includes it.
- If Redis is unreachable, a room can't commit and stops (close 1011) instead of running without durability: consistency over availability for room state, as for the ledger.
- A wrong suspicion by the lobby costs players a reconnect, never a lost or duplicated event.
- Interview angle: fencing tokens (why a lease alone isn't enough when a paused process wakes up), write-then-broadcast as the rule that makes replay exact, and the trade-off against waiting for leases to expire.
