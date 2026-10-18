"""The outbox relay against a real Kafka broker: every event is published, stamped only after
Kafka confirms, again with the same id after a crash, and never twice by two relays."""

import threading
import uuid
from collections.abc import Iterator
from datetime import timedelta
from typing import Any

import pytest
from confluent_kafka import Consumer
from confluent_kafka.admin import AdminClient
from confluent_kafka.cimpl import NewTopic
from django.utils import timezone

from drawings import relay
from drawings.models import OutboxEvent

pytestmark = pytest.mark.django_db(transaction=True)


@pytest.fixture
def topic(kafka_brokers: str) -> Iterator[str]:
    name = f"assets.drawings.v1.test-{uuid.uuid4().hex[:8]}"
    admin = AdminClient({"bootstrap.servers": kafka_brokers})
    admin.create_topics([NewTopic(name, num_partitions=3, replication_factor=1)])[name].result(
        timeout=30
    )
    yield name


def add_events(topic: str, n: int, key: str = "") -> list[OutboxEvent]:
    return [
        OutboxEvent.objects.create(
            topic=topic,
            key=key or f"d-{i}",
            event_type="DrawingApproved",
            payload=f"event {i}".encode(),
            traceparent="00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01",
        )
        for i in range(n)
    ]


def read_all(brokers: str, topic: str, expect: int) -> list[Any]:
    consumer = Consumer(
        {
            "bootstrap.servers": brokers,
            "group.id": uuid.uuid4().hex,
            "auto.offset.reset": "earliest",
        }
    )
    consumer.subscribe([topic])
    msgs: list[Any] = []
    try:
        for _ in range(60):
            msg = consumer.poll(0.5)
            if msg is not None and not msg.error():
                msgs.append(msg)
            if len(msgs) >= expect:
                # Linger a little, to catch duplicates.
                extra = consumer.poll(1.0)
                if extra is not None and not extra.error():
                    msgs.append(extra)
                break
    finally:
        consumer.close()
    return msgs


def test_events_are_published_and_stamped(kafka_brokers: str, topic: str) -> None:
    rows = add_events(topic, 3)
    producer = relay.new_producer(kafka_brokers, "test")

    assert relay.publish_batch(producer) == (3, 0)

    assert not OutboxEvent.objects.filter(published_at__isnull=True).exists()
    msgs = read_all(kafka_brokers, topic, 3)
    assert sorted(m.value() for m in msgs) == [b"event 0", b"event 1", b"event 2"]
    by_key = {m.key(): m for m in msgs}
    assert set(by_key) == {r.key.encode() for r in rows}
    headers = dict(msgs[0].headers())
    # The publish span continues the decider's trace.
    assert headers["traceparent"].decode().startswith("00-4bf92f3577b34da6a3ce929d0e0e4736-")
    assert relay.publish_batch(producer) == (0, 0)


def test_one_drawings_events_stay_in_order(kafka_brokers: str, topic: str) -> None:
    add_events(topic, 5, key="d-same")

    relay.publish_batch(relay.new_producer(kafka_brokers, "test"))

    msgs = read_all(kafka_brokers, topic, 5)
    assert [m.value() for m in msgs] == [f"event {i}".encode() for i in range(5)]
    assert len({m.partition() for m in msgs}) == 1


def test_a_crash_before_the_stamp_publishes_again(
    kafka_brokers: str, topic: str, monkeypatch: pytest.MonkeyPatch
) -> None:
    """Kafka confirmed, then the relay died before stamping: the event goes out again, with the
    same id, which is why consumers dedupe."""
    add_events(topic, 1)
    producer = relay.new_producer(kafka_brokers, "test")

    class Crash:
        @staticmethod
        def now() -> Any:
            raise RuntimeError("killed before the stamp committed")

    monkeypatch.setattr(relay, "timezone", Crash)
    with pytest.raises(RuntimeError):
        relay.publish_batch(producer)
    assert OutboxEvent.objects.filter(published_at__isnull=True).count() == 1

    monkeypatch.setattr(relay, "timezone", timezone)
    assert relay.publish_batch(producer) == (1, 0)
    msgs = read_all(kafka_brokers, topic, 2)
    assert [m.value() for m in msgs] == [b"event 0", b"event 0"]


def test_kafka_down_leaves_the_events_for_later(monkeypatch: pytest.MonkeyPatch) -> None:
    add_events("assets.drawings.v1", 2)
    monkeypatch.setattr(relay, "FLUSH_TIMEOUT", 1.0)
    producer = relay.new_producer("127.0.0.1:1", "test")  # nothing listens there

    published, failed = relay.publish_batch(producer)

    assert published == 0
    assert failed == 2
    assert OutboxEvent.objects.filter(published_at__isnull=True).count() == 2


def test_two_relays_never_publish_the_same_event(kafka_brokers: str, topic: str) -> None:
    add_events(topic, 40)
    results: list[tuple[int, int]] = []

    def one_relay() -> None:
        from django.db import connection

        producer = relay.new_producer(kafka_brokers, "test")
        try:
            while True:
                done = relay.publish_batch(producer, limit=5)
                results.append(done)
                if done == (0, 0):
                    return
        finally:
            connection.close()

    threads = [threading.Thread(target=one_relay) for _ in range(2)]
    for t in threads:
        t.start()
    for t in threads:
        t.join(60)

    assert sum(p for p, _ in results) == 40
    msgs = read_all(kafka_brokers, topic, 40)
    assert len(msgs) == 40
    assert len({m.value() for m in msgs}) == 40


def test_old_published_events_are_purged() -> None:
    old, recent, pending = add_events("assets.drawings.v1", 3)
    OutboxEvent.objects.filter(pk=old.pk).update(published_at=timezone.now() - timedelta(days=8))
    OutboxEvent.objects.filter(pk=recent.pk).update(published_at=timezone.now())

    assert relay.purge_published() == 1
    assert set(OutboxEvent.objects.values_list("pk", flat=True)) == {recent.pk, pending.pk}
