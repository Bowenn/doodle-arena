from doodle.events.v1 import meta_pb2 as _meta_pb2
from google.protobuf.internal import containers as _containers
from google.protobuf.internal import enum_type_wrapper as _enum_type_wrapper
from google.protobuf import descriptor as _descriptor
from google.protobuf import message as _message
from collections.abc import Iterable as _Iterable, Mapping as _Mapping
from typing import ClassVar as _ClassVar, Optional as _Optional, Union as _Union

DESCRIPTOR: _descriptor.FileDescriptor

class DecisionSource(int, metaclass=_enum_type_wrapper.EnumTypeWrapper):
    __slots__ = ()
    DECISION_SOURCE_UNSPECIFIED: _ClassVar[DecisionSource]
    DECISION_SOURCE_AI: _ClassVar[DecisionSource]
    DECISION_SOURCE_MODERATOR: _ClassVar[DecisionSource]
DECISION_SOURCE_UNSPECIFIED: DecisionSource
DECISION_SOURCE_AI: DecisionSource
DECISION_SOURCE_MODERATOR: DecisionSource

class DrawingEvent(_message.Message):
    __slots__ = ("drawing_approved", "drawing_rejected")
    DRAWING_APPROVED_FIELD_NUMBER: _ClassVar[int]
    DRAWING_REJECTED_FIELD_NUMBER: _ClassVar[int]
    drawing_approved: DrawingApproved
    drawing_rejected: DrawingRejected
    def __init__(self, drawing_approved: _Optional[_Union[DrawingApproved, _Mapping]] = ..., drawing_rejected: _Optional[_Union[DrawingRejected, _Mapping]] = ...) -> None: ...

class DrawingApproved(_message.Message):
    __slots__ = ("meta", "drawing_id", "match_id", "round_no", "author_ids", "initial_owner_id", "sha256", "image_key", "thumb_key", "approved_by")
    META_FIELD_NUMBER: _ClassVar[int]
    DRAWING_ID_FIELD_NUMBER: _ClassVar[int]
    MATCH_ID_FIELD_NUMBER: _ClassVar[int]
    ROUND_NO_FIELD_NUMBER: _ClassVar[int]
    AUTHOR_IDS_FIELD_NUMBER: _ClassVar[int]
    INITIAL_OWNER_ID_FIELD_NUMBER: _ClassVar[int]
    SHA256_FIELD_NUMBER: _ClassVar[int]
    IMAGE_KEY_FIELD_NUMBER: _ClassVar[int]
    THUMB_KEY_FIELD_NUMBER: _ClassVar[int]
    APPROVED_BY_FIELD_NUMBER: _ClassVar[int]
    meta: _meta_pb2.EventMeta
    drawing_id: str
    match_id: str
    round_no: int
    author_ids: _containers.RepeatedScalarFieldContainer[str]
    initial_owner_id: str
    sha256: str
    image_key: str
    thumb_key: str
    approved_by: DecisionSource
    def __init__(self, meta: _Optional[_Union[_meta_pb2.EventMeta, _Mapping]] = ..., drawing_id: _Optional[str] = ..., match_id: _Optional[str] = ..., round_no: _Optional[int] = ..., author_ids: _Optional[_Iterable[str]] = ..., initial_owner_id: _Optional[str] = ..., sha256: _Optional[str] = ..., image_key: _Optional[str] = ..., thumb_key: _Optional[str] = ..., approved_by: _Optional[_Union[DecisionSource, str]] = ...) -> None: ...

class DrawingRejected(_message.Message):
    __slots__ = ("meta", "drawing_id", "match_id", "round_no", "author_ids", "rejected_by", "reason")
    META_FIELD_NUMBER: _ClassVar[int]
    DRAWING_ID_FIELD_NUMBER: _ClassVar[int]
    MATCH_ID_FIELD_NUMBER: _ClassVar[int]
    ROUND_NO_FIELD_NUMBER: _ClassVar[int]
    AUTHOR_IDS_FIELD_NUMBER: _ClassVar[int]
    REJECTED_BY_FIELD_NUMBER: _ClassVar[int]
    REASON_FIELD_NUMBER: _ClassVar[int]
    meta: _meta_pb2.EventMeta
    drawing_id: str
    match_id: str
    round_no: int
    author_ids: _containers.RepeatedScalarFieldContainer[str]
    rejected_by: DecisionSource
    reason: str
    def __init__(self, meta: _Optional[_Union[_meta_pb2.EventMeta, _Mapping]] = ..., drawing_id: _Optional[str] = ..., match_id: _Optional[str] = ..., round_no: _Optional[int] = ..., author_ids: _Optional[_Iterable[str]] = ..., rejected_by: _Optional[_Union[DecisionSource, str]] = ..., reason: _Optional[str] = ...) -> None: ...
