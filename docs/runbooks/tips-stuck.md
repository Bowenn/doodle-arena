# Runbook: TipsStuck

- Severity: warning
- Dashboard: [Marketplace, Tips](http://localhost:3000/d/da-marketplace), "Oldest tip in escrow" and "Tips out of escrow"

## What it means

A tip has waited in escrow for more than 15 minutes, for 5 minutes in a row (`market.tips.oldest_pending > 900` s).

A tip normally leaves escrow in one of three ways (ADR 0021):

- when its round's `RoundFinished` is settled, within seconds of the round's end;
- through the sweeper, every 10 s, if it was late (it committed after its round was settled);
- as a refund after 10 minutes, if its round never finished.

So something stopped all three. The tipper's coins are safe in escrow, but neither the drawer nor the tipper has them.

## Check

1. **Is the sweeper running?** Look for `{service_name="marketplace"} |= "tip sweep failed"` in Loki, and check that the marketplace is up (`make ps`).
2. **Is the round consumer keeping up?** Look at the "Rounds received" panel and at consumer lag for group `marketplace` on `game.rounds.v1` (Kafka dashboard). A record the marketplace keeps refusing (`result="failed"` without a skip) is retried forever and holds the partition. Its log shows `record refused by the database`.
3. **The oldest tips and their rounds:**

   ```sql
   SELECT t.tip_id, t.match_id, t.round_no, t.created_at, r.created_at AS round_recorded
   FROM tips t LEFT JOIN tip_rounds r USING (match_id, round_no)
   WHERE t.status = 'PENDING' ORDER BY t.created_at LIMIT 20;
   ```

   - If `round_recorded` is set, the round is known: the sweeper should have paid the tip, so look at its errors.
   - If `round_recorded` is empty, the refund should have closed it: look at its errors (for example a lock that never frees).

## Fix

- **The marketplace or its sweeper is down:** restart it. The sweeper picks every tip up again on its next run.
- **A poison round record holds the partition:** fix the cause. A `RoundFinished` that breaks its contract is already skipped and counted as `failed`; a database refusal is not skipped.
- **A long lock on `tips` or `tip_rounds`:** find it in `pg_stat_activity` (`wait_event_type = 'Lock'`) and end the session that holds it.

The alert clears once the oldest tip is younger than 15 minutes.

## Afterwards

- Record how long tips waited and why.
- If many refunds were due at once (a long outage), check that the "Tips out of escrow" panel shows them draining in batches of 100.
