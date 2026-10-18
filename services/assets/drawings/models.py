"""A drawing is one finished round's canvas, from RoundFinished to an approved or rejected item
(design doc 4.5 and 7.6).

The status is the only state machine: processing -> approved | rejected | review | failed, and
review -> approved | rejected. Progress inside "processing" is the pipeline's own columns
(replay_id, sha256, ...), each filled once, so a redelivered task skips the steps already done.
Status changes go through drawings.moderation, never through save().
"""

import uuid

from django.conf import settings
from django.contrib.postgres.fields import ArrayField
from django.contrib.postgres.indexes import GinIndex
from django.db import models
from django.db.models import Q


class Status(models.TextChoices):
    # Created from RoundFinished; the pipeline is fetching, rendering or moderating it.
    PROCESSING = "processing"
    # The AI service couldn't decide after its last attempt; waits for a moderator.
    REVIEW = "review"
    APPROVED = "approved"
    REJECTED = "rejected"
    # The pipeline can never build it (the stroke log expired or is corrupt). Never minted;
    # decision_reason says why.
    FAILED = "failed"


class DecidedBy(models.TextChoices):
    # The same sources as doodle.events.v1.DecisionSource.
    AI = "ai"
    MODERATOR = "moderator"


class Drawing(models.Model):
    Status = Status
    DecidedBy = DecidedBy

    # UUIDv7: random, so ids can't be guessed, and time-ordered, so new rows land at the end of
    # the primary key index and the gallery can page by id.
    id = models.UUIDField(primary_key=True, default=uuid.uuid7, editable=False)

    # From RoundFinished. (match_id, round_no) is the natural key: a replayed event finds the
    # existing row instead of creating a second drawing.
    match_id = models.CharField(max_length=64)
    round_no = models.PositiveIntegerField()
    room_id = models.CharField(max_length=64)
    word = models.CharField(max_length=64)
    # Everyone who drew it, in drawing order; the first one is the initial owner.
    author_ids = ArrayField(models.CharField(max_length=64))
    initial_owner_id = models.CharField(max_length=64)
    stroke_log_key = models.CharField(max_length=256)
    stroke_count = models.PositiveIntegerField()
    canvas_width = models.PositiveIntegerField()
    canvas_height = models.PositiveIntegerField()
    ended_at = models.DateTimeField()
    # The RoundFinished event_id that created the row (the outbox keeps it fixed, ADR 0009).
    source_event_id = models.CharField(max_length=64)

    # Filled by the pipeline (task 3.2), once each.
    replay_id = models.CharField(max_length=64, blank=True, default="")
    sha256 = models.CharField(max_length=64, blank=True, default="")
    image_key = models.CharField(max_length=256, blank=True, default="")
    thumb_key = models.CharField(max_length=256, blank=True, default="")

    status = models.CharField(max_length=16, choices=Status, default=Status.PROCESSING)
    # Moderation calls made so far (task 3.3), for the retry budget and the admin.
    moderation_attempts = models.PositiveSmallIntegerField(default=0)
    # Set while one task is calling the AI service about this drawing, so a duplicate task
    # (a redelivery, the sweeper) doesn't call too and spend the budget twice. It expires on its
    # own if that task dies.
    moderation_claimed_until = models.DateTimeField(null=True, blank=True)
    decided_by = models.CharField(max_length=16, choices=DecidedBy, blank=True, default="")
    # The moderator who decided, when decided_by is "moderator". Users are never deleted.
    moderator = models.ForeignKey(
        settings.AUTH_USER_MODEL, null=True, blank=True, on_delete=models.PROTECT, related_name="+"
    )
    decided_at = models.DateTimeField(null=True, blank=True)
    decision_reason = models.CharField(max_length=500, blank=True, default="")

    created_at = models.DateTimeField(auto_now_add=True)
    updated_at = models.DateTimeField(auto_now=True)

    class Meta:
        constraints = [
            models.UniqueConstraint(fields=["match_id", "round_no"], name="drawing_unique_round"),
            models.CheckConstraint(
                condition=Q(status__in=[s.value for s in Status]), name="drawing_status_valid"
            ),
            # A decided drawing says who decided and when; an undecided one says nothing.
            models.CheckConstraint(
                condition=(
                    Q(
                        status__in=[Status.PROCESSING, Status.REVIEW, Status.FAILED],
                        decided_by="",
                        decided_at__isnull=True,
                        moderator__isnull=True,
                    )
                    | Q(
                        status__in=[Status.APPROVED, Status.REJECTED],
                        decided_by__in=[d.value for d in DecidedBy],
                        decided_at__isnull=False,
                    )
                ),
                name="drawing_decision_consistent",
            ),
            models.CheckConstraint(
                condition=~Q(decided_by=DecidedBy.MODERATOR) | Q(moderator__isnull=False),
                name="drawing_moderator_named",
            ),
        ]
        indexes = [
            # GET /api/assets/drawings?author_id=: author_ids @> ARRAY[...].
            GinIndex(fields=["author_ids"], name="drawing_author_ids"),
            # The gallery: approved drawings, newest first (keyset on the UUIDv7 id).
            models.Index(fields=["-id"], condition=Q(status="approved"), name="drawing_gallery"),
            # The moderation queue: drawings in review, oldest first. Partial, so it stays as
            # small as the queue.
            models.Index(
                fields=["created_at"],
                condition=Q(status="review"),
                name="drawing_review_queue",
            ),
        ]
        permissions = [("moderate_drawing", "Can approve or reject drawings in review")]

    def __str__(self) -> str:
        return f"{self.match_id} round {self.round_no} ({self.word})"


class ReviewQueue(Drawing):
    """The Django admin's moderation queue: the drawings in review (drawings/admin.py)."""

    class Meta:
        proxy = True
        verbose_name = "drawing in review"
        verbose_name_plural = "moderation queue"


class OutboxEvent(models.Model):
    """A Kafka event waiting to be published (the transactional outbox). It is written in the
    same transaction as the change it announces (drawings.moderation.decide), and the relay
    (drawings/relay.py) publishes it and stamps published_at after Kafka confirms. A crash in
    between publishes it again with the same id, so consumers dedupe on event_id."""

    # Also the event's EventMeta.event_id. UUIDv7, so id order is (roughly) write order.
    id = models.UUIDField(primary_key=True, default=uuid.uuid7, editable=False)
    topic = models.CharField(max_length=64)
    key = models.CharField(max_length=64)
    event_type = models.CharField(max_length=32)
    # The serialized wrapper message (doodle.events.v1.DrawingEvent).
    payload = models.BinaryField()
    # The trace of the change that wrote it, continued by the relay's publish span.
    traceparent = models.CharField(max_length=64, blank=True, default="")
    created_at = models.DateTimeField(auto_now_add=True)
    published_at = models.DateTimeField(null=True, blank=True)

    class Meta:
        indexes = [
            # What the relay reads: unpublished rows in id order. Partial, so it stays small.
            models.Index(
                fields=["id"], condition=Q(published_at__isnull=True), name="outbox_unpublished"
            ),
        ]
