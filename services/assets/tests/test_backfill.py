"""The backfill for drawings decided before the outbox existed, and the session cleanup."""

from datetime import timedelta
from io import StringIO
from typing import Any

import pytest
from django.contrib.sessions.backends.db import SessionStore
from django.contrib.sessions.models import Session
from django.core.management import call_command
from django.utils import timezone

from drawings import moderation
from drawings.models import DecidedBy, Drawing, OutboxEvent, Status
from drawings.tasks import clear_expired_sessions
from tests.conftest import IMAGE

pytestmark = pytest.mark.django_db


def run(*args: str) -> str:
    out = StringIO()
    call_command("backfill_decision_events", *args, stdout=out)
    return out.getvalue().strip()


def test_old_decisions_get_their_event_once(make_drawing: Any) -> None:
    now = timezone.now()
    old_approved = make_drawing(round_no=1, **IMAGE)
    old_rejected = make_drawing(round_no=2, **IMAGE)
    # Decided the way 3.3 did, before decide() wrote to the outbox.
    Drawing.objects.filter(pk=old_approved.pk).update(
        status=Status.APPROVED, decided_by="ai", decided_at=now
    )
    Drawing.objects.filter(pk=old_rejected.pk).update(
        status=Status.REJECTED, decided_by="ai", decided_at=now
    )
    new = make_drawing(round_no=3, **IMAGE)
    moderation.decide(new.id, approve=True, by=DecidedBy.AI)  # already has its event
    make_drawing(round_no=4)  # still processing: nothing to announce

    assert run("--dry-run") == "would write 2 decision events"
    assert OutboxEvent.objects.count() == 1

    assert run() == "wrote 2 decision events"
    assert sorted(OutboxEvent.objects.values_list("event_type", flat=True)) == [
        "DrawingApproved",
        "DrawingApproved",
        "DrawingRejected",
    ]
    assert run() == "wrote 0 decision events"


def test_expired_sessions_are_cleared() -> None:
    live, dead = SessionStore(), SessionStore()
    live.create()
    dead.create()
    Session.objects.filter(session_key=dead.session_key).update(
        expire_date=timezone.now() - timedelta(minutes=1)
    )

    clear_expired_sessions()

    assert list(Session.objects.values_list("session_key", flat=True)) == [live.session_key]
