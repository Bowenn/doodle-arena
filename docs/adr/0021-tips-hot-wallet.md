# ADR 0021: Tips go through a sharded escrow and are settled once per round

- Status: Accepted (2026-10-22)
- Date: 2026-10-22
- Deciders: Bowen (covering Will's track until he joins)

## Context

Task 6.3 lets viewers of a live room tip the drawer with coins. A live room is built for thousands of viewers (ADR 0017), so a popular drawer could receive hundreds of tips a second. That drawer's balance becomes a **hot wallet**, for two reasons:

- Every ledger posting locks the balance row of every account it touches, in `account_id` order (`LedgerService.post`, design doc section 8).
- Tips to one drawer would all queue on that one row. Each tip's transaction would hold the lock until its commit, so throughput to one drawer is bounded by commit latency. The same wait lands on every other posting that touches the drawer, such as a purchase they make.

`rewards_issuance` already works this way, but it is locked once per match, not once per viewer action.

The tip must also reach the right person. A client could name any user id, so either the marketplace checks who is drawing, or someone it trusts tells it.

Constraints from the correctness rules:

- Money is integer coins, and every ledger transaction sums to zero.
- Every money movement has an idempotency key behind a `UNIQUE` constraint.
- Kafka consumers are idempotent.
- The game server is authoritative for the round and its drawers.

## Options

### Option A: Direct posting

Each tip is one ledger transaction: debit the tipper, credit the drawer.

- Pros:
  - Simplest.
  - The drawer sees each coin at once.
- Cons:
  - Every tip locks the drawer's row, which is the hot wallet itself.
  - The client names the recipient, so the marketplace needs a new way to check that the recipient is drawing in that room. It has no such information today.

### Option B: Sharded receiving sub-accounts per drawer

Each tip credits one of N sub-accounts of the drawer (`tips:<user>:<k>`), and a sweeper moves them into the main account every few seconds.

- Pros:
  - Contention is divided by N.
  - The drawer is credited within seconds.
- Cons:
  - Each user needs N more accounts, created on first use.
  - The balance is spread across accounts until the sweep.
  - It has the same problem as A: the client names the recipient.

### Option C: A sharded escrow, settled per round from `RoundFinished`

A tip debits the tipper and credits one of 16 system escrow accounts (`tips_escrow:<k>`), and records the tip as `PENDING` for a match and round. When the game server's `RoundFinished` for that round arrives on Kafka, one ledger transaction per batch pays the round's pending tips to its `drawer_ids`.

- Pros:
  - **A tip never locks the drawer.** It locks only the tipper's row and one shard.
  - **The drawer's row is locked once per settlement.** That is per round, not per tip.
  - **The recipient comes from the authoritative game server,** not from the client.
  - It uses the existing round event, with no new contract with the game server.
- Cons:
  - The drawer is paid at the end of the round, not per tip.
  - Tips wait in escrow, so the escrow needs its own invariant, a sweeper and refunds. A round can end without a `RoundFinished` (a match that ends while the word is being chosen).
  - The marketplace starts reading `game.rounds.v1`.

## Decision

Bowen chose **C**, with these details:

- **The tip** is `POST /api/market/tips {room_id, match_id, round_no, amount}`.
  - It needs an `Idempotency-Key` (ADR 0012), and the amount is 1 to 1,000 coins.
  - One ledger transaction of type `tip` debits the tipper and credits `tips_escrow:<hash(tip_id) % 16>`, and inserts the `tips` row (`PENDING`) in the same commit.
  - It is refused with `409 ROUND_OVER` once its round was settled more than 30 seconds ago.
- **Settlement** happens when the marketplace consumes a `RoundFinished`. It records the round (`tip_rounds`: room, drawers, end time) and settles its pending tips:
  - It debits each shard by its share of the batch.
  - It credits the drawers, split like royalties: `floor(total / n)` each, with the remainder to the first.
  - It refunds, in the same transaction, a tip from one of the round's drawers or a tip whose room doesn't match the round's.
  - It marks the tips `SETTLED` or `REFUNDED`.
  - It writes `CoinsCredited` to the outbox (reasons `TIP` and `TIP_REFUND`, added to `CreditReason`).
  - Settlers lock the round's row, and tippers never do. Each settlement's key is `tip-settle:<match_id>:<round_no>:<n>`, with `n` counted on that row.
  - A redelivered `RoundFinished` finds nothing pending and pays nothing.
- **A sweeper** (every 10 seconds) has two jobs:
  - It settles late tips, committed after their round was settled, to the same drawers.
  - It refunds a tip whose round never finished within 10 minutes, one transaction each, with key `tip-refund:<tip_id>`.
- **Integrity:** escrow accounts are system accounts, so no `CHECK` keeps them at zero or above. The reconciliation job therefore checks each shard's balance against the sum of its pending tips (`market.tips.escrow_mismatch`, which must be 0), next to the existing ledger checks.
- **Measurement** (`docs/performance.md`, section 8):
  - A contention test compares C with A (`TipContentionTests`: 1,000 tippers on one drawer, 32 threads and connections, locally):

    | Option | Throughput | p50 | p99 |
    |---|---|---|---|
    | A, direct | 361 tips/s | 44.8 ms | 521 ms |
    | C, escrow | 987 tips/s | 24.2 ms | 147 ms |

    After C, one 38 ms settlement paid the drawer.
  - A k6 scenario has 200 seeded users tip one round 300 times a second for a minute, straight to the marketplace (`make loadtest-tips`):
    - 17,907 tips, none refused;
    - the lock wait p99 was 23 ms;
    - the drawer had every coin 1.2 s after the `RoundFinished`, in two settlements.
    - The latency tail (p95 344 ms) came from the marketplace JVM's stop-the-world garbage collections, not from locks.

## Consequences

- **Tipping scales with the number of shards, not the number of drawers.** Settlement locks every shard with pending tips for a few milliseconds per round, which briefly delays tips in flight.
- **The drawer sees tips at the end of the round.** For a live audience that is a feature: a round-end total. A live tip feed would need the relays, and is out of scope.
- **Coins can be stuck for up to 10 minutes** when a round never finishes. They are refunded, and TipsStuck alerts if the sweeper falls behind.
- **`RoundFinished` now moves money.** A forged one could send a round's escrowed tips to anyone. Only the game server's Kafka user may write `game.rounds.v1` in AWS (ADR 0013; `dev-tools`, which can too, exists only locally and in CI). The first `RoundFinished` for a round wins: a later one with other drawers changes nothing.
- **Tips move coins between accounts.** One user could tip another account they own while it draws. Coins can't be cashed out, so a wash-tip rule in the Flink job is a follow-up.
- **What to watch:**
  - `market.tips` by result;
  - settled and refunded tips;
  - pending tips and the oldest one's age;
  - `market.tips.escrow_mismatch`;
  - the lock wait of `tip` and `tip_settlement`.
