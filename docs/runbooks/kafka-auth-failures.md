# Runbook: KafkaAuthFailures

- Severity: warning
- Dashboard: Doodle Arena: async pipeline, row "Kafka access" (`da-async-pipeline`)

## What it means

For 5 minutes in a row, the Kafka broker refused SASL logins (a wrong or unknown user, a bad password) or refused requests that no ACL allows (ADR 0013), every minute. Either a service is misconfigured, and its events aren't flowing, or something is trying users, topics or consumer groups it has no right to. A refused write to `game.matches.v1` or `assets.drawings.v1` is exactly what the ACLs exist for: someone tried to mint coins or drawings.

## Check

1. The "Kafka access" row: "Failed Kafka logins" says which listener (INTERNAL is the services' network, EXTERNAL the host tools on 127.0.0.1), and "ACL denials by principal" which user was refused.
2. "Recent ACL denials" (or `make logs s=kafka | grep "is Denied"`) shows each refusal: principal, operation, host and resource. A known service's principal on a topic it should use means a missing grant; a host you don't recognise, or a principal trying many topics or groups, means probing.
3. For failed logins, the logs of the services that just restarted: a service that can't log in logs its Kafka error (`make logs s=<service>`), and the passwords in `.env` (AWS: SSM, `stack/kafka-*-password`) must match what `kafka-init` set.
4. What changed: a new topic or consumer group, a new service, a rotated password, a deploy.

## Fix

- A missing grant: add it to `GRANTS` in `infra/kafka/init.sh` (and the ACL table in ADR 0013), then run `kafka-init` again: `docker compose -f infra/docker-compose.yml --env-file .env up --no-deps kafka-init`. In AWS, a redeploy of the data host runs it.
- A wrong password: the service's `KAFKA_PASSWORD` and the user's password in `kafka-init` come from the same variable; recreate the service after fixing `.env`, or redeploy the task after changing SSM.
- Probing: find the source host in the denial lines. Locally it's a container on the Kafka network; in AWS, the security group already limits 9092 to Kafka's clients, so a probe means one of those tasks is compromised: stop it, rotate that service's Kafka password, and look at its logs.

## Afterwards

Note the cause in the evidence log if it was a real incident. A missing grant found this way deserves a case in `scripts/drills/kafka_acl_check.sh`.
