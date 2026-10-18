"""`python manage.py relay_outbox`: the assets-relay process (drawings/relay.py)."""

import signal
import threading
from typing import Any

from django.conf import settings
from django.core.management.base import BaseCommand

from config import telemetry
from drawings import relay


class Command(BaseCommand):
    help = "Publish the outbox's events (DrawingApproved, DrawingRejected) to Kafka."

    def handle(self, *args: Any, **options: Any) -> None:
        shutdown_telemetry = telemetry.setup_worker_telemetry()
        stop = threading.Event()
        # SIGTERM (docker stop): finish the current pass, flush the producer, exit.
        signal.signal(signal.SIGTERM, lambda *_: stop.set())
        signal.signal(signal.SIGINT, lambda *_: stop.set())
        try:
            relay.run(settings.KAFKA_BROKERS, stop, client_id=settings.SERVICE_INSTANCE_ID)
        finally:
            shutdown_telemetry()
