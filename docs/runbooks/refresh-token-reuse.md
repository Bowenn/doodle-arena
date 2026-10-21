# Runbook: RefreshTokenReuse

- Severity: warning
- Dashboard: [Service overview, Sign-ins and refreshes](http://localhost:3000/d/da-overview)

## What it means

More than 3 rotated refresh tokens were replayed in 10 minutes (`auth.refreshes{result="reuse_detected"}`). Each one revokes that user's whole token family, disables their webhook endpoints (ADR 0015) and logs them out. Two causes: tokens are being stolen and replayed, or a client is refreshing concurrently (two tabs, a retry) and logging its own users out.

## Check

1. One user or many? `{service_name="auth"} |= "reuse"` in Loki, grouped by `user_id`. Many users at once after a web deploy points at a client bug; a few users, from addresses they don't normally use, points at theft.
2. The web client: refreshes must be single-flight (`web/src/auth.ts`). A new version that refreshes from two places at once causes exactly this.
3. Revocations reaching the notifier: `notifier.events{result="revoked"}` and `notifier.endpoints.disabled` should rise with the reuse count. If not, check AuthOutboxStuck.

## Fix

- Client bug: roll back the web app; the affected users sign in again.
- Theft: the revocation already cut the thief off (refresh tokens, access tokens issued before the revocation, webhook endpoints). Find how the tokens leaked (a log, an XSS, a shared device) and fix that.

## Afterwards

Note how many users were logged out and the cause. If this fires during normal use, raise the threshold with a note in observability.md.

## In AWS

The same checks with the AWS commands in [Runbooks in AWS](README.md): logs in Grafana (`make aws-grafana`) or CloudWatch Logs, restarts with `aws ecs update-service --force-new-deployment`, and the stores through a Session Manager shell on the data host.
