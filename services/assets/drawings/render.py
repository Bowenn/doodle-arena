"""Renders a replay to a PNG and a thumbnail, the way the web canvas draws it (web/src/game/
render.ts): a white background, round caps and joins, widths in 1/10000 of the canvas width.

The output is deterministic: the same strokes always give the same bytes, which is what lets
identical drawings share one content-addressed object in the drawings bucket. Pillow draws at
twice the size and downsamples, which anti-aliases the lines.
"""

import io
from dataclasses import dataclass

from PIL import Image, ImageDraw

from drawings.strokes import MAX_COORD, Replay

FULL_LONG_SIDE = 1024
THUMB_LONG_SIDE = 256
SUPERSAMPLE = 2
BACKGROUND = (255, 255, 255)


@dataclass(frozen=True)
class Rendered:
    png: bytes
    thumb: bytes
    width: int
    height: int


def _size(canvas_width: int, canvas_height: int, long_side: int) -> tuple[int, int]:
    """Keeps the drawers' aspect ratio; a missing size falls back to 4:3, the web's default."""
    if canvas_width <= 0 or canvas_height <= 0:
        canvas_width, canvas_height = 4, 3
    if canvas_width >= canvas_height:
        return long_side, max(1, round(long_side * canvas_height / canvas_width))
    return max(1, round(long_side * canvas_width / canvas_height)), long_side


def _hex(color: str) -> tuple[int, int, int]:
    return int(color[1:3], 16), int(color[3:5], 16), int(color[5:7], 16)


def _png(image: Image.Image) -> bytes:
    buf = io.BytesIO()
    # No metadata (no timestamps), fixed settings: the bytes depend only on the pixels.
    image.save(buf, format="PNG", optimize=False, compress_level=9)
    return buf.getvalue()


def render(replay: Replay) -> Rendered:
    width, height = _size(replay.canvas_width, replay.canvas_height, FULL_LONG_SIDE)
    big_w, big_h = width * SUPERSAMPLE, height * SUPERSAMPLE
    image = Image.new("RGB", (big_w, big_h), BACKGROUND)
    draw = ImageDraw.Draw(image)
    sx, sy = big_w / MAX_COORD, big_h / MAX_COORD

    for stroke in replay.visible():
        color = _hex(stroke.color)
        # Pillow lines take whole-pixel widths; the discs use the same width, or they bulge.
        line_width = max(1, round(stroke.width * sx))
        radius = line_width / 2
        pts = [(x * sx, y * sy) for x, y, _ in stroke.points]
        if len(pts) > 1:
            draw.line(pts, fill=color, width=line_width)
        # Round caps and joins: a disc at every point, like lineCap/lineJoin = "round".
        for x, y in pts:
            draw.ellipse((x - radius, y - radius, x + radius, y + radius), fill=color)

    full = image.resize((width, height), Image.Resampling.LANCZOS)
    thumb_size = _size(replay.canvas_width, replay.canvas_height, THUMB_LONG_SIDE)
    thumb = full.resize(thumb_size, Image.Resampling.LANCZOS)
    return Rendered(png=_png(full), thumb=_png(thumb), width=width, height=height)
