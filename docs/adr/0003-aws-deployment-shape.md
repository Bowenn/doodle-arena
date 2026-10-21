# ADR 0003: Shape of the AWS deployment

- Status: Accepted (2026-10-19, task 5.2)
- Date: 2026-10-15, decided 2026-10-19
- Deciders: Bowen (covering Will's track until he joins)

## Context

The system must run in AWS for credible load-test numbers and for the CloudWatch and deployment keywords. It's a student project, so cost matters, and everything should be easy to tear down after a demo.

Fixed regardless of the option: S3 for both buckets, CloudWatch, a budget alarm set before anything else, Terraform in `infra/terraform/`, and secrets in SSM Parameter Store or Secrets Manager.

Managed-service versions as of 2026-10-15, relevant to Option C (the full mapping is in `docs/setup.md`):

- RDS for PostgreSQL offers 18.6, the same as local.
- ElastiCache offers Redis OSS up to 7.1, or Valkey 8 to 9.1, but no Redis 8. The commands the system uses (Streams with explicit IDs, Lua scripts, sorted sets, TTLs) work on all of them.
- MSK offers Kafka 4.2 on Express brokers only, and up to 4.1 on provisioned brokers. Local Kafka is pinned to 4.2.2 to match.
- DocumentDB is MongoDB 8.0 compatible, not MongoDB itself. MongoDB Atlas on AWS is the alternative for real MongoDB.
- Every pinned image is built for both `amd64` and `arm64`, so Graviton instances (usually cheaper) work for Options A and B.

## Options

### Option A: a few EC2 instances running the Compose stack

- Pros: cheapest and fastest to set up; the same files as local development.
- Cons: less realistic; scaling and rolling deploys are mostly manual; a weaker deployment story.

### Option B: ECS on Fargate for the services, with containers for Kafka and the data stores

- Pros: realistic rolling deploys, health checks and service scaling; a strong deployment story.
- Cons: more Terraform; more expensive than Option A; stateful containers need care.

### Option C: ECS for the services plus managed data stores (RDS, ElastiCache, MSK, OpenSearch)

- Pros: the most production-like.
- Cons: the most expensive (managed Kafka in particular); slow to create and tear down; easy to leave running by mistake.

Suggestion for discussion: Option B for the services, keeping the data stores as containers or a single EC2 host, and tearing everything down after each measurement session.

## Decision

Option B: ECS on Fargate for the services, and one EC2 host for the stateful containers. Everything is created per measurement session and torn down after it.

- **Services:** one ECS service per compose service, on Fargate with Graviton (arm64). The two game servers are separate services with fixed server ids (`game-server-1`, `game-server-2`), so the lobby's registry and the gateway's `/ws/<server_id>/` routing (ADR 0002) work unchanged. Services find each other by DNS in a private Cloud Map namespace (`doodle.internal`). Migrations run as one-off ECS tasks.
- **Data host:** one `t4g.large` with a gp3 data volume runs Postgres, Redis, MongoDB, Kafka, otel-lgtm and the kafka-metrics collector with a small compose file. Postgres and Kafka need real disks (fsync), which rules out Fargate with EFS; an EC2 capacity provider would give the same single host with more Terraform. It's a single point of failure, which is acceptable for demos and stated as such.
- **Entry:** a domain in Route 53, an ACM certificate on an Application Load Balancer, WebSockets straight to the ALB, the Nginx gateway behind it. Google sign-in needs an https redirect URI, and the refresh cookie must be `Secure`.
- **Team-only until 5.5:** the ALB's security group admits only the team's IPs (a Terraform variable) until task 5.5 adds Kafka users and ACLs (ADR 0013). Only then does the entry open to everyone.
- **Network:** private subnets in two availability zones for every task and the data host, one NAT gateway for egress (Google, Stripe, outgoing webhooks, ECR, SSM), and a free S3 gateway endpoint. One NAT gateway costs less per hour than a public IPv4 address per task.
- **Secrets:** SSM Parameter Store SecureStrings (standard parameters are free; Secrets Manager is $0.40 per secret per month). Terraform generates the passwords and signing keys; third-party secrets (Google, Stripe, Discord) are put by a human.
- **Telemetry:** otel-lgtm runs on the data host, reachable only through an SSM port forward, with Grafana's CloudWatch data source on the host's read-only IAM role. Container stdout also goes to CloudWatch Logs (3-day retention).
- **Two Terraform roots:** `bootstrap/` (state bucket, the budget, ECR repositories, the third-party SSM parameters) lives between sessions; `stack/` is created and destroyed per session, so a teardown never deletes the budget, the state or the images.

Why not A: Compose on EC2 gives the weakest deployment story, and ADR 0006 relies on security groups between services, which separate nothing between containers on one host. Why not C: managed Kafka alone costs about $1.20 an hour, and RDS, ElastiCache and MSK are slow to create and destroy, which works against tearing down after every session.

## Consequences

- **Per-session cost.** About $0.35–0.45 an hour while running (estimate, 2026-10-19); Cost Explorer measured about $0.30–0.80 per stack-hour (docs/performance.md). A stack left running for a month would exceed the $35 budget many times over, so `make aws-down` after each session is the routine and the budget emails are the safety net.
- **No AWS data survives a teardown.** Postgres, Kafka and the buckets start empty in every session; demos use drills and seeds. Amended by ADR 0016 (task 6.10): a teardown backs up the services' databases and the drawings bucket first, and the next session restores them.
- **The services need small changes to run outside Compose:** signing keys from environment variables (Fargate injects secrets only as variables, and the Go images have no shell), S3 through the task's IAM role, a configurable gateway template, and a static production build of the web app.
- **ADR 0006 holds:** every service has its own security group, so the lobby's and game servers' gRPC port 9090 is open only to each other.
- **What to watch:** the data host's disk and memory (Kafka, Postgres and otel-lgtm share 8 GB), the NAT gateway's data charges, and the real client IP reaching the gateway's rate limits through the ALB.
