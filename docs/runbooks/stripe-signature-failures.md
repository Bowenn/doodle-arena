# Runbook: StripeSignatureFailures

- Severity: warning
- Dashboard: [Marketplace, Stripe webhooks](http://localhost:3000/d/da-marketplace)

## What it means

More than 5 Stripe webhooks in 10 minutes had a signature that didn't verify (`market.stripe.webhooks{result="invalid_signature"}`). Either our signing secret is wrong, so real payments aren't being credited, or someone is sending forged webhooks to credit themselves coins (which the check refuses, as it should).

## Check

1. Did real payments stop crediting? `market.stripe.webhooks{result="processed"}` on the same panel. If processed dropped to 0 when the failures started, it's our secret.
2. The source: the gateway's access log and the `market.stripe.webhook` spans. Stripe's own dashboard (Developers, Webhooks) shows failed deliveries for our endpoint if it's our secret.
3. What changed: a new `STRIPE_WEBHOOK_SECRET`, a new webhook endpoint in Stripe, or, locally, a restarted `stripe listen` (it prints a new `whsec_` each time).

## Fix

- Wrong secret: set `STRIPE_WEBHOOK_SECRET` to the endpoint's current signing secret (never paste it into chat or a log) and recreate marketplace. Stripe retries failed webhooks for up to 3 days, so missed payments are credited once the secret is right; the credit is idempotent per session.
- Forged requests: nothing is credited, so there's no money at risk. If the volume is a problem, rate-limit the path at the gateway.

## Afterwards

If payments were delayed, check that every paid session was credited (`stripe_checkouts`), and note the window.

## In AWS

The same checks with the AWS commands in [Runbooks in AWS](README.md): logs in Grafana (`make aws-grafana`) or CloudWatch Logs, restarts with `aws ecs update-service --force-new-deployment`, and the stores through a Session Manager shell on the data host.
