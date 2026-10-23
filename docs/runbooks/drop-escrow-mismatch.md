# Runbook: DropEscrowMismatch

- Severity: critical
- Dashboard: [Marketplace, Drops](http://localhost:3000/d/da-marketplace), "Drop escrow shards out of balance"

## What it means

A drop purchase moves the buyer's coins into one of 16 escrow accounts (`drop_escrow:0` to `drop_escrow:15`, ADR 0022). They stay there until the sweeper pays the drop's sales to the drawing's authors. So each shard's balance must equal the price of its `PAID` editions (`drop_editions`). The reconciliation job found shards where it doesn't (`market.drops.escrow_mismatch > 0`).

Escrow accounts are system accounts, so no `CHECK` stops one from going negative; this check is their guard. Coins were lost or doubled, or an edition changed status without the posting that moves its coins.

## Check

1. The reconciliation log names the shards. In Loki: `{service_name="marketplace"} |= "invariants broken"`, field `drop_escrow_mismatched_shards`.
2. Compare a shard with its editions, read-only:

   ```sql
   SELECT b.balance,
          (SELECT COALESCE(sum(price), 0) FROM drop_editions WHERE status = 'PAID' AND shard = <k>) AS paid
   FROM balances b JOIN accounts a USING (account_id) WHERE a.owner_id = 'drop_escrow:<k>';
   ```

3. Every `PAID` edition has a `tx_id` (buyer to escrow), and every `SETTLED` one a `settle_tx_id` (escrow to the authors). Look for editions whose status doesn't match their postings:

   ```sql
   SELECT e.drop_id, e.edition_no, e.status, e.price, l.type
   FROM drop_editions e LEFT JOIN ledger_transactions l ON l.tx_id = e.settle_tx_id
   WHERE e.shard = <k> ORDER BY e.purchased_at DESC LIMIT 50;
   ```

4. Is LedgerImbalance firing too? Then the balance itself is wrong (see that runbook). If not, `drop_editions` drifted.

## Fix

- **An edition's status edited by hand:** set it back to what its postings say. Without a `settle_tx_id` it is `PAID`; otherwise it is `SETTLED`. Use one transaction, then wait for the next reconciliation run.
- **A bug in a posting:** stop drop purchases first (block `POST /api/market/drops/*/purchase` at the gateway). Then fix the bug, and correct the shard with a reversing ledger transaction through the service's code path, never by editing `balances` or ledger rows.
- Never pay an author by hand. The sweeper pays once the shard is right.

## Afterwards

- Record the cause and the editions involved in the evidence log.
- Add a test to `DropSettlementTests` that reproduces it.
