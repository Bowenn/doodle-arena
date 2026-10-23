# Runbook: LedgerImbalance

- Severity: critical
- Dashboard: [Marketplace, Invariants](http://localhost:3000/d/da-marketplace) (in AWS, the same dashboard in the deployed Grafana)

## What it means

The reconciliation job found money that doesn't add up: the ledger's entries don't sum to zero (`market.ledger.imbalance != 0`), or an account's balance isn't the sum of its entries, a user balance is negative, or an account has no balance row (`market.ledger.mismatched_accounts > 0`). Coins were created, lost or moved without a matching entry. Every purchase, reward and coin pack trusts these balances, so this is the most serious alert in the system.

## Check

1. The two stats on the dashboard. After a redeploy, the old container's last value stays visible for 5 minutes as a second series; check the newest `host_name` before acting (observability.md, section 5).
2. The job's log line names what's wrong: in Grafana Explore (Loki), `{service_name="marketplace"} |= "invariants broken"`. It lists up to 20 `mismatched_account_ids`. Its `trace_id` leads to the `market.reconcile` span.
3. Look at one account, read-only, as the marketplace's database owner:

   ```bash
   docker compose -f infra/docker-compose.yml --env-file .env exec postgres psql -U postgres -d marketplace
   ```

   ```sql
   SELECT b.account_id, a.owner_type, a.owner_id, b.balance, b.updated_at,
          (SELECT COALESCE(sum(amount), 0) FROM ledger_entries e WHERE e.account_id = b.account_id) AS entries
   FROM balances b JOIN accounts a USING (account_id) WHERE b.account_id = <id>;
   ```

   The latest `ledger_transactions` touching the account (`type`, `idempotency_key`, `created_at`) show which flow (reward, Stripe, sale) wrote last.
4. Did `make seed` run on this database before 2026-10-23's fix? If the log also says "closed ledger month changed", every seeded account is mismatched, and LedgerPartitionMissing fires, follow `docs/runbooks/ledger-partitions.md` (Check 4, Fix 1 and 2): the seed wrote into months the job had already closed, and nothing else is wrong.
5. What changed recently: a deploy of marketplace, or someone running SQL by hand. The ledger tables refuse updates and deletes, and a transaction that doesn't sum to zero can't commit, so a mismatch almost always means `balances` was written outside `LedgerService`.

## Fix

- Don't edit `ledger_entries` or `ledger_transactions`: they're append-only on purpose, and the triggers refuse it.
- If `balances` drifted from correct entries (a manual `UPDATE`, a bug outside the posting path), set the balance back to the sum of its entries in one transaction, then confirm the next run logs `ledger reconciled`.
- If the entries themselves are wrong (a bug posted the wrong amount), post a reversing transaction through the service's code path, never by hand. Freeze trading first if the bug is still live: stop new purchases by scaling marketplace to zero, or block `/api/market/` at the gateway.
- The alert clears on the first clean run (every minute).

## Afterwards

Record the cause and the affected accounts in the evidence log, add a test that reproduces the path that wrote the bad balance, and if it was a manual change, say who ran what. Drill: `scripts/drills/alerts.py` makes this alert fire on purpose with a 1-coin mismatch on a drill user.
