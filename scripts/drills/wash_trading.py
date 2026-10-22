"""Wash-trading drill (task 6.5b, ADR 0018): real sales through the marketplace, flags out of Flink.

    uv run --project services/auth --with confluent-kafka --with boto3 \\
        --with 'protobuf>=7.36.2,<8' python scripts/drills/wash_trading.py [--restart]

Against `PROFILES=analytics make up`; `make wash-check` runs it with --restart. Every sale is a
real one, so nothing on market.sales.v1 is forged (dev-tools can't write it):

1. Three fresh drill users, A, B and C (access tokens from auth's TokenIssuer and the local key,
   like mint_tokens.py; DRILL_JWT_KEY in a worktree).
2. Coins: a MatchFinished with the three of them, published as dev-tools, so the marketplace pays
   its rewards (30, 20 and 10 coins).
3. Two drawings for A: a random stroke log in round-logs (dev-tools' S3 identity) and a
   RoundFinished per drawing, so assets renders and approves them and the marketplace mints them.
4. Drawing 1 goes A to B and back to A (R1, a round trip); drawing 2 goes A to B to C and back to
   A (R3, a cycle). Each move is a listing at 1 coin and a purchase, with Idempotency-Keys.
5. Waits for both WashTradeFlagged on analytics.flags.v1 with the right rule, accounts and sales.

With --restart, the JobManager and TaskManager restart between drawing 1's first sale and the
sale that closes its loop, after a checkpoint that holds the first: the flag then proves that the
job started from that checkpoint, with the sale in its state (ADR 0018).
"""

import gzip
import json
import os
import random
import subprocess
import sys
import time
import urllib.error
import urllib.request
import uuid
from datetime import UTC, datetime, timedelta
from pathlib import Path
from typing import Any

import boto3
from botocore.client import Config
from confluent_kafka import Consumer, Producer

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT / "services" / "auth"))
sys.path.insert(0, str(ROOT / "proto" / "gen" / "python"))
from app.tokens import SigningKey, TokenIssuer  # noqa: E402
from doodle.events.v1 import analytics_pb2, matches_pb2, rounds_pb2  # noqa: E402
from doodle.game.v1 import stroke_log_pb2  # noqa: E402

GW = os.environ.get("GATEWAY", "http://localhost:8080")
FLINK = "http://localhost:8081"
KAFKA = os.environ.get("KAFKA_BROKERS", "localhost:9094")
ENV_FILE = Path(os.environ.get("ENV_FILE", ROOT / ".env"))


def env(name: str) -> str:
    """A value from the environment or ENV_FILE (.env); never printed."""
    if value := os.environ.get(name):
        return value
    if ENV_FILE.is_file():
        for line in ENV_FILE.read_text().splitlines():
            if line.startswith(name + "="):
                return line.split("=", 1)[1]
    sys.exit(f"no {name} in {ENV_FILE}: run make up, or set ENV_FILE")


# Compose publishes Kafka on 127.0.0.1 only and the broker advertises "localhost", which
# librdkafka may resolve to ::1 first: stay on IPv4. dev-tools may write game.rounds.v1 and
# game.matches.v1 and read every topic, with groups named dev-tools-*.
KAFKA_CONF = {
    "bootstrap.servers": KAFKA,
    "broker.address.family": "v4",
    "security.protocol": "SASL_PLAINTEXT",
    "sasl.mechanism": "SCRAM-SHA-512",
    "sasl.username": "dev-tools",
    "sasl.password": env("KAFKA_DEV_TOOLS_PASSWORD"),
}


def http(method: str, url: str, token: str | None = None, body: Any = None, key: str | None = None) -> Any:
    req = urllib.request.Request(url, method=method, data=None if body is None else json.dumps(body).encode())
    if body is not None:
        req.add_header("Content-Type", "application/json")
    if token:
        req.add_header("Authorization", "Bearer " + token)
    if key:
        req.add_header("Idempotency-Key", key)
    try:
        with urllib.request.urlopen(req, timeout=10) as resp:
            raw = resp.read()
            return json.loads(raw) if raw else None
    except urllib.error.HTTPError as e:
        sys.exit(f"FAIL: {method} {url}: HTTP {e.code} {e.read().decode()[:300]}")


def wait(what: str, timeout: float, check):  # type: ignore[no-untyped-def]
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        if (result := check()) is not None:
            return result
        time.sleep(1)
    sys.exit(f"FAIL: {what} within {timeout:.0f} s")


def produce(topic: str, key: str, value: bytes) -> None:
    producer = Producer(KAFKA_CONF)
    producer.produce(topic, key=key.encode(), value=value)
    if producer.flush(10) != 0:
        sys.exit(f"FAIL: {topic} not acknowledged by Kafka")


def pay_rewards(users: list[str]) -> None:
    now = datetime.now(UTC)
    event = matches_pb2.MatchEvent()
    mf = event.match_finished
    mf.meta.event_id, mf.meta.event_type, mf.meta.schema_version = str(uuid.uuid4()), "MatchFinished", 1
    mf.meta.producer = "wash-trading-drill"
    mf.meta.occurred_at.FromDatetime(now)
    mf.match_id, mf.room_id = "m_wash_" + uuid.uuid4().hex[:12], "r_wash_drill"
    mf.started_at.FromDatetime(now)
    mf.ended_at.FromDatetime(now)
    for i, user in enumerate(users):
        mf.results.add(user_id=user, score=100 - 10 * i, placement=i + 1)
    produce("game.matches.v1", "r_wash_drill", event.SerializeToString())


def mint(drawer: str) -> None:
    """One round drawn by drawer, as the game server reports it; the pipeline mints a drawing."""
    match_id = "m_wash_" + uuid.uuid4().hex[:12]
    key = f"{match_id}/1.binpb.gz"
    log = stroke_log_pb2.StrokeLog(schema_version=1, match_id=match_id, room_id="r_wash_drill", round_no=1,
                                   canvas_width=800, canvas_height=600)
    log.round_started_at.FromDatetime(datetime.now(UTC))
    # Random, so every image is new: drawings are stored by content.
    stroke = log.strokes.add(stroke_id="s1", user_id=drawer, color="#2f9e44", width=40, server_received_ms=500)
    for i in range(10):
        stroke.points.add(x=random.randint(500, 9500), y=random.randint(500, 9500), t_ms=i * 40)
    s3 = boto3.client("s3", endpoint_url="http://localhost:8333", region_name="us-east-1",
                      aws_access_key_id=env("S3_ACCESS_KEY"), aws_secret_access_key=env("S3_SECRET_KEY"),
                      config=Config(s3={"addressing_style": "path"}))
    s3.put_object(Bucket="round-logs", Key=key, Body=gzip.compress(log.SerializeToString()))

    event = rounds_pb2.RoundEvent()
    rf = event.round_finished
    rf.meta.event_id, rf.meta.event_type, rf.meta.schema_version = str(uuid.uuid4()), "RoundFinished", 1
    rf.meta.producer = "wash-trading-drill"
    rf.meta.occurred_at.GetCurrentTime()
    rf.match_id, rf.room_id, rf.round_no, rf.word = match_id, "r_wash_drill", 1, "loop"
    rf.drawer_ids.append(drawer)
    rf.stroke_log_key, rf.stroke_count, rf.canvas_width, rf.canvas_height = key, 1, 800, 600
    rf.ended_at.GetCurrentTime()
    produce("game.rounds.v1", "r_wash_drill", event.SerializeToString())


def sell(drawing: str, seller: str, buyer: str, tokens: dict[str, str]) -> str:
    """seller lists drawing at 1 coin and buyer buys it; returns the sale id."""
    listing = http("POST", f"{GW}/api/market/listings", tokens[seller], {"drawing_id": drawing, "price": 1},
                   key=str(uuid.uuid4()))
    http("POST", f"{GW}/api/market/listings/{listing['listing_id']}/purchase", tokens[buyer], key=str(uuid.uuid4()))
    sold = http("GET", f"{GW}/api/market/listings/{listing['listing_id']}", tokens[buyer])
    if sold["status"] != "SOLD" or sold["buyer_id"] != buyer:
        sys.exit(f"FAIL: listing {listing['listing_id']} is {sold['status']}, bought by {sold['buyer_id']}")
    return sold["sale_id"]


def job() -> dict[str, Any] | None:
    try:
        with urllib.request.urlopen(FLINK + "/jobs/overview", timeout=5) as resp:
            jobs = [j for j in json.load(resp)["jobs"] if j["name"] == "analytics" and j["state"] == "RUNNING"]
    except OSError:
        return None
    return jobs[0] if jobs else None


def last_checkpoint_trigger_ms() -> int:
    running = job()
    if running is None:
        return 0
    with urllib.request.urlopen(f"{FLINK}/jobs/{running['jid']}/checkpoints", timeout=5) as resp:
        completed = json.load(resp)["latest"]["completed"]
    return completed["trigger_timestamp"] if completed else 0


def restart_flink() -> None:
    """Restarts both Flink containers once a checkpoint started after the sale has completed."""
    after = int(time.time() * 1000) + 3000  # the sale reaches Flink through the outbox in ~1 s
    wait("a checkpoint after the first sale", 90, lambda: True if last_checkpoint_trigger_ms() > after else None)
    old = job()
    print("  restarting the JobManager and TaskManager (docker restart)")
    subprocess.run(["docker", "restart", "doodle-arena-analytics-jobmanager-1", "doodle-arena-analytics-taskmanager-1"],
                   check=True, capture_output=True)
    started = wait("the job running again", 180,
                   lambda: (j if (j := job()) is not None and (old is None or j["jid"] != old["jid"]) else None))
    with urllib.request.urlopen(f"{FLINK}/jobs/{started['jid']}/checkpoints", timeout=5) as resp:
        restored = json.load(resp)["latest"]["restored"]
    if not restored:
        sys.exit("FAIL: the new job didn't start from a checkpoint")
    print(f"  the new job started from {restored['external_path']}")


def flags(drawings: set[str], want: int) -> dict[str, analytics_pb2.WashTradeFlagged]:
    consumer = Consumer({**KAFKA_CONF, "group.id": "dev-tools-wash-" + uuid.uuid4().hex[:8],
                         "auto.offset.reset": "earliest", "enable.auto.commit": False})
    consumer.subscribe(["analytics.flags.v1"])
    found: dict[str, analytics_pb2.WashTradeFlagged] = {}

    def poll() -> dict[str, analytics_pb2.WashTradeFlagged] | None:
        for msg in consumer.consume(100, timeout=1):
            if msg.error() is None and msg.key().decode() in drawings:
                event = analytics_pb2.AnalyticsFlagEvent.FromString(msg.value())
                found[event.wash_trade_flagged.drawing_id] = event.wash_trade_flagged
        return found if len(found) >= want else None

    try:
        return wait("both flags on analytics.flags.v1", 120, poll)
    finally:
        consumer.close()


def main(restart: bool) -> int:
    if job() is None:
        sys.exit("FAIL: the analytics job isn't running (PROFILES=analytics make up; make logs s=analytics)")
    started = time.monotonic()
    run = uuid.uuid4().hex[:8]
    users = {name: f"u_wash_{run}_{name.lower()}" for name in ("A", "B", "C")}
    issuer = TokenIssuer(SigningKey.load(os.environ.get("DRILL_JWT_PEM", ""),
                                         os.environ.get("DRILL_JWT_KEY") or str(ROOT / "secrets/jwt-private.pem")),
                         GW + "/api/auth", timedelta(minutes=30))
    tokens = {u: issuer.issue(u, ["player"], datetime.now(UTC), name=u)[0] for u in users.values()}
    a, b, c = users["A"], users["B"], users["C"]

    pay_rewards([a, b, c])
    wait("the rewards", 60, lambda: True if all(
        http("GET", f"{GW}/api/market/wallet", tokens[u])["balance"] > 0 for u in (a, b, c)) else None)
    print(f"rewards paid to {a}, {b}, {c}")

    mint(a)
    mint(a)
    owned = wait("two drawings minted for A", 120, lambda: (
        d if len(d := http("GET", f"{GW}/api/market/drawings?owner=me", tokens[a])["drawings"]) >= 2 else None))
    d1, d2 = owned[0]["drawing_id"], owned[1]["drawing_id"]
    print(f"minted {d1} and {d2} for A")

    s1 = sell(d1, a, b, tokens)
    print(f"{d1}: A sold it to B")
    if restart:
        restart_flink()
    s2 = sell(d1, b, a, tokens)
    print(f"{d1}: B sold it back to A (a round trip)")
    s3 = sell(d2, a, b, tokens)
    s4 = sell(d2, b, c, tokens)
    s5 = sell(d2, c, a, tokens)
    print(f"{d2}: A to B to C and back to A (a cycle)")

    got = flags({d1, d2}, 2)
    ok = True
    for drawing, rule, accounts, sales in ((d1, analytics_pb2.WASH_TRADE_RULE_ROUND_TRIP, [a, b, a], [s1, s2]),
                                          (d2, analytics_pb2.WASH_TRADE_RULE_CYCLE, [a, b, c, a], [s3, s4, s5])):
        f = got[drawing]
        good = f.rule == rule and list(f.account_ids) == accounts and list(f.sale_ids) == sales
        ok &= good
        print(f"  {'ok  ' if good else 'FAIL'}  {analytics_pb2.WashTradeRule.Name(f.rule)} on {drawing}: "
              f"{' -> '.join(x.removeprefix(f'u_wash_{run}_') for x in f.account_ids)}, {f.volume} coins, "
              f"flag {f.flag_id}")
    print(f"wash-trading drill: {'OK' if ok else 'FAIL'} in {time.monotonic() - started:.0f} s")
    return 0 if ok else 1


if __name__ == "__main__":
    sys.exit(main("--restart" in sys.argv[1:]))
