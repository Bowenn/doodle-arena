# ADR 0003: Shape of the AWS deployment

- Status: Proposed (decide at the start of Stage 5)
- Date: 2026-10-15
- Deciders: Bowen, Will

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

(empty until decided)

## Consequences

(fill in once decided)
