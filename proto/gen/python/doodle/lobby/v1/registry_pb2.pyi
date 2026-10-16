from google.protobuf.internal import containers as _containers
from google.protobuf import descriptor as _descriptor
from google.protobuf import message as _message
from collections.abc import Iterable as _Iterable
from typing import ClassVar as _ClassVar, Optional as _Optional

DESCRIPTOR: _descriptor.FileDescriptor

class HeartbeatRequest(_message.Message):
    __slots__ = ("server_id", "ws_address", "room_ids", "sockets", "draining")
    SERVER_ID_FIELD_NUMBER: _ClassVar[int]
    WS_ADDRESS_FIELD_NUMBER: _ClassVar[int]
    ROOM_IDS_FIELD_NUMBER: _ClassVar[int]
    SOCKETS_FIELD_NUMBER: _ClassVar[int]
    DRAINING_FIELD_NUMBER: _ClassVar[int]
    server_id: str
    ws_address: str
    room_ids: _containers.RepeatedScalarFieldContainer[str]
    sockets: int
    draining: bool
    def __init__(self, server_id: _Optional[str] = ..., ws_address: _Optional[str] = ..., room_ids: _Optional[_Iterable[str]] = ..., sockets: _Optional[int] = ..., draining: _Optional[bool] = ...) -> None: ...

class HeartbeatResponse(_message.Message):
    __slots__ = ()
    def __init__(self) -> None: ...
