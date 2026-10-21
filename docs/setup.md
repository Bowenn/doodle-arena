# Doodle Arena: setup

Things the humans prepare. Claude Code can install packages and write config, but it can't create accounts or secrets for you.

## 1. Start the repo

1. Create an empty GitHub repository (for example `doodle-arena`) and clone it.
2. Copy the kickoff files into the repo root, keeping their paths: `CLAUDE.md`, `.claude/` and `docs/`.
3. Commit them: `git add . && git commit -m "docs: project kickoff"`, then push.
4. Open Claude Code in the repo root and run `/context`. `CLAUDE.md` should appear under the memory files.
5. Paste the first prompt from `docs/KICKOFF_PROMPT.md`.

`CLAUDE.md` already exists, so you don't need `/init`. If you run it anyway, it suggests improvements instead of overwriting the file.

## 2. Tools on your machine

`make test` uses the host toolchains, so the first group is required from Stage 1.

| Tool | Version | Needed from | Notes |
|---|---|---|---|
| Docker (Desktop, OrbStack or Colima) with Compose | current | Stage 1 | Give the VM at least 8 GB of memory |
| Git | current | Stage 1 | |
| Go | 1.27.x | Stage 1 | Each module's `go.mod` pins the toolchain |
| JDK | Temurin 25 (LTS) | Stage 1 | The Gradle wrapper (9.8.x) downloads Gradle itself |
| Python | 3.14.x, through `uv` (0.13.x or newer) | Stage 1 | `uv` installs the pinned Python per service, from `.python-version`; no system Python is needed |
| Node.js | 24.x LTS, with npm | Stage 1 | `.nvmrc` in `web/` |
| Buf CLI | 1.74.x | Stage 1 | Protobuf lint, breaking-change check and code generation |
| k6 | current | Stage 5 | Load tests |
| Stripe CLI | current | Stage 4 | Forwards test webhooks to your machine |
| AWS CLI | 2.38.x | Stage 5 | Signs in through IAM Identity Center (`aws sso login --profile doodle-arena`); no access keys |
| Terraform | 1.16.x (`required_version = "~> 1.16.0"`, AWS provider `~> 6.69`, lock files committed) | Stage 5 | `make lint s=terraform` also runs tflint 0.64 through `go run` |
| Session Manager plugin for the AWS CLI | current (`brew install --cask session-manager-plugin`) | Stage 5 | The Grafana tunnel (`make aws-grafana`) and shells on the data host |
| `grpcurl`, `kcat` | current | Optional | Debugging; `kcat` can also run as a container |

### Pinned container images

Images are pinned to exact tags in `infra/docker-compose.yml`. Versions were checked on 2026-10-15; bump them deliberately, in their own commit.

| Component | Local image | In AWS |
|---|---|---|
| Postgres | `postgres:18.6-trixie` | Container, or RDS for PostgreSQL 18.6 |
| Redis | `redis:8.10.2-trixie` | Container. ElastiCache offers no Redis 8, only Redis OSS 7.1 or Valkey 8–9, so use only commands that Redis 7 and Valkey also support (Streams, Lua, sorted sets, TTLs all qualify) |
| MongoDB | `mongo:8.0.34` | Container, or DocumentDB with MongoDB 8.0 compatibility |
| Kafka (KRaft) | `apache/kafka:4.2.2` | Container, or MSK 4.2 on Express brokers (MSK provisioned brokers stop at 4.1). Avoid 4.3+ features such as share groups |
| Object storage | `chrislusf/seaweedfs:4.48` | S3 |
| Telemetry | `grafana/otel-lgtm:0.36.0` | See ADR 0003 |
| Kafka lag collector | `otel/opentelemetry-collector-contrib:0.162.0` (the collector version inside otel-lgtm 0.36.0) | The same container, or the collector of the AWS telemetry setup (ADR 0003) |
| Gateway | `nginx:1.30.6-otel` (the variant with the OpenTelemetry and njs modules) | Container behind a load balancer |
| Service build images | `golang:1.27.2-trixie`, `eclipse-temurin:25.0.4.1_1-jdk`, `python:3.14.8-slim-trixie`, `node:24.22.0-trixie-slim` (the web app) | Same images in CI |
| Service runtime images | Go: `gcr.io/distroless/static-debian13:nonroot`, pinned by digest (distroless has no version tags); Java: `eclipse-temurin:25.0.4.1_1-jre`; Python: the build image | Same images on ECS or EC2 |

All of these images are built for both `amd64` and `arm64`, so they run on Apple silicon and on Graviton instances. If Lambda is used in Stage 7, its managed runtimes `python3.14`, `nodejs24.x`, `java25` and `provided.al2023` (for Go) all match these versions.

## 3. Accounts and secrets

Put secrets in `.env`, which is gitignored. `.env.example` lists the variable names without values. Never paste secrets into a chat or a commit.

| What | Where | Needed from | Variables (suggested names) |
|---|---|---|---|
| Google OAuth client (type "Web application"). Authorized redirect URI: exactly `http://localhost:8080/api/auth/google/callback`; authorized JavaScript origin: `http://localhost:8080`. A mismatch shows Google's `redirect_uri_mismatch` error | Google Cloud Console, APIs & Services, Credentials | Stage 1 | `GOOGLE_CLIENT_ID`, `GOOGLE_CLIENT_SECRET` |
| JWT signing key (RSA 2048) | `scripts/gen-jwt-key.sh` creates `secrets/jwt-private.pem` (gitignored); `make up` runs it, and compose mounts it into auth as a secret | Stage 1 | none |
| Join-ticket signing key (Ed25519, ADR 0005) | `scripts/gen-ticket-key.sh` creates `secrets/ticket-private.pem` and `secrets/ticket-public.pem` (gitignored); `make up` runs it, and compose mounts the private key only into the lobby, and the public key into both game servers | Stage 2 | none |
| OAuth state cookie secret | `make up` generates it in `.env` | Stage 1 | `OAUTH_STATE_SECRET` |
| Webhook secret encryption key (notifier, ADR 0014) | `make up` generates it in `.env` | Stage 4 | `NOTIFIER_SECRET_KEY` (random, 32+ characters; losing it makes registered endpoints' secrets unreadable) |
| Stripe account in test mode (or a sandbox); test keys only | Stripe Dashboard | Stage 4 | `STRIPE_SECRET_KEY`, `STRIPE_WEBHOOK_SECRET` (printed by `stripe listen`) |
| Discord channel webhook for alerts | Discord channel settings, Integrations | Stage 5 | `DISCORD_ALERT_WEBHOOK_URL` |
| AWS account with a budget alarm set first | AWS console; see "AWS (task 5.2)" below | Stage 5 | CLI profile `doodle-arena`; never access keys |

The stack is one origin, `http://localhost:8080`. Cookies are not `Secure` locally (`COOKIE_SECURE=false`, because Safari won't send `Secure` cookies to plain-http localhost); they are in AWS. `make up` adds any variable that appears in `.env.example` to an existing `.env` without changing values already there.

### AWS (task 5.2)

The deployment follows ADR 0003 (Option B): ECS on Fargate for the services, one EC2 data host for the stores, created per session and destroyed after it.

Set up once (2026-10-19):

- **Account:** on the Paid plan; root has MFA and no access keys; billing alerts on (billing metrics exist only in us-east-1).
- **Budget first:** `doodle-arena-monthly`, $35 a month, credits excluded (so it tracks real usage), email alerts at 50%, 80%, 100% and 100% forecasted. Created by hand before anything else and kept outside Terraform, so no destroy can remove it.
- **Identity:** IAM Identity Center in us-west-2, MFA at sign-in, user `bowen` with `AdministratorAccess` (4-hour sessions). CLI profile `doodle-arena` (`aws configure sso`); sign in with `aws sso login --profile doodle-arena`. There is no IAM user and no access key anywhere.
- **Region:** us-west-2 (Oregon).
- **Domain:** `icanfly.click`, registered in Route 53 (auto-renew off); its hosted zone was created by the registration and stays between sessions ($0.50 a month).
- **Bootstrap root:** `make aws-bootstrap` creates the Terraform state bucket, one ECR repository per image, empty SSM parameters for the third-party secrets and, since task 6.10 (ADR 0016), the backup bucket `doodle-arena-backups-<account>`, the log group `/doodle-arena/backups` and the generated `/doodle-arena/notifier-secret-key`. Run it once, and again after a change to `infra/terraform/bootstrap/` (the stack reads the notifier's key from there, so `aws-up` fails until it exists); its own state moves into the bucket it creates.
- **Third-party secrets**, put by a human, never pasted into a chat or a file:

  ```bash
  read -rs "VALUE?Paste the value: " && aws ssm put-parameter --profile doodle-arena --overwrite --type SecureString --name /doodle-arena/google/client-id --value "$VALUE"; unset VALUE
  ```

  (zsh; in bash, `read -rsp "Paste the value: " VALUE`). Reading the value first keeps it out of your shell history.

  for `/doodle-arena/google/client-id`, `/doodle-arena/google/client-secret`, `/doodle-arena/stripe/secret-key`, `/doodle-arena/stripe/webhook-secret` and `/doodle-arena/discord/alert-webhook-url`. For Google, add `https://icanfly.click/api/auth/google/callback` as an authorized redirect URI and `https://icanfly.click` as a JavaScript origin of the OAuth client. For Stripe (test mode), add an event destination in the account (or sandbox) the secret key belongs to: a webhook endpoint `https://icanfly.click/api/market/stripe/webhook` for `checkout.session.completed` and `checkout.session.async_payment_succeeded`, with payload style **Snapshot** (a thin event has no session, so nothing would be credited), and put its signing secret. The destination and its secret outlive sessions. A parameter nobody has put yet holds the placeholder `unset`, which auth and the marketplace treat as not configured (Google sign-in and coin packs answer 503). Tasks read these when they start: after a change, redeploy the service (`aws ecs update-service --cluster doodle-arena --service auth --force-new-deployment`; otel-lgtm reads the Discord URL when the data host boots).

Each session (costs money from `aws-up` to `aws-down`; docs/performance.md has the measured cost per hour):

```bash
make aws-push
```

```bash
DOMAIN=icanfly.click make aws-up
```

```bash
make aws-grafana
```

```bash
DOMAIN=icanfly.click make aws-check
```

```bash
DOMAIN=icanfly.click make aws-down
```

- `make aws-push` builds every image for linux/arm64 and pushes it to ECR, tagged with the commit (the tree must be clean; a tag that is already in ECR is skipped).
- `make aws-up` creates the stack with every service stopped, restores the latest backup into its empty stores (below), runs the auth and assets migrations as one-off tasks, then starts the services and waits until they are stable. It deploys the images of `IMAGE_TAG` (default: the current commit). The load balancer is open to everyone (task 5.5: Kafka has users and ACLs, ADR 0013); `ALLOWED_CIDRS=203.0.113.7/32` narrows it to listed addresses. For people outside the team to sign in, the Google OAuth app must be published (Google Cloud console, "OAuth consent screen", Publish app), or each person added as a test user while it's in testing mode. That is a human step.
- `make aws-grafana` (in a second terminal) forwards Grafana to `http://localhost:3300`, and the collector's OTLP port to `localhost:14318` for k6, through Session Manager (other ports than the local stack's, so both can run). The Panel links in AWS's Discord alerts point at `http://localhost:3300` too, so they open while the tunnel runs; keep the default `GRAFANA_PORT`, or the links miss. Anonymous visitors only view; the admin password is the SSM parameter `/doodle-arena/stack/grafana-admin-password`.
- `make aws-check` runs `make smoke` through the load balancer, then `make telemetry-check` and `make alerts-check` through the tunnel.
- otel-lgtm's health on the data host, which includes Tempo and the collector (they can be killed inside a running container; docs/observability.md, section 1): open a shell with `aws ssm start-session --profile doodle-arena --target "$(aws ec2 describe-instances --profile doodle-arena --filters Name=tag:Name,Values=doodle-data-host Name=instance-state-name,Values=running --query 'Reservations[0].Instances[0].InstanceId' --output text)"`, then `sudo docker ps`. Docker doesn't restart an unhealthy container: `sudo docker restart doodle-data-otel-lgtm-1` does.
- `make aws-down` backs up the session's data, then destroys the stack. The bootstrap root (with the backups), the domain and the budget stay.

#### Backups (task 6.10, ADR 0016)

- **aws-down** stops the gateway, the web app, the lobby, auth and the game servers, waits (at most 5 minutes, `DRAIN_TIMEOUT`) until the consumers, the Celery worker and the outbox relays are idle, stops the rest, then dumps the `auth`, `assets`, `marketplace` and `notifier` databases and assets' MongoDB database on the data host and copies the drawings bucket, all into `s3://doodle-arena-backups-<account>/backups/<UTC time>/`. Each file's size and SHA-256 are checked in S3, and `manifest.json` is written last. Only then does `terraform destroy` run. A failure prints `BACKUP FAILED`, leaves the stack up with its services at 0 (it still costs money), and exits: fix the cause and rerun, or end without keeping the data with `SKIP_BACKUP=1 DOMAIN=icanfly.click make aws-down`. The newest 10 backups are kept; a removed one stays recoverable as a noncurrent version for 30 days.
- **aws-up** on a fresh stack restores the newest backup that has a manifest, before the migrations: `BACKUP_ID=<id>` picks another (`aws s3 ls --profile doodle-arena s3://doodle-arena-backups-<account>/backups/`), `RESTORE=none` starts empty. It refuses a backup with a migration the deployed code (`IMAGE_TAG`) doesn't have, and fails if any table's rows differ from the manifest's after the load. The runbook is `docs/runbooks/aws-backup.md`.
- **What comes back:** accounts, drawings, wallets, ownership, listings, purchases and webhook endpoints. Signing keys still rotate every session, so everyone signs in again. Rooms, tickets and the weekly leaderboard (Redis) and the Kafka topics start empty.
- **Where it shows:** CloudWatch metrics in the namespace `DoodleArena/Backups` and log lines in `/doodle-arena/backups` (docs/observability.md), and the Backups row of the service overview in AWS's Grafana.
- **Restores download through presigned URLs** that `aws-up` signs with your SSO session (valid one hour): the data host's role may only put objects in the backup bucket. The URLs are part of the SSM Run Command, so they stay in the command history (`aws ssm list-commands`) until they expire.

### Discord (task 5.1)

Grafana sends alerts to one Discord channel. Without a webhook the stack still runs, and alerts go to the `webhook-receiver` test service instead (`http://localhost:9000/admin/alerts` shows them).

1. In Discord, create a channel for alerts (for example `#doodle-arena-alerts`), then Edit Channel, Integrations, Webhooks, New Webhook, and Copy Webhook URL.
2. Put it into `.env` as `DISCORD_ALERT_WEBHOOK_URL`. It's a secret: anyone with the URL can post to the channel. Don't paste it into chat, an issue or a log; if it leaks, delete the webhook in Discord and make a new one.
3. Recreate otel-lgtm so Grafana reads it: `docker compose -f infra/docker-compose.yml --env-file .env up -d otel-lgtm`.
4. Check it: `python3 scripts/drills/alerts.py ledger` makes LedgerImbalance fire (and clear) within a few minutes; the channel gets a FIRING and a RESOLVED message.

### Stripe (task 4.5)

Coin packs use Stripe Checkout in test mode. Without keys the stack still runs; coin packs answer 503.

1. Create a Stripe account (or a sandbox) and stay in test mode. Copy the test secret key (`sk_test_…`) into `.env` as `STRIPE_SECRET_KEY`.
2. Install the Stripe CLI (`brew install stripe/stripe-cli/stripe`) and run `stripe login`.
3. Forward webhooks to the gateway: `stripe listen --events checkout.session.completed,checkout.session.async_payment_succeeded --forward-to localhost:8080/api/market/stripe/webhook` (current CLI versions require `--events`). It prints a signing secret (`whsec_…`) that stays the same for your account; put it into `.env` as `STRIPE_WEBHOOK_SECRET`.
4. Recreate the marketplace so it reads them: `docker compose -f infra/docker-compose.yml --env-file .env up -d marketplace`.
5. Pay with Stripe's test card `4242 4242 4242 4242`, any future expiry, any CVC. `stripe events resend <evt_id>` redelivers an event (the marketplace answers `duplicate`).

Two things that silently lose the webhook:

- **The CLI and the key must be the same Stripe account.** `stripe login` may pick your main test account while `STRIPE_SECRET_KEY` belongs to a sandbox: the payment then happens in the sandbox and its event has no listener (`pending_webhooks: 0`). Run the listener as the key's account: `stripe listen --api-key "$(grep '^STRIPE_SECRET_KEY=' .env | cut -d= -f2-)" --events … --forward-to …`, and use the `whsec_` that this listener prints.
- **The marketplace reads `.env` only when it is created.** After changing either value, recreate it (step 4); a marketplace started with an old `whsec_` answers every webhook 400.

Only test keys belong in `.env`; never commit it.

## 4. Working as two people

- Each person works on their own track (see `docs/roadmap.md`) in their own branches.
- Pull `master` (the default branch) before starting a task. Merge through pull requests, and have the other person review changes to `proto/` and anything on a critical path.
- When you run several Claude Code sessions on one machine, give each its own git worktree so their changes don't collide.

## 5. Personal Claude Code preferences

Preferences that only apply to you belong in your own `~/.claude/CLAUDE.md`, not in the shared project file. For example:

```markdown
- I'm not a native English speaker. After answering, briefly point out any English mistakes in my message.
```
