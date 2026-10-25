# Doodle Arena: keyword coverage

The project's scope comes from skill keywords in 2027 backend internship postings, sorted into P1 (must cover), P2 and P3. This file maps each keyword to the part of the system that covers it and the stage where it gets built.

Rule for agents: never remove or replace a technology in this table without a human decision. If a task would drop coverage, say so.

## P1

| Keyword | Where it's covered | Stage |
|---|---|---|
| Python | auth and ai (FastAPI), assets (Django) | 1, 3 |
| Django | assets service; Django admin as the moderation console | 3 |
| FastAPI | auth, ai | 1, 3 |
| Java | marketplace (Spring Boot), analytics (Flink) | 4, 6 |
| JUnit | marketplace tests, including the purchase-race test | 4 |
| Go / Golang | lobby, game-server, notifier | 2, 4 |
| AWS | S3, CloudWatch, deployment (ADR 0003); backups that outlive the stack (ADR 0016) | 5, 6 |
| GCP, Azure | Optional: AI service on Cloud Run; drawing backups to Azure Blob | 7 |
| Docker | Every service; Docker Compose locally; multi-stage builds | 1 |
| Amazon S3 | `round-logs` and `drawings` buckets (SeaweedFS locally) | 2, 3, 5 |
| Distributed storage | Content-addressed object storage (SeaweedFS locally, S3 in AWS); MongoDB runs as one node, and a replica set is not built (TODO, design doc section 15); optional cross-cloud copy | 3, 6, 7 |
| Caching | Redis cache-aside in assets (ADR 0024, task 6.7b): a generation key against the cache-aside race, single-flight fills against stampedes, TTLs, failing open, measured before and after with k6; CDN for images | 5, 6 |
| Redis | Room event streams, snapshots, leases, rate limits, leaderboards, caches, Celery broker | 2+ |
| Databases / database systems | Postgres, MongoDB, Redis, Elasticsearch, each with a stated reason | All |
| Database transactions (ACID) | The purchase transaction | 4 |
| Database schema | Marketplace schema (ledger, ownership, listings) | 4 |
| PostgreSQL / Postgres | auth, assets, marketplace, notifier databases | 1+ |
| Query processing / optimization | Seeded data, `EXPLAIN ANALYZE`, indexes before and after | 6 |
| NoSQL | MongoDB (replays), Redis | 2, 3 |
| MongoDB | Stroke replays | 3 |
| Schema migrations | Alembic (auth), Django migrations (assets), Flyway (marketplace), golang-migrate (notifier) | 1–4 |
| Sharding, partitioning & replication | Rooms sharded across game servers; Kafka partitions by room; Postgres read replica; monthly ledger partitions; a sharded tips escrow settled per round (ADR 0021) | 2, 6 |
| Distributed systems | Leases, fencing tokens, failover, idempotent consumers | 2, 4 |
| Consistency models & CAP theorem | Consistency model table (design doc section 10); read-your-writes with a replica | 4, 6 |
| Load balancing & reverse proxies | Nginx gateway; least-loaded room allocation; AWS load balancer | 1, 2, 5 |
| System design / architecture | Design doc, ADRs | All |
| Maintainability / refactoring | A deliberate refactor of agent-written code | 5 |
| Design patterns | Actor per room, transactional outbox, claim check, state machine, idempotency key, adapter for storage | 2–4 |
| Microservices | Eight services, one database per service | All |
| Idempotency & idempotency keys | `Idempotency-Key` on purchases and listings; reward and Stripe keys; idempotent consumers | 3, 4 |
| Retries, timeouts, exponential backoff & jitter | Webhook delivery, moderation calls, gRPC deadlines | 3, 4 |
| Event-driven architecture | Kafka events between services | 2–4 |
| Performance / efficiency | Stroke batching experiment; JSON versus binary experiment | 6 |
| High-traffic / high-volume servers | Live-stream room; flash sales | 6 |
| Low-latency, high-throughput services | Game-server broadcast; relays | 2, 6 |
| Backend load testing (后端压测) | k6 scenarios | 5, 6 |
| Frontend FCP/FMP | Core Web Vitals (LCP, INP, CLS) through Lighthouse (`make lighthouse`, FCP included) and real-user monitoring over OpenTelemetry; FMP is deprecated | 5 |
| CI/CD / continuous integration | GitHub Actions per service | 1, 5 |
| Jenkins | Optional deployment pipeline | 7 |
| GitHub Actions | CI for every service, proto checks | 1, 5 |
| Real-time systems | Game server | 2 |
| Real-time collaboration (实时协作), WebSocket | Shared canvas with two drawers; WebSocket protocol; batch intervals and JSON against protobuf frames measured under load, negotiated per socket with a subprotocol (ADR 0025, task 6.4) | 2, 6 |
| Real-time continuous ingestion (实时持续接收), Kafka, Flink | Kafka in Stage 2; Flink jobs in Stage 6 | 2, 6 |
| Correctness | Server-authoritative ordering; ledger invariants; reconciliation job | 2, 4 |
| Webhooks (signed, retried event delivery) | Notifier (outgoing); Stripe (incoming) | 4 |
| Reliability & high availability | Reconnect, failover, two game-server instances, kill tests | 2, 5, 6 |
| Version control | Git, one branch per task, pull requests | 1+ |
| Concurrency / multithreading | Goroutine per room; concurrent purchases; the race detector | 2, 4 |
| REST/GraphQL | REST APIs (GraphQL intentionally skipped, see D10) | 1+ |
| Rate limiting (token bucket) | Per-player token bucket in the game server; coarse Nginx limit | 1, 2 |
| Build tools / build process | Go modules, Gradle, uv, Vite, Buf, multi-stage Docker builds, CI caching | 1 |
| Deployment & release process | Terraform, rolling deploys, game-server draining, deploy markers | 5, 6 |
| Backup and restore, disaster recovery | `pg_dump`/`mongodump` at every teardown, verified with checksums and row counts; a restore at every start, with a stated recovery point and a measured restore time (ADR 0016, task 6.10) | 6 |
| Stream processing | Flink 2.2 in application mode: event time with watermarks, a hopping window TVF and a window Top-N in Flink SQL, checkpoints to object storage (ADR 0018, task 6.5a); keyed state and timers for wash trading (task 6.5b) | 6 |
| Transactions | Purchase transaction; transactional outbox | 4 |
| End-to-end project ownership | The whole project | All |
| Authentication & authorization (API keys, OAuth 2.0, JWT) | auth service; roles; API keys for developers | 1, 4 |
| Third-party API & service integration | Google OAuth, Stripe, Discord, an AI model API | 1, 4, 5, 7 |
| Background jobs & task queues | Celery pipeline (assets); webhook retry scheduling (notifier) | 3, 4 |
| Shell scripting & automation scripts | `scripts/`: dev-up, seed, migrate, fake events | 1+ |
| Trade-off and edge-case analysis | ADRs; interview notes | All |
| Technical documentation / design docs | `docs/` | All |
| Deployment / release monitoring metrics, logs | Version-tagged telemetry, deploy markers, reconnect watch during deploys | 5, 6 |
| OpenTelemetry, Prometheus & Grafana | The telemetry pipeline | 1, 5 |
| Amazon CloudWatch | AWS resource metrics and logs; billing alarm; backup and restore metrics and logs | 5, 6 |
| Monitoring, Observability, Telemetry | `docs/observability.md`; SLOs with multi-window burn-rate alerts, tail sampling in the collector, deploy markers, browser errors and fetch spans behind a hardened public receiver, and a timed fire drill that found a real blind spot (task 6.9) | 1, 5, 6 |
| Alerting | Grafana alerting to Discord; invariant alerts | 5 |
| Logging | JSON logs with trace IDs into Loki | 1 |
| Automated testing / Testing | Unit, integration (Testcontainers), concurrency, load and fault-injection tests | All |
| Test frameworks | `go test`, JUnit 5, pytest, k6 | All |
| Unit testing | Every service | All |
| DynamoDB (TODO) | Not covered yet | – |

## P2

| Keyword | Where it's covered | Stage |
|---|---|---|
| Elasticsearch | Self-hosted, security on with a read and a write user; an alias over versioned indices, an edge n-gram prefix field, fuzzy matching, filters, `search_after`, a price range aggregation; an event-fed indexer with external versioning and zero-downtime rebuilds (ADR 0023, task 6.7) | 6 |
| Scalability | Relays for the live-stream room; horizontal game servers | 6 |
| Reliability / stability | Same as reliability and high availability above | 2, 5, 6 |
| gRPC & Protocol Buffers | Lobby and game-server gRPC; protobuf event schemas | 1, 2 |
| Kubernetes (TODO) | Optional | 7 |
| AI integration | AI moderation stub (built, task 3.3), then a real model (TODO, Stage 7) | 3, 7 |
| AI agents / agentic AI (TODO) | AI player and an MCP server; the project itself is built with agents | 7 |
| Serverless (TODO) | Lambda thumbnails | 7 |

## P3

| Keyword | Where it's covered | Stage |
|---|---|---|
| Experimentation platforms / A/B testing | A/B test of the shop or hint timing | 7 |
| AI-assisted development | The whole project is built by directing Claude Code | All |
| SDLC / full lifecycle | Design doc, tasks, tests, CI, deploys, monitoring | All |
| MCP | MCP server for AI agents | 7 |
| Computer networking fundamentals (TCP/IP, HTTP, DNS) | WebSocket upgrade over HTTP, TLS, DNS and a domain, CDN caching, connection routing | 2, 5 |
| Code reviews | Humans review every agent pull request | All |
| Security | OAuth with PKCE, JWT, token rotation, HMAC signatures, secrets handling | 1, 4 |
| AWS Lambda (TODO) | Lambda thumbnails | 7 |
| Natural language processing, vector search & embeddings (TODO) | "Close!" hints with embeddings | 7 |
| Search / retrieval | Elasticsearch: relevance (fuzzy and prefix matching, boosting), facets, keyset paging, results read back from the source of truth (task 6.7); pHash still TODO | 6, 7 |
| Computer vision (TODO) | AI guesser | 7 |
| LLM evaluation (TODO) | Eval set for the AI guesser built from real drawings | 7 |
| Data pipelines / ETL | Round logs into replays; analytics jobs | 3, 6 |
| Control plane vs. data plane | Lobby (control plane) versus game servers and relays (data plane) | 2, 6 |
