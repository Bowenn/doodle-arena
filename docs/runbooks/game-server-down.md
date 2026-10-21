# Runbook: GameServerDown

- Severity: critical
- Dashboard: [Service overview, Healthy game servers](http://localhost:3000/d/da-overview)

## What it means

The lobby has seen fewer healthy game servers than expected (2 in the local stack) for a minute: a server stopped heartbeating (`lobby.servers.healthy`). Rooms on it fail over to another server (ADR 0007), and new rooms have fewer places to go. If no server is healthy, nobody can play.

## Check

1. Which server: `make ps`, `make logs s=game-server` (both instances).
2. Failover: `game.room.takeovers` on the game-server dashboard should rise on the surviving server, and `game.reconnects{result="resumed"}` as players come back.
3. The lobby's view: its log shows `game servers expired: no heartbeat` with the server ids. If every server expired at once, suspect the lobby or the network rather than the servers.
4. Was it deliberate? A failover drill (`scripts/drills/failover.py`) or a deploy fires this alert by design.

## Fix

- A crashed server: restart it (`docker compose -f infra/docker-compose.yml --env-file .env up -d game-server-1`). Its rooms already moved; it comes back empty and takes new rooms.
- A frozen server (paused, GC, overload): the lease's fencing token stops it from writing rooms it lost. Restart it.
- The lobby can't reach any server: check the lobby and the internal gRPC port between them.

## Afterwards

Record the failover time from the drill or the incident (the evidence log has a row for it), and whether any room was lost.

## In AWS

The same checks with the AWS commands in [Runbooks in AWS](README.md). In AWS the game servers are the ECS services `game-server-1` and `game-server-2`:

- A deploy fires this alert by design: `aws-up` with a new image replaces the other services first, then one game server at a time, and each drains into the other (task 5.5). Look at the services' deployments before treating it as an incident.
- A stopped task is replaced by ECS on its own. Its log (`aws logs tail /doodle-arena/ecs --log-stream-name-prefix game-server-1`) says why: `drain finished` is a normal stop, a missing `stopped` line a crash or an OOM kill (`aws ecs describe-tasks ... --query 'tasks[0].stoppedReason'`).
- To restart one: `aws ecs update-service --cluster doodle-arena --service game-server-1 --force-new-deployment`. It drains first.
