import datetime

from doodle.events.v1 import meta_pb2 as _meta_pb2
from google.protobuf import timestamp_pb2 as _timestamp_pb2
from google.protobuf import descriptor as _descriptor
from google.protobuf import message as _message
from collections.abc import Mapping as _Mapping
from typing import ClassVar as _ClassVar, Optional as _Optional, Union as _Union

DESCRIPTOR: _descriptor.FileDescriptor

class DeadLetterEvent(_message.Message):
    __slots__ = ("delivery_dead",)
    DELIVERY_DEAD_FIELD_NUMBER: _ClassVar[int]
    delivery_dead: DeliveryDead
    def __init__(self, delivery_dead: _Optional[_Union[DeliveryDead, _Mapping]] = ...) -> None: ...

class DeliveryDead(_message.Message):
    __slots__ = ("meta", "delivery_id", "endpoint_id", "source_event_id", "source_event_type", "attempts", "last_status_code", "last_error", "first_attempt_at", "dead_at")
    META_FIELD_NUMBER: _ClassVar[int]
    DELIVERY_ID_FIELD_NUMBER: _ClassVar[int]
    ENDPOINT_ID_FIELD_NUMBER: _ClassVar[int]
    SOURCE_EVENT_ID_FIELD_NUMBER: _ClassVar[int]
    SOURCE_EVENT_TYPE_FIELD_NUMBER: _ClassVar[int]
    ATTEMPTS_FIELD_NUMBER: _ClassVar[int]
    LAST_STATUS_CODE_FIELD_NUMBER: _ClassVar[int]
    LAST_ERROR_FIELD_NUMBER: _ClassVar[int]
    FIRST_ATTEMPT_AT_FIELD_NUMBER: _ClassVar[int]
    DEAD_AT_FIELD_NUMBER: _ClassVar[int]
    meta: _meta_pb2.EventMeta
    delivery_id: str
    endpoint_id: str
    source_event_id: str
    source_event_type: str
    attempts: int
    last_status_code: int
    last_error: str
    first_attempt_at: _timestamp_pb2.Timestamp
    dead_at: _timestamp_pb2.Timestamp
    def __init__(self, meta: _Optional[_Union[_meta_pb2.EventMeta, _Mapping]] = ..., delivery_id: _Optional[str] = ..., endpoint_id: _Optional[str] = ..., source_event_id: _Optional[str] = ..., source_event_type: _Optional[str] = ..., attempts: _Optional[int] = ..., last_status_code: _Optional[int] = ..., last_error: _Optional[str] = ..., first_attempt_at: _Optional[_Union[datetime.datetime, _timestamp_pb2.Timestamp, _Mapping]] = ..., dead_at: _Optional[_Union[datetime.datetime, _timestamp_pb2.Timestamp, _Mapping]] = ...) -> None: ...
