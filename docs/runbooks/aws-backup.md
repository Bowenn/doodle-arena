# Runbook: BACKUP FAILED or RESTORE FAILED (AWS)

- Severity: critical (a failed backup keeps a stack running and billing; a failed restore keeps a session from starting)
- Dashboard: Service overview, row "Backups (AWS only)" (`make aws-grafana`, then `http://localhost:3300/d/da-overview`)

Not a Grafana alert: `make aws-down` and `make aws-up` print it and exit 1 (task 6.10, ADR 0016). Every command below needs `aws sso login --profile doodle-arena` and `AWS_PROFILE=doodle-arena`.

## What it means

- **BACKUP FAILED:** `aws-down` couldn't back up the session's data, so it destroyed nothing. The stack is still up, its services at 0, and it still costs money (docs/performance.md has the cost per hour).
- **RESTORE FAILED:** `aws-up` couldn't load the backup into the new stack, so it started no service. The backup itself is untouched: the restore only reads it.

## Check

1. The line after `FAILED:` names the step. The run's log lines are in CloudWatch Logs: `aws logs tail /doodle-arena/backups --since 1h`. The last one has `level=error`, the reason, and `store` for the store it stopped on.
2. Anything on the data host (a dump or a load) ran through SSM Run Command: `aws ssm list-command-invocations --details --max-items 3 --query 'CommandInvocations[].[CommandId,Status,CommandPlugins[0].Output]' --output text` shows its output.
3. A shell on the data host (docs/runbooks/README.md) for the stores: `sudo docker ps`, disk space with `df -h /opt/doodle`.

## Fix

Backup:

- **The SSO session expired** (`ExpiredToken`, `No AWS session`): `aws sso login --profile doodle-arena`, then `DOMAIN=icanfly.click make aws-down` again. Each run is a new backup; a run's incomplete files are deleted by the next successful one.
- **SSM can't reach the data host, or a store is down:** look at the host as above. A container that exited: `sudo docker compose -f /opt/doodle/compose.yml up -d <store>`, then retry. If the host itself is gone, so is the data since the last backup: end the session with `SKIP_BACKUP=1 DOMAIN=icanfly.click make aws-down`.
- **A checksum, size or drawings count didn't match:** retry. Twice in a row means something corrupts the files on their way to S3; keep the stack and look before deleting anything.
- **To go on with the session instead** (the services are only stopped): `DOMAIN=icanfly.click make aws-up` starts them again; it sees the stores have data and doesn't restore over them.

Restore:

- **"it is from a newer schema":** the backup has a migration the deployed code doesn't, for example a branch with a new migration was deployed last session. Deploy code that has it (`git switch master`, `make aws-push`, then `aws-up`), or pick an older backup: `aws s3 ls s3://doodle-arena-backups-<account>/backups/`, then `BACKUP_ID=<id> DOMAIN=icanfly.click make aws-up`.
- **The data host was still starting** (`not ready after 600 s`, SSM couldn't reach it): run `make aws-up` again. The stores are still empty, so it restores.
- **A load stopped partway, or the rows didn't match the backup:** the new stores now hold part of a backup. Throw the stack away and start again, which restores the same backup into new stores: `SKIP_BACKUP=1 DOMAIN=icanfly.click make aws-down`, then `make aws-up`. `SKIP_BACKUP` is safe here because nothing in this stack is newer than the backup.
- **To start without the backup:** `RESTORE=none DOMAIN=icanfly.click make aws-up`. The backup stays in the bucket; the next `aws-down` writes a new one, and the 10 newest are kept.
- **Looking inside a backup:** `aws s3 cp s3://doodle-arena-backups-<account>/backups/<id>/pg-marketplace.dump .` and `pg_restore --list pg-marketplace.dump`. The dumps hold every account, wallet and webhook endpoint: delete the copy afterwards.

After a restore, search starts empty: the indexer only sees new events. Republish the listings and rebuild the index as `docs/setup.md` (Backups) says.

To rehearse a failed backup without a real fault: `FAIL_BACKUP_AT=dump DOMAIN=icanfly.click make aws-down` stops just before the dump with BACKUP FAILED. The stack stays up with its services at 0 (task 6.9). Then run `aws-down` again for real; it removes the incomplete backup.

## Afterwards

- Add the failure and its cause to the evidence log in docs/roadmap.md.
- A cause that can happen again needs a check in `scripts/aws/backup.sh` or `restore.sh`, and, if it can be reproduced locally, in `make backup-check`.
