# Runbook: WebhooksFailing, WebhookQueueStuck

- Severity: warning
- Dashboard: [Async pipeline, Webhooks](http://localhost:3000/d/da-async-pipeline)

## What it means

These alerts watch the notifier's own health, not developers' servers (design doc D43). A single developer's endpoint being down is normal: its deliveries retry 8 times and then go to the dead-letter queue, and no alert pages on that.

- **WebhooksFailing**: more than half of all delivery attempts fail, with at least 50 failures in 15 minutes, for 15 minutes. That many failures across the board point at our sender or network: DNS, egress, TLS, the SSRF policy refusing everything.
- **WebhookQueueStuck**: the oldest due delivery has waited more than 300 s for a worker (`notifier.queue.oldest_age`). The workers stopped or Postgres is slow.

## Check

1. Failure reasons: `{service_name="notifier"} |= "webhook failed"` (and `"webhook dead"`) in Loki; each attempt is a `notifier.deliver` span with `http.response.status_code` and `result`. Many different endpoints failing the same way (timeouts, DNS errors) means the problem is ours.
2. Workers: `make logs s=notifier`; is the dispatcher still claiming? A stopped dispatcher leaves the queue age growing while `notifier.deliveries` drops to 0.
3. Postgres: slow queries or locks on `deliveries` (the `Claim` query uses `deliveries_due_idx`).
4. Locally, the `webhook-receiver` test service (`localhost:9000`) shows what it received; `scripts/drills/webhooks.py` exercises the whole path.

## Fix

- Our egress or DNS broken: fix the network; deliveries retry with backoff and catch up.
- Dispatcher stopped: restart the notifier. Claimed deliveries are leased, so ones a dead worker held are taken again after the lease.
- Postgres slow: fix Postgres; check that `deliveries_due_idx` is used (`EXPLAIN`).

## Afterwards

Note how many deliveries went DEAD during the incident: those developers lost events, and the DLQ (`notifier.dlq.v1`) has their `DeliveryDead` for support.

## In AWS

The same checks with the AWS commands in [Runbooks in AWS](README.md): logs in Grafana (`make aws-grafana`) or CloudWatch Logs, restarts with `aws ecs update-service --force-new-deployment`, and the stores through a Session Manager shell on the data host.
