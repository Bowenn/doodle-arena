"""Search end to end (task 6.7, ADR 0023): drawings minted through the pipeline, a listing and a
sale through the marketplace, and every change reaching search through the indexer.

    uv run --project services/auth --with confluent-kafka --with boto3 \\
        --with 'protobuf>=7.36.2,<8' python scripts/drills/search.py

Against `PROFILES=search make up` (or more profiles); `make search-check` runs it (CI's stack job,
full tier). In a worktree, set ENV_FILE and DRILL_JWT_KEY to the main checkout's.

1. Three drawings by one author, with made-up words that share a prefix, through the real
   pipeline (a RoundFinished per drawing; assets approves; the marketplace mints).
2. The author lists two of them (15 and 20 coins); a buyer with match rewards buys the second.
3. Search finds the three by prefix and one by a typo; listed only, it finds the first (the
   second was sold) with its price; a price range and the facet agree.
4. reindex_search rebuilds the index from Postgres, and the same searches still answer the same.
"""

import gzip
import json
import os
import random
import string
import subprocess
import sys
import time
import urllib.error
import urllib.parse
import urllib.request
import uuid
from datetime import UTC, datetime, timedelta
from pathlib import Path
from typing import Any

import boto3
from botocore.client import Config
from confluent_kafka import Producer

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT / "services" / "auth"))
sys.path.insert(0, str(ROOT / "proto" / "gen" / "python"))
from app.tokens import SigningKey, TokenIssuer  # noqa: E402
from doodle.events.v1 import matches_pb2, rounds_pb2  # noqa: E402
from doodle.game.v1 import stroke_log_pb2  # noqa: E402

GW = os.environ.get("GATEWAY", "http://localhost:8080")
KAFKA = os.environ.get("KAFKA_BROKERS", "localhost:9094")
ENV_FILE = Path(os.environ.get("ENV_FILE", ROOT / ".env"))
ROOM = "r_search_drill"


def env(name: str) -> str:
    """A value from the environment or ENV_FILE (.env); never printed."""
    if value := os.environ.get(name):
        return value
    if ENV_FILE.is_file():
        for line in ENV_FILE.read_text().splitlines():
            if line.startswith(name + "="):
                return line.split("=", 1)[1]
    sys.exit(f"no {name} in {ENV_FILE}: run make up, or set ENV_FILE")


KAFKA_CONF = {
    "bootstrap.servers": KAFKA,
    "broker.address.family": "v4",
    "security.protocol": "SASL_PLAINTEXT",
    "sasl.mechanism": "SCRAM-SHA-512",
    "sasl.username": "dev-tools",
    "sasl.password": env("KAFKA_DEV_TOOLS_PASSWORD"),
}


def http(method: str, path: str, token: str, body: Any = None, key: str | None = None) -> tuple[int, Any]:
    req = urllib.request.Request(GW + path, method=method, data=None if body is None else json.dumps(body).encode())
    req.add_header("Authorization", "Bearer " + token)
    if body is not None:
        req.add_header("Content-Type", "application/json")
    if key:
        req.add_header("Idempotency-Key", key)
    try:
        with urllib.request.urlopen(req, timeout=10) as resp:
            raw = resp.read()
            return resp.status, (json.loads(raw) if raw else None)
    except urllib.error.HTTPError as e:
        raw = e.read()
        return e.code, (json.loads(raw) if raw else None)


def wait(what: str, timeout: float, check):  # type: ignore[no-untyped-def]
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        if (result := check()) is not None:
            return result
        time.sleep(1)
    sys.exit(f"FAIL: {what} within {timeout:.0f} s")


def check(expect: bool, what: str) -> None:
    if not expect:
        sys.exit(f"FAIL: {what}")
    print(f"  ok  {what}")


def produce(topic: str, value: bytes) -> None:
    producer = Producer(KAFKA_CONF)
    producer.produce(topic, key=ROOM.encode(), value=value)
    if producer.flush(10) != 0:
        sys.exit(f"FAIL: {topic} not acknowledged by Kafka")


def draw(author: str, word: str) -> None:
    """One round drawn by author with this word, as the game server reports it."""
    match_id = "m_search_" + uuid.uuid4().hex[:12]
    key = f"{match_id}/1.binpb.gz"
    log = stroke_log_pb2.StrokeLog(schema_version=1, match_id=match_id, room_id=ROOM, round_no=1,
                                   canvas_width=800, canvas_height=600)
    log.round_started_at.FromDatetime(datetime.now(UTC))
    stroke = log.strokes.add(stroke_id="s1", user_id=author, color="#1971c2", width=40, server_received_ms=500)
    for i in range(10):
        stroke.points.add(x=random.randint(500, 9500), y=random.randint(500, 9500), t_ms=i * 40)
    s3 = boto3.client("s3", endpoint_url="http://localhost:8333", region_name="us-east-1",
                      aws_access_key_id=env("S3_ACCESS_KEY"), aws_secret_access_key=env("S3_SECRET_KEY"),
                      config=Config(s3={"addressing_style": "path"}))
    s3.put_object(Bucket="round-logs", Key=key, Body=gzip.compress(log.SerializeToString()))
    event = rounds_pb2.RoundEvent()
    rf = event.round_finished
    rf.meta.event_id, rf.meta.event_type, rf.meta.schema_version = str(uuid.uuid4()), "RoundFinished", 1
    rf.meta.producer = "search-drill"
    rf.meta.occurred_at.GetCurrentTime()
    rf.match_id, rf.room_id, rf.round_no, rf.word = match_id, ROOM, 1, word
    rf.drawer_ids.append(author)
    rf.stroke_log_key, rf.stroke_count, rf.canvas_width, rf.canvas_height = key, 1, 800, 600
    rf.ended_at.GetCurrentTime()
    produce("game.rounds.v1", event.SerializeToString())


def pay_rewards(user: str, other: str) -> None:
    now = datetime.now(UTC)
    event = matches_pb2.MatchEvent()
    mf = event.match_finished
    mf.meta.event_id, mf.meta.event_type, mf.meta.schema_version = str(uuid.uuid4()), "MatchFinished", 1
    mf.meta.producer = "search-drill"
    mf.meta.occurred_at.FromDatetime(now)
    mf.match_id, mf.room_id = "m_search_" + uuid.uuid4().hex[:12], ROOM
    mf.started_at.FromDatetime(now)
    mf.ended_at.FromDatetime(now)
    mf.results.add(user_id=user, score=100, placement=1)
    mf.results.add(user_id=other, score=50, placement=2)
    produce("game.matches.v1", event.SerializeToString())


def search(token: str, **params: Any) -> dict[str, Any]:
    status, body = http("GET", "/api/assets/search?" + urllib.parse.urlencode(params), token)
    if status != 200:
        sys.exit(f"FAIL: search {params}: HTTP {status} {body}")
    return body


def main() -> int:
    started = time.monotonic()
    run = uuid.uuid4().hex[:8]
    key = SigningKey.load(os.environ.get("DRILL_JWT_PEM", ""),
                          os.environ.get("DRILL_JWT_KEY") or str(ROOT / "secrets/jwt-private.pem"))
    issuer = TokenIssuer(key, GW + "/api/auth", timedelta(minutes=30))
    author, buyer = f"u_search_{run}_author", f"u_search_{run}_buyer"
    token = {u: issuer.issue(u, ["player"], datetime.now(UTC), name=u)[0] for u in (author, buyer)}
    # Made-up words with a shared stem, so nothing else in the stack matches them.
    stem = "".join(random.choices(string.ascii_lowercase, k=7))
    words = [stem + "ar", stem + "ilo", stem + "umpet"]

    print(f"==> three drawings by one author: {', '.join(words)}")
    for w in words:
        draw(author, w)
    pay_rewards(buyer, author)  # 30 coins for the buyer
    found = wait("the three drawings in search", 120, lambda: (
        r if len((r := search(token[buyer], q=stem))["drawings"]) == 3 else None))
    check(sorted(d["word"] for d in found["drawings"]) == sorted(words), "a prefix finds all three")
    typo = words[2][:-2] + words[2][-1] + words[2][-2]  # two letters swapped
    check(search(token[buyer], q=typo)["drawings"][0]["word"] == words[2], f"a typo ({typo}) finds {words[2]}")

    print("==> the author lists two, the buyer buys the second")
    owned = wait("the drawings minted", 90, lambda: (
        d if len(d := http("GET", "/api/market/drawings?owner=me", token[author])[1]["drawings"]) >= 3 else None))
    ids = {d["id"]: d["word"] for d in found["drawings"]}
    by_word = {ids[o["drawing_id"]]: o["drawing_id"] for o in owned if o["drawing_id"] in ids}
    listings = {}
    for w, price in ((words[0], 15), (words[1], 20)):
        status, listing = http("POST", "/api/market/listings", token[author],
                               {"drawing_id": by_word[w], "price": price}, str(uuid.uuid4()))
        check(status == 201, f"{w} listed at {price} coins")
        listings[w] = listing["listing_id"]
    wait("the buyer's rewards", 60, lambda: True if http("GET", "/api/market/wallet", token[buyer])[1]["balance"] >= 20 else None)
    status, _ = http("POST", f"/api/market/listings/{listings[words[1]]}/purchase", token[buyer], key=str(uuid.uuid4()))
    check(status == 200, f"{words[1]} sold")

    print("==> search follows the listing and the sale")
    listed = wait("search to show one listing", 60, lambda: (
        r if [d["word"] for d in (r := search(token[buyer], q=stem, listed="true"))["drawings"]] == [words[0]] else None))
    check(listed["drawings"][0]["listing"]["price"] == 15, "the listed one carries its price; the sold one isn't listed")
    check([d["word"] for d in search(token[buyer], q=stem, min_price=10, max_price=16)["drawings"]] == [words[0]],
          "a price range finds it")
    facets = {f["key"]: f["count"] for f in listed["facets"]["price"]}
    check(facets.get("11-50") == 1 and sum(facets.values()) == 1, "the price facet counts it")

    print("==> rebuild the index from Postgres (reindex_search)")
    out = subprocess.run(
        ["docker", "compose", "-f", str(ROOT / "infra/docker-compose.yml"), "--env-file", str(ENV_FILE),
         "exec", "-T", "assets-indexer", "python", "manage.py", "reindex_search"],
        check=True, capture_output=True, text=True,
    ).stdout.strip().splitlines()[-1]
    rebuilt = json.loads(out)
    print(f"  rebuilt {rebuilt['indexed']} documents into {rebuilt['index']} in {rebuilt['seconds']} s")
    check(sorted(d["word"] for d in search(token[buyer], q=stem)["drawings"]) == sorted(words), "the prefix still finds all three")
    check([d["word"] for d in search(token[buyer], q=stem, listed="true")["drawings"]] == [words[0]],
          "the listing survived the rebuild")
    print(f"search-check: OK ({time.monotonic() - started:.0f} s)")
    return 0


if __name__ == "__main__":
    sys.exit(main())
