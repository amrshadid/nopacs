import datetime

from google.protobuf import timestamp_pb2 as _timestamp_pb2
from google.protobuf.internal import containers as _containers
from google.protobuf.internal import enum_type_wrapper as _enum_type_wrapper
from google.protobuf import descriptor as _descriptor
from google.protobuf import message as _message
from collections.abc import Iterable as _Iterable, Mapping as _Mapping
from typing import ClassVar as _ClassVar, Optional as _Optional, Union as _Union

DESCRIPTOR: _descriptor.FileDescriptor

class CatalogOrder(int, metaclass=_enum_type_wrapper.EnumTypeWrapper):
    __slots__ = ()
    CATALOG_ORDER_UNSPECIFIED: _ClassVar[CatalogOrder]
    CATALOG_ORDER_NEWEST: _ClassVar[CatalogOrder]
    CATALOG_ORDER_LABEL: _ClassVar[CatalogOrder]
    CATALOG_ORDER_STUDY_DATE: _ClassVar[CatalogOrder]
    CATALOG_ORDER_SERIES_DESCRIPTION: _ClassVar[CatalogOrder]
CATALOG_ORDER_UNSPECIFIED: CatalogOrder
CATALOG_ORDER_NEWEST: CatalogOrder
CATALOG_ORDER_LABEL: CatalogOrder
CATALOG_ORDER_STUDY_DATE: CatalogOrder
CATALOG_ORDER_SERIES_DESCRIPTION: CatalogOrder

class Subject(_message.Message):
    __slots__ = ("label", "session_count", "instance_count", "size_bytes", "archive_time", "update_time")
    LABEL_FIELD_NUMBER: _ClassVar[int]
    SESSION_COUNT_FIELD_NUMBER: _ClassVar[int]
    INSTANCE_COUNT_FIELD_NUMBER: _ClassVar[int]
    SIZE_BYTES_FIELD_NUMBER: _ClassVar[int]
    ARCHIVE_TIME_FIELD_NUMBER: _ClassVar[int]
    UPDATE_TIME_FIELD_NUMBER: _ClassVar[int]
    label: str
    session_count: int
    instance_count: int
    size_bytes: int
    archive_time: _timestamp_pb2.Timestamp
    update_time: _timestamp_pb2.Timestamp
    def __init__(self, label: _Optional[str] = ..., session_count: _Optional[int] = ..., instance_count: _Optional[int] = ..., size_bytes: _Optional[int] = ..., archive_time: _Optional[_Union[datetime.datetime, _timestamp_pb2.Timestamp, _Mapping]] = ..., update_time: _Optional[_Union[datetime.datetime, _timestamp_pb2.Timestamp, _Mapping]] = ...) -> None: ...

class Session(_message.Message):
    __slots__ = ("subject_label", "label", "study_instance_uid", "study_date", "modalities", "scan_count", "instance_count", "size_bytes", "storage", "receiver", "archive_time", "update_time")
    SUBJECT_LABEL_FIELD_NUMBER: _ClassVar[int]
    LABEL_FIELD_NUMBER: _ClassVar[int]
    STUDY_INSTANCE_UID_FIELD_NUMBER: _ClassVar[int]
    STUDY_DATE_FIELD_NUMBER: _ClassVar[int]
    MODALITIES_FIELD_NUMBER: _ClassVar[int]
    SCAN_COUNT_FIELD_NUMBER: _ClassVar[int]
    INSTANCE_COUNT_FIELD_NUMBER: _ClassVar[int]
    SIZE_BYTES_FIELD_NUMBER: _ClassVar[int]
    STORAGE_FIELD_NUMBER: _ClassVar[int]
    RECEIVER_FIELD_NUMBER: _ClassVar[int]
    ARCHIVE_TIME_FIELD_NUMBER: _ClassVar[int]
    UPDATE_TIME_FIELD_NUMBER: _ClassVar[int]
    subject_label: str
    label: str
    study_instance_uid: str
    study_date: str
    modalities: _containers.RepeatedScalarFieldContainer[str]
    scan_count: int
    instance_count: int
    size_bytes: int
    storage: str
    receiver: str
    archive_time: _timestamp_pb2.Timestamp
    update_time: _timestamp_pb2.Timestamp
    def __init__(self, subject_label: _Optional[str] = ..., label: _Optional[str] = ..., study_instance_uid: _Optional[str] = ..., study_date: _Optional[str] = ..., modalities: _Optional[_Iterable[str]] = ..., scan_count: _Optional[int] = ..., instance_count: _Optional[int] = ..., size_bytes: _Optional[int] = ..., storage: _Optional[str] = ..., receiver: _Optional[str] = ..., archive_time: _Optional[_Union[datetime.datetime, _timestamp_pb2.Timestamp, _Mapping]] = ..., update_time: _Optional[_Union[datetime.datetime, _timestamp_pb2.Timestamp, _Mapping]] = ...) -> None: ...

class Scan(_message.Message):
    __slots__ = ("subject_label", "session_label", "label", "series_instance_uid", "modality", "series_description", "series_number", "instance_count", "size_bytes", "storage", "manifest_sha256", "receiver", "archive_time", "update_time")
    SUBJECT_LABEL_FIELD_NUMBER: _ClassVar[int]
    SESSION_LABEL_FIELD_NUMBER: _ClassVar[int]
    LABEL_FIELD_NUMBER: _ClassVar[int]
    SERIES_INSTANCE_UID_FIELD_NUMBER: _ClassVar[int]
    MODALITY_FIELD_NUMBER: _ClassVar[int]
    SERIES_DESCRIPTION_FIELD_NUMBER: _ClassVar[int]
    SERIES_NUMBER_FIELD_NUMBER: _ClassVar[int]
    INSTANCE_COUNT_FIELD_NUMBER: _ClassVar[int]
    SIZE_BYTES_FIELD_NUMBER: _ClassVar[int]
    STORAGE_FIELD_NUMBER: _ClassVar[int]
    MANIFEST_SHA256_FIELD_NUMBER: _ClassVar[int]
    RECEIVER_FIELD_NUMBER: _ClassVar[int]
    ARCHIVE_TIME_FIELD_NUMBER: _ClassVar[int]
    UPDATE_TIME_FIELD_NUMBER: _ClassVar[int]
    subject_label: str
    session_label: str
    label: str
    series_instance_uid: str
    modality: str
    series_description: str
    series_number: int
    instance_count: int
    size_bytes: int
    storage: str
    manifest_sha256: str
    receiver: str
    archive_time: _timestamp_pb2.Timestamp
    update_time: _timestamp_pb2.Timestamp
    def __init__(self, subject_label: _Optional[str] = ..., session_label: _Optional[str] = ..., label: _Optional[str] = ..., series_instance_uid: _Optional[str] = ..., modality: _Optional[str] = ..., series_description: _Optional[str] = ..., series_number: _Optional[int] = ..., instance_count: _Optional[int] = ..., size_bytes: _Optional[int] = ..., storage: _Optional[str] = ..., manifest_sha256: _Optional[str] = ..., receiver: _Optional[str] = ..., archive_time: _Optional[_Union[datetime.datetime, _timestamp_pb2.Timestamp, _Mapping]] = ..., update_time: _Optional[_Union[datetime.datetime, _timestamp_pb2.Timestamp, _Mapping]] = ...) -> None: ...

class ListSubjectsRequest(_message.Message):
    __slots__ = ("tenant", "label_prefix", "start_time", "end_time", "order", "page_size", "page_token")
    TENANT_FIELD_NUMBER: _ClassVar[int]
    LABEL_PREFIX_FIELD_NUMBER: _ClassVar[int]
    START_TIME_FIELD_NUMBER: _ClassVar[int]
    END_TIME_FIELD_NUMBER: _ClassVar[int]
    ORDER_FIELD_NUMBER: _ClassVar[int]
    PAGE_SIZE_FIELD_NUMBER: _ClassVar[int]
    PAGE_TOKEN_FIELD_NUMBER: _ClassVar[int]
    tenant: str
    label_prefix: str
    start_time: _timestamp_pb2.Timestamp
    end_time: _timestamp_pb2.Timestamp
    order: CatalogOrder
    page_size: int
    page_token: str
    def __init__(self, tenant: _Optional[str] = ..., label_prefix: _Optional[str] = ..., start_time: _Optional[_Union[datetime.datetime, _timestamp_pb2.Timestamp, _Mapping]] = ..., end_time: _Optional[_Union[datetime.datetime, _timestamp_pb2.Timestamp, _Mapping]] = ..., order: _Optional[_Union[CatalogOrder, str]] = ..., page_size: _Optional[int] = ..., page_token: _Optional[str] = ...) -> None: ...

class ListSubjectsResponse(_message.Message):
    __slots__ = ("subjects", "next_page_token", "total_count")
    SUBJECTS_FIELD_NUMBER: _ClassVar[int]
    NEXT_PAGE_TOKEN_FIELD_NUMBER: _ClassVar[int]
    TOTAL_COUNT_FIELD_NUMBER: _ClassVar[int]
    subjects: _containers.RepeatedCompositeFieldContainer[Subject]
    next_page_token: str
    total_count: int
    def __init__(self, subjects: _Optional[_Iterable[_Union[Subject, _Mapping]]] = ..., next_page_token: _Optional[str] = ..., total_count: _Optional[int] = ...) -> None: ...

class GetSubjectRequest(_message.Message):
    __slots__ = ("tenant", "subject_label")
    TENANT_FIELD_NUMBER: _ClassVar[int]
    SUBJECT_LABEL_FIELD_NUMBER: _ClassVar[int]
    tenant: str
    subject_label: str
    def __init__(self, tenant: _Optional[str] = ..., subject_label: _Optional[str] = ...) -> None: ...

class GetSubjectResponse(_message.Message):
    __slots__ = ("subject",)
    SUBJECT_FIELD_NUMBER: _ClassVar[int]
    subject: Subject
    def __init__(self, subject: _Optional[_Union[Subject, _Mapping]] = ...) -> None: ...

class ListSessionsRequest(_message.Message):
    __slots__ = ("tenant", "subject_label", "label_prefix", "study_date_start", "study_date_end", "modality", "receiver", "start_time", "end_time", "order", "page_size", "page_token")
    TENANT_FIELD_NUMBER: _ClassVar[int]
    SUBJECT_LABEL_FIELD_NUMBER: _ClassVar[int]
    LABEL_PREFIX_FIELD_NUMBER: _ClassVar[int]
    STUDY_DATE_START_FIELD_NUMBER: _ClassVar[int]
    STUDY_DATE_END_FIELD_NUMBER: _ClassVar[int]
    MODALITY_FIELD_NUMBER: _ClassVar[int]
    RECEIVER_FIELD_NUMBER: _ClassVar[int]
    START_TIME_FIELD_NUMBER: _ClassVar[int]
    END_TIME_FIELD_NUMBER: _ClassVar[int]
    ORDER_FIELD_NUMBER: _ClassVar[int]
    PAGE_SIZE_FIELD_NUMBER: _ClassVar[int]
    PAGE_TOKEN_FIELD_NUMBER: _ClassVar[int]
    tenant: str
    subject_label: str
    label_prefix: str
    study_date_start: str
    study_date_end: str
    modality: str
    receiver: str
    start_time: _timestamp_pb2.Timestamp
    end_time: _timestamp_pb2.Timestamp
    order: CatalogOrder
    page_size: int
    page_token: str
    def __init__(self, tenant: _Optional[str] = ..., subject_label: _Optional[str] = ..., label_prefix: _Optional[str] = ..., study_date_start: _Optional[str] = ..., study_date_end: _Optional[str] = ..., modality: _Optional[str] = ..., receiver: _Optional[str] = ..., start_time: _Optional[_Union[datetime.datetime, _timestamp_pb2.Timestamp, _Mapping]] = ..., end_time: _Optional[_Union[datetime.datetime, _timestamp_pb2.Timestamp, _Mapping]] = ..., order: _Optional[_Union[CatalogOrder, str]] = ..., page_size: _Optional[int] = ..., page_token: _Optional[str] = ...) -> None: ...

class ListSessionsResponse(_message.Message):
    __slots__ = ("sessions", "next_page_token", "total_count")
    SESSIONS_FIELD_NUMBER: _ClassVar[int]
    NEXT_PAGE_TOKEN_FIELD_NUMBER: _ClassVar[int]
    TOTAL_COUNT_FIELD_NUMBER: _ClassVar[int]
    sessions: _containers.RepeatedCompositeFieldContainer[Session]
    next_page_token: str
    total_count: int
    def __init__(self, sessions: _Optional[_Iterable[_Union[Session, _Mapping]]] = ..., next_page_token: _Optional[str] = ..., total_count: _Optional[int] = ...) -> None: ...

class GetSessionRequest(_message.Message):
    __slots__ = ("tenant", "subject_label", "session_label")
    TENANT_FIELD_NUMBER: _ClassVar[int]
    SUBJECT_LABEL_FIELD_NUMBER: _ClassVar[int]
    SESSION_LABEL_FIELD_NUMBER: _ClassVar[int]
    tenant: str
    subject_label: str
    session_label: str
    def __init__(self, tenant: _Optional[str] = ..., subject_label: _Optional[str] = ..., session_label: _Optional[str] = ...) -> None: ...

class GetSessionResponse(_message.Message):
    __slots__ = ("session",)
    SESSION_FIELD_NUMBER: _ClassVar[int]
    session: Session
    def __init__(self, session: _Optional[_Union[Session, _Mapping]] = ...) -> None: ...

class ListScansRequest(_message.Message):
    __slots__ = ("tenant", "subject_label", "session_label", "label_prefix", "modality", "series_description_prefix", "receiver", "start_time", "end_time", "order", "page_size", "page_token")
    TENANT_FIELD_NUMBER: _ClassVar[int]
    SUBJECT_LABEL_FIELD_NUMBER: _ClassVar[int]
    SESSION_LABEL_FIELD_NUMBER: _ClassVar[int]
    LABEL_PREFIX_FIELD_NUMBER: _ClassVar[int]
    MODALITY_FIELD_NUMBER: _ClassVar[int]
    SERIES_DESCRIPTION_PREFIX_FIELD_NUMBER: _ClassVar[int]
    RECEIVER_FIELD_NUMBER: _ClassVar[int]
    START_TIME_FIELD_NUMBER: _ClassVar[int]
    END_TIME_FIELD_NUMBER: _ClassVar[int]
    ORDER_FIELD_NUMBER: _ClassVar[int]
    PAGE_SIZE_FIELD_NUMBER: _ClassVar[int]
    PAGE_TOKEN_FIELD_NUMBER: _ClassVar[int]
    tenant: str
    subject_label: str
    session_label: str
    label_prefix: str
    modality: str
    series_description_prefix: str
    receiver: str
    start_time: _timestamp_pb2.Timestamp
    end_time: _timestamp_pb2.Timestamp
    order: CatalogOrder
    page_size: int
    page_token: str
    def __init__(self, tenant: _Optional[str] = ..., subject_label: _Optional[str] = ..., session_label: _Optional[str] = ..., label_prefix: _Optional[str] = ..., modality: _Optional[str] = ..., series_description_prefix: _Optional[str] = ..., receiver: _Optional[str] = ..., start_time: _Optional[_Union[datetime.datetime, _timestamp_pb2.Timestamp, _Mapping]] = ..., end_time: _Optional[_Union[datetime.datetime, _timestamp_pb2.Timestamp, _Mapping]] = ..., order: _Optional[_Union[CatalogOrder, str]] = ..., page_size: _Optional[int] = ..., page_token: _Optional[str] = ...) -> None: ...

class ListScansResponse(_message.Message):
    __slots__ = ("scans", "next_page_token", "total_count")
    SCANS_FIELD_NUMBER: _ClassVar[int]
    NEXT_PAGE_TOKEN_FIELD_NUMBER: _ClassVar[int]
    TOTAL_COUNT_FIELD_NUMBER: _ClassVar[int]
    scans: _containers.RepeatedCompositeFieldContainer[Scan]
    next_page_token: str
    total_count: int
    def __init__(self, scans: _Optional[_Iterable[_Union[Scan, _Mapping]]] = ..., next_page_token: _Optional[str] = ..., total_count: _Optional[int] = ...) -> None: ...

class GetScanRequest(_message.Message):
    __slots__ = ("tenant", "subject_label", "session_label", "scan_label")
    TENANT_FIELD_NUMBER: _ClassVar[int]
    SUBJECT_LABEL_FIELD_NUMBER: _ClassVar[int]
    SESSION_LABEL_FIELD_NUMBER: _ClassVar[int]
    SCAN_LABEL_FIELD_NUMBER: _ClassVar[int]
    tenant: str
    subject_label: str
    session_label: str
    scan_label: str
    def __init__(self, tenant: _Optional[str] = ..., subject_label: _Optional[str] = ..., session_label: _Optional[str] = ..., scan_label: _Optional[str] = ...) -> None: ...

class GetScanResponse(_message.Message):
    __slots__ = ("scan",)
    SCAN_FIELD_NUMBER: _ClassVar[int]
    scan: Scan
    def __init__(self, scan: _Optional[_Union[Scan, _Mapping]] = ...) -> None: ...
