"""Moderation through the AI service: its decisions are applied once, and a slow or failing
service is retried and then falls back to the review queue (M3)."""

from typing import Any

import pytest

from drawings import ai_client, pipeline
from drawings.models import DecidedBy, Drawing, Status
from drawings.tasks import moderate_drawing
from tests.conftest import FakeAI, stroke_log_bytes

pytestmark = [pytest.mark.django_db, pytest.mark.usefixtures("stores")]


@pytest.fixture
def prepared(make_drawing: Any) -> Drawing:
    """A drawing with its replay and images, waiting for moderation."""
    from drawings import storage

    storage.s3().put_object(
        Bucket="round-logs", Key="m_mod/1.binpb.gz", Body=stroke_log_bytes("m_mod")
    )
    drawing: Drawing = make_drawing(
        match_id="m_mod", stroke_log_key="m_mod/1.binpb.gz", stroke_count=1
    )
    assert pipeline.prepare(drawing.id) == "prepared"
    drawing.refresh_from_db()
    return drawing


@pytest.mark.parametrize(
    ("decision", "status"), [("approve", Status.APPROVED), ("reject", Status.REJECTED)]
)
def test_the_ai_decides(prepared: Drawing, fake_ai: FakeAI, decision: str, status: Status) -> None:
    fake_ai.decision = decision

    assert pipeline.moderate(prepared.id) in ("approved", "rejected")

    prepared.refresh_from_db()
    assert prepared.status == status
    assert prepared.decided_by == DecidedBy.AI
    assert prepared.decision_reason == "fake-1: fake"
    assert prepared.moderation_attempts == 1
    call = fake_ai.calls[0]
    assert call["path"] == "/v1/moderate"
    assert call["content_type"] == "image/png"
    assert call["drawing_id"] == str(prepared.id)
    assert call["body"].startswith(b"\x89PNG")


def test_an_unsure_ai_sends_the_drawing_to_review(prepared: Drawing, fake_ai: FakeAI) -> None:
    fake_ai.decision = "review"

    assert pipeline.moderate(prepared.id) == "manual"
    prepared.refresh_from_db()
    assert prepared.status == Status.REVIEW
    assert prepared.decision_reason.startswith("AI unsure")


@pytest.mark.parametrize("trouble", ["slow", "failing"])
def test_a_slow_or_failing_ai_is_retried_then_falls_back_to_review(
    prepared: Drawing, fake_ai: FakeAI, trouble: str
) -> None:
    """M3: when the AI stub is slow or failing, the call is retried, and after the last attempt
    the drawing lands in the review queue."""
    if trouble == "slow":
        fake_ai.delay = 1.0  # longer than the 0.3 s timeout
    else:
        fake_ai.status = 503

    # Celery runs the retries inline here (eager apply); the countdowns are skipped.
    result = moderate_drawing.apply(args=(str(prepared.id),))

    assert result.get() == "manual"
    prepared.refresh_from_db()
    assert prepared.status == Status.REVIEW
    assert prepared.moderation_attempts == pipeline.MAX_MODERATION_ATTEMPTS
    assert len(fake_ai.calls) == pipeline.MAX_MODERATION_ATTEMPTS
    assert f"after {pipeline.MAX_MODERATION_ATTEMPTS} attempts" in prepared.decision_reason


def test_it_recovers_when_the_ai_comes_back(prepared: Drawing, fake_ai: FakeAI) -> None:
    fake_ai.status = 503
    with pytest.raises(ai_client.ModerationUnavailable):
        pipeline.moderate(prepared.id)

    fake_ai.status = 200
    assert pipeline.moderate(prepared.id) == "approved"
    prepared.refresh_from_db()
    assert prepared.moderation_attempts == 2


@pytest.mark.parametrize("status", [400, 415, 422])
def test_a_refusal_goes_to_review_without_retrying(
    prepared: Drawing, fake_ai: FakeAI, status: int
) -> None:
    fake_ai.status = status

    assert moderate_drawing.apply(args=(str(prepared.id),)).get() == "manual"
    assert len(fake_ai.calls) == 1
    prepared.refresh_from_db()
    assert prepared.status == Status.REVIEW


def test_an_unreadable_answer_goes_to_review(prepared: Drawing, fake_ai: FakeAI) -> None:
    fake_ai.decision = "maybe"

    assert pipeline.moderate(prepared.id) == "manual"


def test_a_drawing_in_the_queue_is_not_asked_again(prepared: Drawing, fake_ai: FakeAI) -> None:
    fake_ai.decision = "review"
    pipeline.moderate(prepared.id)
    fake_ai.decision = "approve"

    assert pipeline.moderate(prepared.id) == "skipped"
    assert len(fake_ai.calls) == 1
    prepared.refresh_from_db()
    assert prepared.status == Status.REVIEW


def test_an_unprepared_drawing_is_not_moderated(make_drawing: Any, fake_ai: FakeAI) -> None:
    drawing = make_drawing(match_id="m_raw")

    assert pipeline.moderate(drawing.id) == "skipped"
    assert fake_ai.calls == []


def test_prepare_sends_the_moderation_task(
    make_drawing: Any, monkeypatch: pytest.MonkeyPatch
) -> None:
    from drawings import storage, tasks

    storage.s3().put_object(
        Bucket="round-logs", Key="m_chain/1.binpb.gz", Body=stroke_log_bytes("m_chain")
    )
    drawing = make_drawing(match_id="m_chain", stroke_log_key="m_chain/1.binpb.gz")
    sent: list[str] = []
    monkeypatch.setattr(tasks.moderate_drawing, "delay", lambda drawing_id: sent.append(drawing_id))

    tasks.prepare_drawing.apply(args=(str(drawing.id),)).get()

    assert sent == [str(drawing.id)]


@pytest.mark.django_db(transaction=True)
def test_two_tasks_for_one_drawing_make_one_call(make_drawing: Any, fake_ai: FakeAI) -> None:
    """A redelivered task (or the sweeper) running alongside the first doesn't call the AI
    service too, so it can't spend the attempt budget twice."""
    import threading

    from django.db import connection

    from drawings import storage

    storage.s3().put_object(
        Bucket="round-logs", Key="m_two/1.binpb.gz", Body=stroke_log_bytes("m_two")
    )
    drawing = make_drawing(match_id="m_two", stroke_log_key="m_two/1.binpb.gz", stroke_count=1)
    pipeline.prepare(drawing.id)
    fake_ai.delay = 0.2  # slow enough for the two to overlap, within the 0.3 s timeout
    results: list[str] = []

    def run() -> None:
        try:
            results.append(pipeline.moderate(drawing.id))
        finally:
            connection.close()

    threads = [threading.Thread(target=run) for _ in range(2)]
    for t in threads:
        t.start()
    for t in threads:
        t.join()

    assert sorted(results) == ["approved", "skipped"]
    assert len(fake_ai.calls) == 1
    drawing.refresh_from_db()
    assert drawing.moderation_attempts == 1
    assert drawing.moderation_claimed_until is None


def test_a_dead_tasks_claim_expires(prepared: Drawing, fake_ai: FakeAI) -> None:
    from datetime import timedelta

    from django.utils import timezone

    Drawing.objects.filter(pk=prepared.pk).update(
        moderation_claimed_until=timezone.now() + timedelta(seconds=30)
    )
    assert pipeline.moderate(prepared.id) == "skipped"
    assert fake_ai.calls == []

    Drawing.objects.filter(pk=prepared.pk).update(
        moderation_claimed_until=timezone.now() - timedelta(seconds=1)
    )
    assert pipeline.moderate(prepared.id) == "approved"


def test_the_claim_is_released_after_a_failed_call(prepared: Drawing, fake_ai: FakeAI) -> None:
    fake_ai.status = 503

    with pytest.raises(ai_client.ModerationUnavailable):
        pipeline.moderate(prepared.id)

    prepared.refresh_from_db()
    assert prepared.moderation_claimed_until is None
    assert prepared.moderation_attempts == 1
