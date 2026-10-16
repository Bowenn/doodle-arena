---
paths:
  - "services/game-server/**"
  - "services/lobby/**"
---

# Game server and lobby rules

Read "Game server internals" in `docs/design-doc.md` before changing room logic.

- One actor goroutine per room owns all of that room's state. Other goroutines talk to it through its channel; never share room state behind a mutex.
- The actor gives every shared room event a `seq` and appends it to the Redis Stream `room:{id}:events` with the explicit ID `<seq>-0`, so resuming is an `XRANGE` from the last seq. Private messages (word options, the word for drawers) are sent directly and never go into the stream.
- Every write to a room's Redis keys goes through a Lua script that checks the lease holder and fencing token. If the check fails, the server stops the room and closes its sockets with a "room moved" message.
- Snapshot the room at every state transition and every 200 events. Recovery is the latest snapshot plus `XRANGE` after the snapshot's seq.
- Deadlines are absolute timestamps stored in the snapshot, so a new owner can continue the timers.
- Each socket has a bounded send buffer. A slow client is disconnected (it can resume); it is never allowed to block the actor.
- Guesses and chat pass through a per-player token bucket. Rate-limited guesses are counted, not processed.
- At the end of a round, upload the compressed stroke log to the `round-logs` bucket and publish `RoundFinished` with its key (the claim-check pattern).

Required tests (`go test -race`): the first correct guess wins under concurrent guesses; the word never reaches guessers early; a client resumes after a disconnect without missing events; a stale owner's writes are rejected after a takeover; failover restores the room.
