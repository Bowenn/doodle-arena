# Runbooks in AWS

The runbooks give the local stack's commands. In an AWS session (ADR 0003, `docs/setup.md` "AWS") the same checks and fixes look like this. Every command needs `aws sso login --profile doodle-arena` and `--profile doodle-arena` (or `AWS_PROFILE=doodle-arena`).

| Local | AWS |
|---|---|
| Grafana on `localhost:3000` | `make aws-grafana`, then Grafana on `localhost:3300`: the same dashboards and alerts, with `deployment_environment="aws"` |
| `make ps` | `aws ecs describe-services --cluster doodle-arena --services <service> --query 'services[0].[runningCount,desiredCount,deployments[].rolloutState]'`; the stores: `sudo docker ps` in a shell on the data host (below) |
| `make logs s=<service>` | Loki in Grafana (`{service_name="<service>"}`), or CloudWatch Logs: `aws logs tail /doodle-arena/ecs --log-stream-name-prefix <service> --follow` |
| Restart a service (`docker compose ... up -d <service>`) | `aws ecs update-service --cluster doodle-arena --service <service> --force-new-deployment` (it also rereads the service's SSM secrets). A game server drains first (task 5.5) |
| `docker compose ... exec <store> ...` (Postgres, Redis, MongoDB, Kafka) | A shell on the data host: `aws ssm start-session --target "$(aws ec2 describe-instances --filters Name=tag:Name,Values=doodle-data-host Name=instance-state-name,Values=running --query 'Reservations[0].Instances[0].InstanceId' --output text)"`, then `sudo docker exec -it doodle-data-<store>-1 ...` (for example `doodle-data-postgres-1 psql -U postgres -d marketplace`, or `doodle-data-kafka-1 kafka-admin ...`) |

Differences that matter during an incident:

- **Deploys fire alerts.** A redeploy (`make aws-push`, then `aws-up` with the new `IMAGE_TAG`) replaces the other services first, then one game server at a time; each drains into the other, and GameServerDown can fire for the minute a server is gone. A one-off `update-service` restarts that service alone.
- **The data host is a single EC2 instance.** If it's gone, every store is gone with it: Postgres, Redis, MongoDB, Kafka and otel-lgtm (so Grafana too; CloudWatch Logs still has the services' logs). Replacing it mid-session starts the stores empty: what was written since the last `aws-down` is lost. A new session's `aws-up` restores the last backup (ADR 0016, task 6.10; a failed backup or restore has its own runbook, `aws-backup.md`).
- **Secrets come from SSM** when a task starts. After changing one, redeploy the service.
