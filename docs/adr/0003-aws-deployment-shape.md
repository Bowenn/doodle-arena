# ADR 0003: Shape of the AWS deployment

- Status: Proposed (decide at the start of Stage 5)
- Date: 2026-10-15
- Deciders: Bowen, Will

## Context

The system must run in AWS for credible load-test numbers and for the CloudWatch and deployment keywords. It's a student project, so cost matters, and everything should be easy to tear down after a demo.

Fixed regardless of the option: S3 for both buckets, CloudWatch, a budget alarm set before anything else, Terraform in `infra/terraform/`, and secrets in SSM Parameter Store or Secrets Manager.

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
