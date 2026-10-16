# Interview notes

Talking points collected after each task (the `/milestone` skill appends to this file). For each one: the problem, the design choice, the alternative that was rejected and why, and how the design fails.

Read this before interviews, and practice with the interview drill prompt in `docs/KICKOFF_PROMPT.md`.

## Stage 1: Foundations

### 1.1 Repo skeleton and local stack

- **Database per service, enforced rather than just agreed.** One Postgres server, but each service has its own database and its own login role, and `PUBLIC` loses `CONNECT`. The `auth` role gets "permission denied" on `marketplace`. Rejected: a shared user with schemas, which is easier but lets one service quietly read another's tables. Failure mode: the Postgres server is still a shared resource, so one service's heavy query can slow every other service. Isolation is logical, not physical.
- **The gateway resolves services at request time.** Nginx normally resolves upstream host names once at startup and refuses to start if one is missing. With Docker's DNS resolver and a variable in `proxy_pass`, the gateway starts and keeps routing while a service is down, and returns 502 only for that service (tested by stopping `ai`). Trade-off: a DNS lookup every 10 seconds per name, and the resolver address is environment-specific (`127.0.0.11` in Docker; different on ECS).
- **Health checks without a shell.** The Go services ship in distroless images (no shell, no curl, smaller attack surface), so the binary has a `healthcheck` subcommand that calls its own `/healthz`. The JRE image has no curl either, so the marketplace image installs it. Alternative: a full base image, which is simpler but larger and has more CVEs to patch. Liveness only: `/healthz` doesn't check dependencies, so a service whose database is down still reports healthy. Readiness checks come later.
- **Don't log what you can't un-log.** The gateway logs `$uri`, not `$request_uri`, because query strings carry OAuth codes and state. This was verified by sending `?code=SECRET123` and finding no match in the logs. Logs get shipped, indexed and kept, so a secret in a log is a leaked secret.
- **Pin to match where you'll deploy, not just to "latest".** Kafka is pinned to 4.2.2, not the newer 4.3, because Amazon MSK stops at 4.2. MongoDB is pinned to 8.0 because DocumentDB is MongoDB-8.0 compatible. Redis 8 runs locally, but the code sticks to commands that ElastiCache's Redis 7 and Valkey also support. Local and cloud can still drift, so each version bump is its own commit.

## Stage 2: Game core

## Stage 3: Drawing lifecycle

## Stage 4: Economy

## Stage 5: Production readiness

## Stage 6: Scale and depth
