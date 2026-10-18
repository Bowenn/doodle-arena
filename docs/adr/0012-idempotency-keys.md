# ADR 0012: Idempotency keys: a leased claim and a fenced completion

- Status: Accepted
- Date: 2026-10-18
- Deciders: Bowen

## Context

Every money-moving request (create a listing, purchase; Stripe checkout in task 4.5) carries an `Idempotency-Key` (design doc 6.3 and 8). The rules were already set: the same key with the same request returns the stored response, the same key with another request is `422 IDEMPOTENCY_KEY_REUSED`, and a key still being processed is `409 IDEMPOTENCY_KEY_IN_PROGRESS`. The design doc left one question open before task 4.3: what happens to a key whose request crashed while `IN_PROGRESS`. Two smaller questions came with it: which answers a key remembers, and how two copies of one request can never both do the work.

Constraints: the work must happen at most once per key even if a request hangs, crashes or races with its own retry; no key may stay stuck forever; and the same pattern should also fit 4.5, where the work calls Stripe and so can't sit inside one database transaction.

## Options

### Option A: the key in the same transaction as the work

The key row is the purchase transaction's first statement and is set to `DONE` before its commit. A crash rolls the key back with everything else. A concurrent request with the same key waits on the unique index; a short `lock_timeout` turns that wait into 409.

- Pros: no state to clean up and no timeout to tune; atomic by construction.
- Cons: only works when all of the work is one Postgres transaction, so Stripe (4.5) would need a second pattern; the "in progress" answer depends on a lock timeout rather than visible state.

### Option B: a claim in its own transaction, with a lease

The request first commits the key as `IN_PROGRESS` with a lease (`locked_until`), does the work in a second transaction, and marks the key `DONE` there. A crash leaves `IN_PROGRESS`; once the lease expires, a retry may take the key over.

- Pros: the state is visible (the key says what is happening); the same pattern covers work with external calls; a crashed request's key frees itself.
- Cons: a slow request whose lease expired could finish after a takeover started, so completion must be fenced; one more timeout to choose.

## Decision

Option B, with a fence: each claim gets a random `claim_token`, and the work's transaction ends with `UPDATE idempotency_keys SET status = 'DONE', … WHERE user_id = ? AND key = ? AND claim_token = ? AND status = 'IN_PROGRESS'`. If a takeover replaced the token, that update matches nothing and the work's whole transaction rolls back, so the work happens at most once even when the original and the takeover overlap. It is the same idea as the game server's lease and fencing token (ADR 0007), applied to a request instead of a room.

- The lease is 30 seconds. A purchase takes milliseconds, so only a crashed or stuck request reaches it.
- Keys remember business outcomes as well as successes: 2xx, and 403, 404, 409 and 422 from the work (a lost race, insufficient funds). The same key always gets the same answer, like Stripe; a new attempt needs a new key. A business outcome is stored with the same fenced update after the work rolled back. Validation errors (400) come before the claim and aren't stored; any other failure deletes the claim (fenced) so a retry can start at once.
- The stored response is the exact JSON text sent, in a `TEXT` column (`JSONB` would reformat it), so a replay is byte for byte the first answer, with an `Idempotency-Replayed: true` header.
- Keys are deleted after 24 hours by an hourly job.

## Consequences

- Easier: task 4.5 reuses the same claim and completion around its Stripe call; a stuck key is visible in a table and frees itself.
- Harder: three transactions per request instead of one (claim, work, and for a business outcome a third), and a fencing rule every future operation must keep: complete the key inside the work's transaction.
- Watch: a crashed request blocks its own retries for up to 30 seconds (409); `market.idempotency.replays` shows how often clients retry.

## Implementation note (2026-10-18, task 4.R1)

Until 4.R1 the checkout called Stripe inside the work's transaction, holding a pooled connection for the whole call. `IdempotencyService.execute` now has an optional call step between the claim and the work: it runs with no transaction open, and its result goes to the work, which records it and completes the key. The Stripe idempotency key is derived per claim (it includes the claim token), not per key: Stripe's API v1 replays a saved response, a 500 included, for 24 hours, so a key-derived Stripe key would fail every retry of a checkout for a day. A takeover therefore creates a second session, but only the claim holder's is recorded and returned.
