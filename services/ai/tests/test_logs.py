import json
import logging
from datetime import datetime

from opentelemetry.sdk.trace import TracerProvider

from app.logs import JsonFormatter, PrimitiveExtras, logging_config


def _format(level: int, msg: str, **extra: object) -> dict[str, object]:
    record = logging.makeLogRecord(
        {"name": "test", "levelno": level, "levelname": logging.getLevelName(level), "msg": msg}
    )
    record.__dict__.update(extra)
    result: dict[str, object] = json.loads(JsonFormatter("ai", "1.2.3").format(record))
    return result


def test_required_fields() -> None:
    entry = _format(logging.INFO, "user logged in", user_id="u_1")

    assert entry["level"] == "info"
    assert entry["service"] == "ai"
    assert entry["version"] == "1.2.3"
    assert entry["msg"] == "user logged in"
    assert entry["user_id"] == "u_1"
    timestamp = str(entry["timestamp"])
    assert timestamp.endswith("Z")
    datetime.fromisoformat(timestamp)


def test_level_names_match_the_spec() -> None:
    assert _format(logging.WARNING, "x")["level"] == "warn"
    assert _format(logging.CRITICAL, "x")["level"] == "error"


def test_trace_and_span_ids_come_from_the_current_span() -> None:
    tracer = TracerProvider().get_tracer("test")

    with tracer.start_as_current_span("work") as span:
        entry = _format(logging.INFO, "inside")

    context = span.get_span_context()
    assert entry["trace_id"] == f"{context.trace_id:032x}"
    assert entry["span_id"] == f"{context.span_id:016x}"
    assert len(str(entry["trace_id"])) == 32 and len(str(entry["span_id"])) == 16


def test_no_trace_ids_outside_a_span() -> None:
    entry = _format(logging.INFO, "outside")

    assert "trace_id" not in entry and "span_id" not in entry


def test_otlp_handler_only_when_enabled() -> None:
    plain = logging_config("ai", "1.2.3", "info")
    exported = logging_config("ai", "1.2.3", "info", otlp=True)

    assert plain["root"]["handlers"] == ["stdout"]
    assert exported["root"]["handlers"] == ["stdout", "otlp"]
    assert exported["handlers"]["otlp"]["class"].endswith(".LoggingHandler")


def test_otlp_handler_gets_other_extras_as_strings() -> None:
    record = logging.makeLogRecord({"msg": "x", "request": object(), "user_id": "u_1", "n": 3})

    assert PrimitiveExtras().filter(record)

    assert str(record.__dict__["request"]).startswith("<object object")
    assert (record.__dict__["user_id"], record.__dict__["n"]) == ("u_1", 3)
