"""The drawing pipeline (design doc 4.5): from a new drawing row to a stored replay and images.

Every step records its result on the row and is skipped when the row already has it, so a task
Celery delivers twice, or the sweeper re-queues, does each step at most once:

1. fetch: the gzipped stroke log from round-logs (the claim check), stored as the replay
   document in MongoDB (replay_id). From then on the replay is the source of truth: the bucket
   copy expires after 7 days.
2. render: the PNG and thumbnail, from the replay.
3. upload: both images under content-addressed keys, so identical drawings share one object
   (sha256, image_key, thumb_key).

Moderation follows in task 3.3. A permanent problem (the stroke log is gone or unreadable)
marks the drawing failed; anything else is raised for Celery to retry.
"""

import hashlib
import logging
import time
import uuid
from collections.abc import Iterator
from contextlib import contextmanager

from django.utils import timezone
from opentelemetry import metrics, trace

from drawings import moderation, storage
from drawings.models import Drawing, Status
from drawings.render import render
from drawings.strokes import (
    CorruptStrokeLog,
    Replay,
    decode_stroke_log,
    replay_document,
    replay_from_document,
    replay_from_log,
)

log = logging.getLogger(__name__)
tracer = trace.get_tracer(__name__)
_meter = metrics.get_meter(__name__)
step_duration = _meter.create_histogram(
    "assets.pipeline.duration", unit="ms", description="Time per pipeline step"
)
results = _meter.create_counter(
    "assets.pipeline.results",
    description="Pipeline runs by result: prepared, skipped (not processing), failed",
)


@contextmanager
def step(name: str, drawing: Drawing) -> Iterator[None]:
    start = time.monotonic()
    with tracer.start_as_current_span(
        f"drawing.{name}", attributes={"drawing.id": str(drawing.id), "pipeline.step": name}
    ):
        try:
            yield
        finally:
            step_duration.record((time.monotonic() - start) * 1000, {"step": name})


def image_keys(sha256: str) -> tuple[str, str]:
    """Content-addressed keys. The thumbnail is made from the full image, so it shares its hash."""
    return f"sha256/{sha256}.png", f"sha256/{sha256}.thumb.png"


def prepare(drawing_id: uuid.UUID) -> str:
    """Runs the steps a drawing still needs. Returns the result it counted."""
    drawing = Drawing.objects.filter(pk=drawing_id).first()
    if drawing is None or drawing.status != Status.PROCESSING:
        results.add(1, {"result": "skipped"})
        return "skipped"
    fields = {
        "drawing_id": str(drawing.id),
        "match_id": drawing.match_id,
        "room_id": drawing.room_id,
    }

    with tracer.start_as_current_span(
        "drawing.prepare", attributes={"drawing.id": str(drawing.id)}
    ):
        try:
            replay = _fetch(drawing) if not drawing.replay_id else None
            if not drawing.sha256:
                if replay is None:
                    # Lost from MongoDB (a restore from an old backup?): fetch it again while
                    # the bucket still has the log.
                    replay = _load_replay(drawing) or _fetch(drawing)
                _render_and_upload(drawing, replay)
        except (storage.StrokeLogMissing, CorruptStrokeLog) as e:
            reason = f"{type(e).__name__}: {e}"
            moderation.fail(drawing.id, reason)
            results.add(1, {"result": "failed"})
            return "failed"

    log.info("drawing prepared", extra={**fields, "sha256": drawing.sha256})
    results.add(1, {"result": "prepared"})
    return "prepared"


def _fetch(drawing: Drawing) -> Replay:
    with step("fetch", drawing):
        stroke_log = decode_stroke_log(storage.get_stroke_log(drawing.stroke_log_key))
        if stroke_log.match_id != drawing.match_id or stroke_log.round_no != drawing.round_no:
            raise CorruptStrokeLog(
                f"the log is for {stroke_log.match_id} round {stroke_log.round_no}"
            )
        replay = replay_from_log(stroke_log)
        visible = len(replay.visible())
        if visible != drawing.stroke_count:
            # The event's count is a sanity check, not a reason to fail: the log is the record.
            log.warning(
                "stroke count differs from the event",
                extra={
                    "drawing_id": str(drawing.id),
                    "expected": drawing.stroke_count,
                    "got": visible,
                },
            )
        storage.save_replay(replay_document(str(drawing.id), replay, stroke_log))
        _record(drawing, replay_id=str(drawing.id))
    return replay


def _load_replay(drawing: Drawing) -> Replay | None:
    doc = storage.get_replay(drawing.replay_id)
    return replay_from_document(doc) if doc is not None else None


def _render_and_upload(drawing: Drawing, replay: Replay) -> None:
    with step("render", drawing):
        rendered = render(replay)
        sha256 = hashlib.sha256(rendered.png).hexdigest()
    image_key, thumb_key = image_keys(sha256)
    with step("upload", drawing):
        storage.put_if_absent(image_key, rendered.png)
        storage.put_if_absent(thumb_key, rendered.thumb)
    _record(drawing, sha256=sha256, image_key=image_key, thumb_key=thumb_key)


def _record(drawing: Drawing, **values: str) -> None:
    """Saves a step's result on the row (and on the object), only while the drawing is still
    processing. updated_at tells the sweeper the drawing is moving."""
    Drawing.objects.filter(pk=drawing.pk, status=Status.PROCESSING).update(
        **values, updated_at=timezone.now()
    )
    for name, value in values.items():
        setattr(drawing, name, value)
