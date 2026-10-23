"""The search indexer (ADR 0023): keeps assets' projection of listings and each drawing's
Elasticsearch document current, from assets.drawings.v1, market.listings.v1 and
market.sales.v1.

Per record: one Postgres transaction applies the event to the projection and bumps the drawing's
search_version; after the commit, the drawing's whole document is built from Postgres and written
with that version (drawings.search). Then the offset is committed.

A drawing's events arrive on three topics, so in no fixed order. The rules make every order end
in the same state:

- ListingCreated sets the active listing, unless the listing is already closed, or an active
  listing that was created later is already there (a late, older ListingCreated).
- ListingCancelled and SaleCompleted record the listing as closed, and clear the active listing
  only if it is that one.
- DrawingApproved and DrawingRejected only bump the version: the drawing row (assets' own) says
  whether the document should exist.

Delivery is at least once, and every step is idempotent: a redelivered event changes nothing in
the projection and writes the same document again. Postgres or Elasticsearch being down is
retried on the same record; a record this build can't read is logged, counted and committed.
"""

import logging
import threading
import time
import uuid
from collections.abc import Callable
from datetime import UTC, datetime
from pathlib import Path
from typing import Any, cast

from confluent_kafka import Consumer, KafkaError, Message
from django.db import (
    InterfaceError,
    OperationalError,
    close_old_connections,
    connection,
    transaction,
)
from django.db.models import F
from doodle.events.v1 import drawings_pb2, listings_pb2, sales_pb2
from elastic_transport import ConnectionError as ESConnectionError
from elastic_transport import ConnectionTimeout, TransportError
from elasticsearch import ApiError, Elasticsearch
from google.protobuf.message import DecodeError
from opentelemetry import metrics, propagate, trace
from opentelemetry.trace import SpanKind

from drawings import search
from drawings.kafka_auth import kafka_auth
from drawings.models import Drawing, SearchClosedListing, SearchListing

DRAWINGS_TOPIC = "assets.drawings.v1"
LISTINGS_TOPIC = "market.listings.v1"
SALES_TOPIC = "market.sales.v1"
TOPICS = [DRAWINGS_TOPIC, LISTINGS_TOPIC, SALES_TOPIC]
GROUP = "assets-search"
# The container's health check reads this file's age (infra/docker-compose.yml).
ALIVE_FILE = Path("/tmp/indexer-alive")

log = logging.getLogger(__name__)
tracer = trace.get_tracer(__name__)
meter = metrics.get_meter(__name__)
indexed = meter.create_counter(
    "assets.search.indexed",
    description="Search events by event (approved, rejected, listing_created, listing_closed) and "
    "result (indexed, deleted, stale, absent, skipped)",
)
index_lag = meter.create_histogram(
    "assets.search.index_lag",
    unit="s",
    description="From an event's time until the drawing's document was written",
)


# ---------------------------------------------------------------------------------- projection


def _bump(drawing_id: uuid.UUID) -> None:
    Drawing.objects.filter(id=drawing_id).update(search_version=F("search_version") + 1)


def drawing_decided(drawing_id: uuid.UUID) -> None:
    """DrawingApproved or DrawingRejected: the row already says which; the document follows."""
    with transaction.atomic():
        _bump(drawing_id)


def listing_created(
    drawing_id: uuid.UUID, listing_id: uuid.UUID, seller_id: str, price: int, created_at: datetime
) -> bool:
    """Makes the listing the drawing's active one, unless it's closed or an older one. Returns
    whether the projection changed."""
    with transaction.atomic():
        if SearchClosedListing.objects.filter(listing_id=listing_id).exists():
            return False
        current = SearchListing.objects.select_for_update().filter(drawing_id=drawing_id).first()
        if current is not None:
            if current.listing_id == listing_id:
                return False  # redelivered
            if (current.listed_at, str(current.listing_id)) > (created_at, str(listing_id)):
                return False  # a later listing is already active: this one is old news
            current.delete()
        SearchListing.objects.create(
            drawing_id=drawing_id,
            listing_id=listing_id,
            seller_id=seller_id,
            price=price,
            listed_at=created_at,
        )
        _bump(drawing_id)
        return True


def listing_closed(drawing_id: uuid.UUID, listing_id: uuid.UUID) -> bool:
    """A listing was cancelled or sold: it can never be active again, and it stops being the
    active one if it is. Returns whether the active listing changed."""
    with transaction.atomic():
        SearchClosedListing.objects.get_or_create(
            listing_id=listing_id, defaults={"drawing_id": drawing_id}
        )
        deleted, _ = SearchListing.objects.filter(
            drawing_id=drawing_id, listing_id=listing_id
        ).delete()
        if deleted:
            _bump(drawing_id)
        return bool(deleted)


# ---------------------------------------------------------------------------------- records


class Skip(Exception):
    """A record this build can't use; it is counted and committed, never retried."""


def _uuid(value: str) -> uuid.UUID:
    try:
        return uuid.UUID(value)
    except ValueError as e:
        raise Skip(f"not a UUID: {value[:64]!r}") from e


def _time(ts: Any) -> datetime:
    return datetime.fromtimestamp(ts.seconds + ts.nanos / 1e9, tz=UTC)


def apply(topic: str, value: bytes) -> tuple[str, uuid.UUID, datetime | None]:
    """Applies one record to the projection. Returns the event's name, the drawing it concerns
    and when it happened. Raises Skip for a record that can't be used."""
    try:
        if topic == DRAWINGS_TOPIC:
            d = drawings_pb2.DrawingEvent.FromString(value)
            kind = d.WhichOneof("event")
            if kind == "drawing_approved":
                e = d.drawing_approved
                drawing_decided(_uuid(e.drawing_id))
                return "approved", _uuid(e.drawing_id), _time(e.meta.occurred_at)
            if kind == "drawing_rejected":
                r = d.drawing_rejected
                drawing_decided(_uuid(r.drawing_id))
                return "rejected", _uuid(r.drawing_id), _time(r.meta.occurred_at)
        elif topic == LISTINGS_TOPIC:
            ev = listings_pb2.ListingEvent.FromString(value)
            kind = ev.WhichOneof("event")
            if kind == "listing_created":
                c = ev.listing_created
                if c.price < 1 or not c.seller_id or len(c.seller_id) > 64:
                    raise Skip("a listing needs a price and a seller")
                drawing = _uuid(c.drawing_id)
                listing_created(
                    drawing, _uuid(c.listing_id), c.seller_id, c.price, _time(c.created_at)
                )
                return "listing_created", drawing, _time(c.meta.occurred_at)
            if kind == "listing_cancelled":
                x = ev.listing_cancelled
                drawing = _uuid(x.drawing_id)
                listing_closed(drawing, _uuid(x.listing_id))
                return "listing_closed", drawing, _time(x.meta.occurred_at)
        elif topic == SALES_TOPIC:
            s = sales_pb2.SaleEvent.FromString(value)
            if s.WhichOneof("event") == "sale_completed":
                sale = s.sale_completed
                drawing = _uuid(sale.drawing_id)
                listing_closed(drawing, _uuid(sale.listing_id))
                return "listing_closed", drawing, _time(sale.meta.occurred_at)
    except DecodeError as e:
        raise Skip(f"malformed record: {e}") from e
    # An unset case, or one added after this build (ADR 0004).
    raise Skip("unknown event")


def handle(msg: Message, es: Elasticsearch) -> str:
    """Processes one record and returns its result. Raises only for errors worth retrying."""
    topic = msg.topic() or ""
    headers: list[tuple[str, bytes | None]] = cast(Any, msg.headers()) or []
    carrier = {k: v.decode("utf-8", "replace") for k, v in headers if isinstance(v, bytes)}
    with tracer.start_as_current_span(
        f"{topic} process",
        context=propagate.extract(carrier),
        kind=SpanKind.CONSUMER,
        attributes={
            "messaging.system": "kafka",
            "messaging.destination.name": topic,
            "messaging.kafka.destination.partition": msg.partition() or 0,
            "messaging.kafka.message.offset": msg.offset() or 0,
            "messaging.consumer.group.name": GROUP,
        },
    ) as span:
        close_old_connections()
        try:
            event, drawing_id, happened = apply(topic, msg.value() or b"")
        except Skip as e:
            log.warning(
                "search record skipped",
                extra={"topic": topic, "offset": msg.offset(), "reason": str(e)},
            )
            indexed.add(1, {"event": "unknown", "result": "skipped"})
            return "skipped"
        span.set_attribute("drawing.id", str(drawing_id))
        result = search.index_drawing(drawing_id, es)
        indexed.add(1, {"event": event, "result": result})
        if happened is not None and result in ("indexed", "deleted"):
            index_lag.record(max(0.0, (datetime.now(UTC) - happened).total_seconds()))
        log.info(
            "search document updated",
            extra={"drawing_id": str(drawing_id), "event": event, "result": result},
        )
        return result


# Postgres or Elasticsearch unreachable, restarting or overloaded: worth waiting for.
RETRYABLE = (
    OperationalError,
    InterfaceError,
    ESConnectionError,
    ConnectionTimeout,
    TransportError,
    ApiError,
)


def handle_with_retry(
    msg: Message,
    es: Elasticsearch,
    stop: threading.Event,
    sleep: Callable[[float], Any] = time.sleep,
) -> bool:
    """Retries one record until it's processed or the indexer stops. True when processed."""
    delay = 0.5
    while not stop.is_set():
        try:
            handle(msg, es)
            return True
        except RETRYABLE as e:
            log.warning(
                "search record not indexed yet; retrying",
                extra={
                    "topic": msg.topic(),
                    "offset": msg.offset(),
                    "error": str(e)[:300],
                    "retry_in_s": delay,
                },
            )
            close_old_connections()
            sleep(delay)
            delay = min(delay * 2, 30.0)
    return False


def run(
    brokers: str,
    stop: threading.Event,
    client_id: str = "assets-indexer",
    topics: list[str] | None = None,
    group: str = GROUP,
    es: Elasticsearch | None = None,
) -> None:
    es = es or search.client()
    # The index and its alias, once Elasticsearch answers.
    delay = 0.5
    while not stop.is_set():
        try:
            search.ensure_index(es)
            break
        except RETRYABLE as e:
            log.warning(
                "search index not ready; retrying",
                extra={"error": str(e)[:300], "retry_in_s": delay},
            )
            ALIVE_FILE.touch()
            time.sleep(delay)
            delay = min(delay * 2, 30.0)
    consumer = Consumer(
        {
            "bootstrap.servers": brokers,
            "group.id": group,
            "client.id": client_id,
            # A new group starts from the oldest record Kafka still has (7 days): older listings
            # come from the marketplace's republish (ADR 0023).
            "auto.offset.reset": "earliest",
            "enable.auto.commit": False,
            # The assets Kafka user: it may read these topics in group assets-search.
            **kafka_auth(),
        }
    )
    consumer.subscribe(topics or TOPICS)
    log.info("search indexer started", extra={"topics": topics or TOPICS, "group": group})
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
            if handle_with_retry(msg, es, stop):
                consumer.commit(message=msg, asynchronous=False)
    finally:
        consumer.close()
        connection.close()
        log.info("search indexer stopped")
