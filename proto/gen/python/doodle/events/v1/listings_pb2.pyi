import datetime

from doodle.events.v1 import meta_pb2 as _meta_pb2
from google.protobuf import timestamp_pb2 as _timestamp_pb2
from google.protobuf import descriptor as _descriptor
from google.protobuf import message as _message
from collections.abc import Mapping as _Mapping
from typing import ClassVar as _ClassVar, Optional as _Optional, Union as _Union

DESCRIPTOR: _descriptor.FileDescriptor

class ListingEvent(_message.Message):
    __slots__ = ("listing_created", "listing_cancelled")
    LISTING_CREATED_FIELD_NUMBER: _ClassVar[int]
    LISTING_CANCELLED_FIELD_NUMBER: _ClassVar[int]
    listing_created: ListingCreated
    listing_cancelled: ListingCancelled
    def __init__(self, listing_created: _Optional[_Union[ListingCreated, _Mapping]] = ..., listing_cancelled: _Optional[_Union[ListingCancelled, _Mapping]] = ...) -> None: ...

class ListingCreated(_message.Message):
    __slots__ = ("meta", "listing_id", "drawing_id", "seller_id", "price", "created_at")
    META_FIELD_NUMBER: _ClassVar[int]
    LISTING_ID_FIELD_NUMBER: _ClassVar[int]
    DRAWING_ID_FIELD_NUMBER: _ClassVar[int]
    SELLER_ID_FIELD_NUMBER: _ClassVar[int]
    PRICE_FIELD_NUMBER: _ClassVar[int]
    CREATED_AT_FIELD_NUMBER: _ClassVar[int]
    meta: _meta_pb2.EventMeta
    listing_id: str
    drawing_id: str
    seller_id: str
    price: int
    created_at: _timestamp_pb2.Timestamp
    def __init__(self, meta: _Optional[_Union[_meta_pb2.EventMeta, _Mapping]] = ..., listing_id: _Optional[str] = ..., drawing_id: _Optional[str] = ..., seller_id: _Optional[str] = ..., price: _Optional[int] = ..., created_at: _Optional[_Union[datetime.datetime, _timestamp_pb2.Timestamp, _Mapping]] = ...) -> None: ...

class ListingCancelled(_message.Message):
    __slots__ = ("meta", "listing_id", "drawing_id", "seller_id", "cancelled_at")
    META_FIELD_NUMBER: _ClassVar[int]
    LISTING_ID_FIELD_NUMBER: _ClassVar[int]
    DRAWING_ID_FIELD_NUMBER: _ClassVar[int]
    SELLER_ID_FIELD_NUMBER: _ClassVar[int]
    CANCELLED_AT_FIELD_NUMBER: _ClassVar[int]
    meta: _meta_pb2.EventMeta
    listing_id: str
    drawing_id: str
    seller_id: str
    cancelled_at: _timestamp_pb2.Timestamp
    def __init__(self, meta: _Optional[_Union[_meta_pb2.EventMeta, _Mapping]] = ..., listing_id: _Optional[str] = ..., drawing_id: _Optional[str] = ..., seller_id: _Optional[str] = ..., cancelled_at: _Optional[_Union[datetime.datetime, _timestamp_pb2.Timestamp, _Mapping]] = ...) -> None: ...
