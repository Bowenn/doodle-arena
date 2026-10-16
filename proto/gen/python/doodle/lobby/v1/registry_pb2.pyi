from google.protobuf.internal import containers as _containers
from google.protobuf.internal import enum_type_wrapper as _enum_type_wrapper
from google.protobuf import descriptor as _descriptor
from google.protobuf import message as _message
from collections.abc import Iterable as _Iterable, Mapping as _Mapping
from typing import ClassVar as _ClassVar, Optional as _Optional, Union as _Union

DESCRIPTOR: _descriptor.FileDescriptor

class RoomPhase(int, metaclass=_enum_type_wrapper.EnumTypeWrapper):
    __slots__ = ()
    ROOM_PHASE_UNSPECIFIED: _ClassVar[RoomPhase]
    ROOM_PHASE_WAITING: _ClassVar[RoomPhase]
    ROOM_PHASE_PLAYING: _ClassVar[RoomPhase]
ROOM_PHASE_UNSPECIFIED: RoomPhase
ROOM_PHASE_WAITING: RoomPhase
ROOM_PHASE_PLAYING: RoomPhase

class HeartbeatRequest(_message.Message):
    __slots__ = ("server_id", "ws_address", "room_ids", "sockets", "draining", "rooms", "control_address")
    SERVER_ID_FIELD_NUMBER: _ClassVar[int]
    WS_ADDRESS_FIELD_NUMBER: _ClassVar[int]
    ROOM_IDS_FIELD_NUMBER: _ClassVar[int]
    SOCKETS_FIELD_NUMBER: _ClassVar[int]
    DRAINING_FIELD_NUMBER: _ClassVar[int]
    ROOMS_FIELD_NUMBER: _ClassVar[int]
    CONTROL_ADDRESS_FIELD_NUMBER: _ClassVar[int]
    server_id: str
    ws_address: str
    room_ids: _containers.RepeatedScalarFieldContainer[str]
    sockets: int
    draining: bool
    rooms: _containers.RepeatedCompositeFieldContainer[RoomStatus]
    control_address: str
    def __init__(self, server_id: _Optional[str] = ..., ws_address: _Optional[str] = ..., room_ids: _Optional[_Iterable[str]] = ..., sockets: _Optional[int] = ..., draining: _Optional[bool] = ..., rooms: _Optional[_Iterable[_Union[RoomStatus, _Mapping]]] = ..., control_address: _Optional[str] = ...) -> None: ...

class RoomStatus(_message.Message):
    __slots__ = ("room_id", "players", "phase")
    ROOM_ID_FIELD_NUMBER: _ClassVar[int]
    PLAYERS_FIELD_NUMBER: _ClassVar[int]
    PHASE_FIELD_NUMBER: _ClassVar[int]
    room_id: str
    players: int
    phase: RoomPhase
    def __init__(self, room_id: _Optional[str] = ..., players: _Optional[int] = ..., phase: _Optional[_Union[RoomPhase, str]] = ...) -> None: ...

class HeartbeatResponse(_message.Message):
    __slots__ = ()
    def __init__(self) -> None: ...
