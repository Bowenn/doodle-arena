"""Tips end to end (task 6.3, ADR 0021): real tips through the gateway, a RoundFinished from
dev-tools, the drawers paid out of escrow.

    uv run --project services/auth --with confluent-kafka --with boto3 \\
        --with 'protobuf>=7.36.2,<8' python scripts/drills/tips.py

Against `make up`; `make tips-check` runs it (CI's stack job). In a worktree, set ENV_FILE and
DRILL_JWT_KEY to the main checkout's.

1. Coins: a MatchFinished, published as dev-tools, pays two drawers and three viewers.
2. Tips to round 1 of a new match: two viewers tip; one drawer tips their own round; one viewer
   names another room. A repeated Idempotency-Key returns the first tip.
3. The round's RoundFinished (with its stroke log, as the game server sends it) settles it: the
   two drawers share the viewers' tips, the first gets the odd coin, and the own tip and the
   wrong room's go back.
4. A tip right after the settlement is paid by the sweeper; 31 s after it, a tip is refused
   (ROUND_OVER).
5. Every payout and refund has its CoinsCredited (reasons TIP and TIP_REFUND) on market.wallet.v1.

    ... tips.py settle <match_id> <drawer_id>

publishes the RoundFinished for round 1 of an existing match and reports how long the drawer
waited for the coins (scripts/loadtest.sh tips uses it after k6).
"""

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
ROOM = "r_tips_drill"


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


class HttpError(Exception):
    def __init__(self, status: int, body: dict[str, Any]) -> None:
        super().__init__(f"HTTP {status}")
        self.status, self.body = status, body


def http(method: str, path: str, token: str, body: Any = None, key: str | None = None) -> tuple[Any, dict[str, str]]:
    req = urllib.request.Request(GW + path, method=method, data=None if body is None else json.dumps(body).encode())
    req.add_header("Authorization", "Bearer " + token)
    if body is not None:
        req.add_header("Content-Type", "application/json")
    if key:
        req.add_header("Idempotency-Key", key)
    try:
        with urllib.request.urlopen(req, timeout=10) as resp:
            raw = resp.read()
            return (json.loads(raw) if raw else None), dict(resp.headers)
    except urllib.error.HTTPError as e:
        raw = e.read()
        raise HttpError(e.code, json.loads(raw) if raw else {}) from None


def wait(what: str, timeout: float, check):  # type: ignore[no-untyped-def]
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        if (result := check()) is not None:
            return result
        time.sleep(0.5)
    sys.exit(f"FAIL: {what} within {timeout:.0f} s")


def produce(topic: str, key: str, value: bytes) -> None:
    producer = Producer(KAFKA_CONF)
    producer.produce(topic, key=key.encode(), value=value)
    if producer.flush(10) != 0:
        sys.exit(f"FAIL: {topic} not acknowledged by Kafka")


def issuer() -> TokenIssuer:
    key = SigningKey.load(os.environ.get("DRILL_JWT_PEM", ""),
                          os.environ.get("DRILL_JWT_KEY") or str(ROOT / "secrets/jwt-private.pem"))
    return TokenIssuer(key, GW + "/api/auth", timedelta(minutes=30))


def balance(token: str) -> int:
    return http("GET", "/api/market/wallet", token)[0]["balance"]


def pay_rewards(users: list[str]) -> None:
    """A MatchFinished with these players in this order: 30, 20, 10, then 5 coins each."""
    now = datetime.now(UTC)
    event = matches_pb2.MatchEvent()
    mf = event.match_finished
    mf.meta.event_id, mf.meta.event_type, mf.meta.schema_version = str(uuid.uuid4()), "MatchFinished", 1
    mf.meta.producer = "tips-drill"
    mf.meta.occurred_at.FromDatetime(now)
    mf.match_id, mf.room_id = "m_tips_" + uuid.uuid4().hex[:12], ROOM
    mf.started_at.FromDatetime(now)
    mf.ended_at.FromDatetime(now)
    for i, user in enumerate(users):
        mf.results.add(user_id=user, score=100 - 10 * i, placement=i + 1)
    produce("game.matches.v1", ROOM, event.SerializeToString())


def finish_round(match_id: str, room: str, drawers: list[str]) -> None:
    """Round 1's RoundFinished as the game server sends it, with its stroke log in round-logs (assets
    reads the same event and renders the drawing)."""
    key = f"{match_id}/1.binpb.gz"
    log = stroke_log_pb2.StrokeLog(schema_version=1, match_id=match_id, room_id=room, round_no=1,
                                   canvas_width=800, canvas_height=600)
    log.round_started_at.FromDatetime(datetime.now(UTC))
    stroke = log.strokes.add(stroke_id="s1", user_id=drawers[0], color="#e8590c", width=40, server_received_ms=500)
    for i in range(10):
        stroke.points.add(x=random.randint(500, 9500), y=random.randint(500, 9500), t_ms=i * 40)
    s3 = boto3.client("s3", endpoint_url="http://localhost:8333", region_name="us-east-1",
                      aws_access_key_id=env("S3_ACCESS_KEY"), aws_secret_access_key=env("S3_SECRET_KEY"),
                      config=Config(s3={"addressing_style": "path"}))
    s3.put_object(Bucket="round-logs", Key=key, Body=gzip.compress(log.SerializeToString()))

    event = rounds_pb2.RoundEvent()
    rf = event.round_finished
    rf.meta.event_id, rf.meta.event_type, rf.meta.schema_version = str(uuid.uuid4()), "RoundFinished", 1
    rf.meta.producer = "tips-drill"
    rf.meta.occurred_at.GetCurrentTime()
    rf.match_id, rf.room_id, rf.round_no, rf.word = match_id, room, 1, "coin"
    rf.drawer_ids.extend(drawers)
    rf.stroke_log_key, rf.stroke_count, rf.canvas_width, rf.canvas_height = key, 1, 800, 600
    rf.ended_at.GetCurrentTime()
    produce("game.rounds.v1", room, event.SerializeToString())


def credits_since(started_ms: int, users: set[str], want: int) -> list[wallet_pb2.CoinsCredited]:
    """The CoinsCredited for these users on market.wallet.v1 since started_ms (by timestamp)."""
    consumer = Consumer({**KAFKA_CONF, "group.id": "dev-tools-tips-" + uuid.uuid4().hex[:8],
                         "enable.auto.commit": False})
    try:
        meta = consumer.list_topics("market.wallet.v1", timeout=10).topics["market.wallet.v1"]
        offsets = consumer.offsets_for_times(
            [TopicPartition("market.wallet.v1", p, started_ms) for p in meta.partitions], timeout=10)
        consumer.assign(offsets)
        found: list[wallet_pb2.CoinsCredited] = []
        deadline = time.monotonic() + 60
        while time.monotonic() < deadline and len(found) < want:
            msg = consumer.poll(1)
            if msg is None or msg.error():
                continue
            event = wallet_pb2.WalletEvent.FromString(msg.value()).coins_credited
            if event.user_id in users and event.reason in (wallet_pb2.CREDIT_REASON_TIP,
                                                           wallet_pb2.CREDIT_REASON_TIP_REFUND):
                found.append(event)
        return found
    finally:
        consumer.close()


def check(expect: bool, what: str) -> None:
    if not expect:
        sys.exit(f"FAIL: {what}")
    print(f"  ok  {what}")


def main() -> int:
    started = time.monotonic()
    started_ms = int(time.time() * 1000)
    run = uuid.uuid4().hex[:8]
    names = ("d1", "d2", "v1", "v2", "v3")
    ids = {n: f"u_tips_{run}_{n}" for n in names}
    mint = issuer()
    tokens = {n: mint.issue(ids[n], ["player"], datetime.now(UTC), name=n)[0] for n in names}

    print("==> coins for two drawers and three viewers (a MatchFinished as dev-tools)")
    # v1 30, v2 20, v3 10, d1 5, d2 5.
    pay_rewards([ids["v1"], ids["v2"], ids["v3"], ids["d1"], ids["d2"]])
    wait("the rewards", 60, lambda: True if balance(tokens["d2"]) == 5 else None)

    match = "m_tips_" + uuid.uuid4().hex[:12]
    rnd = {"room_id": ROOM, "match_id": match, "round_no": 1}
    print(f"==> tips to round 1 of {match}")
    key = str(uuid.uuid4())
    first, _ = http("POST", "/api/market/tips", tokens["v1"], {**rnd, "amount": 10}, key)
    again, headers = http("POST", "/api/market/tips", tokens["v1"], {**rnd, "amount": 10}, key)
    check(again["tip_id"] == first["tip_id"] and headers.get("Idempotency-Replayed") == "true",
          "a repeated Idempotency-Key returns the first tip")
    http("POST", "/api/market/tips", tokens["v2"], {**rnd, "amount": 7}, str(uuid.uuid4()))
    http("POST", "/api/market/tips", tokens["d1"], {**rnd, "amount": 3}, str(uuid.uuid4()))
    http("POST", "/api/market/tips", tokens["v3"], {**rnd, "room_id": "r_elsewhere", "amount": 4}, str(uuid.uuid4()))
    try:
        http("POST", "/api/market/tips", tokens["v3"], {**rnd, "amount": 50}, str(uuid.uuid4()))
        sys.exit("FAIL: a tip beyond the wallet was taken")
    except HttpError as e:
        check(e.status == 422 and e.body["error"]["code"] == "INSUFFICIENT_FUNDS", "a tip beyond the wallet is refused")
    check(balance(tokens["v1"]) == 20 and balance(tokens["d1"]) == 2, "the tippers' coins left at once")

    print("==> the round finishes (RoundFinished as dev-tools)")
    finish_round(match, ROOM, [ids["d1"], ids["d2"]])
    # 17 coins from v1 and v2: d1 gets 9 (the odd coin) and their own 3 back, d2 gets 8.
    wait("the drawers paid", 60, lambda: True if balance(tokens["d2"]) == 13 else None)
    check(balance(tokens["d1"]) == 14, "the drawers share the tips; the first gets the odd coin and their own tip back")
    check(balance(tokens["v3"]) == 10, "a tip naming another room goes back")

    print("==> a tip right after the settlement, then one too late")
    http("POST", "/api/market/tips", tokens["v1"], {**rnd, "amount": 2}, str(uuid.uuid4()))
    wait("the sweeper paying the late tip", 30, lambda: True if balance(tokens["d2"]) == 14 else None)
    check(balance(tokens["d1"]) == 15, "the sweeper pays a late tip to the same drawers")
    time.sleep(31)
    try:
        http("POST", "/api/market/tips", tokens["v1"], {**rnd, "amount": 1}, str(uuid.uuid4()))
        sys.exit("FAIL: a tip 31 s after the settlement was taken")
    except HttpError as e:
        check(e.status == 409 and e.body["error"]["code"] == "ROUND_OVER", "a round settled 31 s ago takes no tips")

    print("==> CoinsCredited on market.wallet.v1")
    events = credits_since(started_ms, set(ids.values()), 6)
    tips = sorted((e.user_id, e.amount) for e in events if e.reason == wallet_pb2.CREDIT_REASON_TIP)
    refunds = sorted((e.user_id, e.amount) for e in events if e.reason == wallet_pb2.CREDIT_REASON_TIP_REFUND)
    check(tips == sorted([(ids["d1"], 9), (ids["d2"], 8), (ids["d1"], 1), (ids["d2"], 1)]),
          "a TIP credit per drawer and settlement")
    check(refunds == sorted([(ids["d1"], 3), (ids["v3"], 4)]), "a TIP_REFUND credit per refunded tip")
    print(f"tips-check: OK ({time.monotonic() - started:.0f} s)")
    return 0


def settle(match: str, drawer: str) -> int:
    """After k6 (scripts/loadtest.sh tips): finish round 1 and time the drawer's payout."""
    token = issuer().issue(drawer, ["player"], datetime.now(UTC), name=drawer)[0]
    before = balance(token)
    sent = time.monotonic()
    finish_round(match, "r_tips_load", [drawer])
    paid = wait("the drawer paid", 300, lambda: (b if (b := balance(token)) > before else None))
    # The settlement may take several batches; wait until the balance stops growing.
    last, stable_since = paid, time.monotonic()
    while time.monotonic() - stable_since < 5:
        time.sleep(0.5)
        if (now := balance(token)) != last:
            last, stable_since = now, time.monotonic()
    print(f"settled: the drawer received {last - before} coins, the last of them {stable_since - sent:.1f} s "
          f"after the RoundFinished was sent (the first {paid - before} sooner)")
    return 0


if __name__ == "__main__":
    if len(sys.argv) == 4 and sys.argv[1] == "settle":
        sys.exit(settle(sys.argv[2], sys.argv[3]))
    if len(sys.argv) != 1:
        sys.exit("usage: tips.py | tips.py settle <match_id> <drawer_id>")
    sys.exit(main())
