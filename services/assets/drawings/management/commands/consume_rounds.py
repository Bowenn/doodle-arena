"""`python manage.py consume_rounds`: the assets-consumer process (drawings/consumer.py)."""

import signal
import threading
from typing import Any

from django.conf import settings
from django.core.management.base import BaseCommand

from config import telemetry
from drawings import consumer


class Command(BaseCommand):
    help = "Consume RoundFinished from game.rounds.v1 and start a drawing for each round."

    def handle(self, *args: Any, **options: Any) -> None:
        shutdown_telemetry = telemetry.setup_worker_telemetry()
        stop = threading.Event()
        # SIGTERM (docker stop): finish the current record, commit it, leave the group.
        signal.signal(signal.SIGTERM, lambda *_: stop.set())
        signal.signal(signal.SIGINT, lambda *_: stop.set())
        try:
            consumer.run(settings.KAFKA_BROKERS, stop, client_id=settings.SERVICE_INSTANCE_ID)
        finally:
            shutdown_telemetry()
