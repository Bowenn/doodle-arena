"""Decisions write DrawingApproved or DrawingRejected to the outbox in the same transaction."""

from typing import Any

import pytest
from django.contrib.auth.models import User
from doodle.events.v1 import drawings_pb2
from opentelemetry import trace
from opentelemetry.trace import NonRecordingSpan, SpanContext, TraceFlags

from drawings import events, moderation
from drawings.models import DecidedBy, Drawing, OutboxEvent, Status
from tests.conftest import IMAGE

pytestmark = pytest.mark.django_db


def decode(row: OutboxEvent) -> drawings_pb2.DrawingEvent:
    return drawings_pb2.DrawingEvent.FromString(bytes(row.payload))


def test_an_ai_approval_writes_drawing_approved(make_drawing: Any) -> None:
    drawing = make_drawing(**IMAGE)

    assert moderation.decide(drawing.id, approve=True, by=DecidedBy.AI, reason="stub-1: ok")

    row = OutboxEvent.objects.get()
    assert (row.topic, row.key, row.event_type) == (
        "assets.drawings.v1",
        str(drawing.id),
        "DrawingApproved",
    )
    assert row.published_at is None
    event = decode(row)
    assert event.WhichOneof("event") == "drawing_approved"
    a = event.drawing_approved
    assert a.meta.event_id == str(row.id)
    assert (a.meta.event_type, a.meta.producer, a.meta.schema_version) == (
        "DrawingApproved",
        "assets",
        1,
    )
    assert a.meta.occurred_at.ToSeconds() > 0
    assert a.drawing_id == str(drawing.id)
    assert (a.match_id, a.round_no) == (drawing.match_id, drawing.round_no)
    assert list(a.author_ids) == ["u_ada", "u_bob"]
    assert a.initial_owner_id == "u_ada"
    assert (a.sha256, a.image_key, a.thumb_key) == (
        IMAGE["sha256"],
        IMAGE["image_key"],
        IMAGE["thumb_key"],
    )
    assert a.approved_by == drawings_pb2.DECISION_SOURCE_AI


def test_a_moderator_rejection_writes_drawing_rejected(make_drawing: Any) -> None:
    mod = User.objects.create(username="u_mod", is_staff=True)
    drawing = make_drawing(**IMAGE)
    moderation.send_to_review(drawing.id)

    moderation.decide(
        drawing.id, approve=False, by=DecidedBy.MODERATOR, moderator=mod, reason="spam"
    )

    r = decode(OutboxEvent.objects.get()).drawing_rejected
    assert r.drawing_id == str(drawing.id)
    assert r.rejected_by == drawings_pb2.DECISION_SOURCE_MODERATOR
    assert r.reason == "manual:rejected"  # the free text stays in the admin


def test_an_ignored_decision_writes_nothing(make_drawing: Any) -> None:
    drawing = make_drawing(**IMAGE)
    moderation.decide(drawing.id, approve=True, by=DecidedBy.AI)

    assert not moderation.decide(drawing.id, approve=False, by=DecidedBy.AI)
    assert OutboxEvent.objects.count() == 1


def test_no_event_means_no_decision(make_drawing: Any, monkeypatch: pytest.MonkeyPatch) -> None:
    """The decision and its event commit together: if writing the event fails, the status
    change rolls back too."""
    drawing = make_drawing(**IMAGE)

    def broken(d: Drawing) -> None:
        raise RuntimeError("outbox insert failed")

    monkeypatch.setattr(events, "enqueue_decision", broken)
    with pytest.raises(RuntimeError):
        moderation.decide(drawing.id, approve=True, by=DecidedBy.AI)

    drawing.refresh_from_db()
    assert drawing.status == Status.PROCESSING
    assert drawing.decided_at is None
    assert not OutboxEvent.objects.exists()


def test_the_event_carries_the_deciders_trace(make_drawing: Any) -> None:
    drawing = make_drawing(**IMAGE)
    ctx = SpanContext(
        trace_id=0x4BF92F3577B34DA6A3CE929D0E0E4736,
        span_id=0x00F067AA0BA902B7,
        is_remote=False,
        trace_flags=TraceFlags(TraceFlags.SAMPLED),
    )

    with trace.use_span(NonRecordingSpan(ctx)):
        moderation.decide(drawing.id, approve=True, by=DecidedBy.AI)

    assert OutboxEvent.objects.get().traceparent.startswith("00-4bf92f3577b34da6a3ce929d0e0e4736-")
