"""Limited drops end to end (task 6.8, ADR 0022): a real drop through the gateway, a lottery
waiting room, a race for fewer editions than buyers, and the authors paid out of escrow.

    uv run --project services/auth --with confluent-kafka --with boto3 \\
        --with 'protobuf>=7.36.2,<8' python scripts/drills/drops.py

Against `make up`; `make drops-check` runs it (CI's stack job; about a minute, most of it waiting
for the drop to open). In a worktree, set ENV_FILE and DRILL_JWT_KEY to the main checkout's.

1. A drawing by two authors: a RoundFinished with its stroke log (as dev-tools), which assets
   approves and the marketplace mints.
2. Coins for six buyers: a MatchFinished, published as dev-tools.
3. An admin creates a drop of 3 editions at 5 coins, opening 31 s later (the earliest allowed).
4. All six join before the open: places by lottery. Then all buy at once: exactly 3 editions sell,
   the other 3 get SOLD_OUT, and only the winners pay.
5. A winner's repeated Idempotency-Key returns the same edition; a new key gets ALREADY_BOUGHT.
6. Sold out, the sweeper pays the 15 coins to the authors (8 and 7), with CoinsCredited DROP_SALE.
"""

import concurrent.futures
import gzip
import json
import os
import random
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
from confluent_kafka import Consumer, Producer, TopicPartition

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT / "services" / "auth"))
sys.path.insert(0, str(ROOT / "proto" / "gen" / "python"))
from app.tokens import SigningKey, TokenIssuer  # noqa: E402
from doodle.events.v1 import matches_pb2, rounds_pb2, wallet_pb2  # noqa: E402
from doodle.game.v1 import stroke_log_pb2  # noqa: E402

GW = os.environ.get("GATEWAY", "http://localhost:8080")
KAFKA = os.environ.get("KAFKA_BROKERS", "localhost:9094")
ENV_FILE = Path(os.environ.get("ENV_FILE", ROOT / ".env"))
ROOM = "r_drops_drill"
EDITIONS, PRICE = 3, 5


def env(name: str) -> str:
    """A value from the environment or ENV_FILE (.env); never printed."""
    if value := os.environ.get(name):
        return value
    if ENV_FILE.is_file():
        for line in ENV_FILE.read_text().splitlines():
            if line.startswith(name + "="):
                return line.split("=", 1)[1]
    sys.exit(f"no {name} in {ENV_FILE}: run make up, or set ENV_FILE")


# dev-tools may write game.rounds.v1 and game.matches.v1 and read every topic (ADR 0013).
KAFKA_CONF = {
    "bootstrap.servers": KAFKA,
    "broker.address.family": "v4",
    "security.protocol": "SASL_PLAINTEXT",
    "sasl.mechanism": "SCRAM-SHA-512",
    "sasl.username": "dev-tools",
    "sasl.password": env("KAFKA_DEV_TOOLS_PASSWORD"),
}


def http(method: str, path: str, token: str, body: Any = None, key: str | None = None) -> tuple[int, Any, dict]:
    req = urllib.request.Request(GW + path, method=method, data=None if body is None else json.dumps(body).encode())
    req.add_header("Authorization", "Bearer " + token)
    if body is not None:
        req.add_header("Content-Type", "application/json")
    if key:
        req.add_header("Idempotency-Key", key)
    try:
        with urllib.request.urlopen(req, timeout=10) as resp:
            raw = resp.read()
            return resp.status, (json.loads(raw) if raw else None), dict(resp.headers)
    except urllib.error.HTTPError as e:
        raw = e.read()
        return e.code, (json.loads(raw) if raw else None), dict(e.headers)


def wait(what: str, timeout: float, check):  # type: ignore[no-untyped-def]
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        if (result := check()) is not None:
            return result
        time.sleep(0.5)
    sys.exit(f"FAIL: {what} within {timeout:.0f} s")


def check(expect: bool, what: str) -> None:
    if not expect:
        sys.exit(f"FAIL: {what}")
    print(f"  ok  {what}")


def produce(topic: str, key: str, value: bytes) -> None:
    producer = Producer(KAFKA_CONF)
    producer.produce(topic, key=key.encode(), value=value)
    if producer.flush(10) != 0:
        sys.exit(f"FAIL: {topic} not acknowledged by Kafka")


def balance(token: str) -> int:
    return http("GET", "/api/market/wallet", token)[1]["balance"]


def pay_rewards(users: list[str]) -> None:
    """A MatchFinished with these players in this order: 30, 20, 10, then 5 coins each."""
    now = datetime.now(UTC)
    event = matches_pb2.MatchEvent()
    mf = event.match_finished
    mf.meta.event_id, mf.meta.event_type, mf.meta.schema_version = str(uuid.uuid4()), "MatchFinished", 1
    mf.meta.producer = "drops-drill"
    mf.meta.occurred_at.FromDatetime(now)
    mf.match_id, mf.room_id = "m_drops_" + uuid.uuid4().hex[:12], ROOM
    mf.started_at.FromDatetime(now)
    mf.ended_at.FromDatetime(now)
    for i, user in enumerate(users):
        mf.results.add(user_id=user, score=100 - 10 * i, placement=i + 1)
    produce("game.matches.v1", ROOM, event.SerializeToString())


def draw(drawers: list[str]) -> None:
    """One round drawn by these drawers, as the game server reports it: the pipeline mints it."""
    match_id = "m_drops_" + uuid.uuid4().hex[:12]
    key = f"{match_id}/1.binpb.gz"
    log = stroke_log_pb2.StrokeLog(schema_version=1, match_id=match_id, room_id=ROOM, round_no=1,
                                   canvas_width=800, canvas_height=600)
    log.round_started_at.FromDatetime(datetime.now(UTC))
    stroke = log.strokes.add(stroke_id="s1", user_id=drawers[0], color="#7048e8", width=40, server_received_ms=500)
    for i in range(10):
        stroke.points.add(x=random.randint(500, 9500), y=random.randint(500, 9500), t_ms=i * 40)
    s3 = boto3.client("s3", endpoint_url="http://localhost:8333", region_name="us-east-1",
                      aws_access_key_id=env("S3_ACCESS_KEY"), aws_secret_access_key=env("S3_SECRET_KEY"),
                      config=Config(s3={"addressing_style": "path"}))
    s3.put_object(Bucket="round-logs", Key=key, Body=gzip.compress(log.SerializeToString()))
    event = rounds_pb2.RoundEvent()
    rf = event.round_finished
    rf.meta.event_id, rf.meta.event_type, rf.meta.schema_version = str(uuid.uuid4()), "RoundFinished", 1
    rf.meta.producer = "drops-drill"
    rf.meta.occurred_at.GetCurrentTime()
    rf.match_id, rf.room_id, rf.round_no, rf.word = match_id, ROOM, 1, "edition"
    rf.drawer_ids.extend(drawers)
    rf.stroke_log_key, rf.stroke_count, rf.canvas_width, rf.canvas_height = key, 1, 800, 600
    rf.ended_at.GetCurrentTime()
    produce("game.rounds.v1", ROOM, event.SerializeToString())


def drop_sales_since(started_ms: int, users: set[str], want: int) -> list[wallet_pb2.CoinsCredited]:
    consumer = Consumer({**KAFKA_CONF, "group.id": "dev-tools-drops-" + uuid.uuid4().hex[:8],
                         "enable.auto.commit": False})
    try:
        meta = consumer.list_topics("market.wallet.v1", timeout=10).topics["market.wallet.v1"]
        consumer.assign(consumer.offsets_for_times(
            [TopicPartition("market.wallet.v1", p, started_ms) for p in meta.partitions], timeout=10))
        found: list[wallet_pb2.CoinsCredited] = []
        deadline = time.monotonic() + 60
        while time.monotonic() < deadline and len(found) < want:
            msg = consumer.poll(1)
            if msg is None or msg.error():
                continue
            event = wallet_pb2.WalletEvent.FromString(msg.value()).coins_credited
            if event.user_id in users and event.reason == wallet_pb2.CREDIT_REASON_DROP_SALE:
                found.append(event)
        return found
    finally:
        consumer.close()


def main() -> int:
    started, started_ms = time.monotonic(), int(time.time() * 1000)
    run = uuid.uuid4().hex[:8]
    key = SigningKey.load(os.environ.get("DRILL_JWT_PEM", ""),
                          os.environ.get("DRILL_JWT_KEY") or str(ROOT / "secrets/jwt-private.pem"))
    issuer = TokenIssuer(key, GW + "/api/auth", timedelta(minutes=30))
    authors = [f"u_drops_{run}_a1", f"u_drops_{run}_a2"]
    buyers = [f"u_drops_{run}_b{i}" for i in range(1, 7)]
    token = {u: issuer.issue(u, ["player"], datetime.now(UTC), name=u)[0] for u in authors + buyers}
    admin = issuer.issue(f"u_drops_{run}_admin", ["player", "admin"], datetime.now(UTC), name="admin")[0]

    print("==> a drawing by two authors, and coins for six buyers (as dev-tools)")
    draw(authors)
    pay_rewards(buyers)
    drawing = wait("the drawing minted", 90, lambda: (
        d[0]["drawing_id"] if (d := http("GET", "/api/market/drawings?owner=me", token[authors[0]])[1]["drawings"])
        else None))
    wait("the rewards", 60, lambda: True if balance(token[buyers[-1]]) == 5 else None)

    print("==> an admin creates the drop: 3 editions at 5 coins, opening in 31 s")
    opens = datetime.now(UTC) + timedelta(seconds=31)
    body = {"drawing_id": drawing, "editions": EDITIONS, "price": PRICE,
            "opens_at": opens.isoformat().replace("+00:00", "Z"), "duration_minutes": 5}
    status, _, _ = http("POST", "/api/market/drops", token[buyers[0]], body, str(uuid.uuid4()))
    check(status == 403, "a player can't create a drop")
    status, drop, _ = http("POST", "/api/market/drops", admin, body, str(uuid.uuid4()))
    check(status == 201 and drop["status"] == "UPCOMING", "an admin creates it")
    path = f"/api/market/drops/{drop['drop_id']}"

    print("==> everyone joins before the open (a lottery), then all buy at once")
    for b in buyers:
        status, q, _ = http("POST", path + "/join", token[b])
        check(status == 200 and q["admitted"] is False, f"{b[-2:]} waits for the open")
    status, _, _ = http("POST", path + "/purchase", token[buyers[0]], key=str(uuid.uuid4()))
    check(status == 409, "nobody buys before the open")
    time.sleep(max(0.0, (opens - datetime.now(UTC)).total_seconds()) + 0.5)

    keys = {b: str(uuid.uuid4()) for b in buyers}
    with concurrent.futures.ThreadPoolExecutor(len(buyers)) as pool:
        answers = dict(zip(buyers, pool.map(lambda b: http("POST", path + "/purchase", token[b], key=keys[b]), buyers)))
    winners = [b for b, (s, _, _) in answers.items() if s == 201]
    losers = [b for b, (s, a, _) in answers.items() if s == 409 and a["error"]["code"] == "SOLD_OUT"]
    check(len(winners) == EDITIONS and len(losers) == len(buyers) - EDITIONS,
          f"exactly {EDITIONS} editions sold, the rest SOLD_OUT")
    numbers = sorted(answers[w][1]["edition_no"] for w in winners)
    check(numbers == list(range(1, EDITIONS + 1)), "editions 1 to 3, each once")
    paid = {b: balance(token[b]) for b in buyers}
    rewards = dict(zip(buyers, [30, 20, 10, 5, 5, 5]))
    check(all(paid[w] == rewards[w] - PRICE for w in winners) and all(paid[x] == rewards[x] for x in losers),
          "only the winners paid")

    winner = winners[0]
    status, again, headers = http("POST", path + "/purchase", token[winner], key=keys[winner])
    check(status == 201 and headers.get("Idempotency-Replayed") == "true"
          and again["edition_no"] == answers[winner][1]["edition_no"], "a repeated key returns the same edition")
    status, again, _ = http("POST", path + "/purchase", token[winner], key=str(uuid.uuid4()))
    check(status == 409 and again["error"]["code"] in ("ALREADY_BOUGHT", "SOLD_OUT"), "one edition per buyer")
    editions = http("GET", "/api/market/editions?owner=me", token[winner])[1]["editions"]
    check(len(editions) == 1 and editions[0]["drawing_id"] == drawing, "the winner owns the edition")

    print("==> sold out: the sweeper pays the authors")
    wait("the authors paid", 60, lambda: True if balance(token[authors[1]]) == 7 else None)
    check(balance(token[authors[0]]) == 8, "the authors share 15 coins, the first gets the odd one")
    sales = drop_sales_since(started_ms, set(authors), 2)
    check(sorted((e.user_id, e.amount) for e in sales) == sorted([(authors[0], 8), (authors[1], 7)])
          and all(e.reference == drop["drop_id"] for e in sales), "a CoinsCredited DROP_SALE per author")
    print(f"drops-check: OK ({time.monotonic() - started:.0f} s)")
    return 0


if __name__ == "__main__":
    sys.exit(main())
