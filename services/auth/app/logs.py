"""JSON logging in the format from docs/observability.md, section 2.

trace_id and span_id are added once OpenTelemetry is wired in (task 1.5).
"""

import json
import logging
from datetime import UTC, datetime
from typing import Any

# Attributes every LogRecord has; anything else was passed through `extra=` and is logged.
# color_message is uvicorn's ANSI-colored copy of msg.
_STANDARD_ATTRS = set(vars(logging.makeLogRecord({}))) | {
    "message",
    "asctime",
    "taskName",
    "color_message",
}
_LEVELS = {"WARNING": "warn", "CRITICAL": "error"}


class JsonFormatter(logging.Formatter):
    def __init__(self, service: str, version: str) -> None:
        super().__init__()
        self.service = service
        self.version = version

    def format(self, record: logging.LogRecord) -> str:
        timestamp = datetime.fromtimestamp(record.created, UTC).isoformat(timespec="milliseconds")
        entry: dict[str, Any] = {
            "timestamp": timestamp.replace("+00:00", "Z"),
            "level": _LEVELS.get(record.levelname, record.levelname.lower()),
            "service": self.service,
            "version": self.version,
            "msg": record.getMessage(),
            "logger": record.name,
        }
        for key, value in vars(record).items():
            if key not in _STANDARD_ATTRS:
                entry[key] = value
        if record.exc_info:
            entry["exc"] = self.formatException(record.exc_info)
        return json.dumps(entry, default=str)


def logging_config(service: str, version: str, level: str) -> dict[str, Any]:
    """A dictConfig that sends every logger, including uvicorn's, through JsonFormatter."""
    return {
        "version": 1,
        "disable_existing_loggers": False,
        "formatters": {
            "json": {"()": JsonFormatter, "service": service, "version": version},
        },
        "handlers": {
            "stdout": {
                "class": "logging.StreamHandler",
                "formatter": "json",
                "stream": "ext://sys.stdout",
            },
        },
        "root": {"handlers": ["stdout"], "level": level.upper()},
        "loggers": {
            "uvicorn": {"handlers": [], "propagate": True},
            "uvicorn.error": {"handlers": [], "propagate": True},
            "uvicorn.access": {"handlers": [], "propagate": True},
        },
    }
