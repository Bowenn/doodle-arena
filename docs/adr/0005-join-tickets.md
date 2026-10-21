# ADR 0005: How the lobby signs join tickets and game servers verify them

- Status: Accepted
- Date: 2026-10-16
- Deciders: Bowen

## Context

A player joins a room through the lobby (`POST /api/lobby/rooms/{id}/join`) and then opens a WebSocket to the game server that owns the room. The game server must know who the player is and that the lobby let them into this room. The WebSocket can't carry the player's JWT: it would have to go in the URL, and URLs end up in logs (design doc, section 11). So the lobby hands out a join ticket, a short-lived signed token that names the user and the room, and the client sends it in its first `hello` message.

What matters:

- **Who can create tickets.** Only the lobby decides who may enter a room. A game server only needs to check that decision.
- **Replay.** A ticket that leaks (a browser extension, a proxy log, a screen share) shouldn't let someone else join as that player.
- **Key rotation** without restarting every service at once.
- Both sides are Go services (lobby and game-server), so library support in other languages doesn't matter here.

## Options

### Option A: a shared HMAC-SHA256 secret

The lobby and every game server hold the same secret. The lobby signs tickets with it; game servers verify with it.

- Pros: the simplest code; tiny signatures; fast.
- Cons: whoever can verify can also sign, so every game server could mint tickets for any user and any room. A compromised or buggy game server breaks room access everywhere. The secret has to be copied to every instance, and rotating it means updating every service at once.

### Option B: an Ed25519 key pair held by the lobby

The lobby signs tickets with its Ed25519 private key. Game servers hold only the public key, so they can verify tickets but never create them.

- Pros: least privilege: only the lobby can mint tickets. Public keys aren't secret, so distributing them is safe. Rotation can publish the new public key before the lobby switches to it, with no coordinated restart. Ed25519 has small keys and signatures, fast verification, and no parameter choices to get wrong. It's in Go's standard library (`crypto/ed25519`).
- Cons: one more key pair to generate, store and rotate. Slightly more code than an HMAC.

## Decision

Option B: the lobby signs join tickets with an Ed25519 private key, and game servers verify them with the public key only.

- **Claims:** a ticket names the user and the room, has a unique ticket id, and is valid for 60 seconds.
- **Single use:** before accepting a ticket, the game server records its id in Redis with `SET NX` and a TTL as long as the ticket can still be accepted: `exp` plus the verifier's 5-second clock-skew leeway, minus now, so at most 65 seconds (refined in task 2.2; a flat 60 seconds would expire 5 seconds before the ticket stops verifying). If the key already exists, the ticket was used before, and the connection is rejected. A leaked ticket is therefore worth nothing once its owner has connected, and a replay within the 60 seconds fails.
- **Rejected: Option A**, because every game server could mint tickets for any user and room, and rotating the shared secret means updating every service at once.

## Consequences

- Every connection, including a reconnect to the same server, needs a fresh ticket from the lobby. Reconnect (design doc, section 7.4) therefore always calls the lobby first, so the lobby is on the reconnect path and must stay cheap and available.
- The single-use check needs Redis. If Redis is down, the game server can't record the ticket and rejects the join (fail closed); a room whose Redis is down can't run anyway (section 9).
- The used-ticket keys live under the game server's own Redis prefix (`room:{room_id}:ticket:{ticket_id}`), so no other service reads or writes them (rule 4 in `CLAUDE.md`).
- Tickets carry a key id, and game servers accept a small set of public keys, so a rotation adds the new public key first and retires the old one after the last ticket signed with it has expired (60 seconds).
- Task 2.1 settles the details: the ticket's wire format (for example a JWT with `alg: EdDSA`, accepting only that algorithm), how game servers get the public key, the allowed clock skew, and the tests (expired, wrong signature, wrong room, other algorithms, reused ticket). The game server's side is task 2.2.
- **Amended by ADR 0017 (task 6.1):** viewers of live rooms get the same tickets with `aud = doodle-watch`. Only relays accept that audience, and game servers keep requiring `doodle-game`, so a viewer's ticket can never take a seat. Relays record a used viewer ticket in `room:{room_id}:viewer-ticket:{ticket_id}` with `SET NX`, without the lease check, because a relay doesn't own the room and writes nothing to it.
- Interview angle: least privilege between services (verify-only keys), why a 60-second ticket is still replayable without the single-use check, and the comparison with HMAC secrets and with putting a JWT in the WebSocket URL.
