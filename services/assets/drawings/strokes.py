"""The stroke log (doodle.game.v1.StrokeLog) and the replay document made from it.

The game server uploads each round's log, gzipped, to the round-logs bucket (the claim check in
RoundFinished). Assets decodes it once and stores it in MongoDB as a replay document, which is
the source of truth from then on: the bucket copy expires after 7 days. The document has its own
schema_version, so its format can change without touching the game server's contract.
"""

import gzip
import io
import re
from dataclasses import dataclass
from typing import Any

from doodle.game.v1 import stroke_log_pb2

# Points are normalized to 0..10000 (design doc 6.4), widths too (1/10000 of the canvas width).
MAX_COORD = 10000
# A round's log is at most about 100 KB gzipped (ADR 0009); refuse anything that inflates past
# this, so a corrupt or hostile object can't exhaust the worker's memory.
MAX_LOG_BYTES = 16 * 1024 * 1024
REPLAY_SCHEMA_VERSION = 1
_COLOR = re.compile(r"^#[0-9a-fA-F]{6}$")


class CorruptStrokeLog(Exception):
    """The object isn't a stroke log this service can read. Retrying won't help."""


@dataclass(frozen=True)
class Stroke:
    user_id: str
    color: str
    width: int
    # [x, y, t_ms] per point, in drawing order.
    points: list[tuple[int, int, int]]
    undone: bool
    server_received_ms: int


@dataclass(frozen=True)
class Replay:
    canvas_width: int
    canvas_height: int
    strokes: list[Stroke]

    def visible(self) -> list[Stroke]:
        """The strokes a renderer draws: undone ones are left out."""
        return [s for s in self.strokes if not s.undone and s.points]


def decode_stroke_log(data: bytes) -> stroke_log_pb2.StrokeLog:
    try:
        with gzip.GzipFile(fileobj=io.BytesIO(data)) as f:
            raw = f.read(MAX_LOG_BYTES + 1)
    except (OSError, EOFError) as e:
        raise CorruptStrokeLog(f"not gzip: {e}") from e
    if len(raw) > MAX_LOG_BYTES:
        raise CorruptStrokeLog(f"inflates past {MAX_LOG_BYTES} bytes")
    log = stroke_log_pb2.StrokeLog()
    try:
        log.ParseFromString(raw)
    except Exception as e:  # protobuf raises DecodeError, a plain Exception subclass
        raise CorruptStrokeLog(f"not a StrokeLog: {e}") from e
    if log.schema_version != 1:
        raise CorruptStrokeLog(f"unknown stroke log schema_version {log.schema_version}")
    return log


def replay_from_log(log: stroke_log_pb2.StrokeLog) -> Replay:
    strokes = []
    for s in log.strokes:
        color = s.color if _COLOR.match(s.color) else "#000000"
        points = [(min(p.x, MAX_COORD), min(p.y, MAX_COORD), p.t_ms) for p in s.points]
        strokes.append(
            Stroke(
                user_id=s.user_id,
                color=color.lower(),
                width=min(s.width, MAX_COORD),
                points=points,
                undone=s.undone,
                server_received_ms=s.server_received_ms,
            )
        )
    return Replay(canvas_width=log.canvas_width, canvas_height=log.canvas_height, strokes=strokes)


def replay_document(
    drawing_id: str, replay: Replay, log: stroke_log_pb2.StrokeLog
) -> dict[str, Any]:
    """The MongoDB document for a replay, keyed by the drawing id. Points are [x, y, t] arrays to
    keep a 40,000-point round well under MongoDB's 16 MB document limit."""
    return {
        "_id": drawing_id,
        "schema_version": REPLAY_SCHEMA_VERSION,
        "match_id": log.match_id,
        "room_id": log.room_id,
        "round_no": log.round_no,
        "canvas_width": replay.canvas_width,
        "canvas_height": replay.canvas_height,
        "round_started_at": log.round_started_at.ToDatetime()
        if log.HasField("round_started_at")
        else None,
        "strokes": [
            {
                "user_id": s.user_id,
                "color": s.color,
                "width": s.width,
                "undone": s.undone,
                "server_received_ms": s.server_received_ms,
                "points": [list(p) for p in s.points],
            }
            for s in replay.strokes
        ],
    }


def replay_from_document(doc: dict[str, Any]) -> Replay:
    if doc.get("schema_version") != REPLAY_SCHEMA_VERSION:
        raise CorruptStrokeLog(f"unknown replay schema_version {doc.get('schema_version')}")
    return Replay(
        canvas_width=int(doc["canvas_width"]),
        canvas_height=int(doc["canvas_height"]),
        strokes=[
            Stroke(
                user_id=s["user_id"],
                color=s["color"],
                width=int(s["width"]),
                points=[(int(p[0]), int(p[1]), int(p[2])) for p in s["points"]],
                undone=bool(s["undone"]),
                server_received_ms=int(s["server_received_ms"]),
            )
            for s in doc["strokes"]
        ],
    )
