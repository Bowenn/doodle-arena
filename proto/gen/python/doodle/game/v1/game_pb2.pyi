from google.protobuf.internal import enum_type_wrapper as _enum_type_wrapper
from google.protobuf import descriptor as _descriptor
from typing import ClassVar as _ClassVar

DESCRIPTOR: _descriptor.FileDescriptor

class GameMode(int, metaclass=_enum_type_wrapper.EnumTypeWrapper):
    __slots__ = ()
    GAME_MODE_UNSPECIFIED: _ClassVar[GameMode]
    GAME_MODE_CLASSIC: _ClassVar[GameMode]
GAME_MODE_UNSPECIFIED: GameMode
GAME_MODE_CLASSIC: GameMode
