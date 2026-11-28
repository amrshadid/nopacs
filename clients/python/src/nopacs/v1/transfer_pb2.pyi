from nopacs.v1 import job_pb2 as _job_pb2
from google.protobuf.internal import containers as _containers
from google.protobuf.internal import enum_type_wrapper as _enum_type_wrapper
from google.protobuf import descriptor as _descriptor
from google.protobuf import message as _message
from collections.abc import Iterable as _Iterable, Mapping as _Mapping
from typing import ClassVar as _ClassVar, Optional as _Optional, Union as _Union

DESCRIPTOR: _descriptor.FileDescriptor

class TransferStep(int, metaclass=_enum_type_wrapper.EnumTypeWrapper):
    __slots__ = ()
    TRANSFER_STEP_UNSPECIFIED: _ClassVar[TransferStep]
    TRANSFER_STEP_LISTING: _ClassVar[TransferStep]
    TRANSFER_STEP_COPYING: _ClassVar[TransferStep]
    TRANSFER_STEP_CONFIRMING: _ClassVar[TransferStep]
    TRANSFER_STEP_DELETING: _ClassVar[TransferStep]
    TRANSFER_STEP_ENDED: _ClassVar[TransferStep]
TRANSFER_STEP_UNSPECIFIED: TransferStep
TRANSFER_STEP_LISTING: TransferStep
TRANSFER_STEP_COPYING: TransferStep
TRANSFER_STEP_CONFIRMING: TransferStep
TRANSFER_STEP_DELETING: TransferStep
TRANSFER_STEP_ENDED: TransferStep

class TransferItemCounts(_message.Message):
    __slots__ = ("listed", "copied", "confirmed", "failed")
    LISTED_FIELD_NUMBER: _ClassVar[int]
    COPIED_FIELD_NUMBER: _ClassVar[int]
    CONFIRMED_FIELD_NUMBER: _ClassVar[int]
    FAILED_FIELD_NUMBER: _ClassVar[int]
    listed: int
    copied: int
    confirmed: int
    failed: int
    def __init__(self, listed: _Optional[int] = ..., copied: _Optional[int] = ..., confirmed: _Optional[int] = ..., failed: _Optional[int] = ...) -> None: ...

class Transfer(_message.Message):
    __slots__ = ("job", "subject_label", "session_label", "target_tenant", "receiver", "move", "reason", "step", "items", "delete_correction_id")
    JOB_FIELD_NUMBER: _ClassVar[int]
    SUBJECT_LABEL_FIELD_NUMBER: _ClassVar[int]
    SESSION_LABEL_FIELD_NUMBER: _ClassVar[int]
    TARGET_TENANT_FIELD_NUMBER: _ClassVar[int]
    RECEIVER_FIELD_NUMBER: _ClassVar[int]
    MOVE_FIELD_NUMBER: _ClassVar[int]
    REASON_FIELD_NUMBER: _ClassVar[int]
    STEP_FIELD_NUMBER: _ClassVar[int]
    ITEMS_FIELD_NUMBER: _ClassVar[int]
    DELETE_CORRECTION_ID_FIELD_NUMBER: _ClassVar[int]
    job: _job_pb2.Job
    subject_label: str
    session_label: str
    target_tenant: str
    receiver: str
    move: bool
    reason: str
    step: TransferStep
    items: TransferItemCounts
    delete_correction_id: str
    def __init__(self, job: _Optional[_Union[_job_pb2.Job, _Mapping]] = ..., subject_label: _Optional[str] = ..., session_label: _Optional[str] = ..., target_tenant: _Optional[str] = ..., receiver: _Optional[str] = ..., move: _Optional[bool] = ..., reason: _Optional[str] = ..., step: _Optional[_Union[TransferStep, str]] = ..., items: _Optional[_Union[TransferItemCounts, _Mapping]] = ..., delete_correction_id: _Optional[str] = ...) -> None: ...

class TransferPreview(_message.Message):
    __slots__ = ("sessions", "scans", "instances", "bytes", "receiver", "receiver_deidentifies", "refused_instances", "refusals_counted")
    SESSIONS_FIELD_NUMBER: _ClassVar[int]
    SCANS_FIELD_NUMBER: _ClassVar[int]
    INSTANCES_FIELD_NUMBER: _ClassVar[int]
    BYTES_FIELD_NUMBER: _ClassVar[int]
    RECEIVER_FIELD_NUMBER: _ClassVar[int]
    RECEIVER_DEIDENTIFIES_FIELD_NUMBER: _ClassVar[int]
    REFUSED_INSTANCES_FIELD_NUMBER: _ClassVar[int]
    REFUSALS_COUNTED_FIELD_NUMBER: _ClassVar[int]
    sessions: int
    scans: int
    instances: int
    bytes: int
    receiver: str
    receiver_deidentifies: bool
    refused_instances: int
    refusals_counted: bool
    def __init__(self, sessions: _Optional[int] = ..., scans: _Optional[int] = ..., instances: _Optional[int] = ..., bytes: _Optional[int] = ..., receiver: _Optional[str] = ..., receiver_deidentifies: _Optional[bool] = ..., refused_instances: _Optional[int] = ..., refusals_counted: _Optional[bool] = ...) -> None: ...

class CreateTransferRequest(_message.Message):
    __slots__ = ("tenant", "subject_label", "session_label", "target_tenant", "receiver", "move", "reason", "validate_only")
    TENANT_FIELD_NUMBER: _ClassVar[int]
    SUBJECT_LABEL_FIELD_NUMBER: _ClassVar[int]
    SESSION_LABEL_FIELD_NUMBER: _ClassVar[int]
    TARGET_TENANT_FIELD_NUMBER: _ClassVar[int]
    RECEIVER_FIELD_NUMBER: _ClassVar[int]
    MOVE_FIELD_NUMBER: _ClassVar[int]
    REASON_FIELD_NUMBER: _ClassVar[int]
    VALIDATE_ONLY_FIELD_NUMBER: _ClassVar[int]
    tenant: str
    subject_label: str
    session_label: str
    target_tenant: str
    receiver: str
    move: bool
    reason: str
    validate_only: bool
    def __init__(self, tenant: _Optional[str] = ..., subject_label: _Optional[str] = ..., session_label: _Optional[str] = ..., target_tenant: _Optional[str] = ..., receiver: _Optional[str] = ..., move: _Optional[bool] = ..., reason: _Optional[str] = ..., validate_only: _Optional[bool] = ...) -> None: ...

class CreateTransferResponse(_message.Message):
    __slots__ = ("preview", "transfer")
    PREVIEW_FIELD_NUMBER: _ClassVar[int]
    TRANSFER_FIELD_NUMBER: _ClassVar[int]
    preview: TransferPreview
    transfer: Transfer
    def __init__(self, preview: _Optional[_Union[TransferPreview, _Mapping]] = ..., transfer: _Optional[_Union[Transfer, _Mapping]] = ...) -> None: ...

class GetTransferRequest(_message.Message):
    __slots__ = ("tenant", "id")
    TENANT_FIELD_NUMBER: _ClassVar[int]
    ID_FIELD_NUMBER: _ClassVar[int]
    tenant: str
    id: str
    def __init__(self, tenant: _Optional[str] = ..., id: _Optional[str] = ...) -> None: ...

class GetTransferResponse(_message.Message):
    __slots__ = ("transfer",)
    TRANSFER_FIELD_NUMBER: _ClassVar[int]
    transfer: Transfer
    def __init__(self, transfer: _Optional[_Union[Transfer, _Mapping]] = ...) -> None: ...

class ListTransfersRequest(_message.Message):
    __slots__ = ("tenant", "page_size", "page_token")
    TENANT_FIELD_NUMBER: _ClassVar[int]
    PAGE_SIZE_FIELD_NUMBER: _ClassVar[int]
    PAGE_TOKEN_FIELD_NUMBER: _ClassVar[int]
    tenant: str
    page_size: int
    page_token: str
    def __init__(self, tenant: _Optional[str] = ..., page_size: _Optional[int] = ..., page_token: _Optional[str] = ...) -> None: ...

class ListTransfersResponse(_message.Message):
    __slots__ = ("transfers", "next_page_token")
    TRANSFERS_FIELD_NUMBER: _ClassVar[int]
    NEXT_PAGE_TOKEN_FIELD_NUMBER: _ClassVar[int]
    transfers: _containers.RepeatedCompositeFieldContainer[Transfer]
    next_page_token: str
    def __init__(self, transfers: _Optional[_Iterable[_Union[Transfer, _Mapping]]] = ..., next_page_token: _Optional[str] = ...) -> None: ...
