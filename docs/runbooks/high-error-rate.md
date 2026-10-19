# Runbook: HighErrorRate

- Severity: warning
- Dashboard: [Service overview, Server errors](http://localhost:3000/d/da-overview)

## What it means

A service answered more than 1% of its requests with a server error for 5 minutes (HTTP 5xx, or gRPC `UNKNOWN`, `DEADLINE_EXCEEDED`, `INTERNAL`, `UNAVAILABLE` or `DATA_LOSS`), with at least 20 requests in the window. The alert's `service_name` label says which service. Players or other services are getting failures.

## Check

1. Which routes fail: in Explore (Prometheus), `sum by (http_route, http_response_status_code) (rate(http_server_request_duration_seconds_count{service_name="<service>", http_response_status_code=~"5.."}[5m]))`.
2. The errors themselves: the overview's "Errors and warnings" panel, or `{service_name="<service>"} | detected_level="error"` in Loki. Each line's `trace_id` opens the failing request's trace, which shows whether the error is the service's own or a dependency's (Postgres, Redis, Kafka, another service).
3. A dependency: a 503 from `ai` usually means the AI provider (or the stub's `fail` mode: `scripts/ai-mode.sh` shows it); a burst of 500s across several services usually means Postgres or Redis.
4. What changed: compare `service_version` on the panels; a new version starting at the same time as the errors is the first suspect.

## Fix

- A bad deploy: roll back to the previous version.
- A dependency down: fix the dependency; the service recovers on its own.
- One broken route: if it's not on a critical path, it can wait for a fix; if it is (purchases, joining rooms), roll back.

## Afterwards

Add a test for the failing path, and note the cause. Drill: `scripts/drills/alerts.py` makes this alert fire on purpose with `ai`'s `fail` mode.
