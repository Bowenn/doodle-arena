# Runbook: ReconciliationStale

- Severity: warning
- Dashboard: [Marketplace, Invariants](http://localhost:3000/d/da-marketplace)

## What it means

`market.ledger.imbalance` hasn't been reported for 5 minutes. The reconciliation job stops reporting its three gauges when no run has succeeded for 3 minutes, so a dead job shows as missing data instead of a reassuring 0. While this fires, LedgerImbalance and OwnershipViolation are blind.

## Check

1. Is marketplace running? `make ps`, and `make logs s=marketplace`.
2. Did runs fail? In Loki, `{service_name="marketplace"} |= "reconcil"` at warn or error: usually a database error (Postgres down, a lock timeout, a statement timeout on a large ledger).
3. Is metric export broken instead? If no marketplace metric reaches Prometheus (`target_info{service_name="marketplace"}`), the problem is OpenTelemetry export, not the job: check otel-lgtm's health.

## Fix

- Marketplace down: restart it (`docker compose -f infra/docker-compose.yml --env-file .env up -d marketplace`) and watch for `ledger reconciled`.
- Postgres down or full: fix Postgres first; the job recovers on its own.
- A run that times out on a large ledger: raise its timeout, or make the query incremental (a Stage 6 task, with partitioning).

## Afterwards

If the job was down for long, look at the invariant gauges once it reports again, and note the gap in the evidence log.
