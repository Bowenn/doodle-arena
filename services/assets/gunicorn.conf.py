"""Gunicorn settings. Gunicorn's own logs use the same JSON format as Django's."""

import os
from typing import Any

from config import SERVICE_NAME, telemetry
from config.logs import logging_config

bind = "0.0.0.0:8080"
workers = int(os.environ.get("WEB_CONCURRENCY", "2"))
# On SIGTERM, workers get this long to finish in-flight requests.
graceful_timeout = 10
accesslog = None
# The master never installs a logger provider, so its OTLP handler drops records; each worker
# installs one in post_fork, and Django then applies the same config again (settings.LOGGING).
logconfig_dict = logging_config(
    SERVICE_NAME,
    os.environ.get("SERVICE_VERSION", "dev"),
    os.environ.get("LOG_LEVEL", "info"),
    telemetry.enabled(),
)
# Gunicorn attaches plain-text handlers to its own loggers; clear them so records reach the
# JSON handler on the root logger.
for _name in ("gunicorn.error", "gunicorn.access"):
    logconfig_dict["loggers"][_name] = {"handlers": [], "propagate": True}


def post_fork(server: Any, worker: Any) -> None:
    # Per worker: export threads started in the master would not survive the fork. Runs before
    # the worker loads the WSGI app, so the Django middleware is in place for every request.
    worker.shutdown_telemetry = telemetry.setup_telemetry()


def worker_exit(server: Any, worker: Any) -> None:
    # Flushes the last spans, metrics and logs when the worker stops (SIGTERM, max_requests).
    shutdown = getattr(worker, "shutdown_telemetry", None)  # missing if post_fork failed
    if shutdown is not None:
        shutdown()
