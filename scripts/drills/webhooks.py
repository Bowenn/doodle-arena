"""Webhook drill (task 4.7, M4): a healthy endpoint receives a signed event and verifies it; a
failing endpoint is retried with backoff and ends in the dead-letter queue.

    uv run --project services/auth --with confluent-kafka --with 'protobuf>=7.36.2,<8' \\
        python scripts/drills/webhooks.py

Against `make up` (with the webhook-receiver service). What it does:

1. Mints an access token for a fresh drill user (auth's TokenIssuer and the local key, like
   mint_tokens.py; DRILL_JWT_KEY in a worktree) and creates an API key with it.
2. Registers two endpoints for coins.credited on the test receiver, one answering 200 and one 500,
   and gives the receiver their signing secrets.
3. Publishes a MatchFinished with the drill user and a second player, so the marketplace pays
   rewards and publishes CoinsCredited, which the notifier turns into webhooks.
4. Waits for the healthy endpoint's verified delivery, then for the failing one to go DEAD
   (8 attempts, full-jitter backoff from 2 s: a few minutes), and finds its DeliveryDead on
   notifier.dlq.v1.
"""

import itertools
import json
import os
import sys
import time
import urllib.error
import urllib.request
import uuid
from datetime import UTC, datetime, timedelta
from pathlib import Path
from typing import Any

from confluent_kafka import Consumer, Producer, TopicPartition

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT / "services" / "auth"))
sys.path.insert(0, str(ROOT / "proto" / "gen" / "python"))
from app.tokens import SigningKey, TokenIssuer
from doodle.events.v1 import dead_letters_pb2, matches_pb2
from google.protobuf.timestamp_pb2 import Timestamp

GW = "http://localhost:8080"
RECEIVER = "http://localhost:9000"
KAFKA = os.environ.get("KAFKA_BROKERS", "localhost:9094")
# Compose publishes Kafka on 127.0.0.1 only and the broker advertises "localhost", which
# librdkafka may resolve to ::1 first: stay on IPv4.
KAFKA_CONF = {"bootstrap.servers": KAFKA, "broker.address.family": "v4"}


def http(
    method: str, url: str, body: Any = None, headers: dict[str, str] | None = None
) -> Any:
    data = None
    if body is not None:
        data = body.encode() if isinstance(body, str) else json.dumps(body).encode()
    req = urllib.request.Request(url, data=data, method=method, headers=headers or {})
    if body is not None and not isinstance(body, str):
        req.add_header("Content-Type", "application/json")
    try:
        with urllib.request.urlopen(req, timeout=10) as resp:
            raw = resp.read()
            return json.loads(raw) if raw else None
    except urllib.error.HTTPError as e:
        sys.exit(f"{method} {url}: HTTP {e.code} {e.read().decode()[:300]}")


def token(user: str) -> str:
    key = os.environ.get("DRILL_JWT_KEY") or str(ROOT / "secrets/jwt-private.pem")
    issuer = TokenIssuer(
        SigningKey.from_file(key), GW + "/api/auth", timedelta(minutes=30)
    )
    return issuer.issue(user, ["player"], datetime.now(UTC), name=user)[0]


def publish_match(players: list[str]) -> str:
    match_id = "m_wh_" + uuid.uuid4().hex[:12]
    now = Timestamp()
    now.GetCurrentTime()
    event = matches_pb2.MatchEvent(
        match_finished=matches_pb2.MatchFinished(
            meta={
                "event_id": str(uuid.uuid4()),
                "event_type": "MatchFinished",
                "schema_version": 1,
                "occurred_at": now,
                "producer": "webhooks-drill",
            },
            match_id=match_id,
            room_id="r_webhooks_drill",
            started_at=now,
            ended_at=now,
            results=[
                matches_pb2.PlayerResult(user_id=p, score=100 - 10 * i, placement=i + 1)
                for i, p in enumerate(players)
            ],
        )
    )
    producer = Producer(KAFKA_CONF)
    producer.produce(
        "game.matches.v1", key=b"r_webhooks_drill", value=event.SerializeToString()
    )
    if producer.flush(10) != 0:
        sys.exit("MatchFinished not acknowledged by Kafka")
    return match_id


def wait(what: str, timeout: float, check):  # type: ignore[no-untyped-def]
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        result = check()
        if result:
            return result
        time.sleep(1)
    sys.exit(f"timed out after {timeout:.0f} s waiting for {what}")


def find_dead_letter(delivery_id: str) -> Any:
    consumer = Consumer(
        {
            **KAFKA_CONF,
            "group.id": "webhooks-drill-" + uuid.uuid4().hex,
            "enable.auto.commit": False,
        }
    )
    meta = consumer.list_topics("notifier.dlq.v1", timeout=10).topics["notifier.dlq.v1"]
    consumer.assign([TopicPartition("notifier.dlq.v1", p, 0) for p in meta.partitions])
    try:
        deadline = time.monotonic() + 30
        while time.monotonic() < deadline:
            msg = consumer.poll(1)
            if msg is None or msg.error():
                continue
            ev = dead_letters_pb2.DeadLetterEvent.FromString(msg.value()).delivery_dead
            if ev.delivery_id == delivery_id:
                return ev
    finally:
        consumer.close()
    return None


def main() -> None:
    run = uuid.uuid4().hex[:8]
    user, other = f"u_wh_{run}", f"u_wh_{run}_b"
    bearer = {"Authorization": f"Bearer {token(user)}"}
    key = http(
        "POST",
        f"{GW}/api/auth/api-keys",
        {"name": f"webhooks drill {run}", "scopes": ["webhooks"]},
        bearer,
    )
    api = {"Authorization": f"Bearer {key['key']}"}
    print(f"user {user}, API key {key['prefix']} (the key itself is never printed)")

    endpoints = {}
    for name, mode in (("ok", "ok"), ("fail", "fail")):
        hook = f"{name}-{run}"
        ep = http(
            "POST",
            f"{GW}/api/notifier/endpoints",
            {
                "url": f"http://webhook-receiver:9000/hooks/{hook}",
                "event_types": ["coins.credited"],
            },
            api,
        )
        http("PUT", f"{RECEIVER}/admin/{hook}/secret", ep["secret"])
        http("PUT", f"{RECEIVER}/admin/{hook}/mode", mode)
        endpoints[name] = (ep["id"], hook)
        print(f"endpoint {name}: {ep['id']} -> /hooks/{hook} ({mode})")

    sent = time.monotonic()
    match_id = publish_match([user, other])
    print(
        f"published MatchFinished {match_id}; the marketplace pays rewards and publishes CoinsCredited"
    )

    ok_id, ok_hook = endpoints["ok"]
    got = wait(
        "the healthy endpoint's webhook",
        60,
        lambda: http("GET", f"{RECEIVER}/admin/{ok_hook}")["received"],
    )
    first = got[0]
    print(
        f"healthy: received {first['event_type']} {first['event_id']} {time.monotonic() - sent:.1f} s after "
        f"the MatchFinished, signature verified: {first['verified']}"
    )
    deliveries = http(
        "GET", f"{GW}/api/notifier/endpoints/{ok_id}/deliveries", headers=api
    )["deliveries"]
    print(
        f"healthy: delivery {deliveries[0]['status']} after {deliveries[0]['attempts']} attempt(s)"
    )

    fail_id, fail_hook = endpoints["fail"]

    def dead() -> Any:
        ds = http(
            "GET", f"{GW}/api/notifier/endpoints/{fail_id}/deliveries", headers=api
        )["deliveries"]
        return ds[0] if ds and ds[0]["status"] == "DEAD" else None

    print(
        "failing: waiting for 8 attempts (full-jitter backoff from 2 s; a few minutes)..."
    )
    d = wait("the failing delivery to go DEAD", 900, dead)
    times = [datetime.fromisoformat(a["at"]) for a in d["attempt_log"]]
    gaps = [f"{(b - a).total_seconds():.1f}" for a, b in itertools.pairwise(times)]
    print(
        f"failing: DEAD after {d['attempts']} attempts, last error {d['last_error']!r}; "
        f"gaps between attempts (s): {', '.join(gaps)}"
    )
    received = http("GET", f"{RECEIVER}/admin/{fail_hook}")["received"]
    print(
        f"failing: the receiver got {len(received)} requests, all verified: {all(r['verified'] for r in received)}"
    )

    dl = wait(
        "its DeliveryDead on notifier.dlq.v1", 60, lambda: find_dead_letter(d["id"])
    )
    print(
        f"dead-letter queue: DeliveryDead for {dl.delivery_id}, {dl.attempts} attempts, "
        f"last status {dl.last_status_code}, event {dl.source_event_type} {dl.source_event_id}"
    )
    ok = (
        first["verified"]
        and deliveries[0]["status"] == "SUCCEEDED"
        and d["attempts"] == 8
    )
    print("PASS" if ok else "FAIL")
    sys.exit(0 if ok else 1)


if __name__ == "__main__":
    main()
