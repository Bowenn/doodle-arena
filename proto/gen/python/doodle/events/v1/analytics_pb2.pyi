import datetime

from doodle.events.v1 import meta_pb2 as _meta_pb2
from google.protobuf import timestamp_pb2 as _timestamp_pb2
from google.protobuf.internal import containers as _containers
from google.protobuf.internal import enum_type_wrapper as _enum_type_wrapper
from google.protobuf import descriptor as _descriptor
from google.protobuf import message as _message
from collections.abc import Iterable as _Iterable, Mapping as _Mapping
from typing import ClassVar as _ClassVar, Optional as _Optional, Union as _Union

DESCRIPTOR: _descriptor.FileDescriptor

class WashTradeRule(int, metaclass=_enum_type_wrapper.EnumTypeWrapper):
    __slots__ = ()
    WASH_TRADE_RULE_UNSPECIFIED: _ClassVar[WashTradeRule]
    WASH_TRADE_RULE_ROUND_TRIP: _ClassVar[WashTradeRule]
    WASH_TRADE_RULE_CYCLE: _ClassVar[WashTradeRule]
WASH_TRADE_RULE_UNSPECIFIED: WashTradeRule
WASH_TRADE_RULE_ROUND_TRIP: WashTradeRule
WASH_TRADE_RULE_CYCLE: WashTradeRule

class AnalyticsFlagEvent(_message.Message):
    __slots__ = ("wash_trade_flagged",)
    WASH_TRADE_FLAGGED_FIELD_NUMBER: _ClassVar[int]
    wash_trade_flagged: WashTradeFlagged
    def __init__(self, wash_trade_flagged: _Optional[_Union[WashTradeFlagged, _Mapping]] = ...) -> None: ...

class WashTradeFlagged(_message.Message):
    __slots__ = ("meta", "flag_id", "rule", "drawing_id", "account_ids", "sale_ids", "first_sale_at", "last_sale_at", "volume")
    META_FIELD_NUMBER: _ClassVar[int]
    FLAG_ID_FIELD_NUMBER: _ClassVar[int]
    RULE_FIELD_NUMBER: _ClassVar[int]
    DRAWING_ID_FIELD_NUMBER: _ClassVar[int]
    ACCOUNT_IDS_FIELD_NUMBER: _ClassVar[int]
    SALE_IDS_FIELD_NUMBER: _ClassVar[int]
    FIRST_SALE_AT_FIELD_NUMBER: _ClassVar[int]
    LAST_SALE_AT_FIELD_NUMBER: _ClassVar[int]
    VOLUME_FIELD_NUMBER: _ClassVar[int]
    meta: _meta_pb2.EventMeta
    flag_id: str
    rule: WashTradeRule
    drawing_id: str
    account_ids: _containers.RepeatedScalarFieldContainer[str]
    sale_ids: _containers.RepeatedScalarFieldContainer[str]
    first_sale_at: _timestamp_pb2.Timestamp
    last_sale_at: _timestamp_pb2.Timestamp
    volume: int
    def __init__(self, meta: _Optional[_Union[_meta_pb2.EventMeta, _Mapping]] = ..., flag_id: _Optional[str] = ..., rule: _Optional[_Union[WashTradeRule, str]] = ..., drawing_id: _Optional[str] = ..., account_ids: _Optional[_Iterable[str]] = ..., sale_ids: _Optional[_Iterable[str]] = ..., first_sale_at: _Optional[_Union[datetime.datetime, _timestamp_pb2.Timestamp, _Mapping]] = ..., last_sale_at: _Optional[_Union[datetime.datetime, _timestamp_pb2.Timestamp, _Mapping]] = ..., volume: _Optional[int] = ...) -> None: ...
