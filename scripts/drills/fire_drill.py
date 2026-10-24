"""The fire drill (task 6.9, docs/fire-drill.md): break something on purpose, check the
right alert fires, then fix it the way its runbook says and time each step.

    python3 scripts/drills/fire_drill.py [game-server|search ...]          the local stack
    DRILL_ENV=aws GRAFANA=http://localhost:3300 GRAFANA_USER=admin \\
      GRAFANA_PASSWORD=... python3 scripts/drills/fire_drill.py              AWS (make aws-grafana first)

Drills:
  game-server  GameServerDown: game-server-1 goes away (locally docker stop; in AWS its ECS
               service scaled to 0, a task that can't come back). The runbook's fix starts it
               again (docker compose up -d; aws ecs update-service --desired-count 1). Run it
               during make loadtest, and the rooms' move is the failover under load.
  search       SearchUnavailable: Elasticsearch stops (locally docker stop; in AWS on the data host
               through SSM). Searches made every 5 s answer 503 until the runbook's fix starts it.
               The alert needs 5 minutes of them, so this drill takes about 8.

Each drill prints when the alert went pending and firing, when the runbook's fix started, when
the service answered again and when the alert resolved. Locally the notification is checked on
webhook-receiver; in AWS it goes to Discord: look at the channel.
"""

import json
import os
import subprocess
import sys
import threading
import time
import urllib.error
import urllib.parse
import urllib.request
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
ENV = os.environ.get("DRILL_ENV", "local")
GRAFANA = os.environ.get("GRAFANA", "http://localhost:3000")
GATEWAY = os.environ.get("GATEWAY", "http://localhost:8080")
RECEIVER = os.environ.get("RECEIVER", "http://localhost:9000")
AWS = ["aws", "--profile", os.environ.get("AWS_PROFILE", "doodle-arena"), "--region", "us-west-2"]
CLUSTER = "doodle-arena"
COMPOSE = ["docker", "compose", "-f", str(ROOT / "infra/docker-compose.yml"), "--env-file",
           os.environ.get("ENV_FILE", str(ROOT / ".env"))]
AUTH = {}
if os.environ.get("GRAFANA_USER"):
    import base64

    pair = f"{os.environ['GRAFANA_USER']}:{os.environ.get('GRAFANA_PASSWORD', '')}".encode()
    AUTH = {"Authorization": "Basic " + base64.b64encode(pair).decode()}

START = time.monotonic()


def log(msg: str) -> None:
    print(f"[{time.monotonic() - START:6.0f} s] {msg}", flush=True)


def http_json(url: str) -> dict:
    with urllib.request.urlopen(urllib.request.Request(url, headers=AUTH), timeout=10) as r:
        return json.load(r)


def state(rule: str) -> str:
    """firing, pending or normal, from Grafana's rule states."""
    for g in http_json(f"{GRAFANA}/api/prometheus/grafana/api/v1/rules")["data"]["groups"]:
        for r in g["rules"]:
            if r["name"] == rule:
                states = [a["state"].lower() for a in r.get("alerts", [])]
                if any(s.startswith("alerting") for s in states):
                    return "firing"
                if any(s.startswith("pending") for s in states):
                    return "pending"
                return "normal"
    sys.exit(f"rule {rule} isn't loaded in Grafana")


def received(rule: str, status: str) -> int:
    """Notifications for rule with status the local webhook-receiver got (0 in AWS)."""
    try:
        hook = http_json(f"{RECEIVER}/admin/alerts")
    except (urllib.error.URLError, OSError):
        return 0
    return sum(1 for r in hook["received"] if rule in r.get("body", "") and status in r.get("body", "").upper())


def until(what: str, check, timeout: float, every: float = 5) -> float | None:
    start = time.monotonic()
    while time.monotonic() - start < timeout:
        if check():
            log(what)
            return time.monotonic() - START
        time.sleep(every)
    log(f"TIMEOUT: {what} ({timeout:.0f} s)")
    return None


def healthy(path: str) -> bool:
    try:
        with urllib.request.urlopen(f"{GATEWAY}{path}", timeout=5) as r:
            return r.status == 200
    except (urllib.error.URLError, OSError):
        return False


def run(cmd: list[str]) -> str:
    return subprocess.run(cmd, check=True, capture_output=True, text=True).stdout.strip()


def on_data_host(script: str) -> None:
    """Runs a shell line on the AWS data host through SSM Run Command."""
    instance = run([*AWS, "ec2", "describe-instances", "--filters", "Name=tag:Name,Values=doodle-data-host", "Name=tag:Root,Values=stack",
                    "Name=instance-state-name,Values=running", "--query", "Reservations[0].Instances[0].InstanceId",
                    "--output", "text"])
    cid = run([*AWS, "ssm", "send-command", "--instance-ids", instance, "--document-name", "AWS-RunShellScript",
               "--parameters", json.dumps({"commands": [script]}), "--query", "Command.CommandId", "--output", "text"])
    for _ in range(60):
        time.sleep(2)
        status = subprocess.run([*AWS, "ssm", "get-command-invocation", "--command-id", cid, "--instance-id", instance,
                                 "--query", "Status", "--output", "text"], capture_output=True, text=True).stdout.strip()
        if status in ("Success", "Failed", "Cancelled", "TimedOut"):
            if status != "Success":
                sys.exit(f"on the data host: {script}: {status}")
            return
    sys.exit(f"on the data host: {script}: no answer")


def drill(rule: str, inject, fix, back) -> dict:
    """Breaks, waits for the alert, fixes as the runbook says, waits for the service and the
    resolution. Returns the timings (seconds since the injection)."""
    sent_before = received(rule, "FIRING")
    if state(rule) != "normal":
        sys.exit(f"{rule} isn't normal before the drill; resolve it first")
    t0 = time.monotonic() - START
    log(f"{rule}: injecting the fault")
    inject()
    times: dict = {"rule": rule}
    pending = until(f"{rule} pending", lambda: state(rule) in ("pending", "firing"), 600)
    firing = until(f"{rule} firing", lambda: state(rule) == "firing", 900)
    times["pending_s"] = round(pending - t0) if pending else None
    times["firing_s"] = round(firing - t0) if firing else None
    if ENV == "local":
        sent = until(f"{rule}'s notification on webhook-receiver", lambda: received(rule, "FIRING") > sent_before, 120)
        times["notified_s"] = round(sent - t0) if sent else None
    log(f"{rule}: following the runbook's fix")
    fix_at = time.monotonic() - START
    fix()
    times["fix_s"] = round(fix_at - t0)
    up = until(f"{rule}: the service answers again", back, 600)
    times["service_back_s"] = round(up - t0) if up else None
    resolved = until(f"{rule} resolved", lambda: state(rule) == "normal", 900, every=10)
    times["resolved_s"] = round(resolved - t0) if resolved else None
    return times


def game_server() -> dict:
    if ENV == "aws":
        def inject():
            run([*AWS, "ecs", "update-service", "--cluster", CLUSTER, "--service", "game-server-1", "--desired-count", "0"])

        def fix():
            # game-server-down.md, In AWS: the service brought back.
            run([*AWS, "ecs", "update-service", "--cluster", CLUSTER, "--service", "game-server-1", "--desired-count", "1"])
    else:
        def inject():
            run(["docker", "stop", "doodle-arena-game-server-1-1"])

        def fix():
            # game-server-down.md, Fix: restart it.
            run([*COMPOSE, "up", "-d", "game-server-1"])
    return drill("GameServerDown", inject, fix, lambda: healthy("/healthz/game-server-1"))


def search() -> dict:
    sys.path.insert(0, str(ROOT / "services" / "auth"))
    from datetime import UTC, datetime, timedelta

    from app.tokens import SigningKey, TokenIssuer

    key = SigningKey.load(os.environ.get("DRILL_JWT_PEM", ""),
                          os.environ.get("DRILL_JWT_KEY") or str(ROOT / "secrets/jwt-private.pem"))
    token = TokenIssuer(key, GATEWAY + "/api/auth", timedelta(minutes=30)).issue(
        "u_fire_drill", ["player"], datetime.now(UTC), name="Fire drill")[0]
    stop = threading.Event()
    answers: list[int] = []

    def searching() -> None:
        # SearchUnavailable counts searches that answered 503: players searching meanwhile.
        while not stop.is_set():
            req = urllib.request.Request(f"{GATEWAY}/api/assets/search?q=cat", headers={"Authorization": f"Bearer {token}"})
            try:
                with urllib.request.urlopen(req, timeout=5) as r:
                    answers.append(r.status)
            except urllib.error.HTTPError as e:
                answers.append(e.code)
            except (urllib.error.URLError, OSError):
                answers.append(0)
            stop.wait(5)

    def search_works() -> bool:
        return bool(answers) and answers[-1] == 200

    if ENV == "aws":
        def inject():
            on_data_host("docker stop doodle-data-elasticsearch-1")

        def fix():
            # search-unavailable.md, Fix: start it; assets reconnects by itself.
            on_data_host("docker start doodle-data-elasticsearch-1")
    else:
        def inject():
            run(["docker", "stop", "doodle-arena-elasticsearch-1"])

        def fix():
            run([*COMPOSE, "--profile", "search", "up", "-d", "elasticsearch"])

    worker = threading.Thread(target=searching, daemon=True)
    worker.start()
    try:
        return drill("SearchUnavailable", inject, fix, search_works)
    finally:
        stop.set()


def main() -> int:
    drills = {"game-server": game_server, "search": search}
    chosen = sys.argv[1:] or list(drills)
    results = [drills[name]() for name in chosen]
    print("\n| Alert | Pending | Firing | Notified | Runbook fix started | Service back | Resolved |")
    print("|---|---|---|---|---|---|---|")
    for r in results:
        cell = lambda k: f"{r[k]} s" if r.get(k) is not None else ("Discord" if k == "notified_s" and ENV == "aws" else "-")
        print(f"| {r['rule']} | {cell('pending_s')} | {cell('firing_s')} | {cell('notified_s')} | {cell('fix_s')} "
              f"| {cell('service_back_s')} | {cell('resolved_s')} |")
    print("\nSeconds since the fault was injected.")
    return 0 if all(r.get("firing_s") and r.get("resolved_s") for r in results) else 1


if __name__ == "__main__":
    sys.exit(main())
