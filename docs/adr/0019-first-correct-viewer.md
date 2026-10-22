# ADR 0019: Viewer guesses and the first correct viewer

- Status: Accepted (2026-10-21)
- Date: 2026-10-21
- Deciders: Bowen (covering Will's track until he joins)

## Context

Task 6.2 lets the viewers of a live room (ADR 0017) guess. A room's viewers are spread over several relays, and a relay only reads the room. The room's owner, a game server, is the only writer of its state, and the game server is authoritative for answer checking and event order (CLAUDE.md, "Correctness rules").

So "who guessed it first" needs one place that decides, and the word must not leak. Viewers' wrong guesses also feed the top-guesses job (ADR 0018) through `live.guesses.v1`, whose list every viewer sees. A correct guess there, or one that contains the word, would give the word away.

## Options

How the first correct viewer of a round is decided, across relays:

- **A. The room owner decides.**
  - Relays check each guess against the word: they already hold the room's snapshot, which has it.
  - A correct guess goes to the owner through a Redis stream per room. The owner's actor takes the first one for the round and appends an event to the room's log, like every other state change.
  - Pros: one writer, in the order the owner sees; fenced like every other write (a stale owner can't announce a winner); a takeover replays it.
  - Cons: the owner gets a new input, and one Redis stream per live room.
- **B. A Redis `SET NX` at the relay.** The first relay to claim `room:{id}:round:<n>:winner` wins.
  - Pros: no change to the owner.
  - Cons: a relay then writes room state outside the owner's fenced commit, and the result isn't in the event log, so players and replays never see it in order.
- **C. Kafka partition order.** Correct guesses go to a topic keyed by room, and the first message in the partition wins.
  - Pros: an ordered log.
  - Cons: a consumer still has to decide and write the result into the room, and the word would reach Kafka.
- **D. Flink event time.** The analytics job picks the earliest correct guess by its relay timestamp.
  - Pros: fair across relays by time.
  - Cons: relay clocks decide, the answer comes after a watermark (seconds), and the analytics job would see the word.

## Decision

Bowen chose **A, the room owner decides**, and with the plan for task 6.2 (2026-10-21):

- **What the first correct viewer gets: an announcement only.** The owner appends `viewer_correct {user_id, name, round_no}`, which players and viewers see. There are no points and no coins: viewers are unlimited and free, so anything worth having invites bot farming.
- **Relays get their own credentials.** Relays face the public, and with the game server's credentials a compromised relay could mint coins (`game.matches.v1`) or rewrite a room. So relays get their own users:
  - **Kafka `relay`:** write `live.guesses.v1`, and read `live.top-guesses.v1` without a consumer group (partitions assigned, from the end). This changes ADR 0013's table.
  - **Redis `relay`:** read `room:*`; write only viewer tickets (`room:{id}:viewer-ticket:*`) and viewer-guess streams (`room:{id}:viewer-guesses`).
- **The owner reads with a blocking reader per live room.** A goroutine per live room runs `XREAD BLOCK` on `room:{id}:viewer-guesses` and feeds the room's actor. Only live rooms have one.
- **Top guesses go to viewers only.** Relays consume `live.top-guesses.v1` and show each room's list on its watch page. Players' screens don't change, so the crowd can't hint guessers toward the word.

How a viewer's guess flows:

1. The relay reads `guess {text}` from the viewer and spends a token from the viewer's bucket (5 at once, then 1 a second, like players).
2. The room's stream goroutine, which holds the snapshot, compares `room.Normalize(text)` with the word, and only while a round is drawing. The viewer gets a private `guess_result`.
   - **Correct:** `XADD room:{id}:viewer-guesses MAXLEN ~ 1000 * user_id name round_no t`.
   - **Contains the word** (as a player's "close" guess): nothing else. It is never published, so the word can't reach the top guesses.
   - **Wrong:** a `ViewerGuess` on `live.guesses.v1`, keyed by room, at least once.
3. The owner's reader passes each entry to the actor. The actor appends `viewer_correct` through the fenced commit if the entry is for the round being drawn and the round has no viewer winner yet; anything else is ignored.
4. After a takeover, the new owner reads the stream from the start. The restored game already knows the round's winner, so nothing is announced twice.

## Consequences

- **One winner per round, decided by the owner's order of arrival.** A viewer on a slower relay can lose by milliseconds, but no two viewers win and nothing is decided twice.
- **The word never leaves the game servers and relays.** `live.guesses.v1` carries only wrong guesses that don't contain the word, so neither analytics nor the top-guesses list can show it.
- **The owner gains one input** (a new actor inbox type) and **the room gains one key** (`room:{id}:viewer-guesses`, deleted with the room). A new stored event type means `Game.apply` must restore it, or takeovers and relay welcomes would fail.
- **Relays become Kafka clients.** They join the Kafka network, with their own user and password (`KAFKA_RELAY_PASSWORD`, `REDIS_RELAY_PASSWORD`).
- **What to watch:**
  - `relay.viewer_guesses{result}` (correct, wrong, close, not_drawing, rate_limited);
  - `relay.guesses.published{result}`;
  - `relay.top_guesses.received`;
  - `game.viewer_correct`.
