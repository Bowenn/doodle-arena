"""Decoding stroke logs and rendering them: deterministic bytes, undone strokes left out."""

import gzip
import io
from typing import Any

import pytest
from doodle.game.v1 import stroke_log_pb2
from PIL import Image

from drawings.render import render
from drawings.strokes import (
    CorruptStrokeLog,
    Replay,
    decode_stroke_log,
    replay_document,
    replay_from_document,
    replay_from_log,
)
from tests.conftest import DEFAULT_STROKE, stroke_log_bytes


def replay(**kwargs: Any) -> tuple[Replay, stroke_log_pb2.StrokeLog]:
    log = decode_stroke_log(stroke_log_bytes(**kwargs))
    return replay_from_log(log), log


def test_same_strokes_render_to_the_same_bytes() -> None:
    a, _ = replay()
    b, _ = replay(match_id="m_other", round_no=7)

    assert render(a).png == render(b).png
    assert render(a).thumb == render(b).thumb


def test_different_strokes_render_differently() -> None:
    a, _ = replay()
    b, _ = replay(strokes=[{"points": [(1000, 9000, 0), (9000, 9000, 50)]}])

    assert render(a).png != render(b).png


def test_undone_strokes_are_left_out() -> None:
    plain, _ = replay()
    with_undo, _ = replay(
        strokes=[DEFAULT_STROKE, {"points": [(100, 100, 0), (9900, 9900, 10)], "undone": True}]
    )

    assert render(with_undo).png == render(plain).png


def test_sizes_keep_the_aspect_ratio() -> None:
    wide, _ = replay(width=800, height=600)
    tall, _ = replay(width=600, height=900)

    out = render(wide)
    assert (out.width, out.height) == (1024, 768)
    assert Image.open(io.BytesIO(out.thumb)).size == (256, 192)
    assert Image.open(io.BytesIO(render(tall).png)).size == (683, 1024)


def test_the_stroke_is_drawn_in_its_color() -> None:
    r, _ = replay(strokes=[{"color": "#e03131", "width": 400, "points": [(5000, 5000, 0)]}])

    image = Image.open(io.BytesIO(render(r).png)).convert("RGB")
    assert image.getpixel((512, 384)) == (224, 49, 49)  # a tap is a dot
    assert image.getpixel((10, 10)) == (255, 255, 255)


def test_a_bad_color_falls_back_to_black() -> None:
    r, _ = replay(strokes=[{"color": "red; drop table", "points": [(5000, 5000, 0)]}])

    assert r.strokes[0].color == "#000000"


def test_replay_document_round_trips() -> None:
    r, log = replay(
        strokes=[DEFAULT_STROKE, {"points": [(1, 2, 3)], "undone": True, "user_id": "u_bob"}]
    )

    doc = replay_document("d-1", r, log)
    assert doc["_id"] == "d-1"
    assert doc["schema_version"] == 1
    assert doc["strokes"][0]["points"][1] == [5000, 5000, 40]
    assert replay_from_document(doc) == r


@pytest.mark.parametrize(
    "data",
    [b"not gzip", gzip.compress(b"\xff\xff not a protobuf"), b""],
)
def test_corrupt_logs_are_refused(data: bytes) -> None:
    with pytest.raises(CorruptStrokeLog):
        decode_stroke_log(data)


def test_an_unknown_schema_version_is_refused() -> None:
    data = gzip.compress(stroke_log_pb2.StrokeLog(schema_version=2).SerializeToString())
    with pytest.raises(CorruptStrokeLog):
        decode_stroke_log(data)
