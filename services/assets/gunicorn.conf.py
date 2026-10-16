"""Gunicorn settings. Gunicorn's own logs use the same JSON format as Django's."""

import os

from config.logs import logging_config

bind = "0.0.0.0:8080"
workers = int(os.environ.get("WEB_CONCURRENCY", "2"))
# On SIGTERM, workers get this long to finish in-flight requests.
graceful_timeout = 10
accesslog = None
logconfig_dict = logging_config(
    "assets", os.environ.get("SERVICE_VERSION", "dev"), os.environ.get("LOG_LEVEL", "info")
)
# Gunicorn attaches plain-text handlers to its own loggers; clear them so records reach the
# JSON handler on the root logger.
for _name in ("gunicorn.error", "gunicorn.access"):
    logconfig_dict["loggers"][_name] = {"handlers": [], "propagate": True}
