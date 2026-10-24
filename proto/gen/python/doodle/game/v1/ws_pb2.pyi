from google.protobuf.internal import containers as _containers
from google.protobuf import descriptor as _descriptor
from google.protobuf import message as _message
from collections.abc import Iterable as _Iterable, Mapping as _Mapping
from typing import ClassVar as _ClassVar, Optional as _Optional, Union as _Union

DESCRIPTOR: _descriptor.FileDescriptor

class StrokeFrame(_message.Message):
    __slots__ = ("batch", "event")
    BATCH_FIELD_NUMBER: _ClassVar[int]
    EVENT_FIELD_NUMBER: _ClassVar[int]
    batch: StrokeBatchIn
    event: StrokeBatchEvent
    def __init__(self, batch: _Optional[_Union[StrokeBatchIn, _Mapping]] = ..., event: _Optional[_Union[StrokeBatchEvent, _Mapping]] = ...) -> None: ...

class StrokeBatchIn(_message.Message):
    __slots__ = ("stroke_id", "points", "color", "width", "done")
    STROKE_ID_FIELD_NUMBER: _ClassVar[int]
    POINTS_FIELD_NUMBER: _ClassVar[int]
    COLOR_FIELD_NUMBER: _ClassVar[int]
    WIDTH_FIELD_NUMBER: _ClassVar[int]
    DONE_FIELD_NUMBER: _ClassVar[int]
    stroke_id: str
    points: _containers.RepeatedScalarFieldContainer[int]
    color: str
    width: int
    done: bool
    def __init__(self, stroke_id: _Optional[str] = ..., points: _Optional[_Iterable[int]] = ..., color: _Optional[str] = ..., width: _Optional[int] = ..., done: _Optional[bool] = ...) -> None: ...

class StrokeBatchEvent(_message.Message):
    __slots__ = ("seq", "user_id", "stroke_id", "points", "color", "width", "done")
    SEQ_FIELD_NUMBER: _ClassVar[int]
    USER_ID_FIELD_NUMBER: _ClassVar[int]
    STROKE_ID_FIELD_NUMBER: _ClassVar[int]
    POINTS_FIELD_NUMBER: _ClassVar[int]
    COLOR_FIELD_NUMBER: _ClassVar[int]
    WIDTH_FIELD_NUMBER: _ClassVar[int]
    DONE_FIELD_NUMBER: _ClassVar[int]
    seq: int
    user_id: str
    stroke_id: str
    points: _containers.RepeatedScalarFieldContainer[int]
    color: str
    width: int
    done: bool
    def __init__(self, seq: _Optional[int] = ..., user_id: _Optional[str] = ..., stroke_id: _Optional[str] = ..., points: _Optional[_Iterable[int]] = ..., color: _Optional[str] = ..., width: _Optional[int] = ..., done: _Optional[bool] = ...) -> None: ...
