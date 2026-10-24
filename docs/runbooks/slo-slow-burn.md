# Runbook: SLOAvailabilitySlowBurn, SLOLatencySlowBurn

- Severity: warning
- Dashboard: [SLOs](http://localhost:3000/d/da-slo) ("Availability burn rate", "Latency burn rate")

## What it means

Over both the last 6 hours and the last 30 minutes, API requests failed (5xx) or were slower than 500 ms at 6 times the rate the 30-day SLO allows. At this rate the budget runs out in about five days. Nothing is necessarily broken right now. A small, steady problem (one endpoint failing, a slow query on growing data, a degraded dependency) is eating the month's budget.

## Check

1. **Which service and which route?** Group the SLI by `service_name` and `http_route` in Explore. For example, the error share per route over 6 hours:

   ```promql
   sum by (service_name, http_route) (rate(http_server_request_duration_seconds_count{http_route=~".*/api/.*", http_response_status_code=~"5.."}[6h]))
     / sum by (service_name, http_route) (rate(http_server_request_duration_seconds_count{http_route=~".*/api/.*"}[6h]))
   ```

2. **When did it start?** Look at the burn-rate panel against the deploy markers.
3. **Slow requests:** Tempo's traces over 500 ms (tail sampling keeps all of them). A query that got slower as data grew shows up as one slow database span (`make explain` reproduces it locally).
4. **Dependencies:** is a cache degraded (AssetsCacheDegraded), is the replica lagging (ReplicaLagHigh), is search down (SearchUnavailable)?

## Fix

- **A route with a regression:** fix it on a branch, or roll back the release that introduced it.
- **A slow query:** add the index or rewrite the query, after measuring with `make explain` (task 6.6).
- **A degraded dependency:** follow its own runbook.
- **The budget is spent and the cause can't be fixed this week:** say so in the team channel and hold risky releases until it recovers. That's what the budget is for.

## Afterwards

Record the route, the cause and the budget spent in the evidence log. A recurring slow burn on one route is a task for the roadmap.
