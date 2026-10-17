from doodle.game.v1 import game_pb2 as _game_pb2
from google.protobuf import descriptor as _descriptor
from google.protobuf import message as _message
from collections.abc import Mapping as _Mapping
from typing import ClassVar as _ClassVar, Optional as _Optional, Union as _Union

DESCRIPTOR: _descriptor.FileDescriptor

class AllocateRoomRequest(_message.Message):
    __slots__ = ("room_id", "mode", "settings", "host_user_id", "server_id")
    ROOM_ID_FIELD_NUMBER: _ClassVar[int]
    MODE_FIELD_NUMBER: _ClassVar[int]
    SETTINGS_FIELD_NUMBER: _ClassVar[int]
    HOST_USER_ID_FIELD_NUMBER: _ClassVar[int]
    SERVER_ID_FIELD_NUMBER: _ClassVar[int]
    room_id: str
    mode: _game_pb2.GameMode
    settings: RoomSettings
    host_user_id: str
    server_id: str
    def __init__(self, room_id: _Optional[str] = ..., mode: _Optional[_Union[_game_pb2.GameMode, str]] = ..., settings: _Optional[_Union[RoomSettings, _Mapping]] = ..., host_user_id: _Optional[str] = ..., server_id: _Optional[str] = ...) -> None: ...

class RoomSettings(_message.Message):
    __slots__ = ("max_players", "choose_seconds", "draw_seconds", "word_options")
    MAX_PLAYERS_FIELD_NUMBER: _ClassVar[int]
    CHOOSE_SECONDS_FIELD_NUMBER: _ClassVar[int]
    DRAW_SECONDS_FIELD_NUMBER: _ClassVar[int]
    WORD_OPTIONS_FIELD_NUMBER: _ClassVar[int]
    max_players: int
    choose_seconds: int
    draw_seconds: int
    word_options: int
    def __init__(self, max_players: _Optional[int] = ..., choose_seconds: _Optional[int] = ..., draw_seconds: _Optional[int] = ..., word_options: _Optional[int] = ...) -> None: ...

class AllocateRoomResponse(_message.Message):
    __slots__ = ("server_id", "ws_address")
    SERVER_ID_FIELD_NUMBER: _ClassVar[int]
    WS_ADDRESS_FIELD_NUMBER: _ClassVar[int]
    server_id: str
    ws_address: str
    def __init__(self, server_id: _Optional[str] = ..., ws_address: _Optional[str] = ...) -> None: ...

class DrainServerRequest(_message.Message):
    __slots__ = ()
    def __init__(self) -> None: ...

class DrainServerResponse(_message.Message):
    __slots__ = ("rooms_remaining",)
    ROOMS_REMAINING_FIELD_NUMBER: _ClassVar[int]
    rooms_remaining: int
    def __init__(self, rooms_remaining: _Optional[int] = ...) -> None: ...
