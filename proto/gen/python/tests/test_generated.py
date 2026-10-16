"""Checks that the generated Python code imports and round-trips, and shows ADR 0004's
forward-compatibility rule: a wrapper with an unknown oneof case decodes with no case set."""

import importlib
from pathlib import Path

from doodle.events.v1 import drawings_pb2, rounds_pb2

PACKAGE_ROOT = Path(__file__).resolve().parent.parent


def test_every_generated_module_imports() -> None:
    # The generated packages have no __init__.py (namespace packages), so pkgutil can't walk
    # them; list the files instead.
    names = [
        ".".join(p.relative_to(PACKAGE_ROOT).with_suffix("").parts)
        for p in sorted((PACKAGE_ROOT / "doodle").rglob("*.py"))
    ]
    assert "doodle.events.v1.rounds_pb2" in names
    assert "doodle.game.v1.control_pb2_grpc" in names
    for name in names:
        importlib.import_module(name)


def test_wrapper_round_trip() -> None:
    event = rounds_pb2.RoundEvent(
        round_finished=rounds_pb2.RoundFinished(match_id="m_1", round_no=2, drawer_ids=["u_1"])
    )

    decoded = rounds_pb2.RoundEvent.FromString(event.SerializeToString())

    assert decoded.WhichOneof("event") == "round_finished"
    assert decoded.round_finished.match_id == "m_1"
    assert list(decoded.round_finished.drawer_ids) == ["u_1"]


def test_unknown_case_decodes_with_no_case_set() -> None:
    # Field 99 stands in for an event type added by a newer producer:
    # tag (99 << 3 | 2), length 0.
    newer = bytes([0x9A, 0x06, 0x00])

    decoded = drawings_pb2.DrawingEvent.FromString(newer)

    assert decoded.WhichOneof("event") is None
