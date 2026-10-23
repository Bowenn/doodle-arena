# Runbook: AssetsCacheDegraded

- Severity: warning
- Dashboard: [Async pipeline, Gallery cache](http://localhost:3000/d/da-async-pipeline), "Cache lookups" (`error`, `skipped`)

## What it means

For 10 minutes, more than half of the gallery cache's lookups met a Redis error, or were skipped because of one (ADR 0024). After an error, each assets process skips the cache for 5 seconds and reads Postgres.

Nothing is wrong for players: every gallery, drawing and Market page still answers, from Postgres (the replica, ADR 0020). Pages are slower, and Postgres takes the load the cache usually absorbs. Watch the replica's lag and Postgres's CPU if traffic is high.

## Check

1. **Is Redis up?** `make ps` (`redis`); in AWS, the data host's containers. A Redis that is down also stops Celery: look for drawings stuck in `processing` (the pipeline's panels on the same dashboard).
2. **Can assets log in?** The cache uses the `assets` Redis user (keys `assets:*`, `infra/redis/start.sh`). A `WRONGPASS` or `NOPERM` in assets' log ("cache unavailable") means the user or its password changed: the container needs a restart after a password change.
3. **Is Redis slow?** The cache gives Redis 100 ms per call. Check its latency and memory:

   ```bash
   docker compose -f infra/docker-compose.yml --env-file .env exec -e REDISCLI_AUTH="$REDIS_ADMIN_PASSWORD" redis redis-cli --user admin info memory
   ```

   Redis has no eviction policy (it holds Celery's queue). Every cache key has a TTL (10 minutes for a drawing, 60 seconds for a page), so the cache alone shouldn't fill it.

## Fix

- **Redis down:** start it (`make up`); the cache comes back by itself within 5 seconds.
- **A wrong password:** restart the assets containers after fixing it.
- **To take the cache out of the picture** while investigating: set `ASSETS_CACHE=off` on the assets web process and recreate it. Every read then goes to Postgres, as before task 6.7b.

## Afterwards

Record how long the cache was degraded and why.
