"""Cross-language check: bytes produced by the Go code (golden files written by
tools/eventctl's tests) decode in Python to the same messages as the JSON fixtures."""

from pathlib import Path

import pytest
from google.protobuf import json_format
from google.protobuf.message import Message

from doodle.events.v1 import matches_pb2, rounds_pb2
from doodle.game.v1 import stroke_log_pb2

FIXTURES = Path(__file__).resolve().parents[4] / "tools" / "eventctl" / "fixtures"

CASES: list[tuple[str, type[Message]]] = [
    ("round_finished", rounds_pb2.RoundEvent),
    ("match_finished", matches_pb2.MatchEvent),
    ("stroke_log", stroke_log_pb2.StrokeLog),
]


@pytest.mark.parametrize(("name", "message_type"), CASES)
def test_go_bytes_decode_to_the_fixture(name: str, message_type: type[Message]) -> None:
    from_go = message_type.FromString((FIXTURES / "golden" / f"{name}.binpb").read_bytes())
    from_json = json_format.Parse((FIXTURES / f"{name}.json").read_text(), message_type())

    assert from_go == from_json
