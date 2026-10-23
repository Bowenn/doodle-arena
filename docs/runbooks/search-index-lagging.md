# Runbook: SearchIndexLagging

- Severity: warning
- Dashboard: [Async pipeline, Search](http://localhost:3000/d/da-async-pipeline), "Index lag" and "Documents indexed"

## What it means

Search documents are written more than 60 seconds after their events (p95), for 10 minutes (ADR 0023). New drawings, new listings and sales show up late in search.

Search results stay truthful: each hit is checked against Postgres, so a rejected drawing never shows. But a sold drawing can still look listed, and a new one can be missing.

## Check

1. **Is `assets-indexer` running?** Check `make ps`, then `make logs s=assets` for "search record not indexed yet; retrying". That line means it can't reach Postgres or Elasticsearch, and it retries the same record (see SearchUnavailable for Elasticsearch).
2. **Is the group behind?** Look at the `assets-search` lag on the "Index lag" panel.
   - Lag growing means the indexer is slower than the events, or stopped.
   - A burst (for example the marketplace's republish of every active listing) lags for a while, then drains.
3. **Was it a republish?** `POST /api/market/listings/republish` writes one event per active listing. On seeded data that is 50,000 records, which take a few minutes to index once. This is expected after a deploy or a restore.

## Fix

- **Stopped or crashing:** restart it (`make up`, or the ECS service). It resumes from its committed offsets; nothing is lost.
- **Elasticsearch slow or down:** see SearchUnavailable.
- **The documents are wrong or missing after a long outage:** rebuild from Postgres (safe while the indexer runs):

  ```bash
  docker compose -f infra/docker-compose.yml --env-file .env exec assets-indexer python manage.py reindex_search
  ```

## Afterwards

- Record the lag and its cause.
- If the indexer can't keep up with normal traffic, measure one record's time (the consumer span) before changing anything.
