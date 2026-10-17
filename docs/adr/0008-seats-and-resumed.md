# ADR 0008: Report held seats to the lobby, and acknowledge every resume

- Status: Accepted
- Date: 2026-10-17
- Deciders: Bowen

## Context

The Stage 1–2 review found two gaps that need a contract change.

1. **Quick play can send a player to a full room.** Game servers report `RoomStatus.players`, the players currently connected. But a seat stays held for a disconnected player during the rejoin grace (and through a match), so a room can have 2 of 4 players connected and 4 of 4 seats taken. Quick play then picks it, and the joiner gets close code 4003 (room full), which the web app treats as final.
2. **A resume can get no reply at all.** A client that resumes with `last_seq` equal to the room's current seq, while the server still thinks its old socket is connected (the old socket died silently, for example on a network switch), gets no replay, no `player_joined` and no welcome. In a quiet room the client's 10-second handshake timer then gives up on a working socket and retries, adding about 10 seconds to the recovery.

## Options

### Seats

#### Option A: an additive field `RoomStatus.seats`

- Pros: the lobby compares exactly what the game server enforces (`len(players)` against `max_players`). `players` keeps meaning connected, which the room list shows. Additive, so `buf breaking` passes and an older game server (which sends 0) still works.
- Cons: a contract change, regenerated in three languages.

#### Option B: no contract change; the client retries quick play after 4003

- Pros: nothing changes in `proto/`.
- Cons: the lobby still picks full rooms; every client has to work around it.

### Resume acknowledgement

#### Option A: a private `resumed {seq}`, sent last

- Pros: every successful resume gets exactly one more message, after the replay, the reconnect's own `player_joined` and the private state, so the client knows it is caught up and can check `seq` against what it has. Unsequenced and never stored, like the other private messages.
- Cons: one more message type for clients to know.

#### Option B: `resumed {from_seq, to_seq}`, sent first

- Pros: the client could show progress.
- Cons: more to handle and test in the client, for no need we have.

## Decision

Seats: Option A, `uint32 seats = 4` in `RoomStatus` (0 means not reported, so the lobby falls back to `players`). Resume: Option A, a private `resumed {seq}` as the last message of every successful resume. A client whose `seq` is behind the server's resyncs.

## Consequences

- Quick play skips rooms whose seats are full, even when some of their players are away.
- A resume always ends with a message, so the handshake timer can't fire on a healthy socket, and a client can detect a replay that came up short.
- Clients that don't know `resumed` must ignore it (the drills and older web builds already ignore unknown private messages).
