"""Status changes of a drawing. Each one is a single conditional UPDATE: it changes the row only
if the drawing is still in a state the change is allowed from, and reports whether it did.

Under Postgres's READ COMMITTED, a second UPDATE of the same row waits for the first to commit
and then re-checks its WHERE clause against the new row, so of two concurrent decisions (two
moderators, or a Celery task delivered twice) exactly one changes the row and the other is a
no-op. There's no read-then-write window, and no lock is held beyond the statement.
"""

import logging
import uuid

from django.contrib.auth.models import AbstractBaseUser
from django.db import transaction
from django.utils import timezone
from opentelemetry import trace

from drawings import events
from drawings.models import DecidedBy, Drawing, Status

log = logging.getLogger(__name__)
tracer = trace.get_tracer(__name__)

# Who may decide from which status. The AI decides while the pipeline runs; once a drawing waits
# in the review queue, only a moderator does, so a late AI answer can't overrule the queue.
_DECIDE_FROM = {
    DecidedBy.AI: [Status.PROCESSING],
    DecidedBy.MODERATOR: [Status.REVIEW],
}


def send_to_review(drawing_id: uuid.UUID, reason: str = "") -> bool:
    """Moves a drawing the AI couldn't decide into the review queue. False if it was already
    decided or queued."""
    changed = Drawing.objects.filter(pk=drawing_id, status=Status.PROCESSING).update(
        status=Status.REVIEW, decision_reason=reason, updated_at=timezone.now()
    )
    if changed:
        log.info("drawing sent to review", extra={"drawing_id": str(drawing_id)})
    return changed == 1


def fail(drawing_id: uuid.UUID, reason: str) -> bool:
    """Marks a drawing the pipeline can never build (a permanent error, not an outage). False if
    it had already left processing."""
    changed = Drawing.objects.filter(pk=drawing_id, status=Status.PROCESSING).update(
        status=Status.FAILED, decision_reason=reason[:500], updated_at=timezone.now()
    )
    if changed:
        log.error("drawing failed", extra={"drawing_id": str(drawing_id), "reason": reason})
    return changed == 1


def decide(
    drawing_id: uuid.UUID,
    *,
    approve: bool,
    by: DecidedBy,
    moderator: AbstractBaseUser | None = None,
    reason: str = "",
) -> bool:
    """Approves or rejects a drawing. Returns True if this call made the decision, False if the
    drawing was already decided (or isn't in a state `by` may decide from)."""
    if (by == DecidedBy.MODERATOR) != (moderator is not None):
        raise ValueError("a moderator decision names the moderator, and only it does")
    status = Status.APPROVED if approve else Status.REJECTED
    with tracer.start_as_current_span(
        "assets.drawing.decide", attributes={"drawing.id": str(drawing_id), "decision.by": by.value}
    ) as span:
        now = timezone.now()
        allowed = Drawing.objects.filter(pk=drawing_id, status__in=_DECIDE_FROM[by])
        if approve:
            allowed = allowed.exclude(sha256="")  # nothing to mint without the image
        # The decision and its event (DrawingApproved or DrawingRejected) commit together or not
        # at all: the outbox row is the only way the decision leaves this service.
        with transaction.atomic():
            changed = allowed.update(
                status=status,
                decided_by=by,
                moderator=moderator,
                decided_at=now,
                decision_reason=reason,
                updated_at=now,
            )
            if changed:
                events.enqueue_decision(Drawing.objects.get(pk=drawing_id))
        span.set_attribute("decision.applied", changed == 1)
        extra = {"drawing_id": str(drawing_id), "status": status.value, "decided_by": by.value}
        if moderator is not None:
            extra["user_id"] = moderator.get_username()  # the auth user id (sso.views)
        if changed:
            log.info("drawing decided", extra=extra)
        else:
            log.info("drawing already decided; decision ignored", extra=extra)
        return changed == 1
