# Runbook: AnalyticsDown

- Severity: warning
- Dashboard: Doodle Arena: analytics (`da-analytics`), "Job running for" and "Since the last checkpoint"

## What it means

Either the analytics Flink job (ADR 0018) ran in the last 6 hours and hasn't reported running for 2 minutes, or it runs but hasn't completed a checkpoint for over 5 minutes.

While it lasts:

- **Live rooms:** viewers get no new top-guesses list.
- **Wash trading:** sales aren't watched. They aren't lost, because they wait on `market.sales.v1` and the job reads them when it's back.

A job that stopped checkpointing would also lose everything since its last checkpoint on its next restart.

Players and money are not affected: nothing in the game or the marketplace waits for analytics.

## Check

1. Is it running? Locally: `make ps` (`analytics-jobmanager`, `analytics-taskmanager`), and Flink's UI at `http://localhost:8081` (Jobs, then the job's Exceptions and Checkpoints tabs). In AWS, the ECS tasks (task 6.9).
2. Read the JobManager's log: `make logs s=analytics`, or Loki `{service_name="analytics"} | level=~"error|warn"`. Look for:
   - `Failed to rollback to checkpoint ... Cannot map checkpoint/savepoint state for operator ...`: the job graph changed in a way the newest checkpoint can't be restored into.
   - `starting from checkpoint s3://...` or `starting without state`: where this start began.
   - S3 errors (`AccessDenied`, timeouts): checkpoints can't be written or read (SeaweedFS or S3, and the `analytics` S3 identity).
   - Kafka errors (`is Denied` in `make logs s=kafka`, authentication failures): the `analytics` user or its grants (ADR 0013).
3. A crash loop shows as a restarting JobManager container (`docker inspect -f '{{.RestartCount}}' doodle-arena-analytics-jobmanager-1`), with the same error each time.
4. Memory: an OOM-killed TaskManager restarts the job from its last checkpoint again and again. Look for `OutOfMemoryError` and the Docker VM's OOM killer (CLAUDE.md, "Gotchas").

## Fix

- **SeaweedFS, S3 or Kafka down:** fix that first; the job recovers on its own (exponential-delay restarts) and checkpoints resume.
- **A checkpoint that can't be restored** (after a deploy that changed wash trading's state or its uids): the job can't start from it. Unmapped SQL state is already dropped on its own (ADR 0018), so this concerns the operators with fixed uids. Either:
  - deploy the previous version again, take a savepoint, and start the new one from it with `ANALYTICS_RESTORE=<savepoint path>`; or
  - accept losing the 7-day history: start empty with `ANALYTICS_RESTORE=none PROFILES=live,analytics make up`. The job then rebuilds wash trading's history only from new sales. Loops that started before the restart go unflagged.
- **The JobManager or TaskManager keeps getting killed for memory:** give Docker Desktop more memory (11 GB with both profiles), or raise `taskmanager.memory.process.size` in `infra/docker-compose.yml`.
- **No sign of the job and no errors:** someone stopped it, or `make up` ran without `PROFILES=analytics`. Start it with `PROFILES=live,analytics make up`. The alert clears; it stops firing 6 hours after the job's last report on its own.

## Afterwards

- A restore failure after a deploy: add a test that restores the previous version's state, and note in ADR 0018 what changed.
- Record how long the job was down and whether it started empty, because flags for that window may be missing.
