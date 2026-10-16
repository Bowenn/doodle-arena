---
paths:
  - "services/marketplace/**"
---

# Marketplace money rules

Read "Marketplace data model and rules" and "Key flows" in `docs/design-doc.md` before changing anything here.

- Double-entry ledger: one `ledger_transactions` row plus `ledger_entries` rows whose amounts sum to zero. User balances never go below zero; system accounts (`stripe_clearing`, `rewards_issuance`) may.
- Purchase transaction (READ COMMITTED with explicit row locks):
  1. Lock the listing with `SELECT ... FOR UPDATE` and check that it is still `ACTIVE`.
  2. Lock every balance row involved in ascending `account_id` order. A fixed lock order prevents deadlocks.
  3. Check the buyer's balance, write the ledger entries (buyer, seller, royalty shares), update balances, mark the listing `SOLD`, move ownership and bump its version, and append an ownership event.
  4. Insert `SaleCompleted` into the outbox in the same transaction.
- A losing buyer gets `409 LISTING_NOT_AVAILABLE`. No coins moved, so there is nothing to refund.
- `Idempotency-Key`: the same key with the same request returns the stored response; the same key with a different request returns `422 IDEMPOTENCY_KEY_REUSED`; a key that is still in progress returns `409 IDEMPOTENCY_KEY_IN_PROGRESS`.
- Royalties use integer math only. Follow the rounding rule in the design doc and test it.
- The reconciliation job exports `market.ledger.imbalance` and `market.ownership.violations`. Both must always be 0.

Required tests: a concurrent purchase race (many buyers, exactly one owner, the ledger sums to zero); idempotent replays of purchases, rewards and Stripe events; concurrent transfers in opposite directions without deadlock; royalty rounding.
