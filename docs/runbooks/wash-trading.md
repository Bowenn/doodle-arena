# Runbook: WashTradingFlagged

- Severity: warning
- Dashboard: Doodle Arena: analytics (`da-analytics`), "Loops flagged" and "Flags (analytics logs)"

## What it means

The analytics Flink job saw a drawing come back to an account that sold it within 7 days (ADR 0018). `rule=round_trip` is R1: A sold to B, B sold back to A. `rule=cycle` is R3: three or more accounts passed it round, A to B to C and back to A.

That's the pattern of accounts trading among themselves:

- **inflating a price:** each sale sets a new, higher last price;
- **moving coins between accounts:** the buyer pays, the seller gets the coins minus royalties.

A flag is a signal for a human, not a verdict. Two friends trading a drawing back for fun look the same. Nothing is reversed automatically: the sales committed, and the ledger is correct.

## Check

1. Find the flags. The "Flags (analytics logs)" panel, or Loki: `{service_name="analytics"} |= "wash trade flagged"`. Each line has the rule, `drawing_id`, the accounts in order, the sale ids, the volume in coins and the `flag_id`.
2. Read the events themselves, as `dev-tools` (locally) or through Session Manager in AWS. The `flag_id` is the same for a repeat (at-least-once), so duplicates are one loop.

   ```bash
   (cd tools/eventctl && KAFKA_DEV_TOOLS_PASSWORD=... go run . decode -topic analytics.flags.v1 -n 20)
   ```

3. Look at the sales in the marketplace database (a sale is a sold listing; `sale_id` is its ledger transaction): the prices, and when each happened.

   ```sql
   SELECT listing_id, drawing_id, seller_id, buyer_id, price, closed_at
   FROM listings WHERE sale_id IN ('<sale id>', '<sale id>') ORDER BY closed_at;
   ```

   Rising prices around a loop, or prices far above the drawing's other sales, point at price inflation. A buyer whose coins came from a fresh Stripe purchase, followed by a sale to an account that then cashes out, points at moving coins.
4. Check the accounts: their age, their other trades, and whether the same accounts appear in other flags (search the logs by user id).

## Fix

- One loop between two friends at ordinary prices: nothing to do. Note it if it repeats.
- A repeated pattern between the same accounts, or prices that climb around the loop: record the accounts and the `flag_id`s for a moderator. There is no automatic penalty and no admin action for accounts yet (Stage 7).
- Many flags at once from accounts you don't recognize: a script is trading. Check `market.purchases` on the marketplace dashboard and the gateway's rate limits.
- Flags you expected and didn't get: see [analytics-down.md](analytics-down.md). The job may not be running, or it may have started empty after a restore.

## Afterwards

- Record real cases, and whether they were abuse, in the evidence log; they tell whether 7 days and these two rules are the right net.
- Repeated false positives (friends trading) are a reason to add a minimum price or volume to the rule, as a change to ADR 0018.
