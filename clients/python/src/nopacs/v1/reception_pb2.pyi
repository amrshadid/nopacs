import datetime

from google.protobuf import timestamp_pb2 as _timestamp_pb2
from google.protobuf.internal import containers as _containers
from google.protobuf.internal import enum_type_wrapper as _enum_type_wrapper
from google.protobuf import descriptor as _descriptor
from google.protobuf import message as _message
from collections.abc import Iterable as _Iterable, Mapping as _Mapping
from typing import ClassVar as _ClassVar, Optional as _Optional, Union as _Union

DESCRIPTOR: _descriptor.FileDescriptor

class ReceptionStep(int, metaclass=_enum_type_wrapper.EnumTypeWrapper):
    __slots__ = ()
    RECEPTION_STEP_UNSPECIFIED: _ClassVar[ReceptionStep]
    RECEPTION_STEP_RECEIVING: _ClassVar[ReceptionStep]
    RECEPTION_STEP_COMPLETE: _ClassVar[ReceptionStep]
    RECEPTION_STEP_IDENTIFIED: _ClassVar[ReceptionStep]
    RECEPTION_STEP_ARCHIVED: _ClassVar[ReceptionStep]
    RECEPTION_STEP_VERIFIED: _ClassVar[ReceptionStep]
    RECEPTION_STEP_DONE: _ClassVar[ReceptionStep]
    RECEPTION_STEP_QUARANTINED: _ClassVar[ReceptionStep]
    RECEPTION_STEP_FAILED: _ClassVar[ReceptionStep]
RECEPTION_STEP_UNSPECIFIED: ReceptionStep
RECEPTION_STEP_RECEIVING: ReceptionStep
RECEPTION_STEP_COMPLETE: ReceptionStep
RECEPTION_STEP_IDENTIFIED: ReceptionStep
RECEPTION_STEP_ARCHIVED: ReceptionStep
RECEPTION_STEP_VERIFIED: ReceptionStep
RECEPTION_STEP_DONE: ReceptionStep
RECEPTION_STEP_QUARANTINED: ReceptionStep
RECEPTION_STEP_FAILED: ReceptionStep

class ReceptionCounts(_message.Message):
    __slots__ = ("files_received", "bytes_received", "files_archived", "bytes_archived", "files_duplicate", "bytes_duplicate", "files_quarantined", "bytes_quarantined", "files_released")
    FILES_RECEIVED_FIELD_NUMBER: _ClassVar[int]
    BYTES_RECEIVED_FIELD_NUMBER: _ClassVar[int]
    FILES_ARCHIVED_FIELD_NUMBER: _ClassVar[int]
    BYTES_ARCHIVED_FIELD_NUMBER: _ClassVar[int]
    FILES_DUPLICATE_FIELD_NUMBER: _ClassVar[int]
    BYTES_DUPLICATE_FIELD_NUMBER: _ClassVar[int]
    FILES_QUARANTINED_FIELD_NUMBER: _ClassVar[int]
    BYTES_QUARANTINED_FIELD_NUMBER: _ClassVar[int]
    FILES_RELEASED_FIELD_NUMBER: _ClassVar[int]
    files_received: int
    bytes_received: int
    files_archived: int
    bytes_archived: int
    files_duplicate: int
    bytes_duplicate: int
    files_quarantined: int
    bytes_quarantined: int
    files_released: int
    def __init__(self, files_received: _Optional[int] = ..., bytes_received: _Optional[int] = ..., files_archived: _Optional[int] = ..., bytes_archived: _Optional[int] = ..., files_duplicate: _Optional[int] = ..., bytes_duplicate: _Optional[int] = ..., files_quarantined: _Optional[int] = ..., bytes_quarantined: _Optional[int] = ..., files_released: _Optional[int] = ...) -> None: ...

class Reception(_message.Message):
    __slots__ = ("id", "receiver", "study_instance_uid", "arrival", "step", "counts", "subject_label", "session_label", "storage", "failure", "start_time", "last_file_time", "step_time", "end_time", "associations", "calling_ae_titles", "calling_ae_titles_truncated", "retry_of", "retry_reception", "discard_reason", "discard_time")
    ID_FIELD_NUMBER: _ClassVar[int]
    RECEIVER_FIELD_NUMBER: _ClassVar[int]
    STUDY_INSTANCE_UID_FIELD_NUMBER: _ClassVar[int]
    ARRIVAL_FIELD_NUMBER: _ClassVar[int]
    STEP_FIELD_NUMBER: _ClassVar[int]
    COUNTS_FIELD_NUMBER: _ClassVar[int]
    SUBJECT_LABEL_FIELD_NUMBER: _ClassVar[int]
    SESSION_LABEL_FIELD_NUMBER: _ClassVar[int]
    STORAGE_FIELD_NUMBER: _ClassVar[int]
    FAILURE_FIELD_NUMBER: _ClassVar[int]
    START_TIME_FIELD_NUMBER: _ClassVar[int]
    LAST_FILE_TIME_FIELD_NUMBER: _ClassVar[int]
    STEP_TIME_FIELD_NUMBER: _ClassVar[int]
    END_TIME_FIELD_NUMBER: _ClassVar[int]
    ASSOCIATIONS_FIELD_NUMBER: _ClassVar[int]
    CALLING_AE_TITLES_FIELD_NUMBER: _ClassVar[int]
    CALLING_AE_TITLES_TRUNCATED_FIELD_NUMBER: _ClassVar[int]
    RETRY_OF_FIELD_NUMBER: _ClassVar[int]
    RETRY_RECEPTION_FIELD_NUMBER: _ClassVar[int]
    DISCARD_REASON_FIELD_NUMBER: _ClassVar[int]
    DISCARD_TIME_FIELD_NUMBER: _ClassVar[int]
    id: str
    receiver: str
    study_instance_uid: str
    arrival: int
    step: ReceptionStep
    counts: ReceptionCounts
    subject_label: str
    session_label: str
    storage: str
    failure: str
    start_time: _timestamp_pb2.Timestamp
    last_file_time: _timestamp_pb2.Timestamp
    step_time: _timestamp_pb2.Timestamp
    end_time: _timestamp_pb2.Timestamp
    associations: int
    calling_ae_titles: _containers.RepeatedScalarFieldContainer[str]
    calling_ae_titles_truncated: bool
    retry_of: str
    retry_reception: str
    discard_reason: str
    discard_time: _timestamp_pb2.Timestamp
    def __init__(self, id: _Optional[str] = ..., receiver: _Optional[str] = ..., study_instance_uid: _Optional[str] = ..., arrival: _Optional[int] = ..., step: _Optional[_Union[ReceptionStep, str]] = ..., counts: _Optional[_Union[ReceptionCounts, _Mapping]] = ..., subject_label: _Optional[str] = ..., session_label: _Optional[str] = ..., storage: _Optional[str] = ..., failure: _Optional[str] = ..., start_time: _Optional[_Union[datetime.datetime, _timestamp_pb2.Timestamp, _Mapping]] = ..., last_file_time: _Optional[_Union[datetime.datetime, _timestamp_pb2.Timestamp, _Mapping]] = ..., step_time: _Optional[_Union[datetime.datetime, _timestamp_pb2.Timestamp, _Mapping]] = ..., end_time: _Optional[_Union[datetime.datetime, _timestamp_pb2.Timestamp, _Mapping]] = ..., associations: _Optional[int] = ..., calling_ae_titles: _Optional[_Iterable[str]] = ..., calling_ae_titles_truncated: _Optional[bool] = ..., retry_of: _Optional[str] = ..., retry_reception: _Optional[str] = ..., discard_reason: _Optional[str] = ..., discard_time: _Optional[_Union[datetime.datetime, _timestamp_pb2.Timestamp, _Mapping]] = ...) -> None: ...

class ReceptionScan(_message.Message):
    __slots__ = ("label", "series_instance_uid", "files", "bytes")
    LABEL_FIELD_NUMBER: _ClassVar[int]
    SERIES_INSTANCE_UID_FIELD_NUMBER: _ClassVar[int]
    FILES_FIELD_NUMBER: _ClassVar[int]
    BYTES_FIELD_NUMBER: _ClassVar[int]
    label: str
    series_instance_uid: str
    files: int
    bytes: int
    def __init__(self, label: _Optional[str] = ..., series_instance_uid: _Optional[str] = ..., files: _Optional[int] = ..., bytes: _Optional[int] = ...) -> None: ...

class ReceptionArrival(_message.Message):
    __slots__ = ("id", "arrival", "step", "start_time")
    ID_FIELD_NUMBER: _ClassVar[int]
    ARRIVAL_FIELD_NUMBER: _ClassVar[int]
    STEP_FIELD_NUMBER: _ClassVar[int]
    START_TIME_FIELD_NUMBER: _ClassVar[int]
    id: str
    arrival: int
    step: ReceptionStep
    start_time: _timestamp_pb2.Timestamp
    def __init__(self, id: _Optional[str] = ..., arrival: _Optional[int] = ..., step: _Optional[_Union[ReceptionStep, str]] = ..., start_time: _Optional[_Union[datetime.datetime, _timestamp_pb2.Timestamp, _Mapping]] = ...) -> None: ...

class ListReceptionsRequest(_message.Message):
    __slots__ = ("tenant", "receiver", "study_instance_uid", "steps", "in_progress", "start_time", "end_time", "page_size", "page_token")
    TENANT_FIELD_NUMBER: _ClassVar[int]
    RECEIVER_FIELD_NUMBER: _ClassVar[int]
    STUDY_INSTANCE_UID_FIELD_NUMBER: _ClassVar[int]
    STEPS_FIELD_NUMBER: _ClassVar[int]
    IN_PROGRESS_FIELD_NUMBER: _ClassVar[int]
    START_TIME_FIELD_NUMBER: _ClassVar[int]
    END_TIME_FIELD_NUMBER: _ClassVar[int]
    PAGE_SIZE_FIELD_NUMBER: _ClassVar[int]
    PAGE_TOKEN_FIELD_NUMBER: _ClassVar[int]
    tenant: str
    receiver: str
    study_instance_uid: str
    steps: _containers.RepeatedScalarFieldContainer[ReceptionStep]
    in_progress: bool
    start_time: _timestamp_pb2.Timestamp
    end_time: _timestamp_pb2.Timestamp
    page_size: int
    page_token: str
    def __init__(self, tenant: _Optional[str] = ..., receiver: _Optional[str] = ..., study_instance_uid: _Optional[str] = ..., steps: _Optional[_Iterable[_Union[ReceptionStep, str]]] = ..., in_progress: _Optional[bool] = ..., start_time: _Optional[_Union[datetime.datetime, _timestamp_pb2.Timestamp, _Mapping]] = ..., end_time: _Optional[_Union[datetime.datetime, _timestamp_pb2.Timestamp, _Mapping]] = ..., page_size: _Optional[int] = ..., page_token: _Optional[str] = ...) -> None: ...

class ListReceptionsResponse(_message.Message):
    __slots__ = ("receptions", "next_page_token")
    RECEPTIONS_FIELD_NUMBER: _ClassVar[int]
    NEXT_PAGE_TOKEN_FIELD_NUMBER: _ClassVar[int]
    receptions: _containers.RepeatedCompositeFieldContainer[Reception]
    next_page_token: str
    def __init__(self, receptions: _Optional[_Iterable[_Union[Reception, _Mapping]]] = ..., next_page_token: _Optional[str] = ...) -> None: ...

class GetReceptionRequest(_message.Message):
    __slots__ = ("tenant", "id")
    TENANT_FIELD_NUMBER: _ClassVar[int]
    ID_FIELD_NUMBER: _ClassVar[int]
    tenant: str
    id: str
    def __init__(self, tenant: _Optional[str] = ..., id: _Optional[str] = ...) -> None: ...

class GetReceptionResponse(_message.Message):
    __slots__ = ("reception", "scans", "scans_truncated", "arrivals")
    RECEPTION_FIELD_NUMBER: _ClassVar[int]
    SCANS_FIELD_NUMBER: _ClassVar[int]
    SCANS_TRUNCATED_FIELD_NUMBER: _ClassVar[int]
    ARRIVALS_FIELD_NUMBER: _ClassVar[int]
    reception: Reception
    scans: _containers.RepeatedCompositeFieldContainer[ReceptionScan]
    scans_truncated: bool
    arrivals: _containers.RepeatedCompositeFieldContainer[ReceptionArrival]
    def __init__(self, reception: _Optional[_Union[Reception, _Mapping]] = ..., scans: _Optional[_Iterable[_Union[ReceptionScan, _Mapping]]] = ..., scans_truncated: _Optional[bool] = ..., arrivals: _Optional[_Iterable[_Union[ReceptionArrival, _Mapping]]] = ...) -> None: ...

class RetryReceptionRequest(_message.Message):
    __slots__ = ("tenant", "id", "receiver")
    TENANT_FIELD_NUMBER: _ClassVar[int]
    ID_FIELD_NUMBER: _ClassVar[int]
    RECEIVER_FIELD_NUMBER: _ClassVar[int]
    tenant: str
    id: str
    receiver: str
    def __init__(self, tenant: _Optional[str] = ..., id: _Optional[str] = ..., receiver: _Optional[str] = ...) -> None: ...

class RetryReceptionResponse(_message.Message):
    __slots__ = ("reception", "retry")
    RECEPTION_FIELD_NUMBER: _ClassVar[int]
    RETRY_FIELD_NUMBER: _ClassVar[int]
    reception: Reception
    retry: Reception
    def __init__(self, reception: _Optional[_Union[Reception, _Mapping]] = ..., retry: _Optional[_Union[Reception, _Mapping]] = ...) -> None: ...

class DiscardReceptionRequest(_message.Message):
    __slots__ = ("tenant", "id", "reason")
    TENANT_FIELD_NUMBER: _ClassVar[int]
    ID_FIELD_NUMBER: _ClassVar[int]
    REASON_FIELD_NUMBER: _ClassVar[int]
    tenant: str
    id: str
    reason: str
    def __init__(self, tenant: _Optional[str] = ..., id: _Optional[str] = ..., reason: _Optional[str] = ...) -> None: ...

class DiscardReceptionResponse(_message.Message):
    __slots__ = ("reception",)
    RECEPTION_FIELD_NUMBER: _ClassVar[int]
    reception: Reception
    def __init__(self, reception: _Optional[_Union[Reception, _Mapping]] = ...) -> None: ...
