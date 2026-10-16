import json
import logging
from datetime import datetime

from app.logs import JsonFormatter


def _format(level: int, msg: str, **extra: object) -> dict[str, object]:
    record = logging.makeLogRecord(
        {"name": "test", "levelno": level, "levelname": logging.getLevelName(level), "msg": msg}
    )
    record.__dict__.update(extra)
    result: dict[str, object] = json.loads(JsonFormatter("auth", "1.2.3").format(record))
    return result


def test_required_fields() -> None:
    entry = _format(logging.INFO, "user logged in", user_id="u_1")

    assert entry["level"] == "info"
    assert entry["service"] == "auth"
    assert entry["version"] == "1.2.3"
    assert entry["msg"] == "user logged in"
    assert entry["user_id"] == "u_1"
    timestamp = str(entry["timestamp"])
    assert timestamp.endswith("Z")
    datetime.fromisoformat(timestamp)


def test_level_names_match_the_spec() -> None:
    assert _format(logging.WARNING, "x")["level"] == "warn"
    assert _format(logging.CRITICAL, "x")["level"] == "error"
