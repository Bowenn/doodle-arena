"""The outbox relay (`manage.py relay_outbox`, the assets-relay process): publishes the outbox's
events to Kafka and stamps each row only after Kafka confirms it.

Each pass locks a batch of unpublished rows in id order (FOR UPDATE SKIP LOCKED, so a second
relay takes other rows, never the same ones), produces them, waits for the acknowledgements and
stamps published_at for the ones Kafka confirmed, all in one transaction. If the relay dies
after Kafka confirmed but before the stamp commits, the rows are published again with the same
event_id: delivery is at least once, and consumers dedupe on event_id or drawing_id.

The producer is idempotent (no duplicates from its own retries) with acks=all. Rows of one
drawing share a key, so they stay in order on one partition.
"""

import logging
import threading
import time
from collections.abc import Iterable
from datetime import timedelta
from pathlib import Path
from typing import Any

from confluent_kafka import KafkaError, Message, Producer
from django.db import OperationalError, close_old_connections, connection, transaction
from django.db.models import Min
from django.utils import timezone
from opentelemetry import metrics, propagate, trace
from opentelemetry.metrics import CallbackOptions, Observation
from opentelemetry.trace import SpanKind

from drawings.kafka_auth import kafka_auth
from drawings.models import OutboxEvent

BATCH = 100
IDLE_SLEEP = 0.2
# How long one pass waits for Kafka's acknowledgements before trying again later.
FLUSH_TIMEOUT = 10.0
KEEP_PUBLISHED = timedelta(days=7)
# The container's health check reads this file's age (infra/docker-compose.yml).
ALIVE_FILE = Path("/tmp/relay-alive")

log = logging.getLogger(__name__)
tracer = trace.get_tracer(__name__)
_meter = metrics.get_meter(__name__)
published = _meter.create_counter(
    "assets.events.published",
    description="Outbox events by result: success (confirmed by Kafka), failed (left to retry)",
)


def _observe_lag(options: CallbackOptions) -> Iterable[Observation]:
    """Age of the oldest unpublished event, 0 when the outbox is drained."""
    try:
        oldest = OutboxEvent.objects.filter(published_at__isnull=True).aggregate(
            m=Min("created_at")
        )["m"]
    except Exception as e:  # the exporter thread must not die with the database
        log.warning("could not read the outbox lag", extra={"error": str(e)})
        return []
    finally:
        connection.close()  # the exporter's thread keeps no connection between exports
    age = (timezone.now() - oldest).total_seconds() if oldest else 0.0
    return [Observation(max(0.0, age))]


def register_lag_gauge() -> None:
    _meter.create_observable_gauge(
        "assets.outbox.lag",
        callbacks=[_observe_lag],
        unit="s",
        description="Age of the oldest outbox event not yet confirmed by Kafka",
    )


def new_producer(brokers: str, client_id: str) -> Producer:
    return Producer(
        {
            "bootstrap.servers": brokers,
            "client.id": client_id,
            "enable.idempotence": True,  # implies acks=all and ordered, deduplicated retries
            "linger.ms": 5,
            # Give up on a message after this long; the row stays unpublished and is retried.
            "message.timeout.ms": int(FLUSH_TIMEOUT * 1000),
            # The assets Kafka user: it may write assets.drawings.v1.
            **kafka_auth(),
        }
    )


def publish_batch(producer: Producer, limit: int = BATCH) -> tuple[int, int]:
    """One pass. Returns (published, failed)."""
    with transaction.atomic():
        rows = list(
            OutboxEvent.objects.select_for_update(skip_locked=True)
            .filter(published_at__isnull=True)
            .order_by("id")[:limit]
        )
        if not rows:
            return 0, 0
        confirmed: list[Any] = []

        for row in rows:
            parent = propagate.extract({"traceparent": row.traceparent} if row.traceparent else {})
            with tracer.start_as_current_span(
                f"{row.topic} publish",
                context=parent,
                kind=SpanKind.PRODUCER,
                attributes={
                    "messaging.system": "kafka",
                    "messaging.destination.name": row.topic,
                    "messaging.message.id": str(row.id),
                    "drawing.id": row.key,
                },
            ):
                headers: dict[str, str] = {}
                propagate.inject(headers)  # the consumer continues this span's trace

                def on_delivery(err: KafkaError | None, msg: Message, row_id: Any = row.id) -> None:
                    if err is None:
                        confirmed.append(row_id)
                    else:
                        log.warning(
                            "outbox event not confirmed",
                            extra={"event_id": str(row_id), "error": str(err)},
                        )

                producer.produce(
                    row.topic,
                    key=row.key.encode(),
                    value=bytes(row.payload),
                    headers=[(k, v.encode()) for k, v in headers.items()],
                    on_delivery=on_delivery,
                )
        producer.flush(FLUSH_TIMEOUT)
        # What Kafka confirmed by now; every other row (refused, or still unanswered: flush's own
        # count also includes librdkafka's internal requests) stays unpublished and is retried. A
        # confirmation that arrives later only means that row goes out twice.
        done = list(confirmed)
        failed = len(rows) - len(done)
        if done:
            OutboxEvent.objects.filter(pk__in=done).update(published_at=timezone.now())
    if done:
        published.add(len(done), {"result": "success"})
        log.info("outbox events published", extra={"count": len(done)})
    if failed:
        published.add(failed, {"result": "failed"})
    return len(done), failed


def purge_published(older_than: timedelta = KEEP_PUBLISHED) -> int:
    """Deletes events published long ago; they're kept a while for debugging and replays."""
    deleted, _ = OutboxEvent.objects.filter(published_at__lt=timezone.now() - older_than).delete()
    return deleted


def run(brokers: str, stop: threading.Event, client_id: str = "assets-relay") -> None:
    producer = new_producer(brokers, client_id)
    register_lag_gauge()
    log.info("outbox relay started")
    backoff = IDLE_SLEEP
    last_purge = 0.0
    try:
        while not stop.is_set():
            ALIVE_FILE.touch()
            close_old_connections()
            try:
                done, failed = publish_batch(producer)
            except OperationalError as e:  # Postgres down or restarting
                log.warning("outbox not readable; retrying", extra={"error": str(e)})
                done, failed = 0, 1
            if failed:
                # Kafka or Postgres is in trouble: back off, up to 5 s between passes.
                backoff = min(backoff * 2, 5.0)
                stop.wait(backoff)
            else:
                backoff = IDLE_SLEEP
                if not done:
                    stop.wait(IDLE_SLEEP)
            if time.monotonic() - last_purge > 3600:
                purge_published()
                last_purge = time.monotonic()
    finally:
        producer.flush(FLUSH_TIMEOUT)
        connection.close()
        log.info("outbox relay stopped")
