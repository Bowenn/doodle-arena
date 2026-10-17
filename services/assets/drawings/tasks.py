"""Celery tasks. The drawing pipeline arrives in task 3.2; every task must be idempotent, because
Celery delivers each one at least once (config/settings.py)."""

import logging

from config.celery import app

log = logging.getLogger(__name__)


@app.task(name="assets.ping")
def ping() -> str:
    """Proves a worker takes tasks from the broker: `celery -A config call assets.ping`, then
    look for "pong" in the worker's logs (or its span in Tempo)."""
    log.info("pong")
    return "pong"
