"""JSON logging in the format from docs/observability.md, section 2.

Every record goes to stdout as one JSON line. With telemetry enabled, records also go to the
OpenTelemetry logger provider, which exports them over OTLP (and Grafana links them to traces).
"""

import json
import logging
from datetime import UTC, datetime
from typing import Any

from opentelemetry import trace

# Attributes every LogRecord has; anything else was passed through `extra=` and is logged.
# color_message is uvicorn's ANSI-colored copy of msg.
_STANDARD_ATTRS = set(vars(logging.makeLogRecord({}))) | {
    "message",
    "asctime",
    "taskName",
    "color_message",
}
_LEVELS = {"WARNING": "warn", "CRITICAL": "error"}


def _trace_ids() -> dict[str, str]:
    """The current span's ids, or nothing outside a span. Handlers run synchronously in the
    logging call, so this is the span that was active when the record was logged."""
    span = trace.get_current_span().get_span_context()
    if not span.is_valid:
        return {}
    return {
        "trace_id": trace.format_trace_id(span.trace_id),
        "span_id": trace.format_span_id(span.span_id),
    }


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
            **_trace_ids(),
            "msg": record.getMessage(),
            "logger": record.name,
        }
        for key, value in vars(record).items():
            if key not in _STANDARD_ATTRS:
                entry[key] = value
        if record.exc_info:
            entry["exc"] = self.formatException(record.exc_info)
        return json.dumps(entry, default=str)


class PrimitiveExtras(logging.Filter):
    """OTLP attributes must be primitive values, so other values passed through `extra=` are
    logged as strings (Django's django.request logger passes the request object). The JSON
    formatter does the same with default=str."""

    def filter(self, record: logging.LogRecord) -> bool:
        for key, value in list(vars(record).items()):
            if key not in _STANDARD_ATTRS and not isinstance(value, str | bool | int | float):
                setattr(record, key, str(value))
        return True


def logging_config(service: str, version: str, level: str, otlp: bool = False) -> dict[str, Any]:
    """A dictConfig that sends every logger, including uvicorn's, through JsonFormatter.
    With otlp, records also go to the global OpenTelemetry logger provider."""
    handlers: dict[str, Any] = {
        "stdout": {
            "class": "logging.StreamHandler",
            "formatter": "json",
            "stream": "ext://sys.stdout",
        },
    }
    if otlp:
        handlers["otlp"] = {
            "class": "opentelemetry.instrumentation.logging.handler.LoggingHandler",
            "filters": ["primitive_extras"],
        }
    return {
        "version": 1,
        "disable_existing_loggers": False,
        "formatters": {
            "json": {"()": JsonFormatter, "service": service, "version": version},
        },
        "filters": {"primitive_extras": {"()": PrimitiveExtras}},
        "handlers": handlers,
        "root": {"handlers": list(handlers), "level": level.upper()},
        "loggers": {
            "uvicorn": {"handlers": [], "propagate": True},
            "uvicorn.error": {"handlers": [], "propagate": True},
            "uvicorn.access": {"handlers": [], "propagate": True},
            # The Elasticsearch client logs every request at INFO; the search span and the
            # indexer's own log line already say what happened.
            "elastic_transport": {"level": "WARNING"},
        },
    }
