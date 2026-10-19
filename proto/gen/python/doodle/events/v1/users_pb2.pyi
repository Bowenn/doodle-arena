import datetime

from doodle.events.v1 import meta_pb2 as _meta_pb2
from google.protobuf import timestamp_pb2 as _timestamp_pb2
from google.protobuf import descriptor as _descriptor
from google.protobuf import message as _message
from collections.abc import Mapping as _Mapping
from typing import ClassVar as _ClassVar, Optional as _Optional, Union as _Union

DESCRIPTOR: _descriptor.FileDescriptor

class UserEvent(_message.Message):
    __slots__ = ("credentials_revoked",)
    CREDENTIALS_REVOKED_FIELD_NUMBER: _ClassVar[int]
    credentials_revoked: CredentialsRevoked
    def __init__(self, credentials_revoked: _Optional[_Union[CredentialsRevoked, _Mapping]] = ...) -> None: ...

class CredentialsRevoked(_message.Message):
    __slots__ = ("meta", "user_id", "reason", "revoked_at")
    META_FIELD_NUMBER: _ClassVar[int]
    USER_ID_FIELD_NUMBER: _ClassVar[int]
    REASON_FIELD_NUMBER: _ClassVar[int]
    REVOKED_AT_FIELD_NUMBER: _ClassVar[int]
    meta: _meta_pb2.EventMeta
    user_id: str
    reason: str
    revoked_at: _timestamp_pb2.Timestamp
    def __init__(self, meta: _Optional[_Union[_meta_pb2.EventMeta, _Mapping]] = ..., user_id: _Optional[str] = ..., reason: _Optional[str] = ..., revoked_at: _Optional[_Union[datetime.datetime, _timestamp_pb2.Timestamp, _Mapping]] = ...) -> None: ...
