import datetime

from google.protobuf import timestamp_pb2 as _timestamp_pb2
from google.protobuf.internal import containers as _containers
from google.protobuf import descriptor as _descriptor
from google.protobuf import message as _message
from collections.abc import Iterable as _Iterable, Mapping as _Mapping
from typing import ClassVar as _ClassVar, Optional as _Optional, Union as _Union

DESCRIPTOR: _descriptor.FileDescriptor

class StrokeLog(_message.Message):
    __slots__ = ("schema_version", "match_id", "room_id", "round_no", "canvas_width", "canvas_height", "round_started_at", "strokes")
    SCHEMA_VERSION_FIELD_NUMBER: _ClassVar[int]
    MATCH_ID_FIELD_NUMBER: _ClassVar[int]
    ROOM_ID_FIELD_NUMBER: _ClassVar[int]
    ROUND_NO_FIELD_NUMBER: _ClassVar[int]
    CANVAS_WIDTH_FIELD_NUMBER: _ClassVar[int]
    CANVAS_HEIGHT_FIELD_NUMBER: _ClassVar[int]
    ROUND_STARTED_AT_FIELD_NUMBER: _ClassVar[int]
    STROKES_FIELD_NUMBER: _ClassVar[int]
    schema_version: int
    match_id: str
    room_id: str
    round_no: int
    canvas_width: int
    canvas_height: int
    round_started_at: _timestamp_pb2.Timestamp
    strokes: _containers.RepeatedCompositeFieldContainer[Stroke]
    def __init__(self, schema_version: _Optional[int] = ..., match_id: _Optional[str] = ..., room_id: _Optional[str] = ..., round_no: _Optional[int] = ..., canvas_width: _Optional[int] = ..., canvas_height: _Optional[int] = ..., round_started_at: _Optional[_Union[datetime.datetime, _timestamp_pb2.Timestamp, _Mapping]] = ..., strokes: _Optional[_Iterable[_Union[Stroke, _Mapping]]] = ...) -> None: ...

class Stroke(_message.Message):
    __slots__ = ("stroke_id", "user_id", "color", "width", "points", "undone", "server_received_ms")
    STROKE_ID_FIELD_NUMBER: _ClassVar[int]
    USER_ID_FIELD_NUMBER: _ClassVar[int]
    COLOR_FIELD_NUMBER: _ClassVar[int]
    WIDTH_FIELD_NUMBER: _ClassVar[int]
    POINTS_FIELD_NUMBER: _ClassVar[int]
    UNDONE_FIELD_NUMBER: _ClassVar[int]
    SERVER_RECEIVED_MS_FIELD_NUMBER: _ClassVar[int]
    stroke_id: str
    user_id: str
    color: str
    width: int
    points: _containers.RepeatedCompositeFieldContainer[Point]
    undone: bool
    server_received_ms: int
    def __init__(self, stroke_id: _Optional[str] = ..., user_id: _Optional[str] = ..., color: _Optional[str] = ..., width: _Optional[int] = ..., points: _Optional[_Iterable[_Union[Point, _Mapping]]] = ..., undone: _Optional[bool] = ..., server_received_ms: _Optional[int] = ...) -> None: ...

class Point(_message.Message):
    __slots__ = ("x", "y", "t_ms")
    X_FIELD_NUMBER: _ClassVar[int]
    Y_FIELD_NUMBER: _ClassVar[int]
    T_MS_FIELD_NUMBER: _ClassVar[int]
    x: int
    y: int
    t_ms: int
    def __init__(self, x: _Optional[int] = ..., y: _Optional[int] = ..., t_ms: _Optional[int] = ...) -> None: ...
