import datetime

from doodle.events.v1 import meta_pb2 as _meta_pb2
from google.protobuf import timestamp_pb2 as _timestamp_pb2
from google.protobuf.internal import containers as _containers
from google.protobuf import descriptor as _descriptor
from google.protobuf import message as _message
from collections.abc import Iterable as _Iterable, Mapping as _Mapping
from typing import ClassVar as _ClassVar, Optional as _Optional, Union as _Union

DESCRIPTOR: _descriptor.FileDescriptor

class ViewerGuessEvent(_message.Message):
    __slots__ = ("viewer_guess",)
    VIEWER_GUESS_FIELD_NUMBER: _ClassVar[int]
    viewer_guess: ViewerGuess
    def __init__(self, viewer_guess: _Optional[_Union[ViewerGuess, _Mapping]] = ...) -> None: ...

class ViewerGuess(_message.Message):
    __slots__ = ("meta", "room_id", "round_no", "text", "guessed_at")
    META_FIELD_NUMBER: _ClassVar[int]
    ROOM_ID_FIELD_NUMBER: _ClassVar[int]
    ROUND_NO_FIELD_NUMBER: _ClassVar[int]
    TEXT_FIELD_NUMBER: _ClassVar[int]
    GUESSED_AT_FIELD_NUMBER: _ClassVar[int]
    meta: _meta_pb2.EventMeta
    room_id: str
    round_no: int
    text: str
    guessed_at: _timestamp_pb2.Timestamp
    def __init__(self, meta: _Optional[_Union[_meta_pb2.EventMeta, _Mapping]] = ..., room_id: _Optional[str] = ..., round_no: _Optional[int] = ..., text: _Optional[str] = ..., guessed_at: _Optional[_Union[datetime.datetime, _timestamp_pb2.Timestamp, _Mapping]] = ...) -> None: ...

class TopGuessesEvent(_message.Message):
    __slots__ = ("top_guesses",)
    TOP_GUESSES_FIELD_NUMBER: _ClassVar[int]
    top_guesses: TopGuesses
    def __init__(self, top_guesses: _Optional[_Union[TopGuesses, _Mapping]] = ...) -> None: ...

class TopGuesses(_message.Message):
    __slots__ = ("meta", "room_id", "round_no", "window_end", "guesses")
    META_FIELD_NUMBER: _ClassVar[int]
    ROOM_ID_FIELD_NUMBER: _ClassVar[int]
    ROUND_NO_FIELD_NUMBER: _ClassVar[int]
    WINDOW_END_FIELD_NUMBER: _ClassVar[int]
    GUESSES_FIELD_NUMBER: _ClassVar[int]
    meta: _meta_pb2.EventMeta
    room_id: str
    round_no: int
    window_end: _timestamp_pb2.Timestamp
    guesses: _containers.RepeatedCompositeFieldContainer[GuessCount]
    def __init__(self, meta: _Optional[_Union[_meta_pb2.EventMeta, _Mapping]] = ..., room_id: _Optional[str] = ..., round_no: _Optional[int] = ..., window_end: _Optional[_Union[datetime.datetime, _timestamp_pb2.Timestamp, _Mapping]] = ..., guesses: _Optional[_Iterable[_Union[GuessCount, _Mapping]]] = ...) -> None: ...

class GuessCount(_message.Message):
    __slots__ = ("text", "count")
    TEXT_FIELD_NUMBER: _ClassVar[int]
    COUNT_FIELD_NUMBER: _ClassVar[int]
    text: str
    count: int
    def __init__(self, text: _Optional[str] = ..., count: _Optional[int] = ...) -> None: ...
