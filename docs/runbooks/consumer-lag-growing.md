# Runbook: ConsumerLagGrowing

- Severity: warning
- Dashboard: [Async pipeline, Consumer lag](http://localhost:3000/d/da-async-pipeline)

## What it means

A Kafka consumer group is behind and still falling further back (lag above 0, and higher than 10 minutes ago, for 5 minutes). The alert's `group` label names it. The lag comes from the broker (the `kafka-metrics` collector), so it covers every consumer whatever its language.

| Group | Consumes | Effect of the lag |
|---|---|---|
| `assets` | `game.rounds.v1` | drawings appear late |
| `marketplace` | `game.matches.v1`, `assets.drawings.v1` | rewards and minting late |
| `lobby-leaderboard` | `game.matches.v1` | leaderboard late |
| `notifier`, `notifier-auth` | sales, wallet, `auth.users.v1` | webhooks late; a revoked user's endpoints stay enabled |

## Check

1. Is the consumer running? `make ps` and `make logs s=<service>`. A stopped consumer has lag that grows with every new event.
2. Is it stuck on one record? Consumers commit after processing, so a record that fails on every try blocks its partition. Look for the same error repeating in the service's log; the offset in `kafka.consumer_group.offset` stops moving.
3. Is it just slow? A load test or a backlog after an outage: the offset moves, but slower than new events arrive.
4. Describe the group from the broker: `docker compose -f infra/docker-compose.yml --env-file .env exec kafka kafka-admin kafka-consumer-groups.sh --describe --group <group>`. This runs as `admin`, so it raises KafkaAdminUsed on purpose: say so in the team channel ([kafka-admin-used](kafka-admin-used.md)).

## Fix

- Stopped: restart the service. Consumers are idempotent (at-least-once delivery), so reprocessing after a restart is safe.
- Stuck on a record: fix the bug, or, if the record is malformed, make the consumer count and skip it (they already do for records they can't decode).
- Slow: add consumer instances up to the topic's partition count, or make processing faster.

## Afterwards

Note the peak lag and how long the catch-up took.

## In AWS

The same checks with the AWS commands in [Runbooks in AWS](README.md): logs in Grafana (`make aws-grafana`) or CloudWatch Logs, restarts with `aws ecs update-service --force-new-deployment`, and the stores through a Session Manager shell on the data host.
