from nopacs.v1 import catalog_pb2 as _catalog_pb2
from nopacs.v1 import job_pb2 as _job_pb2
from google.protobuf.internal import containers as _containers
from google.protobuf.internal import enum_type_wrapper as _enum_type_wrapper
from google.protobuf import descriptor as _descriptor
from google.protobuf import message as _message
from collections.abc import Iterable as _Iterable, Mapping as _Mapping
from typing import ClassVar as _ClassVar, Optional as _Optional, Union as _Union

DESCRIPTOR: _descriptor.FileDescriptor

class SearchLevel(int, metaclass=_enum_type_wrapper.EnumTypeWrapper):
    __slots__ = ()
    SEARCH_LEVEL_UNSPECIFIED: _ClassVar[SearchLevel]
    SEARCH_LEVEL_SCAN: _ClassVar[SearchLevel]
    SEARCH_LEVEL_SESSION: _ClassVar[SearchLevel]
    SEARCH_LEVEL_SUBJECT: _ClassVar[SearchLevel]
SEARCH_LEVEL_UNSPECIFIED: SearchLevel
SEARCH_LEVEL_SCAN: SearchLevel
SEARCH_LEVEL_SESSION: SearchLevel
SEARCH_LEVEL_SUBJECT: SearchLevel

class BackfillAttributeRequest(_message.Message):
    __slots__ = ("tenant", "attribute")
    TENANT_FIELD_NUMBER: _ClassVar[int]
    ATTRIBUTE_FIELD_NUMBER: _ClassVar[int]
    tenant: str
    attribute: str
    def __init__(self, tenant: _Optional[str] = ..., attribute: _Optional[str] = ...) -> None: ...

class BackfillAttributeResponse(_message.Message):
    __slots__ = ("job",)
    JOB_FIELD_NUMBER: _ClassVar[int]
    job: _job_pb2.Job
    def __init__(self, job: _Optional[_Union[_job_pb2.Job, _Mapping]] = ...) -> None: ...

class SearchRequest(_message.Message):
    __slots__ = ("tenant", "level", "query", "page_size", "page_token", "count", "facet")
    TENANT_FIELD_NUMBER: _ClassVar[int]
    LEVEL_FIELD_NUMBER: _ClassVar[int]
    QUERY_FIELD_NUMBER: _ClassVar[int]
    PAGE_SIZE_FIELD_NUMBER: _ClassVar[int]
    PAGE_TOKEN_FIELD_NUMBER: _ClassVar[int]
    COUNT_FIELD_NUMBER: _ClassVar[int]
    FACET_FIELD_NUMBER: _ClassVar[int]
    tenant: str
    level: SearchLevel
    query: str
    page_size: int
    page_token: str
    count: bool
    facet: str
    def __init__(self, tenant: _Optional[str] = ..., level: _Optional[_Union[SearchLevel, str]] = ..., query: _Optional[str] = ..., page_size: _Optional[int] = ..., page_token: _Optional[str] = ..., count: _Optional[bool] = ..., facet: _Optional[str] = ...) -> None: ...

class SearchResponse(_message.Message):
    __slots__ = ("subjects", "sessions", "scans", "next_page_token", "total_count", "total_is_lower_bound", "facet")
    SUBJECTS_FIELD_NUMBER: _ClassVar[int]
    SESSIONS_FIELD_NUMBER: _ClassVar[int]
    SCANS_FIELD_NUMBER: _ClassVar[int]
    NEXT_PAGE_TOKEN_FIELD_NUMBER: _ClassVar[int]
    TOTAL_COUNT_FIELD_NUMBER: _ClassVar[int]
    TOTAL_IS_LOWER_BOUND_FIELD_NUMBER: _ClassVar[int]
    FACET_FIELD_NUMBER: _ClassVar[int]
    subjects: _containers.RepeatedCompositeFieldContainer[_catalog_pb2.Subject]
    sessions: _containers.RepeatedCompositeFieldContainer[_catalog_pb2.Session]
    scans: _containers.RepeatedCompositeFieldContainer[_catalog_pb2.Scan]
    next_page_token: str
    total_count: int
    total_is_lower_bound: bool
    facet: _containers.RepeatedCompositeFieldContainer[FacetValue]
    def __init__(self, subjects: _Optional[_Iterable[_Union[_catalog_pb2.Subject, _Mapping]]] = ..., sessions: _Optional[_Iterable[_Union[_catalog_pb2.Session, _Mapping]]] = ..., scans: _Optional[_Iterable[_Union[_catalog_pb2.Scan, _Mapping]]] = ..., next_page_token: _Optional[str] = ..., total_count: _Optional[int] = ..., total_is_lower_bound: _Optional[bool] = ..., facet: _Optional[_Iterable[_Union[FacetValue, _Mapping]]] = ...) -> None: ...

class FacetValue(_message.Message):
    __slots__ = ("value", "count")
    VALUE_FIELD_NUMBER: _ClassVar[int]
    COUNT_FIELD_NUMBER: _ClassVar[int]
    value: str
    count: int
    def __init__(self, value: _Optional[str] = ..., count: _Optional[int] = ...) -> None: ...

class ListSearchAttributesRequest(_message.Message):
    __slots__ = ("tenant",)
    TENANT_FIELD_NUMBER: _ClassVar[int]
    tenant: str
    def __init__(self, tenant: _Optional[str] = ...) -> None: ...

class ListSearchAttributesResponse(_message.Message):
    __slots__ = ("attributes",)
    ATTRIBUTES_FIELD_NUMBER: _ClassVar[int]
    attributes: _containers.RepeatedCompositeFieldContainer[SearchAttribute]
    def __init__(self, attributes: _Optional[_Iterable[_Union[SearchAttribute, _Mapping]]] = ...) -> None: ...

class SearchAttribute(_message.Message):
    __slots__ = ("field", "ready", "scans_skipped")
    FIELD_FIELD_NUMBER: _ClassVar[int]
    READY_FIELD_NUMBER: _ClassVar[int]
    SCANS_SKIPPED_FIELD_NUMBER: _ClassVar[int]
    field: str
    ready: bool
    scans_skipped: int
    def __init__(self, field: _Optional[str] = ..., ready: _Optional[bool] = ..., scans_skipped: _Optional[int] = ...) -> None: ...
