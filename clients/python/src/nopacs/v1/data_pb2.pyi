from google.protobuf import descriptor as _descriptor
from google.protobuf import message as _message
from collections.abc import Mapping as _Mapping
from typing import ClassVar as _ClassVar, Optional as _Optional, Union as _Union

DESCRIPTOR: _descriptor.FileDescriptor

class ScanMarker(_message.Message):
    __slots__ = ("subject_label", "session_label", "scan_label", "manifest_sha256", "instance_count")
    SUBJECT_LABEL_FIELD_NUMBER: _ClassVar[int]
    SESSION_LABEL_FIELD_NUMBER: _ClassVar[int]
    SCAN_LABEL_FIELD_NUMBER: _ClassVar[int]
    MANIFEST_SHA256_FIELD_NUMBER: _ClassVar[int]
    INSTANCE_COUNT_FIELD_NUMBER: _ClassVar[int]
    subject_label: str
    session_label: str
    scan_label: str
    manifest_sha256: str
    instance_count: int
    def __init__(self, subject_label: _Optional[str] = ..., session_label: _Optional[str] = ..., scan_label: _Optional[str] = ..., manifest_sha256: _Optional[str] = ..., instance_count: _Optional[int] = ...) -> None: ...

class ArchivedFile(_message.Message):
    __slots__ = ("instance_id", "sop_instance_uid", "subject_label", "session_label", "scan_label", "key", "offset", "length", "sha256", "revision")
    INSTANCE_ID_FIELD_NUMBER: _ClassVar[int]
    SOP_INSTANCE_UID_FIELD_NUMBER: _ClassVar[int]
    SUBJECT_LABEL_FIELD_NUMBER: _ClassVar[int]
    SESSION_LABEL_FIELD_NUMBER: _ClassVar[int]
    SCAN_LABEL_FIELD_NUMBER: _ClassVar[int]
    KEY_FIELD_NUMBER: _ClassVar[int]
    OFFSET_FIELD_NUMBER: _ClassVar[int]
    LENGTH_FIELD_NUMBER: _ClassVar[int]
    SHA256_FIELD_NUMBER: _ClassVar[int]
    REVISION_FIELD_NUMBER: _ClassVar[int]
    instance_id: str
    sop_instance_uid: str
    subject_label: str
    session_label: str
    scan_label: str
    key: str
    offset: int
    length: int
    sha256: str
    revision: int
    def __init__(self, instance_id: _Optional[str] = ..., sop_instance_uid: _Optional[str] = ..., subject_label: _Optional[str] = ..., session_label: _Optional[str] = ..., scan_label: _Optional[str] = ..., key: _Optional[str] = ..., offset: _Optional[int] = ..., length: _Optional[int] = ..., sha256: _Optional[str] = ..., revision: _Optional[int] = ...) -> None: ...

class FileChunk(_message.Message):
    __slots__ = ("offset", "data")
    OFFSET_FIELD_NUMBER: _ClassVar[int]
    DATA_FIELD_NUMBER: _ClassVar[int]
    offset: int
    data: bytes
    def __init__(self, offset: _Optional[int] = ..., data: _Optional[bytes] = ...) -> None: ...

class ReadPosition(_message.Message):
    __slots__ = ("session_label", "scan_label", "instance_id")
    SESSION_LABEL_FIELD_NUMBER: _ClassVar[int]
    SCAN_LABEL_FIELD_NUMBER: _ClassVar[int]
    INSTANCE_ID_FIELD_NUMBER: _ClassVar[int]
    session_label: str
    scan_label: str
    instance_id: str
    def __init__(self, session_label: _Optional[str] = ..., scan_label: _Optional[str] = ..., instance_id: _Optional[str] = ...) -> None: ...

class ReadFilesRequest(_message.Message):
    __slots__ = ("tenant", "subject_label", "session_label", "scan_label", "instance_id", "start_after", "announce_only")
    TENANT_FIELD_NUMBER: _ClassVar[int]
    SUBJECT_LABEL_FIELD_NUMBER: _ClassVar[int]
    SESSION_LABEL_FIELD_NUMBER: _ClassVar[int]
    SCAN_LABEL_FIELD_NUMBER: _ClassVar[int]
    INSTANCE_ID_FIELD_NUMBER: _ClassVar[int]
    START_AFTER_FIELD_NUMBER: _ClassVar[int]
    ANNOUNCE_ONLY_FIELD_NUMBER: _ClassVar[int]
    tenant: str
    subject_label: str
    session_label: str
    scan_label: str
    instance_id: str
    start_after: ReadPosition
    announce_only: bool
    def __init__(self, tenant: _Optional[str] = ..., subject_label: _Optional[str] = ..., session_label: _Optional[str] = ..., scan_label: _Optional[str] = ..., instance_id: _Optional[str] = ..., start_after: _Optional[_Union[ReadPosition, _Mapping]] = ..., announce_only: _Optional[bool] = ...) -> None: ...

class ReadFilesResponse(_message.Message):
    __slots__ = ("file", "chunk", "scan")
    FILE_FIELD_NUMBER: _ClassVar[int]
    CHUNK_FIELD_NUMBER: _ClassVar[int]
    SCAN_FIELD_NUMBER: _ClassVar[int]
    file: ArchivedFile
    chunk: FileChunk
    scan: ScanMarker
    def __init__(self, file: _Optional[_Union[ArchivedFile, _Mapping]] = ..., chunk: _Optional[_Union[FileChunk, _Mapping]] = ..., scan: _Optional[_Union[ScanMarker, _Mapping]] = ...) -> None: ...
