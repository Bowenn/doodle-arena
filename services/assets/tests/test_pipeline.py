"""The pipeline against real MongoDB and SeaweedFS: each step once, identical images stored once,
and permanent problems marked failed."""

from typing import Any

import boto3
import pytest
from django.conf import settings

from drawings import pipeline, storage
from drawings.models import Drawing, Status
from drawings.tasks import requeue_stale_drawings
from tests.conftest import S3_KEY, S3_SECRET, stroke_log_bytes

pytestmark = [pytest.mark.django_db, pytest.mark.usefixtures("stores")]


def bucket() -> Any:
    return boto3.client(
        "s3",
        endpoint_url=settings.S3_ENDPOINT,
        region_name="us-east-1",
        aws_access_key_id=S3_KEY,
        aws_secret_access_key=S3_SECRET,
    )


def upload_log(match_id: str, round_no: int, **kwargs: Any) -> str:
    object_name = f"{match_id}/{round_no}.binpb.gz"
    bucket().put_object(
        Bucket="round-logs", Key=object_name, Body=stroke_log_bytes(match_id, round_no, **kwargs)
    )
    return object_name


def drawings_in_bucket() -> list[str]:
    resp = bucket().list_objects_v2(Bucket="drawings")
    return sorted(o["Key"] for o in resp.get("Contents", []))


def new_drawing(make_drawing: Any, match_id: str, round_no: int = 1, **kwargs: Any) -> Drawing:
    log_object = upload_log(match_id, round_no, **kwargs)
    drawing: Drawing = make_drawing(
        match_id=match_id, round_no=round_no, stroke_log_key=log_object, stroke_count=1
    )
    return drawing


def test_a_drawing_gets_its_replay_and_images(make_drawing: Any) -> None:
    drawing = new_drawing(make_drawing, "m_full")

    assert pipeline.prepare(drawing.id) == "prepared"

    drawing.refresh_from_db()
    assert drawing.status == Status.PROCESSING  # moderation comes in task 3.3
    assert drawing.replay_id == str(drawing.id)
    assert len(drawing.sha256) == 64
    assert drawing.image_key == f"sha256/{drawing.sha256}.png"
    assert drawing.thumb_key == f"sha256/{drawing.sha256}.thumb.png"
    assert drawings_in_bucket() == sorted([drawing.image_key, drawing.thumb_key])
    head = bucket().head_object(Bucket="drawings", Key=drawing.image_key)
    assert head["ContentType"] == "image/png"
    replay = storage.get_replay(str(drawing.id))
    assert replay is not None
    assert replay["match_id"] == "m_full"
    assert len(replay["strokes"]) == 1


def test_identical_images_are_stored_once(make_drawing: Any) -> None:
    """M3: two rounds with the same strokes share one object; both rows point to it."""
    a = new_drawing(make_drawing, "m_a")
    b = new_drawing(make_drawing, "m_b")

    pipeline.prepare(a.id)
    pipeline.prepare(b.id)

    a.refresh_from_db()
    b.refresh_from_db()
    assert a.sha256 == b.sha256
    assert a.image_key == b.image_key
    assert drawings_in_bucket() == sorted([a.image_key, a.thumb_key])


def test_running_twice_does_each_step_once(make_drawing: Any, monkeypatch: Any) -> None:
    drawing = new_drawing(make_drawing, "m_twice")
    pipeline.prepare(drawing.id)
    drawing.refresh_from_db()
    before = (drawing.replay_id, drawing.sha256, drawing.image_key)

    calls: list[str] = []

    def fetch(key: str) -> bytes:
        calls.append(f"fetch {key}")
        return b""

    def put(key: str, *args: Any) -> bool:
        calls.append(f"put {key}")
        return True

    monkeypatch.setattr(storage, "get_stroke_log", fetch)
    monkeypatch.setattr(storage, "put_if_absent", put)
    assert pipeline.prepare(drawing.id) == "prepared"

    drawing.refresh_from_db()
    assert calls == []
    assert (drawing.replay_id, drawing.sha256, drawing.image_key) == before


def test_rendering_resumes_from_the_replay_after_the_log_expired(make_drawing: Any) -> None:
    drawing = new_drawing(make_drawing, "m_resume")
    pipeline.prepare(drawing.id)
    drawing.refresh_from_db()
    sha = drawing.sha256
    # A crash after the replay was stored, before the images: and the log has expired since.
    Drawing.objects.filter(pk=drawing.pk).update(sha256="", image_key="", thumb_key="")
    bucket().delete_object(Bucket="round-logs", Key=drawing.stroke_log_key)

    assert pipeline.prepare(drawing.id) == "prepared"
    drawing.refresh_from_db()
    assert drawing.sha256 == sha


def test_a_missing_stroke_log_fails_the_drawing(make_drawing: Any) -> None:
    drawing = make_drawing(match_id="m_gone", stroke_log_key="m_gone/1.binpb.gz")

    assert pipeline.prepare(drawing.id) == "failed"

    drawing.refresh_from_db()
    assert drawing.status == Status.FAILED
    assert "StrokeLogMissing" in drawing.decision_reason


def test_a_log_for_another_round_fails_the_drawing(make_drawing: Any) -> None:
    log_object = upload_log("m_other", 3)
    drawing = make_drawing(match_id="m_mine", round_no=1, stroke_log_key=log_object)

    assert pipeline.prepare(drawing.id) == "failed"
    drawing.refresh_from_db()
    assert drawing.status == Status.FAILED
    assert "m_other round 3" in drawing.decision_reason


def test_a_corrupt_log_fails_the_drawing(make_drawing: Any) -> None:
    bucket().put_object(Bucket="round-logs", Key="m_bad/1.binpb.gz", Body=b"garbage")
    drawing = make_drawing(match_id="m_bad", stroke_log_key="m_bad/1.binpb.gz")

    assert pipeline.prepare(drawing.id) == "failed"


def test_a_store_outage_is_raised_for_a_retry(make_drawing: Any, settings: Any) -> None:
    drawing = new_drawing(make_drawing, "m_outage")
    settings.S3_ENDPOINT = "http://127.0.0.1:9"  # nothing listens there
    storage.reset_clients_for_tests()

    with pytest.raises(storage.TRANSIENT_ERRORS):
        pipeline.prepare(drawing.id)
    drawing.refresh_from_db()
    assert drawing.status == Status.PROCESSING


def test_decided_drawings_are_skipped(make_drawing: Any) -> None:
    drawing = make_drawing(match_id="m_done")
    Drawing.objects.filter(pk=drawing.pk).update(status=Status.FAILED)

    assert pipeline.prepare(drawing.id) == "skipped"


def test_the_sweeper_requeues_only_stale_unprepared_drawings(
    make_drawing: Any, monkeypatch: Any
) -> None:
    from datetime import timedelta

    from django.utils import timezone

    from drawings import tasks

    old = timezone.now() - timedelta(minutes=11)
    stale = make_drawing(match_id="m_stale")
    fresh = make_drawing(match_id="m_fresh")
    prepared = make_drawing(match_id="m_prepared", sha256="ab" * 32)
    failed = make_drawing(match_id="m_failed")
    Drawing.objects.filter(pk__in=[stale.pk, prepared.pk, failed.pk]).update(updated_at=old)
    Drawing.objects.filter(pk=failed.pk).update(status=Status.FAILED)

    sent: list[str] = []
    monkeypatch.setattr(tasks.prepare_drawing, "delay", lambda drawing_id: sent.append(drawing_id))

    assert requeue_stale_drawings() == 1
    assert sent == [str(stale.id)]
    # Touched: the next sweep leaves it alone for another 10 minutes.
    assert requeue_stale_drawings() == 0
    assert fresh.id not in sent
