from doodle.events.v1 import meta_pb2 as _meta_pb2
from google.protobuf.internal import enum_type_wrapper as _enum_type_wrapper
from google.protobuf import descriptor as _descriptor
from google.protobuf import message as _message
from collections.abc import Mapping as _Mapping
from typing import ClassVar as _ClassVar, Optional as _Optional, Union as _Union

DESCRIPTOR: _descriptor.FileDescriptor

class CreditReason(int, metaclass=_enum_type_wrapper.EnumTypeWrapper):
    __slots__ = ()
    CREDIT_REASON_UNSPECIFIED: _ClassVar[CreditReason]
    CREDIT_REASON_STRIPE_PURCHASE: _ClassVar[CreditReason]
    CREDIT_REASON_REWARD: _ClassVar[CreditReason]
    CREDIT_REASON_TIP: _ClassVar[CreditReason]
    CREDIT_REASON_TIP_REFUND: _ClassVar[CreditReason]
    CREDIT_REASON_DROP_SALE: _ClassVar[CreditReason]
CREDIT_REASON_UNSPECIFIED: CreditReason
CREDIT_REASON_STRIPE_PURCHASE: CreditReason
CREDIT_REASON_REWARD: CreditReason
CREDIT_REASON_TIP: CreditReason
CREDIT_REASON_TIP_REFUND: CreditReason
CREDIT_REASON_DROP_SALE: CreditReason

class WalletEvent(_message.Message):
    __slots__ = ("coins_credited",)
    COINS_CREDITED_FIELD_NUMBER: _ClassVar[int]
    coins_credited: CoinsCredited
    def __init__(self, coins_credited: _Optional[_Union[CoinsCredited, _Mapping]] = ...) -> None: ...

class CoinsCredited(_message.Message):
    __slots__ = ("meta", "user_id", "amount", "reason", "reference")
    META_FIELD_NUMBER: _ClassVar[int]
    USER_ID_FIELD_NUMBER: _ClassVar[int]
    AMOUNT_FIELD_NUMBER: _ClassVar[int]
    REASON_FIELD_NUMBER: _ClassVar[int]
    REFERENCE_FIELD_NUMBER: _ClassVar[int]
    meta: _meta_pb2.EventMeta
    user_id: str
    amount: int
    reason: CreditReason
    reference: str
    def __init__(self, meta: _Optional[_Union[_meta_pb2.EventMeta, _Mapping]] = ..., user_id: _Optional[str] = ..., amount: _Optional[int] = ..., reason: _Optional[_Union[CreditReason, str]] = ..., reference: _Optional[str] = ...) -> None: ...
