"""`python manage.py backfill_decision_events`: writes DrawingApproved or DrawingRejected to the
outbox for decided drawings that never got one (decided before task 3.4 added the outbox). The
relay then publishes them. Safe to run again: a drawing that has an outbox event is skipped.
Run it soon after a deploy; published events are purged after 7 days, and a drawing whose event
was purged would get a second one (harmless, since consumers dedupe on drawing_id).
"""

from typing import Any

from django.core.management.base import BaseCommand
from django.db import transaction

from drawings import events
from drawings.models import Drawing, OutboxEvent, Status


class Command(BaseCommand):
    help = "Write the missing decision events of drawings decided before the outbox existed."

    def add_arguments(self, parser: Any) -> None:
        parser.add_argument("--dry-run", action="store_true", help="Only count them.")

    def handle(self, *args: Any, **options: Any) -> None:
        with_event = OutboxEvent.objects.filter(topic=events.TOPIC).values("key")
        missing = (
            Drawing.objects.filter(status__in=[Status.APPROVED, Status.REJECTED])
            .exclude(id__in=[row["key"] for row in with_event])
            .order_by("id")
        )
        count = 0
        for drawing in missing.iterator():
            if not options["dry_run"]:
                with transaction.atomic():
                    events.enqueue_decision(drawing)
            count += 1
        verb = "would write" if options["dry_run"] else "wrote"
        self.stdout.write(f"{verb} {count} decision events")
