"""The events assets publishes on assets.drawings.v1 (design doc 6.2): DrawingApproved and
DrawingRejected, each in the topic's DrawingEvent wrapper (ADR 0004), keyed by drawing_id."""

import uuid
from datetime import datetime

from doodle.events.v1 import drawings_pb2, meta_pb2
from opentelemetry import propagate

from drawings.models import DecidedBy, Drawing, OutboxEvent, Status

TOPIC = "assets.drawings.v1"
PRODUCER = "assets"
SCHEMA_VERSION = 1
_SOURCES = {
    DecidedBy.AI: drawings_pb2.DECISION_SOURCE_AI,
    DecidedBy.MODERATOR: drawings_pb2.DECISION_SOURCE_MODERATOR,
}


def _meta(
    meta: meta_pb2.EventMeta, event_id: uuid.UUID, event_type: str, occurred_at: datetime
) -> None:
    meta.event_id = str(event_id)
    meta.event_type = event_type
    meta.schema_version = SCHEMA_VERSION
    meta.occurred_at.FromDatetime(occurred_at)
    meta.producer = PRODUCER


def decision_event(drawing: Drawing, event_id: uuid.UUID) -> tuple[str, bytes]:
    """The event for a decided drawing: its type and the serialized DrawingEvent."""
    assert drawing.decided_at is not None
    wrapper = drawings_pb2.DrawingEvent()
    source = _SOURCES[DecidedBy(drawing.decided_by)]
    if drawing.status == Status.APPROVED:
        e = wrapper.drawing_approved
        _meta(e.meta, event_id, "DrawingApproved", drawing.decided_at)
        e.drawing_id = str(drawing.id)
        e.match_id, e.round_no = drawing.match_id, drawing.round_no
        e.author_ids.extend(drawing.author_ids)
        e.initial_owner_id = drawing.initial_owner_id
        e.sha256, e.image_key, e.thumb_key = drawing.sha256, drawing.image_key, drawing.thumb_key
        e.approved_by = source
        return "DrawingApproved", wrapper.SerializeToString()
    if drawing.status == Status.REJECTED:
        r = wrapper.drawing_rejected
        _meta(r.meta, event_id, "DrawingRejected", drawing.decided_at)
        r.drawing_id = str(drawing.id)
        r.match_id, r.round_no = drawing.match_id, drawing.round_no
        r.author_ids.extend(drawing.author_ids)
        r.rejected_by = source
        # Machine-readable and short; the free-text reason stays in the admin.
        r.reason = (
            "moderation:rejected"
            if source == drawings_pb2.DECISION_SOURCE_AI
            else "manual:rejected"
        )
        return "DrawingRejected", wrapper.SerializeToString()
    raise ValueError(f"drawing {drawing.id} is {drawing.status}, not decided")


def enqueue_decision(drawing: Drawing) -> OutboxEvent:
    """Writes the decision's event to the outbox. Call it inside the decision's transaction."""
    carrier: dict[str, str] = {}
    propagate.inject(carrier)
    event_id = uuid.uuid7()
    event_type, payload = decision_event(drawing, event_id)
    return OutboxEvent.objects.create(
        id=event_id,
        topic=TOPIC,
        key=str(drawing.id),
        event_type=event_type,
        payload=payload,
        traceparent=carrier.get("traceparent", ""),
    )
