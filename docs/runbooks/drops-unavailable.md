# Runbook: DropsUnavailable

- Severity: warning
- Dashboard: [Marketplace, Drops](http://localhost:3000/d/da-marketplace), "Drop requests" (`unavailable`)

## What it means

Drop requests have answered `503 DROPS_UNAVAILABLE` for 5 minutes. The marketplace can't reach Redis, or Redis's scripts fail.

Redis is the drops' gate (ADR 0022): it holds each drop's inventory, waiting room and the per-user rate limits. Without it, drops fail closed on purpose. Nobody can join or buy, and nothing is sold that Postgres can't stand behind.

Everything else in the marketplace keeps working: listings, purchases, tips, wallets.

## Check

1. **Is Redis up?** `make ps` (`redis`). In AWS, the data host's containers.
2. **Can the marketplace log in?** Its user is `marketplace`, limited to `market:*` (`infra/redis/start.sh`). A missing `REDIS_MARKETPLACE_PASSWORD` or an old Redis ACL file refuses it. Look in `make logs s=marketplace` for `Redis ... failed`, and run `make data-acl-check`.
3. **Is Redis slow?** Commands time out after 500 ms. Look at "Drop scripts p99" on the dashboard and Redis's own latency.

## Fix

- **Redis down:** start it (`make up`). The marketplace reconnects by itself.
  - If Redis came back without its data, the sweeper rebuilds each open drop's inventory from Postgres within 10 seconds ("drop inventory missing in Redis; rebuilt"). Paid buyers are relearned on their next attempt, because Postgres refuses a second edition.
  - Waiting-room places are lost, so buyers must join again.
- **ACL:** recreate Redis after a change to `infra/redis/start.sh` or the password (the ACL file is written at start).
- **If a drop's time ran out during the outage,** it closes as usual and the sweeper pays what sold. Decide with the team whether to schedule a new drop for the editions left.

## Afterwards

- Record how long drops were unavailable and why.
- If it happened during a drop, look at `market.drops.leaked_reservations`: reservations caught mid-purchase are given back by the sweeper after a minute.
