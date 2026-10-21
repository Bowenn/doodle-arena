# ADR 0017: Live-stream relays

- Status: Accepted (2026-10-21)
- Date: 2026-10-21
- Deciders: Bowen (covering Will's track until he joins)

## Context

Stage 6 adds a live-stream room: thousands of viewers watch one room's drawer and, from task 6.2, guess. The M6 exit criterion is one live room holding N thousand viewers with p99 broadcast latency under a target, recorded.

A game server can't carry that. One room runs on one actor that commits every step to Redis and then sends it to each socket (design doc, section 9), and task 5.3 measured about 20 four-player rooms per 0.25 vCPU. A room's fan-out is done on its owner's CPU, so a few thousand sockets on the owner would slow the players' own game.

The design doc already fixes the shape (section 9, D3): relays are the same binary in a relay role, read a room's Redis stream with `XREAD` and fan it out. The room owner still processes every input alone. These questions were open:

1. How a viewer finds a relay.
2. Who may watch.
3. Which rooms can be watched.
4. The target.

## Options

### 1. Finding a relay

- **A. The lobby assigns.** Relays heartbeat to the lobby like game servers, with a role and their viewer counts. `POST /api/lobby/rooms/{id}/watch` picks the least-loaded relay and returns its URL and a viewer ticket.
  - Pros: load-aware placement, and the control plane knows where viewers are (viewer counts in the room list).
  - Pros: it is the same pattern as rooms on game servers, and a relay that dies is simply missing from the next pick.
  - Cons: more lobby code, and a contract change (additive).
- **B. Rendezvous hashing.** A room's viewers always go to the same k relays.
  - Pros: fewer readers of one stream.
  - Cons: one hot room can't spread past k relays without rehashing, and moving viewers when relays come and go needs more care.
- **C. The gateway balances.** Nginx spreads viewers across relays at random.
  - Pros: no lobby change.
  - Cons: no load-aware placement, every relay may read every live room, and nothing knows how many people watch.

### 2. Who may watch

- **A. Signed-in viewers with single-use tickets** (ADR 0005's tickets, for watching).
  - Pros: no JWT in a URL, the same abuse controls as players, and task 6.2's guesses know who guessed.
  - Cons: viewers must sign in.
- **B. Anonymous watching**, with sign-in only to guess.
  - Pros: wider reach.
  - Cons: thousands of unauthenticated sockets, and two kinds of viewer.

### 3. Which rooms can be watched

- **A. A live flag** that the host sets when creating the room. The lobby lists live rooms with their viewer counts.
  - Pros: players choose an audience.
  - Cons: one more field.
- **B. Every public room.**
  - Pros: nothing to build.
  - Cons: players never chose to be watched.

### 4. The target

5,000 viewers on one room with p99 under 250 ms, 10,000 with p99 under 500 ms, or 2,000 with p99 under 250 ms.

## Decision

Bowen chose 1A, 2A, 3A and the target **5,000 viewers on one live room, p99 broadcast latency under 250 ms**:

- Latency is measured from the owner's commit (the stream entry's server time `t`) to the write to the viewer's socket.
- It is measured locally with k6, through the gateway, with two relays. Task 6.9 measures in AWS.
- 250 ms is the SlowBroadcasts alert's threshold for players.

How it works:

- **Viewer tickets have their own audience.** They are the lobby's Ed25519 tickets (ADR 0005), with `aud = doodle-watch` instead of `doodle-game`.
  - Game servers have always required `doodle-game`, so a viewer ticket can't take a player's seat, even on a server older than this ADR.
  - A relay accepts only `doodle-watch`.
  - Each ticket works once: `SET NX room:{id}:viewer-ticket:<jti>`. There is no lease check, because a viewer writes no room state.
- **A relay only reads.** Per watched room, it does this:
  1. Loads the snapshot and the events after it.
  2. Reads new events with `XREAD BLOCK`.
  3. Encodes each event frame once and queues it to every viewer, with the same bounded send queue as players. A slow viewer is closed with 1013 and resumes.
  
  A phase event (match start, choosing, round start or end, match end) comes with a snapshot in the same commit, and the relay reads that snapshot again. A `welcome` is the room rebuilt from the snapshot and events, viewed by a non-member. It never contains the word while a round runs, as for every guesser.
- **The owner doesn't know about viewers.** A takeover changes nothing for them: the relay reads the same keys from whichever server owns the room.
- **Resume** works as for players: up to 200 events are replayed, otherwise a fresh `welcome`. A viewer whose relay stops (drain, crash) asks the lobby again and resumes on another relay with `last_seq`.
- **The contract** (`registry.proto`, additive): `HeartbeatRequest` gains `role` (`SERVER_ROLE_RELAY`; unspecified means a game server), `viewers` and per-room viewer counts. A relay sends no rooms and no control address. The lobby never allocates rooms on relays.

## Consequences

- **Each room is read by every relay that has a viewer for it.** That is one `XREAD` per relay per room, which is cheap next to the fan-out. With 1A, a hot room spreads over every relay.
- **The owner's write path is unchanged.** Its latency doesn't depend on how many people watch.
- **Viewers lag players by one Redis round trip plus the relay's own fan-out.** The relay's metrics separate the two: `relay.stream.lag` (commit until the relay reads the event) and `relay.broadcast.latency` (commit until the viewer's socket write).
- **A viewer is a signed-in user.** Thousands of viewers in a load test need thousands of tokens and tickets. The gateway's per-address limit (20 requests a second) sets how fast one load generator can connect them.
- **Every viewer is two connections at the gateway.** Nginx's `worker_connections` must cover them.
- **Viewer guesses are task 6.2** (`live.guesses.v1`, a separate ADR). Until then a viewer's only message is `ping`.
- **What to watch:**
  - p99 of `relay.broadcast.latency` (alert SlowRelayBroadcast);
  - viewers closed as slow;
  - stream rebuilds, which mean a relay fell behind the stream's trim.
