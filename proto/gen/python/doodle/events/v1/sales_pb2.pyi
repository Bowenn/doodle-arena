import datetime

from doodle.events.v1 import meta_pb2 as _meta_pb2
from google.protobuf import timestamp_pb2 as _timestamp_pb2
from google.protobuf.internal import containers as _containers
from google.protobuf import descriptor as _descriptor
from google.protobuf import message as _message
from collections.abc import Iterable as _Iterable, Mapping as _Mapping
from typing import ClassVar as _ClassVar, Optional as _Optional, Union as _Union

DESCRIPTOR: _descriptor.FileDescriptor

class SaleEvent(_message.Message):
    __slots__ = ("sale_completed",)
    SALE_COMPLETED_FIELD_NUMBER: _ClassVar[int]
    sale_completed: SaleCompleted
    def __init__(self, sale_completed: _Optional[_Union[SaleCompleted, _Mapping]] = ...) -> None: ...

class SaleCompleted(_message.Message):
    __slots__ = ("meta", "sale_id", "listing_id", "drawing_id", "seller_id", "buyer_id", "price", "royalties", "completed_at")
    META_FIELD_NUMBER: _ClassVar[int]
    SALE_ID_FIELD_NUMBER: _ClassVar[int]
    LISTING_ID_FIELD_NUMBER: _ClassVar[int]
    DRAWING_ID_FIELD_NUMBER: _ClassVar[int]
    SELLER_ID_FIELD_NUMBER: _ClassVar[int]
    BUYER_ID_FIELD_NUMBER: _ClassVar[int]
    PRICE_FIELD_NUMBER: _ClassVar[int]
    ROYALTIES_FIELD_NUMBER: _ClassVar[int]
    COMPLETED_AT_FIELD_NUMBER: _ClassVar[int]
    meta: _meta_pb2.EventMeta
    sale_id: str
    listing_id: str
    drawing_id: str
    seller_id: str
    buyer_id: str
    price: int
    royalties: _containers.RepeatedCompositeFieldContainer[RoyaltyPayment]
    completed_at: _timestamp_pb2.Timestamp
    def __init__(self, meta: _Optional[_Union[_meta_pb2.EventMeta, _Mapping]] = ..., sale_id: _Optional[str] = ..., listing_id: _Optional[str] = ..., drawing_id: _Optional[str] = ..., seller_id: _Optional[str] = ..., buyer_id: _Optional[str] = ..., price: _Optional[int] = ..., royalties: _Optional[_Iterable[_Union[RoyaltyPayment, _Mapping]]] = ..., completed_at: _Optional[_Union[datetime.datetime, _timestamp_pb2.Timestamp, _Mapping]] = ...) -> None: ...

class RoyaltyPayment(_message.Message):
    __slots__ = ("user_id", "amount")
    USER_ID_FIELD_NUMBER: _ClassVar[int]
    AMOUNT_FIELD_NUMBER: _ClassVar[int]
    user_id: str
    amount: int
    def __init__(self, user_id: _Optional[str] = ..., amount: _Optional[int] = ...) -> None: ...
