"""The Celery app: the drawing pipeline's workers, with Redis as the broker (design doc 4.5).

Run a worker with `celery -A config worker`. Every CELERY_* Django setting configures it
(config/settings.py): tasks are acknowledged after they finish and redelivered if their worker
dies, so they run at least once and must be idempotent.
"""

import logging
import os
import time
from collections.abc import Callable
from typing import Any

from celery import Celery, signals
from celery.utils.log import current_process_index  # type: ignore[attr-defined]

os.environ.setdefault("DJANGO_SETTINGS_MODULE", "config.settings")

app = Celery("assets")
app.config_from_object("django.conf:settings", namespace="CELERY")
app.autodiscover_tasks()

log = logging.getLogger(__name__)
_shutdown_telemetry: Callable[[], None] | None = None


@signals.setup_logging.connect
def keep_django_logging(**kwargs: Any) -> None:
    """Connecting this stops Celery from installing its own log handlers: the worker keeps the
    JSON logging config from settings.LOGGING, which Django applies when Celery starts it."""


@signals.before_task_publish.connect
def stamp_sent_at(headers: dict[str, Any] | None = None, **kwargs: Any) -> None:
    """Records when a task was sent, so assets.jobs.oldest_age can tell how long the oldest
    waiting message has waited (config/jobs.py). Celery's own message has no send time."""
    if headers is not None:
        headers.setdefault("sent_at", time.time())


@signals.worker_process_init.connect
def start_pool_process_telemetry(**kwargs: Any) -> None:
    # Per pool process, after the fork: export threads started before it wouldn't survive.
    global _shutdown_telemetry
    from config import jobs, telemetry

    # The pool index (0, 1, ...) is reused when a pool process is replaced, so ids stay stable.
    _shutdown_telemetry = telemetry.setup_worker_telemetry(
        process=f"p{current_process_index(base=0)}"
    )
    # One process per worker reports the queue gauge, so the series isn't written twice.
    if telemetry.enabled() and current_process_index(base=0) == 0:
        jobs.register_queue_age_gauge(app.conf.broker_url, [app.conf.task_default_queue])


@signals.beat_init.connect
def start_beat_telemetry(**kwargs: Any) -> None:
    """Beat is a single process: it sends the sweeper's task, with the trace context, and logs
    over OTLP like the rest of the service."""
    import atexit

    from config import telemetry

    atexit.register(telemetry.setup_worker_telemetry())


@signals.worker_process_shutdown.connect
def stop_pool_process_telemetry(**kwargs: Any) -> None:
    # Flushes the last spans, metrics and logs when the pool process exits.
    if _shutdown_telemetry is not None:
        _shutdown_telemetry()
