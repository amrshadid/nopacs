import datetime

from google.protobuf import timestamp_pb2 as _timestamp_pb2
from google.protobuf.internal import containers as _containers
from google.protobuf import descriptor as _descriptor
from google.protobuf import message as _message
from collections.abc import Iterable as _Iterable, Mapping as _Mapping
from typing import ClassVar as _ClassVar, Optional as _Optional, Union as _Union

DESCRIPTOR: _descriptor.FileDescriptor

class StorageOwner(_message.Message):
    __slots__ = ("storage", "generation")
    STORAGE_FIELD_NUMBER: _ClassVar[int]
    GENERATION_FIELD_NUMBER: _ClassVar[int]
    storage: str
    generation: int
    def __init__(self, storage: _Optional[str] = ..., generation: _Optional[int] = ...) -> None: ...

class BackupSection(_message.Message):
    __slots__ = ("tenant", "schema_version", "tables", "rows", "sha256", "snapshot_time", "event_history", "last_correction", "last_reception", "owners", "key_version")
    TENANT_FIELD_NUMBER: _ClassVar[int]
    SCHEMA_VERSION_FIELD_NUMBER: _ClassVar[int]
    TABLES_FIELD_NUMBER: _ClassVar[int]
    ROWS_FIELD_NUMBER: _ClassVar[int]
    SHA256_FIELD_NUMBER: _ClassVar[int]
    SNAPSHOT_TIME_FIELD_NUMBER: _ClassVar[int]
    EVENT_HISTORY_FIELD_NUMBER: _ClassVar[int]
    LAST_CORRECTION_FIELD_NUMBER: _ClassVar[int]
    LAST_RECEPTION_FIELD_NUMBER: _ClassVar[int]
    OWNERS_FIELD_NUMBER: _ClassVar[int]
    KEY_VERSION_FIELD_NUMBER: _ClassVar[int]
    tenant: str
    schema_version: int
    tables: int
    rows: int
    sha256: bytes
    snapshot_time: _timestamp_pb2.Timestamp
    event_history: str
    last_correction: str
    last_reception: str
    owners: _containers.RepeatedCompositeFieldContainer[StorageOwner]
    key_version: int
    def __init__(self, tenant: _Optional[str] = ..., schema_version: _Optional[int] = ..., tables: _Optional[int] = ..., rows: _Optional[int] = ..., sha256: _Optional[bytes] = ..., snapshot_time: _Optional[_Union[datetime.datetime, _timestamp_pb2.Timestamp, _Mapping]] = ..., event_history: _Optional[str] = ..., last_correction: _Optional[str] = ..., last_reception: _Optional[str] = ..., owners: _Optional[_Iterable[_Union[StorageOwner, _Mapping]]] = ..., key_version: _Optional[int] = ...) -> None: ...

class BackupSummary(_message.Message):
    __slots__ = ("format", "format_version", "installation_id", "time", "size_bytes", "sha256", "sections", "key_version")
    FORMAT_FIELD_NUMBER: _ClassVar[int]
    FORMAT_VERSION_FIELD_NUMBER: _ClassVar[int]
    INSTALLATION_ID_FIELD_NUMBER: _ClassVar[int]
    TIME_FIELD_NUMBER: _ClassVar[int]
    SIZE_BYTES_FIELD_NUMBER: _ClassVar[int]
    SHA256_FIELD_NUMBER: _ClassVar[int]
    SECTIONS_FIELD_NUMBER: _ClassVar[int]
    KEY_VERSION_FIELD_NUMBER: _ClassVar[int]
    format: str
    format_version: int
    installation_id: str
    time: _timestamp_pb2.Timestamp
    size_bytes: int
    sha256: bytes
    sections: _containers.RepeatedCompositeFieldContainer[BackupSection]
    key_version: int
    def __init__(self, format: _Optional[str] = ..., format_version: _Optional[int] = ..., installation_id: _Optional[str] = ..., time: _Optional[_Union[datetime.datetime, _timestamp_pb2.Timestamp, _Mapping]] = ..., size_bytes: _Optional[int] = ..., sha256: _Optional[bytes] = ..., sections: _Optional[_Iterable[_Union[BackupSection, _Mapping]]] = ..., key_version: _Optional[int] = ...) -> None: ...

class CreateBackupRequest(_message.Message):
    __slots__ = ()
    def __init__(self) -> None: ...

class CreateBackupResponse(_message.Message):
    __slots__ = ("data", "summary")
    DATA_FIELD_NUMBER: _ClassVar[int]
    SUMMARY_FIELD_NUMBER: _ClassVar[int]
    data: bytes
    summary: BackupSummary
    def __init__(self, data: _Optional[bytes] = ..., summary: _Optional[_Union[BackupSummary, _Mapping]] = ...) -> None: ...

class ExportTenantRequest(_message.Message):
    __slots__ = ("tenant",)
    TENANT_FIELD_NUMBER: _ClassVar[int]
    tenant: str
    def __init__(self, tenant: _Optional[str] = ...) -> None: ...

class TenantExportSummary(_message.Message):
    __slots__ = ("summary", "transfer_key")
    SUMMARY_FIELD_NUMBER: _ClassVar[int]
    TRANSFER_KEY_FIELD_NUMBER: _ClassVar[int]
    summary: BackupSummary
    transfer_key: bytes
    def __init__(self, summary: _Optional[_Union[BackupSummary, _Mapping]] = ..., transfer_key: _Optional[bytes] = ...) -> None: ...

class ExportTenantResponse(_message.Message):
    __slots__ = ("data", "summary")
    DATA_FIELD_NUMBER: _ClassVar[int]
    SUMMARY_FIELD_NUMBER: _ClassVar[int]
    data: bytes
    summary: TenantExportSummary
    def __init__(self, data: _Optional[bytes] = ..., summary: _Optional[_Union[TenantExportSummary, _Mapping]] = ...) -> None: ...
