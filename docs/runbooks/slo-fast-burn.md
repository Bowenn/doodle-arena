# Runbook: SLOAvailabilityFastBurn, SLOLatencyFastBurn

- Severity: critical
- Dashboard: [SLOs](http://localhost:3000/d/da-slo) ("Availability burn rate", "Latency burn rate"); the [service overview](http://localhost:3000/d/da-overview) for the service behind it

## What it means

Over both the last hour and the last 5 minutes, API requests failed with a 5xx (availability) or took longer than 500 ms (latency) 14.4 times faster than the 30-day SLO allows. The SLOs are 99.5% without a 5xx and 99% under 500 ms. At this rate, 2% of the month's budget goes every hour, and the whole budget in about two days. Players are seeing errors or slow pages now: this is an incident, not a trend.

## Check

1. **Which service?** On the service overview, the server-error share and p95 latency per service: one service, or all of them (the data host, the network)?
2. **What changed recently?**
   - **Deploy markers** (the dashed vertical lines, tag `deploy`): a release in the last hour is the first suspect.
   - **The other alerts** firing with it: GameServerDown, ReplicaLagHigh, KafkaConsumerLag and the store alerts often explain an SLO burn.
3. **Errors:** Loki, `{service_name="<service>"} | level="error"`, and Tempo's errored traces (tail sampling keeps every one). An error trace shows the failing hop.
4. **Latency:** Tempo's slow traces (all traces over 500 ms are kept): the slowest span says whether it's Postgres, Redis, Kafka or another service.

## Fix

- **A bad release:** roll back. Locally, `git checkout` the previous commit and `make up`; in AWS, `make aws-push` and `aws-up` from the previous commit (the deploy marker names it).
- **A store is down or full** (Postgres, Redis, Kafka, Elasticsearch): follow that alert's runbook; the SLO recovers on its own.
- **Overload** (load test or real traffic): the load-test dashboard shows saturation. Add capacity (game servers, `WEB_CONCURRENCY` for assets, the marketplace's size in `infra/terraform/stack/ecs.tf`) or shed load.
- **Nothing obvious, one service:** restart its container or ECS task, and keep the traces of the first errors.

## Afterwards

- Record the cause, how long the burn lasted and how much budget it cost (the "budget left" panels) in the evidence log.
- If a release caused it, add the missing test.
