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
| AWS CLI and Terraform | current | Stage 5 | |
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
| Stripe account in test mode (or a sandbox); test keys only | Stripe Dashboard | Stage 4 | `STRIPE_SECRET_KEY`, `STRIPE_WEBHOOK_SECRET` (printed by `stripe listen`) |
| Discord channel webhook for alerts | Discord channel settings, Integrations | Stage 5 | `DISCORD_ALERT_WEBHOOK_URL` |
| AWS account with a budget alarm set first | AWS console | Stage 5 | AWS CLI profile; never commit keys |

The stack is one origin, `http://localhost:8080`. Cookies are not `Secure` locally (`COOKIE_SECURE=false`, because Safari won't send `Secure` cookies to plain-http localhost); they are in AWS. `make up` adds any variable that appears in `.env.example` to an existing `.env` without changing values already there.

### Stripe (task 4.5)

Coin packs use Stripe Checkout in test mode. Without keys the stack still runs; coin packs answer 503.

1. Create a Stripe account (or a sandbox) and stay in test mode. Copy the test secret key (`sk_test_…`) into `.env` as `STRIPE_SECRET_KEY`.
2. Install the Stripe CLI (`brew install stripe/stripe-cli/stripe`) and run `stripe login`.
3. Forward webhooks to the gateway: `stripe listen --forward-to localhost:8080/api/market/stripe/webhook`. It prints a signing secret (`whsec_…`) that stays the same for your account; put it into `.env` as `STRIPE_WEBHOOK_SECRET`.
4. Recreate the marketplace so it reads them: `docker compose -f infra/docker-compose.yml --env-file .env up -d marketplace`.
5. Pay with Stripe's test card `4242 4242 4242 4242`, any future expiry, any CVC. `stripe events resend <evt_id>` redelivers an event (the marketplace answers `duplicate`).

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
