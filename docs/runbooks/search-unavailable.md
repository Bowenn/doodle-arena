# Runbook: SearchUnavailable

- Severity: warning
- Dashboard: [Async pipeline, Search](http://localhost:3000/d/da-async-pipeline), "Search requests" (`unavailable`)

## What it means

Searches have answered `503 SEARCH_UNAVAILABLE` for 5 minutes: assets' web process can't reach Elasticsearch, or Elasticsearch refuses it (ADR 0023).

Only search is affected. The gallery and the market keep their own lists, which come from Postgres and the marketplace, and nothing is lost. The indexer retries its records until Elasticsearch is back.

## Check

1. **Is Elasticsearch up?**
   - Locally it is an opt-in profile: was the stack started with `PROFILES=...,search make up`? Then `make ps` (`elasticsearch`).
   - In AWS, the data host's containers.
2. **Is it healthy?** As the superuser:

   ```bash
   curl -s -u "elastic:$ELASTIC_PASSWORD" 'http://localhost:9200/_cluster/health?pretty'
   ```

   - Red or yellow with unassigned shards: look at its log.
   - A full disk makes indices read-only: the compose file turns the disk threshold off locally, but not the OS's own limits.
3. **Can assets log in?** The web process uses the user `assets`, which can only read `drawings*`. `elasticsearch-init` creates it: after a new volume or a changed password, that job must have run (`make logs s=elasticsearch-init`). A `401` or `403` in assets' log ("search unavailable") points here.
4. **Memory:** the heap is 512 MB in a 1 GB container. An OOM kill shows in `docker inspect` (`OOMKilled`) or in the VM's `dmesg`.

## Fix

- **Down:** start it (`PROFILES=live,analytics,search make up`; in AWS, in a Session Manager shell on the data host, `sudo docker start doodle-data-elasticsearch-1`). Assets reconnects by itself.
- **No users** (a new volume): rerun `elasticsearch-init`.
- **The index was lost:** rebuild it from Postgres in the indexer's container:

  ```bash
  docker compose -f infra/docker-compose.yml --env-file .env exec assets-indexer python manage.py reindex_search
  ```

## Afterwards

Record how long search was down and why.
