"""The round consumer against a real Kafka broker and Postgres: one drawing per round however
often RoundFinished arrives, unreadable records skipped and committed, outages retried."""

import tempfile
import threading
import time
import uuid
from collections.abc import Callable, Iterator
from pathlib import Path
from typing import Any

import pytest
from confluent_kafka import Producer
from confluent_kafka.admin import AdminClient
from confluent_kafka.cimpl import NewTopic
from django.db import OperationalError
from doodle.events.v1 import rounds_pb2

from drawings import consumer, tasks
from drawings.models import Drawing

pytestmark = pytest.mark.django_db(transaction=True)


def round_event(
    match_id: str, round_no: int = 1, stroke_count: int = 3, event_id: str = ""
) -> bytes:
    event = rounds_pb2.RoundEvent()
    rf = event.round_finished
    rf.meta.event_id = event_id or str(uuid.uuid4())
    rf.meta.event_type = "RoundFinished"
    rf.match_id, rf.room_id, rf.round_no, rf.word = match_id, "r_1", round_no, "cat"
    rf.drawer_ids.extend(["u_ada", "u_bob"])
    rf.stroke_log_key = f"{match_id}/{round_no}.binpb.gz"
    rf.stroke_count, rf.canvas_width, rf.canvas_height = stroke_count, 800, 600
    rf.ended_at.GetCurrentTime()
    return event.SerializeToString()


class FakeRecord:
    def offset(self) -> int:
        return 42


class Harness:
    def __init__(self, brokers: str) -> None:
        self.brokers = brokers
        self.topic = f"game.rounds.v1.test-{uuid.uuid4().hex[:8]}"
        self.group = f"assets-test-{uuid.uuid4().hex[:8]}"
        # Keep the client alive until the future resolves, or librdkafka destroys the handle.
        self.admin = AdminClient({"bootstrap.servers": brokers})
        self.admin.create_topics([NewTopic(self.topic, num_partitions=1, replication_factor=1)])[
            self.topic
        ].result(timeout=30)
        self.producer = Producer({"bootstrap.servers": brokers})
        self.sent: list[str] = []

    def publish(
        self, value: bytes, headers: list[tuple[str, str | bytes | None]] | None = None
    ) -> None:
        self.producer.produce(self.topic, key=b"r_1", value=value, headers=headers or [])
        self.producer.flush(10)

    def consume_until(self, done: Callable[[], bool], timeout: float = 30) -> None:
        stop = threading.Event()
        thread = threading.Thread(
            target=consumer.run,
            args=(self.brokers, stop),
            kwargs={"topic": self.topic, "group": self.group},
        )
        thread.start()
        try:
            deadline = time.monotonic() + timeout
            while not done():
                assert time.monotonic() < deadline, "the consumer didn't get there in time"
                time.sleep(0.2)
        finally:
            stop.set()
            thread.join(15)


@pytest.fixture
def harness(kafka_brokers: str, monkeypatch: pytest.MonkeyPatch) -> Iterator[Harness]:
    h = Harness(kafka_brokers)
    monkeypatch.setattr(
        tasks.prepare_drawing, "delay", lambda drawing_id: h.sent.append(drawing_id)
    )
    monkeypatch.setattr(consumer, "ALIVE_FILE", Path(tempfile.gettempdir()) / "consumer-alive-test")
    yield h


def test_a_round_becomes_a_drawing_and_its_task(harness: Harness) -> None:
    harness.publish(round_event("m_new"))

    harness.consume_until(lambda: len(harness.sent) == 1)

    drawing = Drawing.objects.get(match_id="m_new", round_no=1)
    assert harness.sent == [str(drawing.id)]
    assert drawing.author_ids == ["u_ada", "u_bob"]
    assert drawing.initial_owner_id == "u_ada"
    assert drawing.stroke_log_key == "m_new/1.binpb.gz"
    assert drawing.source_event_id


def test_a_replayed_round_finished_creates_no_duplicate(harness: Harness) -> None:
    """M3: the same round twice (the outbox resends with the same event_id; a replay tool may
    use a new one) leaves one drawing. While it's still processing, each delivery sends its
    task again, which the idempotent pipeline absorbs."""
    harness.publish(round_event("m_dup", event_id="e-1"))
    harness.publish(round_event("m_dup", event_id="e-1"))
    harness.publish(round_event("m_dup", event_id="e-2"))

    harness.consume_until(lambda: len(harness.sent) == 3)

    assert Drawing.objects.filter(match_id="m_dup").count() == 1
    drawing = Drawing.objects.get(match_id="m_dup")
    assert set(harness.sent) == {str(drawing.id)}
    assert drawing.source_event_id == "e-1"


def test_unreadable_records_are_skipped_and_committed(harness: Harness) -> None:
    harness.publish(b"\xff\xfe not a protobuf")
    harness.publish(rounds_pb2.RoundEvent().SerializeToString())  # an empty wrapper
    harness.publish(round_event("m_empty", stroke_count=0))  # nothing drawn
    harness.publish(round_event("m_after"))

    harness.consume_until(lambda: len(harness.sent) == 1)
    assert Drawing.objects.filter(match_id__in=["m_empty", "m_after"]).count() == 1

    # Committed: a restart of the group gets nothing again.
    harness.sent.clear()
    harness.publish(round_event("m_marker"))
    harness.consume_until(lambda: len(harness.sent) >= 1)
    assert harness.sent == [str(Drawing.objects.get(match_id="m_marker").id)]


def test_the_producers_trace_is_continued(
    harness: Harness, monkeypatch: pytest.MonkeyPatch
) -> None:
    trace_id = "4bf92f3577b34da6a3ce929d0e0e4736"
    seen: list[str] = []
    original = consumer.handle

    def spy(msg: Any) -> str:
        from opentelemetry import propagate, trace

        ctx = propagate.extract(consumer._headers(msg))
        seen.append(format(trace.get_current_span(ctx).get_span_context().trace_id, "032x"))
        return original(msg)

    monkeypatch.setattr(consumer, "handle", spy)
    harness.publish(
        round_event("m_traced"),
        headers=[("traceparent", f"00-{trace_id}-00f067aa0ba902b7-01".encode())],
    )

    harness.consume_until(lambda: len(harness.sent) == 1)
    assert seen == [trace_id]


def test_an_outage_is_retried_on_the_same_record(monkeypatch: pytest.MonkeyPatch) -> None:
    attempts: list[int] = []

    def flaky(msg: Any) -> str:
        attempts.append(1)
        if len(attempts) < 3:
            raise OperationalError("postgres is restarting")
        return "created"

    monkeypatch.setattr(consumer, "handle", flaky)
    sleeps: list[float] = []

    assert consumer.handle_with_retry(FakeRecord(), threading.Event(), sleep=sleeps.append)  # type: ignore[arg-type]
    assert len(attempts) == 3
    assert sleeps == [0.5, 1.0]


def test_a_stopping_consumer_leaves_the_record_uncommitted(monkeypatch: pytest.MonkeyPatch) -> None:
    stop = threading.Event()

    def down(msg: Any) -> str:
        stop.set()
        raise OperationalError("still down")

    monkeypatch.setattr(consumer, "handle", down)

    assert not consumer.handle_with_retry(FakeRecord(), stop, sleep=lambda s: None)  # type: ignore[arg-type]
