"""Celery tasks. Celery delivers each one at least once (config/settings.py), so every task is
idempotent: the pipeline skips the steps a drawing already has."""

import logging
import random
import uuid
from datetime import timedelta

from celery import Task
from django.db import transaction
from django.utils import timezone
from opentelemetry import metrics

from config.celery import app
from drawings import ai_client, pipeline, storage
from drawings.models import Drawing, Status

log = logging.getLogger(__name__)
requeued = metrics.get_meter(__name__).create_counter(
    "assets.pipeline.requeued",
    description="Stale drawings the sweeper sent through the pipeline again",
)

# A drawing still processing this long after its last change has lost its task: the broker lost
# the message, or the retries ran out during an outage.
STALE_AFTER = timedelta(minutes=10)
SWEEP_LIMIT = 100


@app.task(name="assets.ping")
def ping() -> str:
    """Proves a worker takes tasks from the broker: `celery -A config call assets.ping`, then
    look for "pong" in the worker's logs (or its span in Tempo)."""
    log.info("pong")
    return "pong"


@app.task(
    name="assets.prepare_drawing",
    autoretry_for=storage.TRANSIENT_ERRORS,
    # Exponential backoff with full jitter: about 2, 4, 8, 16, 32 s, each drawn at random below
    # that, so workers retrying after an outage don't all hit the store at once.
    retry_backoff=2,
    retry_backoff_max=60,
    retry_jitter=True,
    max_retries=5,
)
def prepare_drawing(drawing_id: str) -> str:
    result = pipeline.prepare(uuid.UUID(drawing_id))
    if result == "prepared":
        # Also when every step was already done: a redelivered or re-queued drawing still
        # needs its decision, and moderate_drawing skips one that has it.
        moderate_drawing.delay(drawing_id)
    return result


def _full_jitter(retries: int, base: float = 1.0, cap: float = 30.0) -> float:
    """Exponential backoff with full jitter: a random wait below base * 2^retries (about 1, 2,
    4 s), so drawings that failed together don't all retry together."""
    return random.uniform(0, min(cap, base * 2**retries))


@app.task(
    name="assets.moderate_drawing",
    bind=True,
    # The thumbnail comes from our own bucket; an outage there retries like prepare_drawing.
    autoretry_for=storage.TRANSIENT_ERRORS,
    retry_backoff=2,
    retry_backoff_max=60,
    retry_jitter=True,
    # A ceiling for both kinds of retry. The AI's own budget is pipeline.MAX_MODERATION_ATTEMPTS,
    # counted on the row, after which the drawing goes to the review queue.
    max_retries=10,
)
def moderate_drawing(self: Task, drawing_id: str) -> str:  # type: ignore[type-arg]
    try:
        return pipeline.moderate(uuid.UUID(drawing_id))
    except ai_client.ModerationUnavailable as e:
        raise self.retry(exc=e, countdown=_full_jitter(self.request.retries)) from e


@app.task(name="assets.requeue_stale_drawings")
def requeue_stale_drawings() -> int:
    """Run every minute by Celery beat. Sends stale drawings through the pipeline again and
    touches them, so the next sweep leaves them alone for another STALE_AFTER.

    The rows are locked while they're touched (SKIP LOCKED, so two sweepers never take the same
    drawing), and the tasks are sent only after that commits."""
    now = timezone.now()
    with transaction.atomic():
        stale = list(
            Drawing.objects.select_for_update(skip_locked=True)
            # Still processing: not prepared, or prepared and not moderated. prepare_drawing
            # skips what's done and sends the moderation task.
            .filter(status=Status.PROCESSING, updated_at__lt=now - STALE_AFTER)
            .order_by("updated_at")
            .values_list("id", "match_id")[:SWEEP_LIMIT]
        )
        Drawing.objects.filter(pk__in=[drawing_id for drawing_id, _ in stale]).update(
            updated_at=now
        )
    for drawing_id, match_id in stale:
        prepare_drawing.delay(str(drawing_id))
        log.warning(
            "re-queued a stale drawing", extra={"drawing_id": str(drawing_id), "match_id": match_id}
        )
    if stale:
        requeued.add(len(stale))
    return len(stale)


@app.task(name="assets.clear_expired_sessions")
def clear_expired_sessions() -> None:
    """Run hourly by Celery beat: deletes expired admin sessions, like `manage.py clearsessions`."""
    from django.contrib.sessions.backends.db import SessionStore

    SessionStore.clear_expired()
