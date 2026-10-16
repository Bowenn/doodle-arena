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

### 1.2 Contracts

- **One wrapper per topic, not a type header (ADR 0004).** Protobuf bytes don't say what type they are, and some topics carry two events. A `oneof` wrapper puts "what can appear on this topic" into the contract, where `buf breaking` checks it. A header lives outside the schema, so a typo only fails at consumption time. Failure mode: a consumer built before a new event type sees an empty `oneof`. The rule is to log it, count it and move on, never to crash or block the partition. A test checks that unknown field 99 decodes to "no case set".
- **No schema registry.** Compatibility is enforced in CI (`buf breaking` against `master`), not at produce time, which saves running another stateful service. Trade-off: a producer built from an unmerged branch could publish an incompatible message to a shared environment. With many teams or external consumers, a registry would earn its keep.
- **Claim check for big payloads.** `RoundFinished` carries only the object key of a gzipped `StrokeLog` in S3. Kafka messages stay small, and there's no "have I seen every stroke yet?" ordering problem. The stroke log is protobuf rather than JSON, so its format is versioned and lint-checked like the events. The cost: it isn't readable with `zcat`, and objects expire after 7 days, so assets must fetch the log before then.
- **Committed generated code, built in three languages on every run.** `make proto` regenerates with pinned remote plugins (identical output everywhere, and running it twice changes nothing), then compiles the Go, Java and Python code. Golden files written from Go are decoded in Python, which proves the wire format agrees across languages, not just that each language compiles.
- **Keys and partitions.** Events are keyed by `room_id`, `drawing_id`, `user_id` or `endpoint_id`. franz-go uses the same murmur2 partitioner as the Java client, so one room's events land on one partition in order, whichever language produced them. Replaying a match with `-match-id` lands on the same partition, which is what idempotent consumers will be tested against.

### 1.3 Auth and gateway

- **Refresh-token rotation with reuse detection.** Every refresh swaps the token for a new one in the same family. If an already-used token comes back, someone holds a copy, so the whole family is revoked and both thief and user must sign in again. The row is locked with `SELECT … FOR UPDATE`, so 10 concurrent refreshes of one token produce exactly 1 winner, and the other 9 count as reuse (a test checks this). Failure mode: a legitimate client that refreshes twice at once logs itself out. That's why the web app refreshes single-flight, which also covers React StrictMode's double effects in development. Rejected: a grace window, friendlier with several tabs but it blurs theft detection.
- **A bug the tests caught: rollback would undo the revocation.** `rotate()` first raised an exception inside the transaction when it detected reuse, which rolled back the family revocation it had just written. The route happened to work around it, but the concurrency test, written naturally, left 2 live tokens. The fix makes `rotate()` return an outcome instead of raising, so the security-critical write always commits. Lesson: never signal an expected business outcome with an exception that crosses a transaction boundary.
- **Login CSRF and PKCE without server-side state.** The `state`, OIDC `nonce` and PKCE verifier live in a short-lived, HMAC-signed, HttpOnly cookie scoped to the callback path. Matching `state` to the cookie binds the login to this browser, so an attacker can't log you into their account by sending you their callback URL. The `nonce` binds Google's ID token to this login. PKCE makes a stolen authorization code useless without the verifier. Rejected: a database table, which would add single-use enforcement that Google already provides.
- **Where tokens live.** The access token (15 minutes, RS256) lives only in JavaScript memory, never in localStorage, which XSS can read. The refresh token is an HttpOnly, SameSite=Strict cookie scoped to `/api/auth`, so page scripts can't read it and other sites can't send it. `/refresh` and `/logout` also check `Origin`. Trade-off: a page reload costs one refresh call.
- **Coarse limits at the edge, fine ones in the service.** Nginx `limit_req` is a leaky bucket per IP: 20 r/s on the API, 5 r/s on sign-in and refresh, with a JSON 429 and `Retry-After`. A burst of 40 gets 29 rejections. Behind a load balancer the client IP must come from `X-Forwarded-For`, or every user shares one bucket. Per-player limits (guesses) need game context, so they live in the game server.

### 1.4 CI

- **CI runs exactly what developers run.** Every job sets up a toolchain, then calls `scripts/lint.sh` and `scripts/test.sh`, the same scripts as `make`. No build or test logic is duplicated in YAML, so "green locally, red in CI" can only mean an environment difference, not a different command. Rejected: hand-written steps per workflow, which drift from the Makefile within weeks.
- **Per-service workflows on reusable building blocks.** Ten thin workflows, one per service, each with a path filter, call five reusable workflows (one per language, plus Docker). A change to `services/auth` runs only auth's lint, test and image build, which matters on a private repo with a minutes quota. Trade-off: a workflow skipped by its path filter never reports, so it can't be a required check. Branch protection would need an always-run gate job, which a single matrix workflow gives for free.
- **Contracts are checked twice.** `buf breaking` compares the PR against its base commit, so a renumbered or retyped field fails before anyone depends on it. Then CI regenerates all three languages and fails if the committed code differs, so nobody can merge a `.proto` change without its generated code, or edit generated code by hand.
- **Supply-chain hygiene.** Every action is pinned to a 40-character commit SHA, because a tag like `v7` can be moved to malicious code and a SHA can't. Dependabot keeps the pins current. Workflows get read-only tokens, checkouts don't persist credentials, and event data reaches shell scripts only through environment variables (no `${{ }}` inside `run:`), which closes the classic workflow-injection hole.
- **Cost-aware integration testing.** The full-stack smoke test (`make up` plus `make smoke`, about 5 minutes) runs only when the stack itself changes, on `master`, and on demand. Unit and Testcontainers integration tests run on every relevant PR. That spends minutes where breakage is likely.

## Stage 2: Game core

## Stage 3: Drawing lifecycle

## Stage 4: Economy

## Stage 5: Production readiness

## Stage 6: Scale and depth
