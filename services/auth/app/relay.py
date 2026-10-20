"""The outbox relay (`python -m app.relay`, the auth-relay process, ADR 0015): publishes the
outbox's events to Kafka and stamps each row only after Kafka confirms it.

Each pass locks a batch of unpublished rows (FOR UPDATE SKIP LOCKED, so a second relay takes
other rows), produces them, waits for the acknowledgements and stamps published_at for the ones
Kafka confirmed, all in one transaction. If the relay dies after Kafka confirmed but before the
stamp commits, the rows go out again with the same event_id: delivery is at least once, and the
notifier's handling of CredentialsRevoked is idempotent. The producer is idempotent with
acks=all; rows of one user share a key, so they stay in order on one partition.

Mirrors the assets relay (services/assets/drawings/relay.py), in SQLAlchemy Core.
"""

import logging
import logging.config
import os
import signal
import threading
import time
from collections.abc import Iterable
from datetime import UTC, datetime, timedelta
from pathlib import Path
from typing import Any

from confluent_kafka import KafkaError, Message, Producer
from opentelemetry import metrics, propagate, trace
from opentelemetry.metrics import CallbackOptions, Observation
from opentelemetry.trace import SpanKind
from sqlalchemy import Engine, delete, func, select, update
from sqlalchemy.exc import OperationalError

from app import SERVICE_NAME, telemetry
from app.db import make_engine, outbox
from app.kafka_auth import kafka_auth
from app.logs import logging_config
from app.settings import Settings

BATCH = 100
IDLE_SLEEP = 0.2
# How long one pass waits for Kafka's acknowledgements before trying again later.
FLUSH_TIMEOUT = 10.0
KEEP_PUBLISHED = timedelta(days=7)
# The container's health check reads this file's age (infra/docker-compose.yml).
ALIVE_FILE = Path("/tmp/relay-alive")

log = logging.getLogger(__name__)
tracer = trace.get_tracer(__name__)


class RelayMetrics:
    def __init__(self, engine: Engine) -> None:
        meter = metrics.get_meter(SERVICE_NAME)
        self.published = meter.create_counter(
            "auth.events.published",
            unit="{event}",
            description="Outbox events by result: success (confirmed by Kafka), failed (retried)",
        )
        self._engine = engine
        meter.create_observable_gauge(
            "auth.outbox.lag",
            callbacks=[self._observe_lag],
            unit="s",
            description="Age of the oldest outbox event not yet confirmed by Kafka",
        )

    def _observe_lag(self, options: CallbackOptions) -> Iterable[Observation]:
        try:
            with self._engine.connect() as conn:
                oldest = conn.execute(
                    select(func.min(outbox.c.created_at)).where(outbox.c.published_at.is_(None))
                ).scalar_one()
        except Exception as e:  # the exporter thread must not die with the database
            log.warning("could not read the outbox lag", extra={"error": str(e)})
            return []
        age = (datetime.now(UTC) - oldest).total_seconds() if oldest else 0.0
        return [Observation(max(0.0, age))]


def new_producer(brokers: str, client_id: str) -> Producer:
    return Producer(
        {
            "bootstrap.servers": brokers,
            "client.id": client_id,
            "enable.idempotence": True,  # implies acks=all and ordered, deduplicated retries
            "linger.ms": 5,
            # Give up on a message after this long; the row stays unpublished and is retried.
            "message.timeout.ms": int(FLUSH_TIMEOUT * 1000),
            # Compose publishes Kafka on 127.0.0.1 for host-side tools; stay on IPv4.
            "broker.address.family": "v4",
            # The auth Kafka user: it may write auth.users.v1.
            **kafka_auth(),
        }
    )


def publish_batch(
    engine: Engine, producer: Producer, m: RelayMetrics | None = None, limit: int = BATCH
) -> tuple[int, int]:
    """One pass. Returns (published, failed)."""
    with engine.begin() as conn:
        rows = conn.execute(
            select(outbox)
            .where(outbox.c.published_at.is_(None))
            .order_by(outbox.c.created_at, outbox.c.event_id)
            .limit(limit)
            .with_for_update(skip_locked=True)
        ).all()
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
                    "messaging.message.id": str(row.event_id),
                    "user.id": row.key,
                },
            ):
                headers: dict[str, str] = {}
                propagate.inject(headers)  # the consumer continues this span's trace

                def on_delivery(
                    err: KafkaError | None, msg: Message, event_id: Any = row.event_id
                ) -> None:
                    if err is None:
                        confirmed.append(event_id)
                    else:
                        log.warning(
                            "outbox event not confirmed",
                            extra={"event_id": str(event_id), "error": str(err)},
                        )

                producer.produce(
                    row.topic,
                    key=row.key.encode(),
                    value=bytes(row.payload),
                    headers=[(k, v.encode()) for k, v in headers.items()],
                    on_delivery=on_delivery,
                )
        producer.flush(FLUSH_TIMEOUT)
        # What Kafka confirmed by now; every other row stays unpublished and is retried. A
        # confirmation that arrives later only means that row goes out twice.
        done = list(confirmed)
        failed = len(rows) - len(done)
        if done:
            conn.execute(
                update(outbox).where(outbox.c.event_id.in_(done)).values(published_at=func.now())
            )
    if m is not None:
        if done:
            m.published.add(len(done), {"result": "success"})
        if failed:
            m.published.add(failed, {"result": "failed"})
    if done:
        log.info("outbox events published", extra={"count": len(done)})
    return len(done), failed


def purge_published(engine: Engine, older_than: timedelta = KEEP_PUBLISHED) -> int:
    """Deletes events published long ago; they're on the topic by then."""
    with engine.begin() as conn:
        result = conn.execute(
            delete(outbox).where(outbox.c.published_at < datetime.now(UTC) - older_than)
        )
    return result.rowcount


def run(engine: Engine, brokers: str, stop: threading.Event, client_id: str = "auth-relay") -> None:
    producer = new_producer(brokers, client_id)
    m = RelayMetrics(engine)
    log.info("outbox relay started")
    backoff = IDLE_SLEEP
    last_purge = 0.0
    try:
        while not stop.is_set():
            ALIVE_FILE.touch()
            try:
                done, failed = publish_batch(engine, producer, m)
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
                purge_published(engine)
                last_purge = time.monotonic()
    finally:
        producer.flush(FLUSH_TIMEOUT)
        log.info("outbox relay stopped")


def main() -> None:
    settings = Settings()
    engine = make_engine(settings.database_url())
    shutdown_telemetry = telemetry.setup_process_telemetry(settings, engine)
    logging.config.dictConfig(
        logging_config(
            SERVICE_NAME, settings.service_version, settings.log_level, telemetry.enabled()
        )
    )
    stop = threading.Event()
    signal.signal(signal.SIGTERM, lambda *_: stop.set())
    signal.signal(signal.SIGINT, lambda *_: stop.set())
    try:
        run(engine, os.environ.get("KAFKA_BROKERS", "kafka:9092"), stop)
    finally:
        engine.dispose()
        shutdown_telemetry()


if __name__ == "__main__":
    main()
