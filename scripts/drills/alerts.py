"""Alert drill (task 5.1, M5): the ledger-imbalance and error-rate alerts fire, reach the contact
point, and clear again.

    python3 scripts/drills/alerts.py [ledger|errors|kafka|wash]

Against `make up` (standard library only; needs `~/.docker/bin` on PATH or there). It changes the
shared stack for a few minutes and puts everything back, even when interrupted:

1. LedgerImbalance: gives the drill account `drill-alerts` (a user account with no ledger entries,
   created on the first run) a balance of 1 coin, so its balance is no longer the sum of its
   entries (`market.ledger.mismatched_accounts` = 1). It never touches the append-only ledger.
   Waits for the alert to fire and to be sent, then sets the balance back to 0 and waits for the
   alert to resolve.
2. HighErrorRate: switches the AI moderation stub to `fail` (every call answers 503) and sends it
   two moderation calls a second from inside its container until the alert fires for `ai`
   (5xx above 1% for 5 minutes: about 6 minutes), then restores the stub's previous mode. It
   doesn't wait for the alert to resolve, which takes another 5 minutes of clean traffic.
   Drawings moderated meanwhile retry, or wait in the review queue.
3. KafkaAdminUsed and KafkaAuthFailures (ADR 0013): runs one kafka-admin command inside the broker
   container (an admin login on its localhost ADMIN listener) and waits for KafkaAdminUsed; then
   tries a wrong password on the broker every 15 s until KafkaAuthFailures fires (failures every
   minute for 5 minutes: about 6 minutes). Nothing changes, so there is nothing to put back; the
   alerts resolve on their own 5 minutes later. Not in the default run: `alerts.py kafka`.
4. WashTradingFlagged (ADR 0018): runs scripts/drills/wash_trading.py (real sales: a round trip
   and a cycle; needs `PROFILES=analytics make up` and uv) and waits for both rule instances to
   fire. Real sales can't be taken back, so the alert resolves on its own after 15 minutes, and
   a second run within them refuses to start. Not in the default run: `alerts.py wash`.

Notifications go to Discord when DISCORD_ALERT_WEBHOOK_URL is set in .env; check the channel.
Otherwise they land on the webhook-receiver test service, and the drill checks them there.
Prints the time each alert took to fire, for the evidence log in docs/roadmap.md.
"""

import json
import os
import subprocess
import sys
import time
import urllib.error
import urllib.request
from pathlib import Path

GRAFANA = os.environ.get("GRAFANA", "http://localhost:3000")
RECEIVER = os.environ.get("RECEIVER", "http://localhost:9000")
POSTGRES = "doodle-arena-postgres-1"
AI = "doodle-arena-ai-1"
KAFKA = "doodle-arena-kafka-1"
# A client config with a password no Kafka user has, written inside the broker container.
BAD_LOGIN = """cat > /tmp/drill-bad-login.properties <<'EOF'
security.protocol=SASL_PLAINTEXT
sasl.mechanism=SCRAM-SHA-512
sasl.jaas.config=org.apache.kafka.common.security.scram.ScramLoginModule required username="marketplace" password="not-the-password";
EOF
timeout 20 /opt/kafka/bin/kafka-topics.sh --bootstrap-server localhost:9092 --command-config /tmp/drill-bad-login.properties --list >/dev/null 2>&1
true"""
DRILL_OWNER = "drill-alerts"
ROOT = Path(__file__).resolve().parents[2]

os.environ["PATH"] = os.path.expanduser("~/.docker/bin") + os.pathsep + os.environ["PATH"]


def log(msg: str) -> None:
    print(f"{time.strftime('%H:%M:%S')} {msg}", flush=True)


def http_json(url: str, method: str = "GET", body: dict | None = None) -> dict:
    data = json.dumps(body).encode() if body is not None else None
    req = urllib.request.Request(url, data=data, method=method, headers={"Content-Type": "application/json"})
    with urllib.request.urlopen(req, timeout=10) as resp:
        return json.load(resp)


def sql(statement: str) -> str:
    """Runs SQL in the marketplace database as its owner; returns the unaligned output."""
    out = subprocess.run(
        ["docker", "exec", "-i", POSTGRES, "psql", "-U", "postgres", "-d", "marketplace", "-v", "ON_ERROR_STOP=1", "-tAq"],
        input=statement, capture_output=True, text=True, check=True,
    )
    return out.stdout.strip()


def ai_mode(config: dict | None = None) -> dict:
    """Reads (or sets) the AI stub's moderation config through its internal port."""
    script = (
        "import json, os, sys, urllib.request\n"
        "body = os.environ['BODY'].encode() or None\n"
        "req = urllib.request.Request('http://127.0.0.1:9091/internal/moderation-mode', data=body,"
        " method='PUT' if body else 'GET', headers={'Content-Type': 'application/json'})\n"
        "print(urllib.request.urlopen(req, timeout=5).read().decode())\n"
    )
    body = json.dumps(config) if config else ""
    out = subprocess.run(["docker", "exec", "-i", "-e", f"BODY={body}", AI, "python", "-"],
                         input=script, capture_output=True, text=True, check=True)
    return json.loads(out.stdout)


# Two moderation calls a second for `seconds`, inside the ai container (port 8080 isn't published).
# Bounded, so an interrupted drill leaves no traffic running.
TRAFFIC = """
import time, urllib.error, urllib.request
png = b"\\x89PNG\\r\\n\\x1a\\n" + bytes(64)
end = time.monotonic() + {seconds}
while time.monotonic() < end:
    req = urllib.request.Request("http://127.0.0.1:8080/v1/moderate", data=png, method="POST",
                                 headers={{"Content-Type": "image/png"}})
    try:
        urllib.request.urlopen(req, timeout=5).read()
    except urllib.error.HTTPError:
        pass
    time.sleep(0.5)
"""


def alerts(rule: str) -> list[dict]:
    groups = http_json(f"{GRAFANA}/api/prometheus/grafana/api/v1/rules")["data"]["groups"]
    for g in groups:
        for r in g["rules"]:
            if r["name"] == rule:
                return r.get("alerts", [])
    sys.exit(f"rule {rule} isn't loaded in Grafana; run make alerts-check")


def state(rule: str, **labels: str) -> str:
    """firing, pending or normal (also when no instance matches)."""
    for a in alerts(rule):
        if all(a["labels"].get(k) == v for k, v in labels.items()):
            s = a["state"].lower()  # Grafana's instance states: Alerting, Pending, Normal (with a reason)
            return "firing" if s.startswith("alerting") else "pending" if s.startswith("pending") else "normal"
    return "normal"


def wait_for(what: str, check, timeout: float, every: float = 5) -> float | None:
    """Polls check() until it is true; returns the seconds it took, or None after timeout."""
    start = time.monotonic()
    while time.monotonic() - start < timeout:
        if check():
            took = time.monotonic() - start
            log(f"{what} after {took:.0f} s")
            return took
        time.sleep(every)
    log(f"TIMEOUT waiting for {what} ({timeout:.0f} s)")
    return None


def received(rule: str, status: str) -> int:
    """Notifications for rule with status (FIRING or RESOLVED) the webhook-receiver got."""
    try:
        hook = http_json(f"{RECEIVER}/admin/alerts")
    except (urllib.error.URLError, OSError):
        return 0
    return sum(1 for r in hook["received"] if rule in r.get("body", "") and status in r.get("body", "").upper())


def ledger_drill() -> dict:
    log("LedgerImbalance: giving the drill account 1 coin its ledger entries don't explain")
    account = sql(f"""
        INSERT INTO accounts (owner_type, owner_id) VALUES ('user', '{DRILL_OWNER}') ON CONFLICT DO NOTHING;
        INSERT INTO balances (account_id, owner_type)
            SELECT account_id, 'user' FROM accounts WHERE owner_type = 'user' AND owner_id = '{DRILL_OWNER}'
            ON CONFLICT DO NOTHING;
        SELECT account_id FROM accounts WHERE owner_type = 'user' AND owner_id = '{DRILL_OWNER}';
    """)
    if sql(f"SELECT count(*) FROM ledger_entries WHERE account_id = {account}") != "0":
        sys.exit(f"the drill account {account} has ledger entries; refusing to touch it")
    sent_before = received("LedgerImbalance", "FIRING")
    result: dict = {}
    try:
        sql(f"UPDATE balances SET balance = 1, updated_at = now() WHERE account_id = {account}")
        result["fire_s"] = wait_for("LedgerImbalance firing", lambda: state("LedgerImbalance") == "firing", 300)
        if result["fire_s"] is not None:
            # The policy waits 30 s (group_wait) before the first notification of a group.
            start = time.monotonic()
            sent = wait_for("its notification on webhook-receiver",
                            lambda: received("LedgerImbalance", "FIRING") > sent_before, 90)
            result["notify_s"] = None if sent is None else result["fire_s"] + (time.monotonic() - start)
    finally:
        sql(f"UPDATE balances SET balance = 0, updated_at = now() WHERE account_id = {account}")
        log("LedgerImbalance: balance restored")
    result["resolve_s"] = wait_for("LedgerImbalance resolved", lambda: state("LedgerImbalance") == "normal", 300)
    return result


def errors_drill() -> dict:
    previous = ai_mode()
    sent_before = received("HighErrorRate", "FIRING")
    log(f"HighErrorRate: AI stub to fail (was {previous['mode']}), two calls a second")
    result: dict = {}
    start = time.monotonic()
    traffic = None
    try:
        ai_mode({**previous, "mode": "fail", "failure_rate": 0})
        while time.monotonic() - start < 720:
            if traffic is None or traffic.poll() is not None:
                traffic = subprocess.Popen(["docker", "exec", "-i", AI, "python", "-"], stdin=subprocess.PIPE,
                                           stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, text=True)
                traffic.stdin.write(TRAFFIC.format(seconds=60))
                traffic.stdin.close()
            s = state("HighErrorRate", service_name="ai")
            if s == "firing":
                result["fire_s"] = time.monotonic() - start
                log(f"HighErrorRate firing for ai after {result['fire_s']:.0f} s")
                break
            time.sleep(5)
        else:
            log("TIMEOUT waiting for HighErrorRate firing for ai (720 s)")
            result["fire_s"] = None
    finally:
        if traffic is not None and traffic.poll() is None:
            traffic.terminate()  # the loop inside the container ends within 60 s on its own
        ai_mode(previous)
        log(f"HighErrorRate: AI stub back to {previous['mode']}")
    if result.get("fire_s") is not None:
        sent = wait_for("its notification on webhook-receiver",
                        lambda: received("HighErrorRate", "FIRING") > sent_before, 90)
        result["notify_s"] = None if sent is None else result["fire_s"] + sent
    return result


def kafka_drill() -> dict:
    result: dict = {}
    sent_before = received("KafkaAdminUsed", "FIRING")
    log("KafkaAdminUsed: one kafka-admin command inside the broker container")
    subprocess.run(["docker", "exec", KAFKA, "kafka-admin", "kafka-topics.sh", "--list"],
                   check=True, capture_output=True)
    result["admin_fire_s"] = wait_for("KafkaAdminUsed firing", lambda: state("KafkaAdminUsed") == "firing", 240)
    if result["admin_fire_s"] is not None:
        start = time.monotonic()
        sent = wait_for("its notification on webhook-receiver",
                        lambda: received("KafkaAdminUsed", "FIRING") > sent_before, 90)
        result["admin_notify_s"] = None if sent is None else result["admin_fire_s"] + (time.monotonic() - start)

    sent_before = received("KafkaAuthFailures", "FIRING")
    log("KafkaAuthFailures: a wrong password on the broker every 15 s")
    start = time.monotonic()
    result["auth_fire_s"] = None
    while time.monotonic() - start < 600:
        subprocess.run(["docker", "exec", KAFKA, "bash", "-c", BAD_LOGIN], capture_output=True)
        if state("KafkaAuthFailures") == "firing":
            result["auth_fire_s"] = time.monotonic() - start
            log(f"KafkaAuthFailures firing after {result['auth_fire_s']:.0f} s")
            break
        time.sleep(15)
    else:
        log("TIMEOUT waiting for KafkaAuthFailures firing (600 s)")
    subprocess.run(["docker", "exec", KAFKA, "rm", "-f", "/tmp/drill-bad-login.properties"], capture_output=True)
    if result["auth_fire_s"] is not None:
        sent = wait_for("its notification on webhook-receiver",
                        lambda: received("KafkaAuthFailures", "FIRING") > sent_before, 90)
        result["auth_notify_s"] = None if sent is None else result["auth_fire_s"] + sent
    return result


def wash_drill() -> dict:
    if "firing" in (state("WashTradingFlagged", rule="round_trip"), state("WashTradingFlagged", rule="cycle")):
        sys.exit("WashTradingFlagged is already firing (a run in the last 15 minutes); try again once it resolves")
    sent_before = received("WashTradingFlagged", "FIRING")
    log("WashTradingFlagged: a round trip and a cycle of real sales (scripts/drills/wash_trading.py)")
    start = time.monotonic()
    subprocess.run(["uv", "run", "--project", "services/auth", "--with", "confluent-kafka", "--with", "boto3",
                    "--with", "protobuf>=7.36.2,<8", "python", "scripts/drills/wash_trading.py"], cwd=ROOT, check=True)
    result: dict = {}
    fired = wait_for("WashTradingFlagged firing for round_trip and cycle", lambda: all(
        state("WashTradingFlagged", rule=r) == "firing" for r in ("round_trip", "cycle")), 300)
    result["fire_s"] = None if fired is None else time.monotonic() - start
    if result["fire_s"] is not None:
        sent = wait_for("its notification on webhook-receiver",
                        lambda: received("WashTradingFlagged", "FIRING") > sent_before, 90)
        result["notify_s"] = None if sent is None else result["fire_s"] + sent
    return result


def main() -> int:
    which = sys.argv[1:] or ["ledger", "errors"]
    if not set(which) <= {"ledger", "errors", "kafka", "wash"}:
        sys.exit("usage: alerts.py [ledger|errors|kafka|wash]")
    try:
        urllib.request.urlopen(f"{RECEIVER}/healthz", timeout=3)
        target = "webhook-receiver (or Discord, if DISCORD_ALERT_WEBHOOK_URL is set: check the channel)"
    except (urllib.error.URLError, OSError):
        target = "Discord (check the channel)"
    results = {}
    if "ledger" in which:
        results["LedgerImbalance"] = ledger_drill()
    if "errors" in which:
        results["HighErrorRate"] = errors_drill()
    if "kafka" in which:
        k = kafka_drill()
        results["KafkaAdminUsed"] = {"fire_s": k.get("admin_fire_s"), "notify_s": k.get("admin_notify_s")}
        results["KafkaAuthFailures"] = {"fire_s": k.get("auth_fire_s"), "notify_s": k.get("auth_notify_s")}
    if "wash" in which:
        results["WashTradingFlagged"] = wash_drill()

    print("\nalert             fired after   sent after   resolved after")
    ok = True
    for name, r in results.items():
        fmt = lambda v: "-" if v is None else f"{v:.0f} s"  # noqa: E731
        print(f"{name:17} {fmt(r.get('fire_s')):>11} {fmt(r.get('notify_s')):>12} {fmt(r.get('resolve_s')):>16}")
        ok = ok and r.get("fire_s") is not None
    print(f"\nNotifications go to {target}.")
    print("Only the webhook-receiver is checked here; with Discord configured, 'sent after' stays '-'.")
    return 0 if ok else 1


if __name__ == "__main__":
    sys.exit(main())
