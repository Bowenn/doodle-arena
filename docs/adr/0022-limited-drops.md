# ADR 0022: Limited drops: a Redis gate in front of Postgres, with a lottery waiting room

- Status: Accepted (2026-10-23)
- Date: 2026-10-23
- Deciders: Bowen (covering Will's track until he joins)

## Context

Task 6.8 adds limited drops: N numbered editions of an approved drawing, released at a set time and price, with a waiting room, atomic inventory and rate limits. The M6 exit criterion is "a flash-sale load test shows no overselling".

A drop is the opposite of a tip:

- Tips are many payers and one receiver (ADR 0021).
- A drop is many buyers racing for a few items at the same second. Most of them must be told "sold out" quickly, and none may get an edition that doesn't exist.

Three questions needed answers:

1. What is sold, and who is paid?
2. Where is the inventory counted?
3. In what order do buyers get their chance?

Constraints from the correctness rules:

- Money is integer coins, and every ledger transaction sums to zero.
- Every money movement has an idempotency key behind a `UNIQUE` constraint.
- Never oversell.

## Options

### What is sold and who is paid

- **A. An admin drop that pays the drawing's authors.** The authors are paid through a sharded escrow, in one payout at sell-out or close.
- **B. An owner drop that pays the owner.** It's a creator economy, but any player could start a flash sale.
- **C. An admin drop with the coins burned.** It's a coin sink, with no payout.

### Where the inventory is counted

- **A. A Redis gate in front of Postgres, which stays the truth.**
  - A Lua script reserves one of the N edition numbers per admitted buyer, atomically and once per user. So at most N purchases reach Postgres.
  - Postgres assigns the edition in the purchase transaction behind `PRIMARY KEY(drop_id, edition_no)`, `UNIQUE(drop_id, buyer_id)` and `CHECK (edition_no BETWEEN 1 AND editions)`. It can't oversell even if Redis is wrong.
  - Pros:
    - The burst never reaches Postgres. Sold-out answers come from Redis in well under a millisecond.
    - No row is hot.
  - Cons:
    - The marketplace needs Redis: a new user, key prefix and secrets.
    - A reservation can leak if the marketplace dies between Redis and Postgres.
- **B. Postgres only.** One conditional `UPDATE drops SET sold = sold + 1 WHERE sold < editions` per purchase.
  - Pros: simplest, one store.
  - Cons: every buyer, including every one who will be told "sold out", locks the same row. That's the hot row of ADR 0021 again.
- **C. A Kafka queue.** Purchase requests go to a topic partitioned by drop, and one consumer allocates editions in order.
  - Pros: fair, and no contention.
  - Cons: every purchase becomes asynchronous, and the client has to poll for its result.

### The waiting room

- **A. A lottery at open.** Everyone who joins before `opens_at` gets a random place; later joiners queue behind them in arrival order. Places are admitted at K a second.
- **B. First come, first served.** It's transparent, but it rewards bots, early refreshing and the fastest connections.

## Decision

Bowen chose **A, A and A**: an admin drop that pays the authors, a Redis gate with Postgres as the truth, and a lottery at open. Also one edition per buyer and a per-user token bucket.

- **Keys:** each drop's keys share one hash tag (`market:{drop:<id>}:free`, `:buyers`, `:queue`), as the lobby's keys do for Redis Cluster. The marketplace's Redis user may touch only `market:*`.
- **The waiting room:**
  - Joining before the open scores the user with a random number in [0, 1), drawn by the marketplace (`SecureRandom`). After the open, the score is `1 + now_ms`.
  - A user is admitted once their rank is below `admit_per_second × (seconds since open + 1)`. Admission is a function of time, so nothing has to run to admit people.
- **A purchase:**
  1. Claim the `Idempotency-Key` (ADR 0012), then pass the user's token bucket.
  2. Reserve an edition in Redis.
  3. One Postgres transaction:
     - insert the edition row;
     - post a `drop_purchase` ledger transaction (buyer to one of 16 `drop_escrow` accounts), keyed `drop:<drop_id>:<buyer>`;
     - check the drop's time window.
  4. Mark the reservation paid in Redis.
  - **Refused for funds:** the edition goes back to the pool.
  - **Redis unreachable:** drops answer `503`. Nothing falls back to a Postgres-only path.
  - **A reservation older than 60 s without its Postgres row** is released by the sweeper. The worst case is an unsold edition, never an extra one.
- **The payout:** once a drop sold out or closed, the sweeper pays the escrow to the drawing's authors, split like royalties. It posts one transaction per batch, keyed `drop-settle:<drop_id>:<n>` with the count kept on the drop row. `CoinsCredited` carries the new reason `DROP_SALE`. Reconciliation checks each `drop_escrow` shard against its paid editions (`market.drops.escrow_mismatch`).
- **Measurement:**
  - a test with 300 concurrent buyers for 20 editions, with Redis healthy and with Redis made to lie;
  - a comparison with a Postgres-only counter;
  - a k6 flash sale on the dev box: 1,000 seeded buyers for 100 editions.
  - The numbers go to `docs/performance.md`.

## Consequences

- **The marketplace depends on Redis for drops only.** Listings, purchases, tips and wallets work without it, and drops fail closed.
- **Two stores can disagree.** Postgres's constraints make disagreement cost an unsold edition, never an oversold one. The sweeper repairs leaked reservations.
- **Editions are new owned items** (`drop_editions`), separate from the unique drawing, which keeps its owner. Reselling editions is out of scope.
- **Fairness depends on joining before the open.** Bots can still join many times with many accounts. One edition per account and per-user rate limits bound that, but sign-up abuse is auth's problem.
- **What to watch:**
  - purchases by result;
  - the Redis scripts' latency;
  - unavailable purchases (DropsUnavailable);
  - leaked reservations;
  - `market.drops.escrow_mismatch` (DropEscrowMismatch).
