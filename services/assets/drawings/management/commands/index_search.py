"""`python manage.py index_search`: the assets-indexer process (drawings/indexer.py, ADR 0023)."""

import signal
import threading
from typing import Any

from django.conf import settings
from django.core.management.base import BaseCommand

from config import telemetry
from drawings import indexer


class Command(BaseCommand):
    help = "Keep the search projection and index current from drawing, listing and sale events."

    def handle(self, *args: Any, **options: Any) -> None:
        shutdown_telemetry = telemetry.setup_worker_telemetry()
        stop = threading.Event()
        # SIGTERM (docker stop): finish the current record, commit it, leave the group.
        signal.signal(signal.SIGTERM, lambda *_: stop.set())
        signal.signal(signal.SIGINT, lambda *_: stop.set())
        try:
            indexer.run(settings.KAFKA_BROKERS, stop, client_id=settings.SERVICE_INSTANCE_ID)
        finally:
            shutdown_telemetry()
