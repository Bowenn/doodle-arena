import datetime

from doodle.events.v1 import meta_pb2 as _meta_pb2
from doodle.game.v1 import game_pb2 as _game_pb2
from google.protobuf import timestamp_pb2 as _timestamp_pb2
from google.protobuf.internal import containers as _containers
from google.protobuf import descriptor as _descriptor
from google.protobuf import message as _message
from collections.abc import Iterable as _Iterable, Mapping as _Mapping
from typing import ClassVar as _ClassVar, Optional as _Optional, Union as _Union

DESCRIPTOR: _descriptor.FileDescriptor

class MatchEvent(_message.Message):
    __slots__ = ("match_finished",)
    MATCH_FINISHED_FIELD_NUMBER: _ClassVar[int]
    match_finished: MatchFinished
    def __init__(self, match_finished: _Optional[_Union[MatchFinished, _Mapping]] = ...) -> None: ...

class MatchFinished(_message.Message):
    __slots__ = ("meta", "match_id", "room_id", "mode", "started_at", "ended_at", "results")
    META_FIELD_NUMBER: _ClassVar[int]
    MATCH_ID_FIELD_NUMBER: _ClassVar[int]
    ROOM_ID_FIELD_NUMBER: _ClassVar[int]
    MODE_FIELD_NUMBER: _ClassVar[int]
    STARTED_AT_FIELD_NUMBER: _ClassVar[int]
    ENDED_AT_FIELD_NUMBER: _ClassVar[int]
    RESULTS_FIELD_NUMBER: _ClassVar[int]
    meta: _meta_pb2.EventMeta
    match_id: str
    room_id: str
    mode: _game_pb2.GameMode
    started_at: _timestamp_pb2.Timestamp
    ended_at: _timestamp_pb2.Timestamp
    results: _containers.RepeatedCompositeFieldContainer[PlayerResult]
    def __init__(self, meta: _Optional[_Union[_meta_pb2.EventMeta, _Mapping]] = ..., match_id: _Optional[str] = ..., room_id: _Optional[str] = ..., mode: _Optional[_Union[_game_pb2.GameMode, str]] = ..., started_at: _Optional[_Union[datetime.datetime, _timestamp_pb2.Timestamp, _Mapping]] = ..., ended_at: _Optional[_Union[datetime.datetime, _timestamp_pb2.Timestamp, _Mapping]] = ..., results: _Optional[_Iterable[_Union[PlayerResult, _Mapping]]] = ...) -> None: ...

class PlayerResult(_message.Message):
    __slots__ = ("user_id", "score", "placement")
    USER_ID_FIELD_NUMBER: _ClassVar[int]
    SCORE_FIELD_NUMBER: _ClassVar[int]
    PLACEMENT_FIELD_NUMBER: _ClassVar[int]
    user_id: str
    score: int
    placement: int
    def __init__(self, user_id: _Optional[str] = ..., score: _Optional[int] = ..., placement: _Optional[int] = ...) -> None: ...
