import datetime

from doodle.events.v1 import meta_pb2 as _meta_pb2
from google.protobuf import timestamp_pb2 as _timestamp_pb2
from google.protobuf.internal import containers as _containers
from google.protobuf import descriptor as _descriptor
from google.protobuf import message as _message
from collections.abc import Iterable as _Iterable, Mapping as _Mapping
from typing import ClassVar as _ClassVar, Optional as _Optional, Union as _Union

DESCRIPTOR: _descriptor.FileDescriptor

class RoundEvent(_message.Message):
    __slots__ = ("round_finished",)
    ROUND_FINISHED_FIELD_NUMBER: _ClassVar[int]
    round_finished: RoundFinished
    def __init__(self, round_finished: _Optional[_Union[RoundFinished, _Mapping]] = ...) -> None: ...

class RoundFinished(_message.Message):
    __slots__ = ("meta", "match_id", "room_id", "round_no", "word", "drawer_ids", "stroke_log_key", "stroke_count", "canvas_width", "canvas_height", "ended_at")
    META_FIELD_NUMBER: _ClassVar[int]
    MATCH_ID_FIELD_NUMBER: _ClassVar[int]
    ROOM_ID_FIELD_NUMBER: _ClassVar[int]
    ROUND_NO_FIELD_NUMBER: _ClassVar[int]
    WORD_FIELD_NUMBER: _ClassVar[int]
    DRAWER_IDS_FIELD_NUMBER: _ClassVar[int]
    STROKE_LOG_KEY_FIELD_NUMBER: _ClassVar[int]
    STROKE_COUNT_FIELD_NUMBER: _ClassVar[int]
    CANVAS_WIDTH_FIELD_NUMBER: _ClassVar[int]
    CANVAS_HEIGHT_FIELD_NUMBER: _ClassVar[int]
    ENDED_AT_FIELD_NUMBER: _ClassVar[int]
    meta: _meta_pb2.EventMeta
    match_id: str
    room_id: str
    round_no: int
    word: str
    drawer_ids: _containers.RepeatedScalarFieldContainer[str]
    stroke_log_key: str
    stroke_count: int
    canvas_width: int
    canvas_height: int
    ended_at: _timestamp_pb2.Timestamp
    def __init__(self, meta: _Optional[_Union[_meta_pb2.EventMeta, _Mapping]] = ..., match_id: _Optional[str] = ..., room_id: _Optional[str] = ..., round_no: _Optional[int] = ..., word: _Optional[str] = ..., drawer_ids: _Optional[_Iterable[str]] = ..., stroke_log_key: _Optional[str] = ..., stroke_count: _Optional[int] = ..., canvas_width: _Optional[int] = ..., canvas_height: _Optional[int] = ..., ended_at: _Optional[_Union[datetime.datetime, _timestamp_pb2.Timestamp, _Mapping]] = ...) -> None: ...
