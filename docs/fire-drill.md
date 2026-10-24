# The fire drill

- Severity: none (a drill: the alerts it fires are expected)
- Dashboard: [Service overview](http://localhost:3000/d/da-overview) for firing alerts; [Game server](http://localhost:3000/d/da-game-server) and [Async pipeline](http://localhost:3000/d/da-async-pipeline) for the two drills

## What it means

The M6 exit criterion (task 6.9): break something on purpose, check that the right alert fires and reaches the team, then fix it by following that alert's runbook, step by step, and time it. A drill tests the alert, the notification and the runbook together. `scripts/drills/fire_drill.py` injects the fault and records the times; a human follows the runbook alongside and judges whether it was enough.

Two drills:

| Drill | Fault | Alert | Runbook |
|---|---|---|---|
| `game-server` | game-server-1 goes away: locally `docker stop`, in AWS its ECS service scaled to 0. Run it during `make loadtest`, so its rooms move under load | GameServerDown (critical, after 1 minute) | [game-server-down.md](runbooks/game-server-down.md) |
| `search` | Elasticsearch stops: locally `docker stop`, in AWS on the data host through SSM. The script searches every 5 s, as players would | SearchUnavailable (warning, after 5 minutes of 503s) | [search-unavailable.md](runbooks/search-unavailable.md) |

## Check

Before a drill:

1. **Is the stack healthy?** `make smoke` (in AWS, `DOMAIN=icanfly.click make aws-check`), and no alert firing on the service overview: the script refuses to start if its alert isn't normal.
2. **Is the team told?** Say "fire drill" in the team channel first: the alerts reach Discord in AWS.
3. **For the game-server drill, is the load test running?** `make loadtest` (locally, or `GATEWAY=https://icanfly.click` from the dev box in AWS). Its rooms' resumes are the failover under load.

## Fix

Running it:

- Locally: `uv run --project services/auth python scripts/drills/fire_drill.py [game-server] [search]`.
- In AWS: run `make aws-grafana` in another terminal, then:

  ```bash
  DRILL_ENV=aws GATEWAY=https://icanfly.click GRAFANA=http://localhost:3300 GRAFANA_USER=admin GRAFANA_PASSWORD=<from SSM> DRILL_JWT_PEM="$(aws ssm get-parameter ... jwt-private-key ...)" uv run --project services/auth python scripts/drills/fire_drill.py
  ```

What happens:

1. The script injects the fault and waits for the alert, pending and then firing.
2. The alert's notification arrives: locally on webhook-receiver, which the script checks; in AWS in Discord, which a human checks.
3. Open the alert's runbook and follow its Check steps. The script applies the runbook's Fix, which is the command named there: `docker compose up -d …`, `aws ecs update-service … --desired-count 1`, or `docker start` on the data host.
4. The script waits until the service answers again and the alert resolves, then prints a table: seconds from the fault to pending, firing, notified, the runbook's fix, the service answering and resolved.

## Afterwards

- Record the table, and the failover times from the load test, in the evidence log (`docs/roadmap.md`).
- If a runbook step was missing, wrong or unclear while following it, fix the runbook in the same PR. That is the point of the drill.
- In AWS, say in the team channel that the drill is over.
