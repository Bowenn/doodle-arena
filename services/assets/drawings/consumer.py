"""Consumes RoundFinished from game.rounds.v1 and starts a drawing for each round (design doc 4.5).

Delivery is at least once: the game server's outbox resends an event after a crash (ADR 0009),
and this consumer commits an offset only after the round is stored and its task sent. So:

- The row insert is the dedupe point: UNIQUE(match_id, round_no) makes a replayed RoundFinished
  find the existing drawing instead of creating a second one.
- A drawing still processing gets its task again on every delivery. That's safe (the pipeline
  skips what's done) and it covers a crash between the insert and sending the task.
- A record this build can't read (unknown or empty event, malformed bytes) is logged, counted
  and committed, so it never blocks the partition (design doc 6.2).
- Postgres or the Celery broker being down is retried with backoff, on the same record, and
  nothing is committed until it succeeds. Only connection-level errors are retried: a record
  the database refuses (a value too long, out of range) would fail forever and block its
  partition, so it is validated first, and skipped and committed if it still fails.
"""

import logging
import threading
import time
from collections.abc import Callable
from datetime import UTC
from pathlib import Path
from typing import Any, cast

from confluent_kafka import Consumer, KafkaError, Message
from django.db import (
    DataError,
    IntegrityError,
    InterfaceError,
    OperationalError,
    close_old_connections,
    connection,
)
from doodle.events.v1 import rounds_pb2
from google.protobuf.message import DecodeError
from kombu.exceptions import OperationalError as BrokerError
from opentelemetry import metrics, propagate, trace
from opentelemetry.trace import SpanKind

from drawings.kafka_auth import kafka_auth
from drawings.models import Drawing, Status
from drawings.tasks import prepare_drawing

TOPIC = "game.rounds.v1"
GROUP = "assets"
# The container's health check reads this file's age (infra/docker-compose.yml).
ALIVE_FILE = Path("/tmp/consumer-alive")

log = logging.getLogger(__name__)
tracer = trace.get_tracer(__name__)
received = metrics.get_meter(__name__).create_counter(
    "assets.rounds.received",
    description="RoundFinished records by result: created, duplicate, empty (nothing drawn), "
    "skipped (unknown, empty or malformed)",
)


def ingest(rf: rounds_pb2.RoundFinished) -> tuple[str, Drawing | None]:
    """Stores the drawing for a finished round. Returns the result and the drawing."""
    if rf.stroke_count == 0:
        return "empty", None  # nothing was drawn, so there's nothing to mint
    if not _valid(rf):
        return "skipped", None
    drawing, created = Drawing.objects.get_or_create(
        match_id=rf.match_id,
        round_no=rf.round_no,
        defaults={
            "room_id": rf.room_id,
            "word": rf.word,
            "author_ids": list(rf.drawer_ids),
            "initial_owner_id": rf.drawer_ids[0],
            "stroke_log_key": rf.stroke_log_key,
            "stroke_count": rf.stroke_count,
            "canvas_width": rf.canvas_width,
            "canvas_height": rf.canvas_height,
            "ended_at": rf.ended_at.ToDatetime(tzinfo=UTC),
            "source_event_id": rf.meta.event_id,
        },
    )
    return ("created" if created else "duplicate"), drawing


# Column sizes in drawings.models.Drawing.
_ID, _KEY, _MAX_INT = 64, 256, 2**31 - 1


def _valid(rf: rounds_pb2.RoundFinished) -> bool:
    """Whether the row can be stored. A record that can't is skipped, never retried."""
    ids = [rf.match_id, rf.room_id, *rf.drawer_ids]
    return bool(
        rf.match_id
        and rf.drawer_ids
        and rf.stroke_log_key
        and 0 < rf.round_no <= _MAX_INT
        and all(len(i) <= _ID for i in ids)
        and len(rf.word) <= _ID
        and len(rf.stroke_log_key) <= _KEY
        and len(rf.meta.event_id) <= _ID
        and max(rf.stroke_count, rf.canvas_width, rf.canvas_height) <= _MAX_INT
        and 0 <= rf.ended_at.seconds <= 253402300799  # a datetime Python can represent
    )


def handle(msg: Message) -> str:
    """Processes one record and returns its result. Raises only for errors worth retrying."""
    carrier = _headers(msg)
    attributes: dict[str, Any] = {
        "messaging.system": "kafka",
        "messaging.destination.name": msg.topic() or TOPIC,
        "messaging.kafka.destination.partition": msg.partition() or 0,
        "messaging.kafka.message.offset": msg.offset() or 0,
        "messaging.consumer.group.name": GROUP,
    }
    with tracer.start_as_current_span(
        f"{TOPIC} process",
        context=propagate.extract(carrier),
        kind=SpanKind.CONSUMER,
        attributes=attributes,
    ) as span:
        try:
            event = rounds_pb2.RoundEvent.FromString(msg.value() or b"")
        except DecodeError as e:
            log.warning(
                "malformed round record skipped", extra={"offset": msg.offset(), "error": str(e)}
            )
            return _count(span, "skipped")
        if event.WhichOneof("event") != "round_finished":
            # A newer producer's event type, or an empty wrapper (ADR 0004).
            log.warning("unknown round event skipped", extra={"offset": msg.offset()})
            return _count(span, "skipped")

        rf = event.round_finished
        fields = {
            "match_id": rf.match_id,
            "room_id": rf.room_id,
            "round_no": rf.round_no,
            "event_id": rf.meta.event_id,
        }
        close_old_connections()
        try:
            result, drawing = ingest(rf)
        except (DataError, IntegrityError, ValueError) as e:
            # The database refuses this record itself; retrying can't change that.
            log.error(
                "round record refused by the database; skipped", extra={**fields, "error": str(e)}
            )
            return _count(span, "skipped")
        if drawing is not None:
            fields["drawing_id"] = str(drawing.id)
            span.set_attribute("drawing.id", str(drawing.id))
            if drawing.status == Status.PROCESSING:
                prepare_drawing.delay(str(drawing.id))
        log.info("round received", extra={**fields, "result": result})
        return _count(span, result)


def _headers(msg: Message) -> dict[str, str]:
    """The record's headers as text, for the trace context (traceparent, tracestate)."""
    headers: list[tuple[str, bytes | None]] = cast(Any, msg.headers()) or []
    return {k: v.decode("utf-8", "replace") for k, v in headers if isinstance(v, bytes)}


def _count(span: trace.Span, result: str) -> str:
    span.set_attribute("round.result", result)
    received.add(1, {"result": result})
    return result


# Postgres unreachable or restarting, or the Celery broker down: worth waiting for.
RETRYABLE = (OperationalError, InterfaceError, BrokerError)


def handle_with_retry(
    msg: Message, stop: threading.Event, sleep: Callable[[float], Any] = time.sleep
) -> bool:
    """Retries one record until it's processed or the consumer stops. True when processed."""
    delay = 0.5
    while not stop.is_set():
        try:
            handle(msg)
            return True
        except RETRYABLE as e:
            log.warning(
                "round not stored yet; retrying",
                extra={"offset": msg.offset(), "error": str(e), "retry_in_s": delay},
            )
            close_old_connections()  # a broken connection is reopened on the next try
            sleep(delay)
            delay = min(delay * 2, 30.0)
    return False


def run(
    brokers: str,
    stop: threading.Event,
    client_id: str = "assets",
    topic: str = TOPIC,
    group: str = GROUP,
) -> None:
    consumer = Consumer(
        {
            "bootstrap.servers": brokers,
            "group.id": group,
            "client.id": client_id,
            # A new group starts from the oldest round Kafka still has, so nothing is missed.
            "auto.offset.reset": "earliest",
            # Commit by hand, after the round is stored and its task sent.
            "enable.auto.commit": False,
            # The assets Kafka user: it may read game.rounds.v1 in group assets.
            **kafka_auth(),
        }
    )
    consumer.subscribe([topic])
    log.info("round consumer started", extra={"topic": topic, "group": group})
    try:
        while not stop.is_set():
            ALIVE_FILE.touch()
            msg = consumer.poll(1.0)
            if msg is None:
                continue
            err = msg.error()
            if err is not None:
                if err.code() != KafkaError._PARTITION_EOF:
                    log.warning("kafka error", extra={"error": str(err)})
                continue
            # A long outage can outlast max.poll.interval.ms; the group then rebalances and
            # this record comes back, uncommitted, to whoever owns the partition next.
            if handle_with_retry(msg, stop):
                consumer.commit(message=msg, asynchronous=False)
    finally:
        consumer.close()  # leaves the group, so the partitions move without a session timeout
        connection.close()  # this thread's Django connection
        log.info("round consumer stopped")
