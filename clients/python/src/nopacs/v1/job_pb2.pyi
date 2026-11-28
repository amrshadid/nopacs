import datetime

from google.protobuf import timestamp_pb2 as _timestamp_pb2
from nopacs.v1 import quarantine_pb2 as _quarantine_pb2
from google.protobuf.internal import containers as _containers
from google.protobuf.internal import enum_type_wrapper as _enum_type_wrapper
from google.protobuf import descriptor as _descriptor
from google.protobuf import message as _message
from collections.abc import Iterable as _Iterable, Mapping as _Mapping
from typing import ClassVar as _ClassVar, Optional as _Optional, Union as _Union

DESCRIPTOR: _descriptor.FileDescriptor

class JobState(int, metaclass=_enum_type_wrapper.EnumTypeWrapper):
    __slots__ = ()
    JOB_STATE_UNSPECIFIED: _ClassVar[JobState]
    JOB_STATE_QUEUED: _ClassVar[JobState]
    JOB_STATE_RUNNING: _ClassVar[JobState]
    JOB_STATE_DONE: _ClassVar[JobState]
    JOB_STATE_PARTIAL: _ClassVar[JobState]
    JOB_STATE_FAILED: _ClassVar[JobState]
    JOB_STATE_CANCELED: _ClassVar[JobState]

class DeidJobMode(int, metaclass=_enum_type_wrapper.EnumTypeWrapper):
    __slots__ = ()
    DEID_JOB_MODE_UNSPECIFIED: _ClassVar[DeidJobMode]
    DEID_JOB_MODE_DRY_RUN: _ClassVar[DeidJobMode]
    DEID_JOB_MODE_CHECK: _ClassVar[DeidJobMode]

class RebuildJobMode(int, metaclass=_enum_type_wrapper.EnumTypeWrapper):
    __slots__ = ()
    REBUILD_JOB_MODE_UNSPECIFIED: _ClassVar[RebuildJobMode]
    REBUILD_JOB_MODE_COMPARE: _ClassVar[RebuildJobMode]
    REBUILD_JOB_MODE_REPLACE: _ClassVar[RebuildJobMode]

class ReplicateJobMode(int, metaclass=_enum_type_wrapper.EnumTypeWrapper):
    __slots__ = ()
    REPLICATE_JOB_MODE_UNSPECIFIED: _ClassVar[ReplicateJobMode]
    REPLICATE_JOB_MODE_BACKFILL: _ClassVar[ReplicateJobMode]

class ImportSkipCode(int, metaclass=_enum_type_wrapper.EnumTypeWrapper):
    __slots__ = ()
    IMPORT_SKIP_CODE_UNSPECIFIED: _ClassVar[ImportSkipCode]
    IMPORT_SKIP_CODE_NOT_PART10: _ClassVar[ImportSkipCode]
    IMPORT_SKIP_CODE_OUTSIDE_LAYOUT: _ClassVar[ImportSkipCode]
    IMPORT_SKIP_CODE_UNMAPPED_SESSION: _ClassVar[ImportSkipCode]
    IMPORT_SKIP_CODE_DIRECTORY_TOO_WIDE: _ClassVar[ImportSkipCode]
    IMPORT_SKIP_CODE_UNREADABLE: _ClassVar[ImportSkipCode]
    IMPORT_SKIP_CODE_NOT_REGULAR: _ClassVar[ImportSkipCode]
    IMPORT_SKIP_CODE_TOO_LARGE: _ClassVar[ImportSkipCode]
    IMPORT_SKIP_CODE_PATH_TOO_LONG: _ClassVar[ImportSkipCode]
    IMPORT_SKIP_CODE_BAD_NAME: _ClassVar[ImportSkipCode]

class JobFailureCode(int, metaclass=_enum_type_wrapper.EnumTypeWrapper):
    __slots__ = ()
    JOB_FAILURE_CODE_UNSPECIFIED: _ClassVar[JobFailureCode]
    JOB_FAILURE_CODE_PEER_REFUSED: _ClassVar[JobFailureCode]
    JOB_FAILURE_CODE_PEER_WARNING: _ClassVar[JobFailureCode]
    JOB_FAILURE_CODE_CLASS_NOT_ACCEPTED: _ClassVar[JobFailureCode]
    JOB_FAILURE_CODE_SYNTAX_NOT_ACCEPTED: _ClassVar[JobFailureCode]
    JOB_FAILURE_CODE_PEER_UNACCOUNTED: _ClassVar[JobFailureCode]
    JOB_FAILURE_CODE_DAMAGED_IN_STORAGE: _ClassVar[JobFailureCode]
    JOB_FAILURE_CODE_META_UNREADABLE: _ClassVar[JobFailureCode]
    JOB_FAILURE_CODE_WOULD_HOLD: _ClassVar[JobFailureCode]
    JOB_FAILURE_CODE_CHECK_FAILED: _ClassVar[JobFailureCode]
    JOB_FAILURE_CODE_UNDECIDED: _ClassVar[JobFailureCode]
    JOB_FAILURE_CODE_NOT_DEIDENTIFIED: _ClassVar[JobFailureCode]
    JOB_FAILURE_CODE_TOO_LARGE: _ClassVar[JobFailureCode]
    JOB_FAILURE_CODE_DELETED_IN_ARCHIVE: _ClassVar[JobFailureCode]
    JOB_FAILURE_CODE_NOT_ONLINE: _ClassVar[JobFailureCode]
    JOB_FAILURE_CODE_AS_RECEIVED: _ClassVar[JobFailureCode]
    JOB_FAILURE_CODE_NO_PROFILE_PROVENANCE: _ClassVar[JobFailureCode]
    JOB_FAILURE_CODE_PIXEL_RISK_ACCEPTED: _ClassVar[JobFailureCode]

class ImportFileKind(int, metaclass=_enum_type_wrapper.EnumTypeWrapper):
    __slots__ = ()
    IMPORT_FILE_KIND_UNSPECIFIED: _ClassVar[ImportFileKind]
    IMPORT_FILE_KIND_SKIPPED: _ClassVar[ImportFileKind]
    IMPORT_FILE_KIND_QUARANTINED: _ClassVar[ImportFileKind]
    IMPORT_FILE_KIND_LOST: _ClassVar[ImportFileKind]
    IMPORT_FILE_KIND_FAILED: _ClassVar[ImportFileKind]
    IMPORT_FILE_KIND_DISCARDED: _ClassVar[ImportFileKind]

class ImportUnitState(int, metaclass=_enum_type_wrapper.EnumTypeWrapper):
    __slots__ = ()
    IMPORT_UNIT_STATE_UNSPECIFIED: _ClassVar[ImportUnitState]
    IMPORT_UNIT_STATE_QUEUED: _ClassVar[ImportUnitState]
    IMPORT_UNIT_STATE_WALKING: _ClassVar[ImportUnitState]
    IMPORT_UNIT_STATE_WALKED: _ClassVar[ImportUnitState]
    IMPORT_UNIT_STATE_DONE: _ClassVar[ImportUnitState]
    IMPORT_UNIT_STATE_FAILED: _ClassVar[ImportUnitState]
    IMPORT_UNIT_STATE_CANCELED: _ClassVar[ImportUnitState]
JOB_STATE_UNSPECIFIED: JobState
JOB_STATE_QUEUED: JobState
JOB_STATE_RUNNING: JobState
JOB_STATE_DONE: JobState
JOB_STATE_PARTIAL: JobState
JOB_STATE_FAILED: JobState
JOB_STATE_CANCELED: JobState
DEID_JOB_MODE_UNSPECIFIED: DeidJobMode
DEID_JOB_MODE_DRY_RUN: DeidJobMode
DEID_JOB_MODE_CHECK: DeidJobMode
REBUILD_JOB_MODE_UNSPECIFIED: RebuildJobMode
REBUILD_JOB_MODE_COMPARE: RebuildJobMode
REBUILD_JOB_MODE_REPLACE: RebuildJobMode
REPLICATE_JOB_MODE_UNSPECIFIED: ReplicateJobMode
REPLICATE_JOB_MODE_BACKFILL: ReplicateJobMode
IMPORT_SKIP_CODE_UNSPECIFIED: ImportSkipCode
IMPORT_SKIP_CODE_NOT_PART10: ImportSkipCode
IMPORT_SKIP_CODE_OUTSIDE_LAYOUT: ImportSkipCode
IMPORT_SKIP_CODE_UNMAPPED_SESSION: ImportSkipCode
IMPORT_SKIP_CODE_DIRECTORY_TOO_WIDE: ImportSkipCode
IMPORT_SKIP_CODE_UNREADABLE: ImportSkipCode
IMPORT_SKIP_CODE_NOT_REGULAR: ImportSkipCode
IMPORT_SKIP_CODE_TOO_LARGE: ImportSkipCode
IMPORT_SKIP_CODE_PATH_TOO_LONG: ImportSkipCode
IMPORT_SKIP_CODE_BAD_NAME: ImportSkipCode
JOB_FAILURE_CODE_UNSPECIFIED: JobFailureCode
JOB_FAILURE_CODE_PEER_REFUSED: JobFailureCode
JOB_FAILURE_CODE_PEER_WARNING: JobFailureCode
JOB_FAILURE_CODE_CLASS_NOT_ACCEPTED: JobFailureCode
JOB_FAILURE_CODE_SYNTAX_NOT_ACCEPTED: JobFailureCode
JOB_FAILURE_CODE_PEER_UNACCOUNTED: JobFailureCode
JOB_FAILURE_CODE_DAMAGED_IN_STORAGE: JobFailureCode
JOB_FAILURE_CODE_META_UNREADABLE: JobFailureCode
JOB_FAILURE_CODE_WOULD_HOLD: JobFailureCode
JOB_FAILURE_CODE_CHECK_FAILED: JobFailureCode
JOB_FAILURE_CODE_UNDECIDED: JobFailureCode
JOB_FAILURE_CODE_NOT_DEIDENTIFIED: JobFailureCode
JOB_FAILURE_CODE_TOO_LARGE: JobFailureCode
JOB_FAILURE_CODE_DELETED_IN_ARCHIVE: JobFailureCode
JOB_FAILURE_CODE_NOT_ONLINE: JobFailureCode
JOB_FAILURE_CODE_AS_RECEIVED: JobFailureCode
JOB_FAILURE_CODE_NO_PROFILE_PROVENANCE: JobFailureCode
JOB_FAILURE_CODE_PIXEL_RISK_ACCEPTED: JobFailureCode
IMPORT_FILE_KIND_UNSPECIFIED: ImportFileKind
IMPORT_FILE_KIND_SKIPPED: ImportFileKind
IMPORT_FILE_KIND_QUARANTINED: ImportFileKind
IMPORT_FILE_KIND_LOST: ImportFileKind
IMPORT_FILE_KIND_FAILED: ImportFileKind
IMPORT_FILE_KIND_DISCARDED: ImportFileKind
IMPORT_UNIT_STATE_UNSPECIFIED: ImportUnitState
IMPORT_UNIT_STATE_QUEUED: ImportUnitState
IMPORT_UNIT_STATE_WALKING: ImportUnitState
IMPORT_UNIT_STATE_WALKED: ImportUnitState
IMPORT_UNIT_STATE_DONE: ImportUnitState
IMPORT_UNIT_STATE_FAILED: ImportUnitState
IMPORT_UNIT_STATE_CANCELED: ImportUnitState

class JobCounts(_message.Message):
    __slots__ = ("instances", "bytes", "instances_sent", "bytes_sent", "instances_warned", "bytes_warned", "instances_refused", "bytes_refused", "instances_remaining", "bytes_remaining", "scans", "scans_done", "listed")
    INSTANCES_FIELD_NUMBER: _ClassVar[int]
    BYTES_FIELD_NUMBER: _ClassVar[int]
    INSTANCES_SENT_FIELD_NUMBER: _ClassVar[int]
    BYTES_SENT_FIELD_NUMBER: _ClassVar[int]
    INSTANCES_WARNED_FIELD_NUMBER: _ClassVar[int]
    BYTES_WARNED_FIELD_NUMBER: _ClassVar[int]
    INSTANCES_REFUSED_FIELD_NUMBER: _ClassVar[int]
    BYTES_REFUSED_FIELD_NUMBER: _ClassVar[int]
    INSTANCES_REMAINING_FIELD_NUMBER: _ClassVar[int]
    BYTES_REMAINING_FIELD_NUMBER: _ClassVar[int]
    SCANS_FIELD_NUMBER: _ClassVar[int]
    SCANS_DONE_FIELD_NUMBER: _ClassVar[int]
    LISTED_FIELD_NUMBER: _ClassVar[int]
    instances: int
    bytes: int
    instances_sent: int
    bytes_sent: int
    instances_warned: int
    bytes_warned: int
    instances_refused: int
    bytes_refused: int
    instances_remaining: int
    bytes_remaining: int
    scans: int
    scans_done: int
    listed: bool
    def __init__(self, instances: _Optional[int] = ..., bytes: _Optional[int] = ..., instances_sent: _Optional[int] = ..., bytes_sent: _Optional[int] = ..., instances_warned: _Optional[int] = ..., bytes_warned: _Optional[int] = ..., instances_refused: _Optional[int] = ..., bytes_refused: _Optional[int] = ..., instances_remaining: _Optional[int] = ..., bytes_remaining: _Optional[int] = ..., scans: _Optional[int] = ..., scans_done: _Optional[int] = ..., listed: _Optional[bool] = ...) -> None: ...

class SendJob(_message.Message):
    __slots__ = ("sender", "subject_label", "session_label", "scan_label", "reception", "receiver")
    SENDER_FIELD_NUMBER: _ClassVar[int]
    SUBJECT_LABEL_FIELD_NUMBER: _ClassVar[int]
    SESSION_LABEL_FIELD_NUMBER: _ClassVar[int]
    SCAN_LABEL_FIELD_NUMBER: _ClassVar[int]
    RECEPTION_FIELD_NUMBER: _ClassVar[int]
    RECEIVER_FIELD_NUMBER: _ClassVar[int]
    sender: str
    subject_label: str
    session_label: str
    scan_label: str
    reception: str
    receiver: str
    def __init__(self, sender: _Optional[str] = ..., subject_label: _Optional[str] = ..., session_label: _Optional[str] = ..., scan_label: _Optional[str] = ..., reception: _Optional[str] = ..., receiver: _Optional[str] = ...) -> None: ...

class DeidJob(_message.Message):
    __slots__ = ("mode", "profile", "profile_version", "subject_label", "session_label", "scan_label")
    MODE_FIELD_NUMBER: _ClassVar[int]
    PROFILE_FIELD_NUMBER: _ClassVar[int]
    PROFILE_VERSION_FIELD_NUMBER: _ClassVar[int]
    SUBJECT_LABEL_FIELD_NUMBER: _ClassVar[int]
    SESSION_LABEL_FIELD_NUMBER: _ClassVar[int]
    SCAN_LABEL_FIELD_NUMBER: _ClassVar[int]
    mode: DeidJobMode
    profile: str
    profile_version: int
    subject_label: str
    session_label: str
    scan_label: str
    def __init__(self, mode: _Optional[_Union[DeidJobMode, str]] = ..., profile: _Optional[str] = ..., profile_version: _Optional[int] = ..., subject_label: _Optional[str] = ..., session_label: _Optional[str] = ..., scan_label: _Optional[str] = ...) -> None: ...

class ScrubJob(_message.Message):
    __slots__ = ("storage", "scheduled")
    STORAGE_FIELD_NUMBER: _ClassVar[int]
    SCHEDULED_FIELD_NUMBER: _ClassVar[int]
    storage: str
    scheduled: bool
    def __init__(self, storage: _Optional[str] = ..., scheduled: _Optional[bool] = ...) -> None: ...

class RebuildJob(_message.Message):
    __slots__ = ("mode", "scratch_schema", "from_storage")
    MODE_FIELD_NUMBER: _ClassVar[int]
    SCRATCH_SCHEMA_FIELD_NUMBER: _ClassVar[int]
    FROM_STORAGE_FIELD_NUMBER: _ClassVar[int]
    mode: RebuildJobMode
    scratch_schema: str
    from_storage: str
    def __init__(self, mode: _Optional[_Union[RebuildJobMode, str]] = ..., scratch_schema: _Optional[str] = ..., from_storage: _Optional[str] = ...) -> None: ...

class ReplicateJob(_message.Message):
    __slots__ = ("replica", "primary", "mode", "scheduled")
    REPLICA_FIELD_NUMBER: _ClassVar[int]
    PRIMARY_FIELD_NUMBER: _ClassVar[int]
    MODE_FIELD_NUMBER: _ClassVar[int]
    SCHEDULED_FIELD_NUMBER: _ClassVar[int]
    replica: str
    primary: str
    mode: ReplicateJobMode
    scheduled: bool
    def __init__(self, replica: _Optional[str] = ..., primary: _Optional[str] = ..., mode: _Optional[_Union[ReplicateJobMode, str]] = ..., scheduled: _Optional[bool] = ...) -> None: ...

class RepairJob(_message.Message):
    __slots__ = ("storage", "findings", "all", "automatic")
    STORAGE_FIELD_NUMBER: _ClassVar[int]
    FROM_FIELD_NUMBER: _ClassVar[int]
    FINDINGS_FIELD_NUMBER: _ClassVar[int]
    ALL_FIELD_NUMBER: _ClassVar[int]
    AUTOMATIC_FIELD_NUMBER: _ClassVar[int]
    storage: str
    findings: _containers.RepeatedScalarFieldContainer[str]
    all: bool
    automatic: bool
    def __init__(self, storage: _Optional[str] = ..., findings: _Optional[_Iterable[str]] = ..., all: _Optional[bool] = ..., automatic: _Optional[bool] = ..., **kwargs) -> None: ...

class DisposeJob(_message.Message):
    __slots__ = ("rule", "reason", "sessions", "scheduled")
    RULE_FIELD_NUMBER: _ClassVar[int]
    REASON_FIELD_NUMBER: _ClassVar[int]
    SESSIONS_FIELD_NUMBER: _ClassVar[int]
    SCHEDULED_FIELD_NUMBER: _ClassVar[int]
    rule: str
    reason: str
    sessions: int
    scheduled: bool
    def __init__(self, rule: _Optional[str] = ..., reason: _Optional[str] = ..., sessions: _Optional[int] = ..., scheduled: _Optional[bool] = ...) -> None: ...

class DeleteBatchJob(_message.Message):
    __slots__ = ("reason", "targets")
    REASON_FIELD_NUMBER: _ClassVar[int]
    TARGETS_FIELD_NUMBER: _ClassVar[int]
    reason: str
    targets: int
    def __init__(self, reason: _Optional[str] = ..., targets: _Optional[int] = ...) -> None: ...

class SearchBackfillJob(_message.Message):
    __slots__ = ("fields", "scans_skipped")
    FIELDS_FIELD_NUMBER: _ClassVar[int]
    SCANS_SKIPPED_FIELD_NUMBER: _ClassVar[int]
    fields: _containers.RepeatedScalarFieldContainer[str]
    scans_skipped: int
    def __init__(self, fields: _Optional[_Iterable[str]] = ..., scans_skipped: _Optional[int] = ...) -> None: ...

class ImportJob(_message.Message):
    __slots__ = ("receiver", "path", "map_file", "counts", "map_csv", "map_key_columns", "map_value_column", "map_key_parts", "map_gives", "retention_start")
    RECEIVER_FIELD_NUMBER: _ClassVar[int]
    PATH_FIELD_NUMBER: _ClassVar[int]
    MAP_FILE_FIELD_NUMBER: _ClassVar[int]
    COUNTS_FIELD_NUMBER: _ClassVar[int]
    MAP_CSV_FIELD_NUMBER: _ClassVar[int]
    MAP_KEY_COLUMNS_FIELD_NUMBER: _ClassVar[int]
    MAP_VALUE_COLUMN_FIELD_NUMBER: _ClassVar[int]
    MAP_KEY_PARTS_FIELD_NUMBER: _ClassVar[int]
    MAP_GIVES_FIELD_NUMBER: _ClassVar[int]
    RETENTION_START_FIELD_NUMBER: _ClassVar[int]
    receiver: str
    path: str
    map_file: str
    counts: ImportCounts
    map_csv: bytes
    map_key_columns: _containers.RepeatedScalarFieldContainer[str]
    map_value_column: str
    map_key_parts: _containers.RepeatedScalarFieldContainer[str]
    map_gives: str
    retention_start: _timestamp_pb2.Timestamp
    def __init__(self, receiver: _Optional[str] = ..., path: _Optional[str] = ..., map_file: _Optional[str] = ..., counts: _Optional[_Union[ImportCounts, _Mapping]] = ..., map_csv: _Optional[bytes] = ..., map_key_columns: _Optional[_Iterable[str]] = ..., map_value_column: _Optional[str] = ..., map_key_parts: _Optional[_Iterable[str]] = ..., map_gives: _Optional[str] = ..., retention_start: _Optional[_Union[datetime.datetime, _timestamp_pb2.Timestamp, _Mapping]] = ...) -> None: ...

class ImportSkipCount(_message.Message):
    __slots__ = ("code", "count")
    CODE_FIELD_NUMBER: _ClassVar[int]
    COUNT_FIELD_NUMBER: _ClassVar[int]
    code: ImportSkipCode
    count: int
    def __init__(self, code: _Optional[_Union[ImportSkipCode, str]] = ..., count: _Optional[int] = ...) -> None: ...

class ImportQuarantineCount(_message.Message):
    __slots__ = ("reason", "count")
    REASON_FIELD_NUMBER: _ClassVar[int]
    COUNT_FIELD_NUMBER: _ClassVar[int]
    reason: _quarantine_pb2.QuarantineReason
    count: int
    def __init__(self, reason: _Optional[_Union[_quarantine_pb2.QuarantineReason, str]] = ..., count: _Optional[int] = ...) -> None: ...

class ImportCounts(_message.Message):
    __slots__ = ("found", "skipped", "handed_in", "handed_again", "archived", "already_held", "quarantined", "lost", "processing", "bytes_handed_in", "label_differs", "units", "units_walked", "failed", "discarded")
    FOUND_FIELD_NUMBER: _ClassVar[int]
    SKIPPED_FIELD_NUMBER: _ClassVar[int]
    HANDED_IN_FIELD_NUMBER: _ClassVar[int]
    HANDED_AGAIN_FIELD_NUMBER: _ClassVar[int]
    ARCHIVED_FIELD_NUMBER: _ClassVar[int]
    ALREADY_HELD_FIELD_NUMBER: _ClassVar[int]
    QUARANTINED_FIELD_NUMBER: _ClassVar[int]
    LOST_FIELD_NUMBER: _ClassVar[int]
    PROCESSING_FIELD_NUMBER: _ClassVar[int]
    BYTES_HANDED_IN_FIELD_NUMBER: _ClassVar[int]
    LABEL_DIFFERS_FIELD_NUMBER: _ClassVar[int]
    UNITS_FIELD_NUMBER: _ClassVar[int]
    UNITS_WALKED_FIELD_NUMBER: _ClassVar[int]
    FAILED_FIELD_NUMBER: _ClassVar[int]
    DISCARDED_FIELD_NUMBER: _ClassVar[int]
    found: int
    skipped: _containers.RepeatedCompositeFieldContainer[ImportSkipCount]
    handed_in: int
    handed_again: int
    archived: int
    already_held: int
    quarantined: _containers.RepeatedCompositeFieldContainer[ImportQuarantineCount]
    lost: int
    processing: int
    bytes_handed_in: int
    label_differs: int
    units: int
    units_walked: int
    failed: int
    discarded: int
    def __init__(self, found: _Optional[int] = ..., skipped: _Optional[_Iterable[_Union[ImportSkipCount, _Mapping]]] = ..., handed_in: _Optional[int] = ..., handed_again: _Optional[int] = ..., archived: _Optional[int] = ..., already_held: _Optional[int] = ..., quarantined: _Optional[_Iterable[_Union[ImportQuarantineCount, _Mapping]]] = ..., lost: _Optional[int] = ..., processing: _Optional[int] = ..., bytes_handed_in: _Optional[int] = ..., label_differs: _Optional[int] = ..., units: _Optional[int] = ..., units_walked: _Optional[int] = ..., failed: _Optional[int] = ..., discarded: _Optional[int] = ...) -> None: ...

class JobTally(_message.Message):
    __slots__ = ("key", "count")
    KEY_FIELD_NUMBER: _ClassVar[int]
    COUNT_FIELD_NUMBER: _ClassVar[int]
    key: str
    count: int
    def __init__(self, key: _Optional[str] = ..., count: _Optional[int] = ...) -> None: ...

class Job(_message.Message):
    __slots__ = ("id", "send", "deid", "scrub", "rebuild", "import_job", "replicate", "repair", "dispose", "delete_batch", "search_backfill", "state", "counts", "reason", "created_by_key_id", "create_time", "start_time", "state_time", "end_time", "retry_of", "retry_job", "failures_not_listed", "tallies", "paused")
    ID_FIELD_NUMBER: _ClassVar[int]
    SEND_FIELD_NUMBER: _ClassVar[int]
    DEID_FIELD_NUMBER: _ClassVar[int]
    SCRUB_FIELD_NUMBER: _ClassVar[int]
    REBUILD_FIELD_NUMBER: _ClassVar[int]
    IMPORT_JOB_FIELD_NUMBER: _ClassVar[int]
    REPLICATE_FIELD_NUMBER: _ClassVar[int]
    REPAIR_FIELD_NUMBER: _ClassVar[int]
    DISPOSE_FIELD_NUMBER: _ClassVar[int]
    DELETE_BATCH_FIELD_NUMBER: _ClassVar[int]
    SEARCH_BACKFILL_FIELD_NUMBER: _ClassVar[int]
    STATE_FIELD_NUMBER: _ClassVar[int]
    COUNTS_FIELD_NUMBER: _ClassVar[int]
    REASON_FIELD_NUMBER: _ClassVar[int]
    CREATED_BY_KEY_ID_FIELD_NUMBER: _ClassVar[int]
    CREATE_TIME_FIELD_NUMBER: _ClassVar[int]
    START_TIME_FIELD_NUMBER: _ClassVar[int]
    STATE_TIME_FIELD_NUMBER: _ClassVar[int]
    END_TIME_FIELD_NUMBER: _ClassVar[int]
    RETRY_OF_FIELD_NUMBER: _ClassVar[int]
    RETRY_JOB_FIELD_NUMBER: _ClassVar[int]
    FAILURES_NOT_LISTED_FIELD_NUMBER: _ClassVar[int]
    TALLIES_FIELD_NUMBER: _ClassVar[int]
    PAUSED_FIELD_NUMBER: _ClassVar[int]
    id: str
    send: SendJob
    deid: DeidJob
    scrub: ScrubJob
    rebuild: RebuildJob
    import_job: ImportJob
    replicate: ReplicateJob
    repair: RepairJob
    dispose: DisposeJob
    delete_batch: DeleteBatchJob
    search_backfill: SearchBackfillJob
    state: JobState
    counts: JobCounts
    reason: str
    created_by_key_id: str
    create_time: _timestamp_pb2.Timestamp
    start_time: _timestamp_pb2.Timestamp
    state_time: _timestamp_pb2.Timestamp
    end_time: _timestamp_pb2.Timestamp
    retry_of: str
    retry_job: str
    failures_not_listed: int
    tallies: _containers.RepeatedCompositeFieldContainer[JobTally]
    paused: bool
    def __init__(self, id: _Optional[str] = ..., send: _Optional[_Union[SendJob, _Mapping]] = ..., deid: _Optional[_Union[DeidJob, _Mapping]] = ..., scrub: _Optional[_Union[ScrubJob, _Mapping]] = ..., rebuild: _Optional[_Union[RebuildJob, _Mapping]] = ..., import_job: _Optional[_Union[ImportJob, _Mapping]] = ..., replicate: _Optional[_Union[ReplicateJob, _Mapping]] = ..., repair: _Optional[_Union[RepairJob, _Mapping]] = ..., dispose: _Optional[_Union[DisposeJob, _Mapping]] = ..., delete_batch: _Optional[_Union[DeleteBatchJob, _Mapping]] = ..., search_backfill: _Optional[_Union[SearchBackfillJob, _Mapping]] = ..., state: _Optional[_Union[JobState, str]] = ..., counts: _Optional[_Union[JobCounts, _Mapping]] = ..., reason: _Optional[str] = ..., created_by_key_id: _Optional[str] = ..., create_time: _Optional[_Union[datetime.datetime, _timestamp_pb2.Timestamp, _Mapping]] = ..., start_time: _Optional[_Union[datetime.datetime, _timestamp_pb2.Timestamp, _Mapping]] = ..., state_time: _Optional[_Union[datetime.datetime, _timestamp_pb2.Timestamp, _Mapping]] = ..., end_time: _Optional[_Union[datetime.datetime, _timestamp_pb2.Timestamp, _Mapping]] = ..., retry_of: _Optional[str] = ..., retry_job: _Optional[str] = ..., failures_not_listed: _Optional[int] = ..., tallies: _Optional[_Iterable[_Union[JobTally, _Mapping]]] = ..., paused: _Optional[bool] = ...) -> None: ...

class JobFailure(_message.Message):
    __slots__ = ("instance_id", "subject_label", "session_label", "scan_label", "code", "peer_status", "sop_class_uid", "transfer_syntax_uid", "detail", "time", "peer_status_http", "revision")
    INSTANCE_ID_FIELD_NUMBER: _ClassVar[int]
    SUBJECT_LABEL_FIELD_NUMBER: _ClassVar[int]
    SESSION_LABEL_FIELD_NUMBER: _ClassVar[int]
    SCAN_LABEL_FIELD_NUMBER: _ClassVar[int]
    CODE_FIELD_NUMBER: _ClassVar[int]
    PEER_STATUS_FIELD_NUMBER: _ClassVar[int]
    SOP_CLASS_UID_FIELD_NUMBER: _ClassVar[int]
    TRANSFER_SYNTAX_UID_FIELD_NUMBER: _ClassVar[int]
    DETAIL_FIELD_NUMBER: _ClassVar[int]
    TIME_FIELD_NUMBER: _ClassVar[int]
    PEER_STATUS_HTTP_FIELD_NUMBER: _ClassVar[int]
    REVISION_FIELD_NUMBER: _ClassVar[int]
    instance_id: str
    subject_label: str
    session_label: str
    scan_label: str
    code: JobFailureCode
    peer_status: int
    sop_class_uid: str
    transfer_syntax_uid: str
    detail: str
    time: _timestamp_pb2.Timestamp
    peer_status_http: bool
    revision: int
    def __init__(self, instance_id: _Optional[str] = ..., subject_label: _Optional[str] = ..., session_label: _Optional[str] = ..., scan_label: _Optional[str] = ..., code: _Optional[_Union[JobFailureCode, str]] = ..., peer_status: _Optional[int] = ..., sop_class_uid: _Optional[str] = ..., transfer_syntax_uid: _Optional[str] = ..., detail: _Optional[str] = ..., time: _Optional[_Union[datetime.datetime, _timestamp_pb2.Timestamp, _Mapping]] = ..., peer_status_http: _Optional[bool] = ..., revision: _Optional[int] = ...) -> None: ...

class CreateJobRequest(_message.Message):
    __slots__ = ("tenant", "send", "deid", "scrub", "rebuild", "import_job")
    TENANT_FIELD_NUMBER: _ClassVar[int]
    SEND_FIELD_NUMBER: _ClassVar[int]
    DEID_FIELD_NUMBER: _ClassVar[int]
    SCRUB_FIELD_NUMBER: _ClassVar[int]
    REBUILD_FIELD_NUMBER: _ClassVar[int]
    IMPORT_JOB_FIELD_NUMBER: _ClassVar[int]
    tenant: str
    send: SendJob
    deid: DeidJob
    scrub: ScrubJob
    rebuild: RebuildJob
    import_job: ImportJob
    def __init__(self, tenant: _Optional[str] = ..., send: _Optional[_Union[SendJob, _Mapping]] = ..., deid: _Optional[_Union[DeidJob, _Mapping]] = ..., scrub: _Optional[_Union[ScrubJob, _Mapping]] = ..., rebuild: _Optional[_Union[RebuildJob, _Mapping]] = ..., import_job: _Optional[_Union[ImportJob, _Mapping]] = ...) -> None: ...

class CreateJobResponse(_message.Message):
    __slots__ = ("job",)
    JOB_FIELD_NUMBER: _ClassVar[int]
    job: Job
    def __init__(self, job: _Optional[_Union[Job, _Mapping]] = ...) -> None: ...

class GetJobRequest(_message.Message):
    __slots__ = ("tenant", "id")
    TENANT_FIELD_NUMBER: _ClassVar[int]
    ID_FIELD_NUMBER: _ClassVar[int]
    tenant: str
    id: str
    def __init__(self, tenant: _Optional[str] = ..., id: _Optional[str] = ...) -> None: ...

class GetJobResponse(_message.Message):
    __slots__ = ("job",)
    JOB_FIELD_NUMBER: _ClassVar[int]
    job: Job
    def __init__(self, job: _Optional[_Union[Job, _Mapping]] = ...) -> None: ...

class ListJobsRequest(_message.Message):
    __slots__ = ("tenant", "states", "in_progress", "sender", "start_time", "end_time", "page_size", "page_token")
    TENANT_FIELD_NUMBER: _ClassVar[int]
    STATES_FIELD_NUMBER: _ClassVar[int]
    IN_PROGRESS_FIELD_NUMBER: _ClassVar[int]
    SENDER_FIELD_NUMBER: _ClassVar[int]
    START_TIME_FIELD_NUMBER: _ClassVar[int]
    END_TIME_FIELD_NUMBER: _ClassVar[int]
    PAGE_SIZE_FIELD_NUMBER: _ClassVar[int]
    PAGE_TOKEN_FIELD_NUMBER: _ClassVar[int]
    tenant: str
    states: _containers.RepeatedScalarFieldContainer[JobState]
    in_progress: bool
    sender: str
    start_time: _timestamp_pb2.Timestamp
    end_time: _timestamp_pb2.Timestamp
    page_size: int
    page_token: str
    def __init__(self, tenant: _Optional[str] = ..., states: _Optional[_Iterable[_Union[JobState, str]]] = ..., in_progress: _Optional[bool] = ..., sender: _Optional[str] = ..., start_time: _Optional[_Union[datetime.datetime, _timestamp_pb2.Timestamp, _Mapping]] = ..., end_time: _Optional[_Union[datetime.datetime, _timestamp_pb2.Timestamp, _Mapping]] = ..., page_size: _Optional[int] = ..., page_token: _Optional[str] = ...) -> None: ...

class ListJobsResponse(_message.Message):
    __slots__ = ("jobs", "next_page_token")
    JOBS_FIELD_NUMBER: _ClassVar[int]
    NEXT_PAGE_TOKEN_FIELD_NUMBER: _ClassVar[int]
    jobs: _containers.RepeatedCompositeFieldContainer[Job]
    next_page_token: str
    def __init__(self, jobs: _Optional[_Iterable[_Union[Job, _Mapping]]] = ..., next_page_token: _Optional[str] = ...) -> None: ...

class CancelJobRequest(_message.Message):
    __slots__ = ("tenant", "id")
    TENANT_FIELD_NUMBER: _ClassVar[int]
    ID_FIELD_NUMBER: _ClassVar[int]
    tenant: str
    id: str
    def __init__(self, tenant: _Optional[str] = ..., id: _Optional[str] = ...) -> None: ...

class CancelJobResponse(_message.Message):
    __slots__ = ("job",)
    JOB_FIELD_NUMBER: _ClassVar[int]
    job: Job
    def __init__(self, job: _Optional[_Union[Job, _Mapping]] = ...) -> None: ...

class PauseJobRequest(_message.Message):
    __slots__ = ("tenant", "id")
    TENANT_FIELD_NUMBER: _ClassVar[int]
    ID_FIELD_NUMBER: _ClassVar[int]
    tenant: str
    id: str
    def __init__(self, tenant: _Optional[str] = ..., id: _Optional[str] = ...) -> None: ...

class PauseJobResponse(_message.Message):
    __slots__ = ("job",)
    JOB_FIELD_NUMBER: _ClassVar[int]
    job: Job
    def __init__(self, job: _Optional[_Union[Job, _Mapping]] = ...) -> None: ...

class ResumeJobRequest(_message.Message):
    __slots__ = ("tenant", "id")
    TENANT_FIELD_NUMBER: _ClassVar[int]
    ID_FIELD_NUMBER: _ClassVar[int]
    tenant: str
    id: str
    def __init__(self, tenant: _Optional[str] = ..., id: _Optional[str] = ...) -> None: ...

class ResumeJobResponse(_message.Message):
    __slots__ = ("job",)
    JOB_FIELD_NUMBER: _ClassVar[int]
    job: Job
    def __init__(self, job: _Optional[_Union[Job, _Mapping]] = ...) -> None: ...

class RetryJobRequest(_message.Message):
    __slots__ = ("tenant", "id")
    TENANT_FIELD_NUMBER: _ClassVar[int]
    ID_FIELD_NUMBER: _ClassVar[int]
    tenant: str
    id: str
    def __init__(self, tenant: _Optional[str] = ..., id: _Optional[str] = ...) -> None: ...

class RetryJobResponse(_message.Message):
    __slots__ = ("job", "retry")
    JOB_FIELD_NUMBER: _ClassVar[int]
    RETRY_FIELD_NUMBER: _ClassVar[int]
    job: Job
    retry: Job
    def __init__(self, job: _Optional[_Union[Job, _Mapping]] = ..., retry: _Optional[_Union[Job, _Mapping]] = ...) -> None: ...

class ListJobFailuresRequest(_message.Message):
    __slots__ = ("tenant", "id", "include_warnings", "page_size", "page_token")
    TENANT_FIELD_NUMBER: _ClassVar[int]
    ID_FIELD_NUMBER: _ClassVar[int]
    INCLUDE_WARNINGS_FIELD_NUMBER: _ClassVar[int]
    PAGE_SIZE_FIELD_NUMBER: _ClassVar[int]
    PAGE_TOKEN_FIELD_NUMBER: _ClassVar[int]
    tenant: str
    id: str
    include_warnings: bool
    page_size: int
    page_token: str
    def __init__(self, tenant: _Optional[str] = ..., id: _Optional[str] = ..., include_warnings: _Optional[bool] = ..., page_size: _Optional[int] = ..., page_token: _Optional[str] = ...) -> None: ...

class ListJobFailuresResponse(_message.Message):
    __slots__ = ("failures", "next_page_token")
    FAILURES_FIELD_NUMBER: _ClassVar[int]
    NEXT_PAGE_TOKEN_FIELD_NUMBER: _ClassVar[int]
    failures: _containers.RepeatedCompositeFieldContainer[JobFailure]
    next_page_token: str
    def __init__(self, failures: _Optional[_Iterable[_Union[JobFailure, _Mapping]]] = ..., next_page_token: _Optional[str] = ...) -> None: ...

class ImportFile(_message.Message):
    __slots__ = ("path", "kind", "skip_code", "quarantined_file", "quarantine_reason", "unit", "size_bytes", "detail", "time", "reception")
    PATH_FIELD_NUMBER: _ClassVar[int]
    KIND_FIELD_NUMBER: _ClassVar[int]
    SKIP_CODE_FIELD_NUMBER: _ClassVar[int]
    QUARANTINED_FILE_FIELD_NUMBER: _ClassVar[int]
    QUARANTINE_REASON_FIELD_NUMBER: _ClassVar[int]
    UNIT_FIELD_NUMBER: _ClassVar[int]
    SIZE_BYTES_FIELD_NUMBER: _ClassVar[int]
    DETAIL_FIELD_NUMBER: _ClassVar[int]
    TIME_FIELD_NUMBER: _ClassVar[int]
    RECEPTION_FIELD_NUMBER: _ClassVar[int]
    path: str
    kind: ImportFileKind
    skip_code: ImportSkipCode
    quarantined_file: str
    quarantine_reason: _quarantine_pb2.QuarantineReason
    unit: str
    size_bytes: int
    detail: str
    time: _timestamp_pb2.Timestamp
    reception: str
    def __init__(self, path: _Optional[str] = ..., kind: _Optional[_Union[ImportFileKind, str]] = ..., skip_code: _Optional[_Union[ImportSkipCode, str]] = ..., quarantined_file: _Optional[str] = ..., quarantine_reason: _Optional[_Union[_quarantine_pb2.QuarantineReason, str]] = ..., unit: _Optional[str] = ..., size_bytes: _Optional[int] = ..., detail: _Optional[str] = ..., time: _Optional[_Union[datetime.datetime, _timestamp_pb2.Timestamp, _Mapping]] = ..., reception: _Optional[str] = ...) -> None: ...

class ImportUnit(_message.Message):
    __slots__ = ("id", "folder", "state", "counts", "reason", "attempts", "walk_end_time")
    ID_FIELD_NUMBER: _ClassVar[int]
    FOLDER_FIELD_NUMBER: _ClassVar[int]
    STATE_FIELD_NUMBER: _ClassVar[int]
    COUNTS_FIELD_NUMBER: _ClassVar[int]
    REASON_FIELD_NUMBER: _ClassVar[int]
    ATTEMPTS_FIELD_NUMBER: _ClassVar[int]
    WALK_END_TIME_FIELD_NUMBER: _ClassVar[int]
    id: str
    folder: str
    state: ImportUnitState
    counts: ImportCounts
    reason: str
    attempts: int
    walk_end_time: _timestamp_pb2.Timestamp
    def __init__(self, id: _Optional[str] = ..., folder: _Optional[str] = ..., state: _Optional[_Union[ImportUnitState, str]] = ..., counts: _Optional[_Union[ImportCounts, _Mapping]] = ..., reason: _Optional[str] = ..., attempts: _Optional[int] = ..., walk_end_time: _Optional[_Union[datetime.datetime, _timestamp_pb2.Timestamp, _Mapping]] = ...) -> None: ...

class ListImportFilesRequest(_message.Message):
    __slots__ = ("tenant", "id", "kinds", "unit", "page_size", "page_token")
    TENANT_FIELD_NUMBER: _ClassVar[int]
    ID_FIELD_NUMBER: _ClassVar[int]
    KINDS_FIELD_NUMBER: _ClassVar[int]
    UNIT_FIELD_NUMBER: _ClassVar[int]
    PAGE_SIZE_FIELD_NUMBER: _ClassVar[int]
    PAGE_TOKEN_FIELD_NUMBER: _ClassVar[int]
    tenant: str
    id: str
    kinds: _containers.RepeatedScalarFieldContainer[ImportFileKind]
    unit: str
    page_size: int
    page_token: str
    def __init__(self, tenant: _Optional[str] = ..., id: _Optional[str] = ..., kinds: _Optional[_Iterable[_Union[ImportFileKind, str]]] = ..., unit: _Optional[str] = ..., page_size: _Optional[int] = ..., page_token: _Optional[str] = ...) -> None: ...

class ListImportFilesResponse(_message.Message):
    __slots__ = ("files", "next_page_token")
    FILES_FIELD_NUMBER: _ClassVar[int]
    NEXT_PAGE_TOKEN_FIELD_NUMBER: _ClassVar[int]
    files: _containers.RepeatedCompositeFieldContainer[ImportFile]
    next_page_token: str
    def __init__(self, files: _Optional[_Iterable[_Union[ImportFile, _Mapping]]] = ..., next_page_token: _Optional[str] = ...) -> None: ...

class ListImportUnitsRequest(_message.Message):
    __slots__ = ("tenant", "id", "states", "page_size", "page_token")
    TENANT_FIELD_NUMBER: _ClassVar[int]
    ID_FIELD_NUMBER: _ClassVar[int]
    STATES_FIELD_NUMBER: _ClassVar[int]
    PAGE_SIZE_FIELD_NUMBER: _ClassVar[int]
    PAGE_TOKEN_FIELD_NUMBER: _ClassVar[int]
    tenant: str
    id: str
    states: _containers.RepeatedScalarFieldContainer[ImportUnitState]
    page_size: int
    page_token: str
    def __init__(self, tenant: _Optional[str] = ..., id: _Optional[str] = ..., states: _Optional[_Iterable[_Union[ImportUnitState, str]]] = ..., page_size: _Optional[int] = ..., page_token: _Optional[str] = ...) -> None: ...

class ListImportUnitsResponse(_message.Message):
    __slots__ = ("units", "next_page_token")
    UNITS_FIELD_NUMBER: _ClassVar[int]
    NEXT_PAGE_TOKEN_FIELD_NUMBER: _ClassVar[int]
    units: _containers.RepeatedCompositeFieldContainer[ImportUnit]
    next_page_token: str
    def __init__(self, units: _Optional[_Iterable[_Union[ImportUnit, _Mapping]]] = ..., next_page_token: _Optional[str] = ...) -> None: ...
