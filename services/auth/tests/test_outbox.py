"""CredentialsRevoked (ADR 0015): written to the outbox in the transaction that detects the theft,
published by the relay to Kafka at least once."""

import uuid
from datetime import UTC, datetime, timedelta

import pytest
from confluent_kafka import Consumer, Producer
from doodle.events.v1 import users_pb2
from fastapi.testclient import TestClient
from sqlalchemy import Engine, text

from app import outbox, relay
from tests.fake_google import FakeGoogle
from tests.test_api_keys import create, verify
from tests.test_refresh import ORIGIN, current_cookie, login, use


def events(db: Engine) -> list[tuple[str, str, users_pb2.UserEvent]]:
    with db.connect() as conn:
        rows = conn.execute(
            text("SELECT topic, key, payload FROM outbox ORDER BY created_at")
        ).all()
    return [(r.topic, r.key, users_pb2.UserEvent.FromString(r.payload)) for r in rows]


def user_id(db: Engine) -> str:
    with db.connect() as conn:
        return str(conn.execute(text("SELECT id FROM users")).scalar_one())


def test_a_replayed_rotated_token_writes_credentials_revoked(
    client: TestClient, google: FakeGoogle, db: Engine
) -> None:
    stolen = login(client, google)
    assert use(client, stolen) == 200
    assert events(db) == []

    before = datetime.now(UTC)
    assert use(client, stolen) == 401  # theft detected

    [(topic, key, event)] = events(db)
    revoked = event.credentials_revoked
    assert topic == "auth.users.v1" and key == user_id(db) == revoked.user_id
    assert revoked.reason == "refresh_token_reuse"
    assert revoked.meta.event_type == "CredentialsRevoked" and revoked.meta.producer == "auth"
    uuid.UUID(revoked.meta.event_id)
    at = revoked.revoked_at.ToDatetime(tzinfo=UTC)
    assert before - timedelta(seconds=1) <= at <= datetime.now(UTC)


def test_replaying_the_stolen_token_again_changes_nothing_more(
    client: TestClient, google: FakeGoogle, db: Engine
) -> None:
    """The theft response runs once per family: otherwise anyone holding an old token could
    keep revoking the user's new API keys and disabling their webhooks."""
    stolen = login(client, google)
    assert use(client, stolen) == 200
    assert use(client, stolen) == 401  # detected: keys revoked, one event

    login(client, google)  # the user signs in again and makes a new key
    key = create(client, user_id(db), "after")["key"]
    for _ in range(3):
        assert use(client, stolen) == 401

    assert len(events(db)) == 1
    assert verify(client, key).status_code == 200


def test_a_logged_out_token_coming_back_writes_nothing(
    client: TestClient, google: FakeGoogle, db: Engine
) -> None:
    token = login(client, google)
    assert client.post("/api/auth/logout", headers=ORIGIN).status_code == 204
    assert use(client, token) == 401
    assert events(db) == []


def test_ordinary_refreshes_write_nothing(
    client: TestClient, google: FakeGoogle, db: Engine
) -> None:
    login(client, google)
    for _ in range(3):
        token = current_cookie(client)
        assert token and use(client, token) == 200
    assert events(db) == []


def test_an_event_of_a_rolled_back_transaction_never_exists(db: Engine) -> None:
    with pytest.raises(RuntimeError), db.begin() as conn:
        outbox.credentials_revoked(conn, "u-rollback", datetime.now(UTC))
        raise RuntimeError("the revocation failed")
    assert events(db) == []


def test_the_relay_publishes_and_stamps(db: Engine, kafka_brokers: str) -> None:
    user = "u-" + uuid.uuid4().hex
    with db.begin() as conn:
        event_id = outbox.credentials_revoked(conn, user, datetime.now(UTC))

    producer = relay.new_producer(kafka_brokers, "auth-relay-test")
    assert relay.publish_batch(db, producer) == (1, 0)
    assert relay.publish_batch(db, producer) == (0, 0)  # stamped: nothing left

    consumer = Consumer(
        {
            "bootstrap.servers": kafka_brokers,
            "broker.address.family": "v4",
            "group.id": "test-" + uuid.uuid4().hex,
            "auto.offset.reset": "earliest",
        }
    )
    consumer.subscribe([outbox.USERS_TOPIC])
    try:
        found = None
        for _ in range(60):
            msg = consumer.poll(1)
            value = msg.value() if msg is not None and not msg.error() else None
            if value is None:
                continue
            event = users_pb2.UserEvent.FromString(value)
            if event.credentials_revoked.meta.event_id == str(event_id):
                found = msg
                break
        assert found is not None
        assert found.key() == user.encode()
    finally:
        consumer.close()


def test_kafka_down_leaves_the_row_for_the_next_pass(db: Engine, kafka_brokers: str) -> None:
    with db.begin() as conn:
        outbox.credentials_revoked(conn, "u-" + uuid.uuid4().hex, datetime.now(UTC))
    nowhere = Producer(
        {
            "bootstrap.servers": "127.0.0.1:1",
            "enable.idempotence": True,
            "message.timeout.ms": 1000,
            "broker.address.family": "v4",
        }
    )
    assert relay.publish_batch(db, nowhere) == (0, 1)
    with db.connect() as conn:
        assert (
            conn.execute(text("SELECT count(*) FROM outbox WHERE published_at IS NULL")).scalar()
            == 1
        )

    assert relay.publish_batch(db, relay.new_producer(kafka_brokers, "auth-relay-test")) == (1, 0)


def test_old_published_events_are_purged(db: Engine) -> None:
    with db.begin() as conn:
        old = outbox.credentials_revoked(conn, "u-old", datetime.now(UTC))
        recent = outbox.credentials_revoked(conn, "u-recent", datetime.now(UTC))
        waiting = outbox.credentials_revoked(conn, "u-waiting", datetime.now(UTC))
        conn.execute(
            text("UPDATE outbox SET published_at = now() - interval '8 days' WHERE event_id = :e"),
            {"e": old},
        )
        conn.execute(
            text("UPDATE outbox SET published_at = now() WHERE event_id = :e"), {"e": recent}
        )
        conn.execute(
            text("UPDATE outbox SET created_at = now() - interval '30 days' WHERE event_id = :e"),
            {"e": waiting},
        )

    assert relay.purge_published(db) == 1
    with db.connect() as conn:
        left = set(conn.execute(text("SELECT event_id FROM outbox")).scalars())
    assert left == {recent, waiting}
