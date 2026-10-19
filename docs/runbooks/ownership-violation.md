# Runbook: OwnershipViolation

- Severity: critical
- Dashboard: [Marketplace, Invariants](http://localhost:3000/d/da-marketplace)

## What it means

`market.ownership.violations > 0`: a drawing's current owner in `ownership` doesn't match the last transfer in its provenance (`ownership_events`). Someone could sell a drawing they no longer own, or the real owner can't sell it.

## Check

1. The reconciliation log line: `{service_name="marketplace"} |= "invariants broken"` in Loki lists up to 20 `violating_drawing_ids`.
2. For one drawing (as the database owner: `docker compose -f infra/docker-compose.yml --env-file .env exec postgres psql -U postgres -d marketplace`):

   ```sql
   SELECT * FROM ownership WHERE drawing_id = '<id>';
   SELECT * FROM ownership_events WHERE drawing_id = '<id>' ORDER BY created_at;
   ```

3. The last sale of that drawing: its `market.purchase` trace (search Tempo for `drawing.id`) shows whether the transfer and the coins moved in one transaction.
4. Recent deploys of marketplace, and any manual SQL.

## Fix

- A purchase moves coins and ownership in one transaction, so a mismatch means a write outside it. Stop sales of the affected drawings (cancel their open listings) while you investigate.
- Correct `ownership` to the owner the provenance says, in one transaction, and record why in the evidence log. Never rewrite `ownership_events`: it's the history.
- If coins moved without the transfer (or the reverse), treat it as a LedgerImbalance too and follow that runbook.

## Afterwards

Add a test for the path that broke the invariant, and note the drawings and owners involved in the evidence log.
