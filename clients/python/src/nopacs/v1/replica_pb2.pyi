import datetime

from google.protobuf import duration_pb2 as _duration_pb2
from google.protobuf import timestamp_pb2 as _timestamp_pb2
from nopacs.v1 import job_pb2 as _job_pb2
from nopacs.v1 import storage_pb2 as _storage_pb2
from google.protobuf.internal import containers as _containers
from google.protobuf import descriptor as _descriptor
from google.protobuf import message as _message
from collections.abc import Iterable as _Iterable, Mapping as _Mapping
from typing import ClassVar as _ClassVar, Optional as _Optional, Union as _Union

DESCRIPTOR: _descriptor.FileDescriptor

class ListReplicasRequest(_message.Message):
    __slots__ = ("tenant", "primary", "page_size", "page_token")
    TENANT_FIELD_NUMBER: _ClassVar[int]
    PRIMARY_FIELD_NUMBER: _ClassVar[int]
    PAGE_SIZE_FIELD_NUMBER: _ClassVar[int]
    PAGE_TOKEN_FIELD_NUMBER: _ClassVar[int]
    tenant: str
    primary: str
    page_size: int
    page_token: str
    def __init__(self, tenant: _Optional[str] = ..., primary: _Optional[str] = ..., page_size: _Optional[int] = ..., page_token: _Optional[str] = ...) -> None: ...

class ListReplicasResponse(_message.Message):
    __slots__ = ("replicas", "next_page_token")
    REPLICAS_FIELD_NUMBER: _ClassVar[int]
    NEXT_PAGE_TOKEN_FIELD_NUMBER: _ClassVar[int]
    replicas: _containers.RepeatedCompositeFieldContainer[_storage_pb2.ReplicaStatus]
    next_page_token: str
    def __init__(self, replicas: _Optional[_Iterable[_Union[_storage_pb2.ReplicaStatus, _Mapping]]] = ..., next_page_token: _Optional[str] = ...) -> None: ...

class AttachReplicaRequest(_message.Message):
    __slots__ = ("tenant", "replica", "primary")
    TENANT_FIELD_NUMBER: _ClassVar[int]
    REPLICA_FIELD_NUMBER: _ClassVar[int]
    PRIMARY_FIELD_NUMBER: _ClassVar[int]
    tenant: str
    replica: str
    primary: str
    def __init__(self, tenant: _Optional[str] = ..., replica: _Optional[str] = ..., primary: _Optional[str] = ...) -> None: ...

class AttachReplicaResponse(_message.Message):
    __slots__ = ("storage", "backfill")
    STORAGE_FIELD_NUMBER: _ClassVar[int]
    BACKFILL_FIELD_NUMBER: _ClassVar[int]
    storage: _storage_pb2.Storage
    backfill: _job_pb2.Job
    def __init__(self, storage: _Optional[_Union[_storage_pb2.Storage, _Mapping]] = ..., backfill: _Optional[_Union[_job_pb2.Job, _Mapping]] = ...) -> None: ...

class DetachReplicaRequest(_message.Message):
    __slots__ = ("tenant", "replica", "abandon_purges", "abandon_reason")
    TENANT_FIELD_NUMBER: _ClassVar[int]
    REPLICA_FIELD_NUMBER: _ClassVar[int]
    ABANDON_PURGES_FIELD_NUMBER: _ClassVar[int]
    ABANDON_REASON_FIELD_NUMBER: _ClassVar[int]
    tenant: str
    replica: str
    abandon_purges: bool
    abandon_reason: str
    def __init__(self, tenant: _Optional[str] = ..., replica: _Optional[str] = ..., abandon_purges: _Optional[bool] = ..., abandon_reason: _Optional[str] = ...) -> None: ...

class DetachReplicaResponse(_message.Message):
    __slots__ = ("storage",)
    STORAGE_FIELD_NUMBER: _ClassVar[int]
    storage: _storage_pb2.Storage
    def __init__(self, storage: _Optional[_Union[_storage_pb2.Storage, _Mapping]] = ...) -> None: ...

class BackfillReplicaRequest(_message.Message):
    __slots__ = ("tenant", "replica")
    TENANT_FIELD_NUMBER: _ClassVar[int]
    REPLICA_FIELD_NUMBER: _ClassVar[int]
    tenant: str
    replica: str
    def __init__(self, tenant: _Optional[str] = ..., replica: _Optional[str] = ...) -> None: ...

class BackfillReplicaResponse(_message.Message):
    __slots__ = ("job",)
    JOB_FIELD_NUMBER: _ClassVar[int]
    job: _job_pb2.Job
    def __init__(self, job: _Optional[_Union[_job_pb2.Job, _Mapping]] = ...) -> None: ...

class RepairRequest(_message.Message):
    __slots__ = ("tenant", "storage", "findings", "all")
    TENANT_FIELD_NUMBER: _ClassVar[int]
    STORAGE_FIELD_NUMBER: _ClassVar[int]
    FROM_FIELD_NUMBER: _ClassVar[int]
    FINDINGS_FIELD_NUMBER: _ClassVar[int]
    ALL_FIELD_NUMBER: _ClassVar[int]
    tenant: str
    storage: str
    findings: _containers.RepeatedScalarFieldContainer[str]
    all: bool
    def __init__(self, tenant: _Optional[str] = ..., storage: _Optional[str] = ..., findings: _Optional[_Iterable[str]] = ..., all: _Optional[bool] = ..., **kwargs) -> None: ...

class RepairResponse(_message.Message):
    __slots__ = ("job",)
    JOB_FIELD_NUMBER: _ClassVar[int]
    job: _job_pb2.Job
    def __init__(self, job: _Optional[_Union[_job_pb2.Job, _Mapping]] = ...) -> None: ...

class RecallRequest(_message.Message):
    __slots__ = ("tenant", "subject_label", "session_label", "scan_label", "storage", "days", "validate_only", "confirm")
    TENANT_FIELD_NUMBER: _ClassVar[int]
    SUBJECT_LABEL_FIELD_NUMBER: _ClassVar[int]
    SESSION_LABEL_FIELD_NUMBER: _ClassVar[int]
    SCAN_LABEL_FIELD_NUMBER: _ClassVar[int]
    STORAGE_FIELD_NUMBER: _ClassVar[int]
    DAYS_FIELD_NUMBER: _ClassVar[int]
    VALIDATE_ONLY_FIELD_NUMBER: _ClassVar[int]
    CONFIRM_FIELD_NUMBER: _ClassVar[int]
    tenant: str
    subject_label: str
    session_label: str
    scan_label: str
    storage: str
    days: int
    validate_only: bool
    confirm: str
    def __init__(self, tenant: _Optional[str] = ..., subject_label: _Optional[str] = ..., session_label: _Optional[str] = ..., scan_label: _Optional[str] = ..., storage: _Optional[str] = ..., days: _Optional[int] = ..., validate_only: _Optional[bool] = ..., confirm: _Optional[str] = ...) -> None: ...

class RecallResponse(_message.Message):
    __slots__ = ("storages", "objects", "bytes", "objects_online", "objects_recalling", "objects_requested", "bytes_requested", "confirm")
    STORAGES_FIELD_NUMBER: _ClassVar[int]
    OBJECTS_FIELD_NUMBER: _ClassVar[int]
    BYTES_FIELD_NUMBER: _ClassVar[int]
    OBJECTS_ONLINE_FIELD_NUMBER: _ClassVar[int]
    OBJECTS_RECALLING_FIELD_NUMBER: _ClassVar[int]
    OBJECTS_REQUESTED_FIELD_NUMBER: _ClassVar[int]
    BYTES_REQUESTED_FIELD_NUMBER: _ClassVar[int]
    CONFIRM_FIELD_NUMBER: _ClassVar[int]
    storages: _containers.RepeatedScalarFieldContainer[str]
    objects: int
    bytes: int
    objects_online: int
    objects_recalling: int
    objects_requested: int
    bytes_requested: int
    confirm: str
    def __init__(self, storages: _Optional[_Iterable[str]] = ..., objects: _Optional[int] = ..., bytes: _Optional[int] = ..., objects_online: _Optional[int] = ..., objects_recalling: _Optional[int] = ..., objects_requested: _Optional[int] = ..., bytes_requested: _Optional[int] = ..., confirm: _Optional[str] = ...) -> None: ...

class SwitchStorageRequest(_message.Message):
    __slots__ = ("tenant", "primary", "replica", "validate_only", "drain_timeout", "confirm", "skip_verification", "skip_reason")
    TENANT_FIELD_NUMBER: _ClassVar[int]
    PRIMARY_FIELD_NUMBER: _ClassVar[int]
    REPLICA_FIELD_NUMBER: _ClassVar[int]
    VALIDATE_ONLY_FIELD_NUMBER: _ClassVar[int]
    DRAIN_TIMEOUT_FIELD_NUMBER: _ClassVar[int]
    CONFIRM_FIELD_NUMBER: _ClassVar[int]
    SKIP_VERIFICATION_FIELD_NUMBER: _ClassVar[int]
    SKIP_REASON_FIELD_NUMBER: _ClassVar[int]
    tenant: str
    primary: str
    replica: str
    validate_only: bool
    drain_timeout: _duration_pb2.Duration
    confirm: str
    skip_verification: bool
    skip_reason: str
    def __init__(self, tenant: _Optional[str] = ..., primary: _Optional[str] = ..., replica: _Optional[str] = ..., validate_only: _Optional[bool] = ..., drain_timeout: _Optional[_Union[datetime.timedelta, _duration_pb2.Duration, _Mapping]] = ..., confirm: _Optional[str] = ..., skip_verification: _Optional[bool] = ..., skip_reason: _Optional[str] = ...) -> None: ...

class SwitchStorageResponse(_message.Message):
    __slots__ = ("ready", "blockers", "scans_pending", "receivers", "primary", "replica", "pause", "associations_refused", "switch_time", "confirm", "switch_generation")
    READY_FIELD_NUMBER: _ClassVar[int]
    BLOCKERS_FIELD_NUMBER: _ClassVar[int]
    SCANS_PENDING_FIELD_NUMBER: _ClassVar[int]
    RECEIVERS_FIELD_NUMBER: _ClassVar[int]
    PRIMARY_FIELD_NUMBER: _ClassVar[int]
    REPLICA_FIELD_NUMBER: _ClassVar[int]
    PAUSE_FIELD_NUMBER: _ClassVar[int]
    ASSOCIATIONS_REFUSED_FIELD_NUMBER: _ClassVar[int]
    SWITCH_TIME_FIELD_NUMBER: _ClassVar[int]
    CONFIRM_FIELD_NUMBER: _ClassVar[int]
    SWITCH_GENERATION_FIELD_NUMBER: _ClassVar[int]
    ready: bool
    blockers: _containers.RepeatedScalarFieldContainer[str]
    scans_pending: int
    receivers: _containers.RepeatedScalarFieldContainer[str]
    primary: _storage_pb2.Storage
    replica: _storage_pb2.Storage
    pause: _duration_pb2.Duration
    associations_refused: int
    switch_time: _timestamp_pb2.Timestamp
    confirm: str
    switch_generation: int
    def __init__(self, ready: _Optional[bool] = ..., blockers: _Optional[_Iterable[str]] = ..., scans_pending: _Optional[int] = ..., receivers: _Optional[_Iterable[str]] = ..., primary: _Optional[_Union[_storage_pb2.Storage, _Mapping]] = ..., replica: _Optional[_Union[_storage_pb2.Storage, _Mapping]] = ..., pause: _Optional[_Union[datetime.timedelta, _duration_pb2.Duration, _Mapping]] = ..., associations_refused: _Optional[int] = ..., switch_time: _Optional[_Union[datetime.datetime, _timestamp_pb2.Timestamp, _Mapping]] = ..., confirm: _Optional[str] = ..., switch_generation: _Optional[int] = ...) -> None: ...

class VerifySampleRequest(_message.Message):
    __slots__ = ("tenant", "storage", "count", "seed")
    TENANT_FIELD_NUMBER: _ClassVar[int]
    STORAGE_FIELD_NUMBER: _ClassVar[int]
    COUNT_FIELD_NUMBER: _ClassVar[int]
    SEED_FIELD_NUMBER: _ClassVar[int]
    tenant: str
    storage: str
    count: int
    seed: int
    def __init__(self, tenant: _Optional[str] = ..., storage: _Optional[str] = ..., count: _Optional[int] = ..., seed: _Optional[int] = ...) -> None: ...

class SampledObject(_message.Message):
    __slots__ = ("key", "size_bytes", "sha256", "instance_id", "revision", "subject_label", "session_label", "scan_label")
    KEY_FIELD_NUMBER: _ClassVar[int]
    SIZE_BYTES_FIELD_NUMBER: _ClassVar[int]
    SHA256_FIELD_NUMBER: _ClassVar[int]
    INSTANCE_ID_FIELD_NUMBER: _ClassVar[int]
    REVISION_FIELD_NUMBER: _ClassVar[int]
    SUBJECT_LABEL_FIELD_NUMBER: _ClassVar[int]
    SESSION_LABEL_FIELD_NUMBER: _ClassVar[int]
    SCAN_LABEL_FIELD_NUMBER: _ClassVar[int]
    key: str
    size_bytes: int
    sha256: str
    instance_id: str
    revision: int
    subject_label: str
    session_label: str
    scan_label: str
    def __init__(self, key: _Optional[str] = ..., size_bytes: _Optional[int] = ..., sha256: _Optional[str] = ..., instance_id: _Optional[str] = ..., revision: _Optional[int] = ..., subject_label: _Optional[str] = ..., session_label: _Optional[str] = ..., scan_label: _Optional[str] = ...) -> None: ...

class VerifySampleResponse(_message.Message):
    __slots__ = ("objects", "population")
    OBJECTS_FIELD_NUMBER: _ClassVar[int]
    POPULATION_FIELD_NUMBER: _ClassVar[int]
    objects: _containers.RepeatedCompositeFieldContainer[SampledObject]
    population: int
    def __init__(self, objects: _Optional[_Iterable[_Union[SampledObject, _Mapping]]] = ..., population: _Optional[int] = ...) -> None: ...
