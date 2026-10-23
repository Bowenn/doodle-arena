# Runbook: TipEscrowMismatch

- Severity: critical
- Dashboard: [Marketplace, Tips](http://localhost:3000/d/da-marketplace), "Escrow shards out of balance" (in AWS, the same dashboard in the deployed Grafana)

## What it means

A tip moves coins into one of 16 escrow accounts (`tips_escrow:0` to `tips_escrow:15`, ADR 0021). They stay there until a settlement pays them to the round's drawers or a refund returns them. So each shard's balance must equal the sum of its `PENDING` tips. The reconciliation job found one or more shards where it doesn't (`market.tips.escrow_mismatch > 0`).

Escrow accounts are system accounts, so no `CHECK` stops one from going negative. This check is their guard.

Some tipped coins were lost or doubled, or a tip changed status without its posting. Settlement trusts these balances, so a shard that holds less than its tips would pay drawers coins that never came in.

## Check

1. The reconciliation log names the shards. In Grafana Explore (Loki): `{service_name="marketplace"} |= "invariants broken"`, field `escrow_mismatched_shards`.
2. Compare one shard with its tips, read-only:

   ```bash
   docker compose -f infra/docker-compose.yml --env-file .env exec postgres psql -U postgres -d marketplace
   ```

   ```sql
   SELECT a.owner_id, b.balance,
          (SELECT COALESCE(sum(amount), 0) FROM tips t WHERE t.status = 'PENDING' AND t.shard = <k>) AS pending
   FROM balances b JOIN accounts a USING (account_id) WHERE a.owner_id = 'tips_escrow:<k>';
   ```

3. Look at the shard's latest tips and their postings:
   - Every `PENDING` tip has a `tx_id` (tipper to escrow), and every closed one has a `closing_tx_id`.
   - Look for tips whose status changed without a closing posting that took their coins out:

   ```sql
   SELECT t.tip_id, t.status, t.amount, t.tx_id, t.closing_tx_id, l.type
   FROM tips t LEFT JOIN ledger_transactions l ON l.tx_id = t.closing_tx_id
   WHERE t.shard = <k> ORDER BY t.created_at DESC LIMIT 50;
   ```

4. Is LedgerImbalance firing too? Then the shard's balance itself is wrong (see that runbook). If not, the ledger is consistent and the `tips` table is the one that drifted.
5. What changed recently: a marketplace deploy, or SQL run by hand on `tips`.

## Fix

- **A tip's status edited by hand:** set it back to what its postings say. If it has no `closing_tx_id`, it is `PENDING`; otherwise it is `SETTLED` or `REFUNDED`, by the closing posting's type. Do this in one transaction, then wait for the next reconciliation run (every minute).
- **A posting bug:**
  1. Stop tips first: block `POST /api/market/tips` at the gateway, or scale marketplace to zero.
  2. Fix the bug.
  3. Correct the shard through a reversing ledger transaction in the service's code path, never by editing `balances` or ledger rows.
- Never pay a round's drawers by hand. Let the settlement do it once the shard is right again.

## Afterwards

- Record the cause, the shards and the tips involved in the evidence log.
- Add a test to `TipSettlementTests` that reproduces it.
