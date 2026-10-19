# Runbook: DrawingsFailing, JobsBacklog

- Severity: warning
- Dashboard: [Async pipeline, Drawings](http://localhost:3000/d/da-async-pipeline)

## What it means

- **DrawingsFailing**: drawings kept failing for 10 minutes (`assets.pipeline.results{result="failed"}`). A drawing fails when its stroke log is missing or corrupt: an S3 outage longer than the bucket's 7-day expiry, or a game-server bug that uploaded a bad log.
- **JobsBacklog**: the oldest Celery job has waited more than 300 s, or no worker has reported for 5 minutes (`assets.jobs.oldest_age` missing). Drawings and moderation are late.

## Check

1. Failures: `{service_name="assets"} |= "failed"` in Loki; the drawing's row has the reason. Its `trace_id` goes back to the game server's `game.round.end`.
2. Workers: `make ps` (`assets-worker`, `assets-beat`), `make logs s=assets`. No worker means no `assets.jobs.oldest_age` series at all.
3. Dependencies: SeaweedFS (stroke logs and images), Redis (Celery's broker), MongoDB (replays), and `ai` (moderation; `scripts/ai-mode.sh` shows the stub's mode).

## Fix

- No worker: restart `assets-worker` (and `assets-beat`, which runs the sweeper that re-queues stuck drawings every minute).
- Slow AI: drawings that run out of moderation attempts go to the Django admin review queue (ADR 0011), so the backlog clears; moderators approve them at `http://localhost:8080/admin/`.
- Failing logs from one game-server version: roll that version back; drawings already failed stay failed (their logs are gone).

## Afterwards

Count the drawings that failed and why; if a game-server bug lost stroke logs, add a test for it.
