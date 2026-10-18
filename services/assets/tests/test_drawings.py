"""The Drawing model's constraints and its guarded status changes, against a real Postgres."""

import threading
import uuid
from typing import Any

import pytest
from django.contrib.auth.models import User
from django.db import IntegrityError, connection, transaction
from django.utils import timezone

from drawings import moderation
from drawings.models import DecidedBy, Drawing, Status
from tests.conftest import IMAGE

pytestmark = pytest.mark.django_db


def test_ids_are_uuid7_in_creation_order(make_drawing: Any) -> None:
    first = make_drawing(round_no=1)
    second = make_drawing(round_no=2)

    assert first.id.version == 7
    assert first.id < second.id
    assert first.status == Status.PROCESSING


def test_one_drawing_per_round(make_drawing: Any) -> None:
    make_drawing(match_id="m_9", round_no=3)

    with pytest.raises(IntegrityError), transaction.atomic():
        make_drawing(match_id="m_9", round_no=3, source_event_id="another-event")
    assert Drawing.objects.filter(match_id="m_9").count() == 1


def test_the_database_refuses_a_decision_without_a_decider(make_drawing: Any) -> None:
    drawing = make_drawing()

    with pytest.raises(IntegrityError), transaction.atomic():
        Drawing.objects.filter(pk=drawing.pk).update(status=Status.APPROVED)


def test_the_database_refuses_a_moderator_decision_without_the_moderator(
    make_drawing: Any,
) -> None:
    drawing = make_drawing()

    with pytest.raises(IntegrityError), transaction.atomic():
        Drawing.objects.filter(pk=drawing.pk).update(
            status=Status.APPROVED, decided_by=DecidedBy.MODERATOR, decided_at=timezone.now()
        )


def test_ai_decides_a_processing_drawing_once(make_drawing: Any) -> None:
    drawing = make_drawing(**IMAGE)

    assert moderation.decide(drawing.id, approve=True, by=DecidedBy.AI)
    # A redelivered task (Celery delivers at least once) changes nothing.
    assert not moderation.decide(drawing.id, approve=False, by=DecidedBy.AI)

    drawing.refresh_from_db()
    assert drawing.status == Status.APPROVED
    assert drawing.decided_by == DecidedBy.AI
    assert drawing.decided_at is not None
    assert drawing.moderator is None


def test_ai_cannot_overrule_the_review_queue(make_drawing: Any) -> None:
    drawing = make_drawing(**IMAGE)
    assert moderation.send_to_review(drawing.id, reason="moderation timed out")

    assert not moderation.decide(drawing.id, approve=True, by=DecidedBy.AI)
    drawing.refresh_from_db()
    assert drawing.status == Status.REVIEW
    assert drawing.decision_reason == "moderation timed out"


def test_a_moderator_decides_only_from_review(make_drawing: Any) -> None:
    mod = User.objects.create(username="u_mod", is_staff=True)
    drawing = make_drawing()

    assert not moderation.decide(drawing.id, approve=False, by=DecidedBy.MODERATOR, moderator=mod)
    moderation.send_to_review(drawing.id)
    assert moderation.decide(
        drawing.id, approve=False, by=DecidedBy.MODERATOR, moderator=mod, reason="not a cat"
    )

    drawing.refresh_from_db()
    assert drawing.status == Status.REJECTED
    assert drawing.moderator == mod
    assert drawing.decision_reason == "not a cat"


def test_send_to_review_only_from_processing(make_drawing: Any) -> None:
    drawing = make_drawing(**IMAGE)
    moderation.decide(drawing.id, approve=True, by=DecidedBy.AI)

    assert not moderation.send_to_review(drawing.id)
    drawing.refresh_from_db()
    assert drawing.status == Status.APPROVED


def test_a_moderator_decision_must_name_the_moderator(make_drawing: Any) -> None:
    drawing = make_drawing()

    with pytest.raises(ValueError):
        moderation.decide(drawing.id, approve=True, by=DecidedBy.MODERATOR)


def test_a_drawing_without_its_image_is_never_approved(make_drawing: Any) -> None:
    drawing = make_drawing()  # not prepared: nothing to mint

    assert not moderation.decide(drawing.id, approve=True, by=DecidedBy.AI)
    assert moderation.decide(drawing.id, approve=False, by=DecidedBy.AI)


def test_unknown_drawing_is_not_decided() -> None:
    assert not moderation.decide(uuid.uuid7(), approve=True, by=DecidedBy.AI)


@pytest.mark.django_db(transaction=True)
def test_concurrent_moderators_exactly_one_decides(make_drawing: Any) -> None:
    """Many moderators approve and reject the same drawing at once, each on its own connection.
    Exactly one decision is applied, and the row shows that one."""
    mods = [User.objects.create(username=f"u_mod{i}", is_staff=True) for i in range(8)]
    drawing = make_drawing(**IMAGE)
    moderation.send_to_review(drawing.id)

    barrier = threading.Barrier(len(mods))
    results: dict[str, bool] = {}

    def moderate(i: int) -> None:
        try:
            barrier.wait()
            results[mods[i].username] = moderation.decide(
                drawing.id, approve=i % 2 == 0, by=DecidedBy.MODERATOR, moderator=mods[i]
            )
        finally:
            connection.close()

    threads = [threading.Thread(target=moderate, args=(i,)) for i in range(len(mods))]
    for t in threads:
        t.start()
    for t in threads:
        t.join()

    winners = [name for name, won in results.items() if won]
    assert len(results) == len(mods)
    assert len(winners) == 1
    drawing.refresh_from_db()
    assert drawing.moderator is not None
    assert drawing.moderator.username == winners[0]
    winner_index = int(winners[0].removeprefix("u_mod"))
    expected = Status.APPROVED if winner_index % 2 == 0 else Status.REJECTED
    assert drawing.status == expected


def test_migrations_match_the_models() -> None:
    from django.core.management import call_command

    call_command("makemigrations", "--check", "--dry-run", verbosity=0)


def test_fail_only_from_processing(make_drawing: Any) -> None:
    drawing = make_drawing(**IMAGE)

    assert moderation.fail(drawing.id, "stroke log not found: m_1/1.binpb.gz")
    assert not moderation.fail(drawing.id, "again")
    assert not moderation.decide(drawing.id, approve=True, by=DecidedBy.AI)
    assert not moderation.send_to_review(drawing.id)

    drawing.refresh_from_db()
    assert drawing.status == Status.FAILED
    assert drawing.decision_reason == "stroke log not found: m_1/1.binpb.gz"
    assert drawing.decided_by == ""
