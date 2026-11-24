import datetime

from google.protobuf import timestamp_pb2 as _timestamp_pb2
from nopacs.v1 import receiver_pb2 as _receiver_pb2
from google.protobuf.internal import containers as _containers
from google.protobuf.internal import enum_type_wrapper as _enum_type_wrapper
from google.protobuf import descriptor as _descriptor
from google.protobuf import message as _message
from collections.abc import Iterable as _Iterable, Mapping as _Mapping
from typing import ClassVar as _ClassVar, Optional as _Optional, Union as _Union

DESCRIPTOR: _descriptor.FileDescriptor

class QuarantineReason(int, metaclass=_enum_type_wrapper.EnumTypeWrapper):
    __slots__ = ()
    QUARANTINE_REASON_UNSPECIFIED: _ClassVar[QuarantineReason]
    QUARANTINE_REASON_NOT_DICOM: _ClassVar[QuarantineReason]
    QUARANTINE_REASON_SOP_CLASS_NOT_ACCEPTED: _ClassVar[QuarantineReason]
    QUARANTINE_REASON_TOO_LARGE: _ClassVar[QuarantineReason]
    QUARANTINE_REASON_BAD_UID: _ClassVar[QuarantineReason]
    QUARANTINE_REASON_BAD_LABEL: _ClassVar[QuarantineReason]
    QUARANTINE_REASON_LABEL_CONFLICT: _ClassVar[QuarantineReason]
    QUARANTINE_REASON_DIFFERENT_FILE: _ClassVar[QuarantineReason]
    QUARANTINE_REASON_UID_ELSEWHERE: _ClassVar[QuarantineReason]
    QUARANTINE_REASON_STORAGE_REFUSED: _ClassVar[QuarantineReason]
    QUARANTINE_REASON_READ_BACK_MISMATCH: _ClassVar[QuarantineReason]
    QUARANTINE_REASON_HEADER_TOO_LARGE: _ClassVar[QuarantineReason]
    QUARANTINE_REASON_INCOMING_LOST: _ClassVar[QuarantineReason]
    QUARANTINE_REASON_UID_MISMATCH: _ClassVar[QuarantineReason]
    QUARANTINE_REASON_DEID_FAILED: _ClassVar[QuarantineReason]
    QUARANTINE_REASON_RESIDUAL_VALUE: _ClassVar[QuarantineReason]
    QUARANTINE_REASON_PROFILE_REFUSED: _ClassVar[QuarantineReason]
    QUARANTINE_REASON_UNROUTED: _ClassVar[QuarantineReason]
    QUARANTINE_REASON_BURNED_IN_RISK: _ClassVar[QuarantineReason]
    QUARANTINE_REASON_WAS_DELETED: _ClassVar[QuarantineReason]
    QUARANTINE_REASON_REPAIRS_DAMAGED: _ClassVar[QuarantineReason]

class QuarantineState(int, metaclass=_enum_type_wrapper.EnumTypeWrapper):
    __slots__ = ()
    QUARANTINE_STATE_UNSPECIFIED: _ClassVar[QuarantineState]
    QUARANTINE_STATE_HELD: _ClassVar[QuarantineState]
    QUARANTINE_STATE_RELEASED: _ClassVar[QuarantineState]
    QUARANTINE_STATE_REJECTED: _ClassVar[QuarantineState]

class ReleaseMode(int, metaclass=_enum_type_wrapper.EnumTypeWrapper):
    __slots__ = ()
    RELEASE_MODE_UNSPECIFIED: _ClassVar[ReleaseMode]
    RELEASE_MODE_AS_IS: _ClassVar[ReleaseMode]
QUARANTINE_REASON_UNSPECIFIED: QuarantineReason
QUARANTINE_REASON_NOT_DICOM: QuarantineReason
QUARANTINE_REASON_SOP_CLASS_NOT_ACCEPTED: QuarantineReason
QUARANTINE_REASON_TOO_LARGE: QuarantineReason
QUARANTINE_REASON_BAD_UID: QuarantineReason
QUARANTINE_REASON_BAD_LABEL: QuarantineReason
QUARANTINE_REASON_LABEL_CONFLICT: QuarantineReason
QUARANTINE_REASON_DIFFERENT_FILE: QuarantineReason
QUARANTINE_REASON_UID_ELSEWHERE: QuarantineReason
QUARANTINE_REASON_STORAGE_REFUSED: QuarantineReason
QUARANTINE_REASON_READ_BACK_MISMATCH: QuarantineReason
QUARANTINE_REASON_HEADER_TOO_LARGE: QuarantineReason
QUARANTINE_REASON_INCOMING_LOST: QuarantineReason
QUARANTINE_REASON_UID_MISMATCH: QuarantineReason
QUARANTINE_REASON_DEID_FAILED: QuarantineReason
QUARANTINE_REASON_RESIDUAL_VALUE: QuarantineReason
QUARANTINE_REASON_PROFILE_REFUSED: QuarantineReason
QUARANTINE_REASON_UNROUTED: QuarantineReason
QUARANTINE_REASON_BURNED_IN_RISK: QuarantineReason
QUARANTINE_REASON_WAS_DELETED: QuarantineReason
QUARANTINE_REASON_REPAIRS_DAMAGED: QuarantineReason
QUARANTINE_STATE_UNSPECIFIED: QuarantineState
QUARANTINE_STATE_HELD: QuarantineState
QUARANTINE_STATE_RELEASED: QuarantineState
QUARANTINE_STATE_REJECTED: QuarantineState
RELEASE_MODE_UNSPECIFIED: ReleaseMode
RELEASE_MODE_AS_IS: ReleaseMode

class Caller(_message.Message):
    __slots__ = ("calling_ae_title", "called_ae_title", "address")
    CALLING_AE_TITLE_FIELD_NUMBER: _ClassVar[int]
    CALLED_AE_TITLE_FIELD_NUMBER: _ClassVar[int]
    ADDRESS_FIELD_NUMBER: _ClassVar[int]
    calling_ae_title: str
    called_ae_title: str
    address: str
    def __init__(self, calling_ae_title: _Optional[str] = ..., called_ae_title: _Optional[str] = ..., address: _Optional[str] = ...) -> None: ...

class QuarantinedFile(_message.Message):
    __slots__ = ("id", "reason", "details", "state", "receiver", "reception", "caller", "size_bytes", "sha256", "kept_sha256", "sop_class_uid", "sop_instance_uid", "study_instance_uid", "series_instance_uid", "transfer_syntax_uid", "content_kept", "quarantine_time", "decide_time", "release_mode", "release_reception", "reject_reason", "released_from", "release_receiver", "release_reason", "release_storage", "release_profile", "import_job", "import_path")
    class DetailsEntry(_message.Message):
        __slots__ = ("key", "value")
        KEY_FIELD_NUMBER: _ClassVar[int]
        VALUE_FIELD_NUMBER: _ClassVar[int]
        key: str
        value: str
        def __init__(self, key: _Optional[str] = ..., value: _Optional[str] = ...) -> None: ...
    ID_FIELD_NUMBER: _ClassVar[int]
    REASON_FIELD_NUMBER: _ClassVar[int]
    DETAILS_FIELD_NUMBER: _ClassVar[int]
    STATE_FIELD_NUMBER: _ClassVar[int]
    RECEIVER_FIELD_NUMBER: _ClassVar[int]
    RECEPTION_FIELD_NUMBER: _ClassVar[int]
    CALLER_FIELD_NUMBER: _ClassVar[int]
    SIZE_BYTES_FIELD_NUMBER: _ClassVar[int]
    SHA256_FIELD_NUMBER: _ClassVar[int]
    KEPT_SHA256_FIELD_NUMBER: _ClassVar[int]
    SOP_CLASS_UID_FIELD_NUMBER: _ClassVar[int]
    SOP_INSTANCE_UID_FIELD_NUMBER: _ClassVar[int]
    STUDY_INSTANCE_UID_FIELD_NUMBER: _ClassVar[int]
    SERIES_INSTANCE_UID_FIELD_NUMBER: _ClassVar[int]
    TRANSFER_SYNTAX_UID_FIELD_NUMBER: _ClassVar[int]
    CONTENT_KEPT_FIELD_NUMBER: _ClassVar[int]
    QUARANTINE_TIME_FIELD_NUMBER: _ClassVar[int]
    DECIDE_TIME_FIELD_NUMBER: _ClassVar[int]
    RELEASE_MODE_FIELD_NUMBER: _ClassVar[int]
    RELEASE_RECEPTION_FIELD_NUMBER: _ClassVar[int]
    REJECT_REASON_FIELD_NUMBER: _ClassVar[int]
    RELEASED_FROM_FIELD_NUMBER: _ClassVar[int]
    RELEASE_RECEIVER_FIELD_NUMBER: _ClassVar[int]
    RELEASE_REASON_FIELD_NUMBER: _ClassVar[int]
    RELEASE_STORAGE_FIELD_NUMBER: _ClassVar[int]
    RELEASE_PROFILE_FIELD_NUMBER: _ClassVar[int]
    IMPORT_JOB_FIELD_NUMBER: _ClassVar[int]
    IMPORT_PATH_FIELD_NUMBER: _ClassVar[int]
    id: str
    reason: QuarantineReason
    details: _containers.ScalarMap[str, str]
    state: QuarantineState
    receiver: str
    reception: str
    caller: Caller
    size_bytes: int
    sha256: str
    kept_sha256: str
    sop_class_uid: str
    sop_instance_uid: str
    study_instance_uid: str
    series_instance_uid: str
    transfer_syntax_uid: str
    content_kept: bool
    quarantine_time: _timestamp_pb2.Timestamp
    decide_time: _timestamp_pb2.Timestamp
    release_mode: ReleaseMode
    release_reception: str
    reject_reason: str
    released_from: str
    release_receiver: str
    release_reason: str
    release_storage: str
    release_profile: _receiver_pb2.ProfileVersionRef
    import_job: str
    import_path: str
    def __init__(self, id: _Optional[str] = ..., reason: _Optional[_Union[QuarantineReason, str]] = ..., details: _Optional[_Mapping[str, str]] = ..., state: _Optional[_Union[QuarantineState, str]] = ..., receiver: _Optional[str] = ..., reception: _Optional[str] = ..., caller: _Optional[_Union[Caller, _Mapping]] = ..., size_bytes: _Optional[int] = ..., sha256: _Optional[str] = ..., kept_sha256: _Optional[str] = ..., sop_class_uid: _Optional[str] = ..., sop_instance_uid: _Optional[str] = ..., study_instance_uid: _Optional[str] = ..., series_instance_uid: _Optional[str] = ..., transfer_syntax_uid: _Optional[str] = ..., content_kept: _Optional[bool] = ..., quarantine_time: _Optional[_Union[datetime.datetime, _timestamp_pb2.Timestamp, _Mapping]] = ..., decide_time: _Optional[_Union[datetime.datetime, _timestamp_pb2.Timestamp, _Mapping]] = ..., release_mode: _Optional[_Union[ReleaseMode, str]] = ..., release_reception: _Optional[str] = ..., reject_reason: _Optional[str] = ..., released_from: _Optional[str] = ..., release_receiver: _Optional[str] = ..., release_reason: _Optional[str] = ..., release_storage: _Optional[str] = ..., release_profile: _Optional[_Union[_receiver_pb2.ProfileVersionRef, _Mapping]] = ..., import_job: _Optional[str] = ..., import_path: _Optional[str] = ...) -> None: ...

class ListQuarantinedFilesRequest(_message.Message):
    __slots__ = ("tenant", "state", "reason", "receiver", "reception", "start_time", "end_time", "page_size", "page_token", "study_instance_uid", "sop_instance_uid")
    TENANT_FIELD_NUMBER: _ClassVar[int]
    STATE_FIELD_NUMBER: _ClassVar[int]
    REASON_FIELD_NUMBER: _ClassVar[int]
    RECEIVER_FIELD_NUMBER: _ClassVar[int]
    RECEPTION_FIELD_NUMBER: _ClassVar[int]
    START_TIME_FIELD_NUMBER: _ClassVar[int]
    END_TIME_FIELD_NUMBER: _ClassVar[int]
    PAGE_SIZE_FIELD_NUMBER: _ClassVar[int]
    PAGE_TOKEN_FIELD_NUMBER: _ClassVar[int]
    STUDY_INSTANCE_UID_FIELD_NUMBER: _ClassVar[int]
    SOP_INSTANCE_UID_FIELD_NUMBER: _ClassVar[int]
    tenant: str
    state: QuarantineState
    reason: QuarantineReason
    receiver: str
    reception: str
    start_time: _timestamp_pb2.Timestamp
    end_time: _timestamp_pb2.Timestamp
    page_size: int
    page_token: str
    study_instance_uid: str
    sop_instance_uid: str
    def __init__(self, tenant: _Optional[str] = ..., state: _Optional[_Union[QuarantineState, str]] = ..., reason: _Optional[_Union[QuarantineReason, str]] = ..., receiver: _Optional[str] = ..., reception: _Optional[str] = ..., start_time: _Optional[_Union[datetime.datetime, _timestamp_pb2.Timestamp, _Mapping]] = ..., end_time: _Optional[_Union[datetime.datetime, _timestamp_pb2.Timestamp, _Mapping]] = ..., page_size: _Optional[int] = ..., page_token: _Optional[str] = ..., study_instance_uid: _Optional[str] = ..., sop_instance_uid: _Optional[str] = ...) -> None: ...

class ListQuarantinedFilesResponse(_message.Message):
    __slots__ = ("files", "next_page_token")
    FILES_FIELD_NUMBER: _ClassVar[int]
    NEXT_PAGE_TOKEN_FIELD_NUMBER: _ClassVar[int]
    files: _containers.RepeatedCompositeFieldContainer[QuarantinedFile]
    next_page_token: str
    def __init__(self, files: _Optional[_Iterable[_Union[QuarantinedFile, _Mapping]]] = ..., next_page_token: _Optional[str] = ...) -> None: ...

class GetQuarantinedFileRequest(_message.Message):
    __slots__ = ("tenant", "id", "include_header")
    TENANT_FIELD_NUMBER: _ClassVar[int]
    ID_FIELD_NUMBER: _ClassVar[int]
    INCLUDE_HEADER_FIELD_NUMBER: _ClassVar[int]
    tenant: str
    id: str
    include_header: bool
    def __init__(self, tenant: _Optional[str] = ..., id: _Optional[str] = ..., include_header: _Optional[bool] = ...) -> None: ...

class GetQuarantinedFileResponse(_message.Message):
    __slots__ = ("file", "header_json", "header_too_large")
    FILE_FIELD_NUMBER: _ClassVar[int]
    HEADER_JSON_FIELD_NUMBER: _ClassVar[int]
    HEADER_TOO_LARGE_FIELD_NUMBER: _ClassVar[int]
    file: QuarantinedFile
    header_json: str
    header_too_large: bool
    def __init__(self, file: _Optional[_Union[QuarantinedFile, _Mapping]] = ..., header_json: _Optional[str] = ..., header_too_large: _Optional[bool] = ...) -> None: ...

class ReleaseQuarantinedFileRequest(_message.Message):
    __slots__ = ("tenant", "id", "mode", "receiver", "reason", "storage", "profile")
    TENANT_FIELD_NUMBER: _ClassVar[int]
    ID_FIELD_NUMBER: _ClassVar[int]
    MODE_FIELD_NUMBER: _ClassVar[int]
    RECEIVER_FIELD_NUMBER: _ClassVar[int]
    REASON_FIELD_NUMBER: _ClassVar[int]
    STORAGE_FIELD_NUMBER: _ClassVar[int]
    PROFILE_FIELD_NUMBER: _ClassVar[int]
    tenant: str
    id: str
    mode: ReleaseMode
    receiver: str
    reason: str
    storage: str
    profile: _receiver_pb2.ProfileVersionRef
    def __init__(self, tenant: _Optional[str] = ..., id: _Optional[str] = ..., mode: _Optional[_Union[ReleaseMode, str]] = ..., receiver: _Optional[str] = ..., reason: _Optional[str] = ..., storage: _Optional[str] = ..., profile: _Optional[_Union[_receiver_pb2.ProfileVersionRef, _Mapping]] = ...) -> None: ...

class ReleaseQuarantinedFileResponse(_message.Message):
    __slots__ = ("file",)
    FILE_FIELD_NUMBER: _ClassVar[int]
    file: QuarantinedFile
    def __init__(self, file: _Optional[_Union[QuarantinedFile, _Mapping]] = ...) -> None: ...

class RejectQuarantinedFileRequest(_message.Message):
    __slots__ = ("tenant", "id", "reason")
    TENANT_FIELD_NUMBER: _ClassVar[int]
    ID_FIELD_NUMBER: _ClassVar[int]
    REASON_FIELD_NUMBER: _ClassVar[int]
    tenant: str
    id: str
    reason: str
    def __init__(self, tenant: _Optional[str] = ..., id: _Optional[str] = ..., reason: _Optional[str] = ...) -> None: ...

class RejectQuarantinedFileResponse(_message.Message):
    __slots__ = ("file",)
    FILE_FIELD_NUMBER: _ClassVar[int]
    file: QuarantinedFile
    def __init__(self, file: _Optional[_Union[QuarantinedFile, _Mapping]] = ...) -> None: ...
