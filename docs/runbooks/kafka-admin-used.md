# Runbook: KafkaAdminUsed

- Severity: warning
- Dashboard: Doodle Arena: async pipeline, panel "Admin logins" (`da-async-pipeline`)

## What it means

Someone logged in to Kafka as `admin`, the super user for humans (ADR 0013). `admin` has no SCRAM credential: it exists only as a PLAIN user on the broker's localhost ADMIN listener, so a login means a shell inside the broker container, normally `kafka-admin` (infra/kafka/admin.sh). The broker itself uses `broker` and the init job `kafka-init`, so neither raises this. `admin` can create topics, change any ACL and read or delete anything.

## Check

1. Was it you, or a teammate who said so? A planned `kafka-admin` call is the expected cause: acknowledge it and move on.
2. If nobody did it: who has a shell on the broker? Locally, `docker exec` on the Kafka container; in AWS, a Session Manager session on the data host (CloudTrail: `StartSession`).
3. What admin did: the broker's authorizer logs denials only, so look at the effects. `kafka-admin kafka-acls.sh --list` against `GRANTS` in `infra/kafka/init.sh`, `kafka-admin kafka-configs.sh --describe --entity-type users`, and the topics' configs (retention).

## Fix

- An unplanned login: rotate `KAFKA_ADMIN_PASSWORD` (`.env`, or SSM `stack/kafka-admin-password` and a data host redeploy), restore the ACLs with `kafka-init`, and remove any ACL or user that isn't in `GRANTS`.
- In AWS, close whoever's access to the data host made it possible (IAM, Session Manager).

## Afterwards

Record unplanned admin use in the evidence log with what was changed and how it was put back.
