import datetime

from google.protobuf import timestamp_pb2 as _timestamp_pb2
from google.protobuf.internal import containers as _containers
from google.protobuf.internal import enum_type_wrapper as _enum_type_wrapper
from google.protobuf import descriptor as _descriptor
from google.protobuf import message as _message
from collections.abc import Iterable as _Iterable, Mapping as _Mapping
from typing import ClassVar as _ClassVar, Optional as _Optional, Union as _Union

DESCRIPTOR: _descriptor.FileDescriptor

class CorrectionKind(int, metaclass=_enum_type_wrapper.EnumTypeWrapper):
    __slots__ = ()
    CORRECTION_KIND_UNSPECIFIED: _ClassVar[CorrectionKind]
    CORRECTION_KIND_DELETE: _ClassVar[CorrectionKind]
    CORRECTION_KIND_RELABEL: _ClassVar[CorrectionKind]
    CORRECTION_KIND_REPLACE: _ClassVar[CorrectionKind]
    CORRECTION_KIND_EDIT: _ClassVar[CorrectionKind]

class CorrectionOrigin(int, metaclass=_enum_type_wrapper.EnumTypeWrapper):
    __slots__ = ()
    CORRECTION_ORIGIN_UNSPECIFIED: _ClassVar[CorrectionOrigin]
    CORRECTION_ORIGIN_PERSON: _ClassVar[CorrectionOrigin]
    CORRECTION_ORIGIN_RETENTION: _ClassVar[CorrectionOrigin]

class CorrectionState(int, metaclass=_enum_type_wrapper.EnumTypeWrapper):
    __slots__ = ()
    CORRECTION_STATE_UNSPECIFIED: _ClassVar[CorrectionState]
    CORRECTION_STATE_PREPARED: _ClassVar[CorrectionState]
    CORRECTION_STATE_COMMITTED: _ClassVar[CorrectionState]
    CORRECTION_STATE_PURGING: _ClassVar[CorrectionState]
    CORRECTION_STATE_PURGED: _ClassVar[CorrectionState]
    CORRECTION_STATE_UNDONE: _ClassVar[CorrectionState]
    CORRECTION_STATE_ABANDONED: _ClassVar[CorrectionState]

class ObjectDisposition(int, metaclass=_enum_type_wrapper.EnumTypeWrapper):
    __slots__ = ()
    OBJECT_DISPOSITION_UNSPECIFIED: _ClassVar[ObjectDisposition]
    OBJECT_DISPOSITION_REMOVED: _ClassVar[ObjectDisposition]
    OBJECT_DISPOSITION_HIDDEN: _ClassVar[ObjectDisposition]
    OBJECT_DISPOSITION_RETAINED: _ClassVar[ObjectDisposition]
    OBJECT_DISPOSITION_UNKNOWN: _ClassVar[ObjectDisposition]

class HeldValueKind(int, metaclass=_enum_type_wrapper.EnumTypeWrapper):
    __slots__ = ()
    HELD_VALUE_KIND_UNSPECIFIED: _ClassVar[HeldValueKind]
    HELD_VALUE_KIND_SUBJECT: _ClassVar[HeldValueKind]
    HELD_VALUE_KIND_STUDY: _ClassVar[HeldValueKind]
    HELD_VALUE_KIND_SERIES: _ClassVar[HeldValueKind]
    HELD_VALUE_KIND_INSTANCE: _ClassVar[HeldValueKind]

class ScrubFindingKind(int, metaclass=_enum_type_wrapper.EnumTypeWrapper):
    __slots__ = ()
    SCRUB_FINDING_KIND_UNSPECIFIED: _ClassVar[ScrubFindingKind]
    SCRUB_FINDING_KIND_MISSING: _ClassVar[ScrubFindingKind]
    SCRUB_FINDING_KIND_ORPHAN: _ClassVar[ScrubFindingKind]
    SCRUB_FINDING_KIND_DAMAGED: _ClassVar[ScrubFindingKind]
    SCRUB_FINDING_KIND_DISAGREEING: _ClassVar[ScrubFindingKind]
    SCRUB_FINDING_KIND_MANIFEST_DAMAGED: _ClassVar[ScrubFindingKind]

class InstanceDisposition(int, metaclass=_enum_type_wrapper.EnumTypeWrapper):
    __slots__ = ()
    INSTANCE_DISPOSITION_UNSPECIFIED: _ClassVar[InstanceDisposition]
    INSTANCE_DISPOSITION_CURRENT: _ClassVar[InstanceDisposition]
    INSTANCE_DISPOSITION_SUPERSEDED: _ClassVar[InstanceDisposition]
    INSTANCE_DISPOSITION_DELETED: _ClassVar[InstanceDisposition]
    INSTANCE_DISPOSITION_UNKNOWN: _ClassVar[InstanceDisposition]
CORRECTION_KIND_UNSPECIFIED: CorrectionKind
CORRECTION_KIND_DELETE: CorrectionKind
CORRECTION_KIND_RELABEL: CorrectionKind
CORRECTION_KIND_REPLACE: CorrectionKind
CORRECTION_KIND_EDIT: CorrectionKind
CORRECTION_ORIGIN_UNSPECIFIED: CorrectionOrigin
CORRECTION_ORIGIN_PERSON: CorrectionOrigin
CORRECTION_ORIGIN_RETENTION: CorrectionOrigin
CORRECTION_STATE_UNSPECIFIED: CorrectionState
CORRECTION_STATE_PREPARED: CorrectionState
CORRECTION_STATE_COMMITTED: CorrectionState
CORRECTION_STATE_PURGING: CorrectionState
CORRECTION_STATE_PURGED: CorrectionState
CORRECTION_STATE_UNDONE: CorrectionState
CORRECTION_STATE_ABANDONED: CorrectionState
OBJECT_DISPOSITION_UNSPECIFIED: ObjectDisposition
OBJECT_DISPOSITION_REMOVED: ObjectDisposition
OBJECT_DISPOSITION_HIDDEN: ObjectDisposition
OBJECT_DISPOSITION_RETAINED: ObjectDisposition
OBJECT_DISPOSITION_UNKNOWN: ObjectDisposition
HELD_VALUE_KIND_UNSPECIFIED: HeldValueKind
HELD_VALUE_KIND_SUBJECT: HeldValueKind
HELD_VALUE_KIND_STUDY: HeldValueKind
HELD_VALUE_KIND_SERIES: HeldValueKind
HELD_VALUE_KIND_INSTANCE: HeldValueKind
SCRUB_FINDING_KIND_UNSPECIFIED: ScrubFindingKind
SCRUB_FINDING_KIND_MISSING: ScrubFindingKind
SCRUB_FINDING_KIND_ORPHAN: ScrubFindingKind
SCRUB_FINDING_KIND_DAMAGED: ScrubFindingKind
SCRUB_FINDING_KIND_DISAGREEING: ScrubFindingKind
SCRUB_FINDING_KIND_MANIFEST_DAMAGED: ScrubFindingKind
INSTANCE_DISPOSITION_UNSPECIFIED: InstanceDisposition
INSTANCE_DISPOSITION_CURRENT: InstanceDisposition
INSTANCE_DISPOSITION_SUPERSEDED: InstanceDisposition
INSTANCE_DISPOSITION_DELETED: InstanceDisposition
INSTANCE_DISPOSITION_UNKNOWN: InstanceDisposition

class CorrectionTarget(_message.Message):
    __slots__ = ("subject_label", "session_label", "scan_label", "instance_id")
    SUBJECT_LABEL_FIELD_NUMBER: _ClassVar[int]
    SESSION_LABEL_FIELD_NUMBER: _ClassVar[int]
    SCAN_LABEL_FIELD_NUMBER: _ClassVar[int]
    INSTANCE_ID_FIELD_NUMBER: _ClassVar[int]
    subject_label: str
    session_label: str
    scan_label: str
    instance_id: str
    def __init__(self, subject_label: _Optional[str] = ..., session_label: _Optional[str] = ..., scan_label: _Optional[str] = ..., instance_id: _Optional[str] = ...) -> None: ...

class StorageDisposition(_message.Message):
    __slots__ = ("storage", "objects_removed", "objects_hidden", "objects_retained", "objects_unknown", "versions_removed", "retain_until", "replica_of", "switch_generation")
    STORAGE_FIELD_NUMBER: _ClassVar[int]
    OBJECTS_REMOVED_FIELD_NUMBER: _ClassVar[int]
    OBJECTS_HIDDEN_FIELD_NUMBER: _ClassVar[int]
    OBJECTS_RETAINED_FIELD_NUMBER: _ClassVar[int]
    OBJECTS_UNKNOWN_FIELD_NUMBER: _ClassVar[int]
    VERSIONS_REMOVED_FIELD_NUMBER: _ClassVar[int]
    RETAIN_UNTIL_FIELD_NUMBER: _ClassVar[int]
    REPLICA_OF_FIELD_NUMBER: _ClassVar[int]
    SWITCH_GENERATION_FIELD_NUMBER: _ClassVar[int]
    storage: str
    objects_removed: int
    objects_hidden: int
    objects_retained: int
    objects_unknown: int
    versions_removed: int
    retain_until: _timestamp_pb2.Timestamp
    replica_of: str
    switch_generation: int
    def __init__(self, storage: _Optional[str] = ..., objects_removed: _Optional[int] = ..., objects_hidden: _Optional[int] = ..., objects_retained: _Optional[int] = ..., objects_unknown: _Optional[int] = ..., versions_removed: _Optional[int] = ..., retain_until: _Optional[_Union[datetime.datetime, _timestamp_pb2.Timestamp, _Mapping]] = ..., replica_of: _Optional[str] = ..., switch_generation: _Optional[int] = ...) -> None: ...

class StoragePurge(_message.Message):
    __slots__ = ("storage", "expected", "why", "replica_of")
    STORAGE_FIELD_NUMBER: _ClassVar[int]
    EXPECTED_FIELD_NUMBER: _ClassVar[int]
    WHY_FIELD_NUMBER: _ClassVar[int]
    REPLICA_OF_FIELD_NUMBER: _ClassVar[int]
    storage: str
    expected: ObjectDisposition
    why: str
    replica_of: str
    def __init__(self, storage: _Optional[str] = ..., expected: _Optional[_Union[ObjectDisposition, str]] = ..., why: _Optional[str] = ..., replica_of: _Optional[str] = ...) -> None: ...

class CorrectionCounts(_message.Message):
    __slots__ = ("subjects", "sessions", "scans", "instances", "bytes", "objects_removed", "bytes_removed", "objects_copied", "objects_superseded", "objects_kept", "objects_owned")
    SUBJECTS_FIELD_NUMBER: _ClassVar[int]
    SESSIONS_FIELD_NUMBER: _ClassVar[int]
    SCANS_FIELD_NUMBER: _ClassVar[int]
    INSTANCES_FIELD_NUMBER: _ClassVar[int]
    BYTES_FIELD_NUMBER: _ClassVar[int]
    OBJECTS_REMOVED_FIELD_NUMBER: _ClassVar[int]
    BYTES_REMOVED_FIELD_NUMBER: _ClassVar[int]
    OBJECTS_COPIED_FIELD_NUMBER: _ClassVar[int]
    OBJECTS_SUPERSEDED_FIELD_NUMBER: _ClassVar[int]
    OBJECTS_KEPT_FIELD_NUMBER: _ClassVar[int]
    OBJECTS_OWNED_FIELD_NUMBER: _ClassVar[int]
    subjects: int
    sessions: int
    scans: int
    instances: int
    bytes: int
    objects_removed: int
    bytes_removed: int
    objects_copied: int
    objects_superseded: int
    objects_kept: int
    objects_owned: int
    def __init__(self, subjects: _Optional[int] = ..., sessions: _Optional[int] = ..., scans: _Optional[int] = ..., instances: _Optional[int] = ..., bytes: _Optional[int] = ..., objects_removed: _Optional[int] = ..., bytes_removed: _Optional[int] = ..., objects_copied: _Optional[int] = ..., objects_superseded: _Optional[int] = ..., objects_kept: _Optional[int] = ..., objects_owned: _Optional[int] = ...) -> None: ...

class HeldValue(_message.Message):
    __slots__ = ("kind", "value")
    KIND_FIELD_NUMBER: _ClassVar[int]
    VALUE_FIELD_NUMBER: _ClassVar[int]
    kind: HeldValueKind
    value: str
    def __init__(self, kind: _Optional[_Union[HeldValueKind, str]] = ..., value: _Optional[str] = ...) -> None: ...

class PeerCopy(_message.Message):
    __slots__ = ("sender", "instances", "last_send_time")
    SENDER_FIELD_NUMBER: _ClassVar[int]
    INSTANCES_FIELD_NUMBER: _ClassVar[int]
    LAST_SEND_TIME_FIELD_NUMBER: _ClassVar[int]
    sender: str
    instances: int
    last_send_time: _timestamp_pb2.Timestamp
    def __init__(self, sender: _Optional[str] = ..., instances: _Optional[int] = ..., last_send_time: _Optional[_Union[datetime.datetime, _timestamp_pb2.Timestamp, _Mapping]] = ...) -> None: ...

class DeletePreview(_message.Message):
    __slots__ = ("counts", "storages", "sent_to", "jobs", "jobs_not_listed", "confirm", "purge_time", "storage_purges", "holds", "sessions_under_minimum", "keep_until", "quarantined", "receptions", "held_after_purge", "tenant_jobs")
    COUNTS_FIELD_NUMBER: _ClassVar[int]
    STORAGES_FIELD_NUMBER: _ClassVar[int]
    SENT_TO_FIELD_NUMBER: _ClassVar[int]
    JOBS_FIELD_NUMBER: _ClassVar[int]
    JOBS_NOT_LISTED_FIELD_NUMBER: _ClassVar[int]
    CONFIRM_FIELD_NUMBER: _ClassVar[int]
    PURGE_TIME_FIELD_NUMBER: _ClassVar[int]
    STORAGE_PURGES_FIELD_NUMBER: _ClassVar[int]
    HOLDS_FIELD_NUMBER: _ClassVar[int]
    SESSIONS_UNDER_MINIMUM_FIELD_NUMBER: _ClassVar[int]
    KEEP_UNTIL_FIELD_NUMBER: _ClassVar[int]
    QUARANTINED_FIELD_NUMBER: _ClassVar[int]
    RECEPTIONS_FIELD_NUMBER: _ClassVar[int]
    HELD_AFTER_PURGE_FIELD_NUMBER: _ClassVar[int]
    TENANT_JOBS_FIELD_NUMBER: _ClassVar[int]
    counts: CorrectionCounts
    storages: _containers.RepeatedScalarFieldContainer[str]
    sent_to: _containers.RepeatedCompositeFieldContainer[PeerCopy]
    jobs: _containers.RepeatedScalarFieldContainer[str]
    jobs_not_listed: int
    confirm: str
    purge_time: _timestamp_pb2.Timestamp
    storage_purges: _containers.RepeatedCompositeFieldContainer[StoragePurge]
    holds: _containers.RepeatedScalarFieldContainer[str]
    sessions_under_minimum: int
    keep_until: _timestamp_pb2.Timestamp
    quarantined: int
    receptions: int
    held_after_purge: _containers.RepeatedScalarFieldContainer[str]
    tenant_jobs: int
    def __init__(self, counts: _Optional[_Union[CorrectionCounts, _Mapping]] = ..., storages: _Optional[_Iterable[str]] = ..., sent_to: _Optional[_Iterable[_Union[PeerCopy, _Mapping]]] = ..., jobs: _Optional[_Iterable[str]] = ..., jobs_not_listed: _Optional[int] = ..., confirm: _Optional[str] = ..., purge_time: _Optional[_Union[datetime.datetime, _timestamp_pb2.Timestamp, _Mapping]] = ..., storage_purges: _Optional[_Iterable[_Union[StoragePurge, _Mapping]]] = ..., holds: _Optional[_Iterable[str]] = ..., sessions_under_minimum: _Optional[int] = ..., keep_until: _Optional[_Union[datetime.datetime, _timestamp_pb2.Timestamp, _Mapping]] = ..., quarantined: _Optional[int] = ..., receptions: _Optional[int] = ..., held_after_purge: _Optional[_Iterable[str]] = ..., tenant_jobs: _Optional[int] = ...) -> None: ...

class Correction(_message.Message):
    __slots__ = ("id", "kind", "state", "target", "to_label", "quarantined_file", "reason", "created_by_key_id", "create_time", "purge_time", "end_time", "counts", "sent_to", "state_reason", "old_sha256", "new_sha256", "arrivals_allowed", "undo_reason", "old_revision", "new_revision", "commit_time", "dispositions", "unresolved", "origin", "retention_rule", "retention_due_time", "override_retention", "override_reason", "job", "copies_abandoned", "holds", "swept")
    ID_FIELD_NUMBER: _ClassVar[int]
    KIND_FIELD_NUMBER: _ClassVar[int]
    STATE_FIELD_NUMBER: _ClassVar[int]
    TARGET_FIELD_NUMBER: _ClassVar[int]
    TO_LABEL_FIELD_NUMBER: _ClassVar[int]
    QUARANTINED_FILE_FIELD_NUMBER: _ClassVar[int]
    REASON_FIELD_NUMBER: _ClassVar[int]
    CREATED_BY_KEY_ID_FIELD_NUMBER: _ClassVar[int]
    CREATE_TIME_FIELD_NUMBER: _ClassVar[int]
    PURGE_TIME_FIELD_NUMBER: _ClassVar[int]
    END_TIME_FIELD_NUMBER: _ClassVar[int]
    COUNTS_FIELD_NUMBER: _ClassVar[int]
    SENT_TO_FIELD_NUMBER: _ClassVar[int]
    STATE_REASON_FIELD_NUMBER: _ClassVar[int]
    OLD_SHA256_FIELD_NUMBER: _ClassVar[int]
    NEW_SHA256_FIELD_NUMBER: _ClassVar[int]
    ARRIVALS_ALLOWED_FIELD_NUMBER: _ClassVar[int]
    UNDO_REASON_FIELD_NUMBER: _ClassVar[int]
    OLD_REVISION_FIELD_NUMBER: _ClassVar[int]
    NEW_REVISION_FIELD_NUMBER: _ClassVar[int]
    COMMIT_TIME_FIELD_NUMBER: _ClassVar[int]
    DISPOSITIONS_FIELD_NUMBER: _ClassVar[int]
    UNRESOLVED_FIELD_NUMBER: _ClassVar[int]
    ORIGIN_FIELD_NUMBER: _ClassVar[int]
    RETENTION_RULE_FIELD_NUMBER: _ClassVar[int]
    RETENTION_DUE_TIME_FIELD_NUMBER: _ClassVar[int]
    OVERRIDE_RETENTION_FIELD_NUMBER: _ClassVar[int]
    OVERRIDE_REASON_FIELD_NUMBER: _ClassVar[int]
    JOB_FIELD_NUMBER: _ClassVar[int]
    COPIES_ABANDONED_FIELD_NUMBER: _ClassVar[int]
    HOLDS_FIELD_NUMBER: _ClassVar[int]
    SWEPT_FIELD_NUMBER: _ClassVar[int]
    id: str
    kind: CorrectionKind
    state: CorrectionState
    target: CorrectionTarget
    to_label: str
    quarantined_file: str
    reason: str
    created_by_key_id: str
    create_time: _timestamp_pb2.Timestamp
    purge_time: _timestamp_pb2.Timestamp
    end_time: _timestamp_pb2.Timestamp
    counts: CorrectionCounts
    sent_to: _containers.RepeatedCompositeFieldContainer[PeerCopy]
    state_reason: str
    old_sha256: str
    new_sha256: str
    arrivals_allowed: bool
    undo_reason: str
    old_revision: int
    new_revision: int
    commit_time: _timestamp_pb2.Timestamp
    dispositions: _containers.RepeatedCompositeFieldContainer[StorageDisposition]
    unresolved: bool
    origin: CorrectionOrigin
    retention_rule: str
    retention_due_time: _timestamp_pb2.Timestamp
    override_retention: bool
    override_reason: str
    job: str
    copies_abandoned: _containers.RepeatedScalarFieldContainer[str]
    holds: _containers.RepeatedCompositeFieldContainer[HeldValue]
    swept: bool
    def __init__(self, id: _Optional[str] = ..., kind: _Optional[_Union[CorrectionKind, str]] = ..., state: _Optional[_Union[CorrectionState, str]] = ..., target: _Optional[_Union[CorrectionTarget, _Mapping]] = ..., to_label: _Optional[str] = ..., quarantined_file: _Optional[str] = ..., reason: _Optional[str] = ..., created_by_key_id: _Optional[str] = ..., create_time: _Optional[_Union[datetime.datetime, _timestamp_pb2.Timestamp, _Mapping]] = ..., purge_time: _Optional[_Union[datetime.datetime, _timestamp_pb2.Timestamp, _Mapping]] = ..., end_time: _Optional[_Union[datetime.datetime, _timestamp_pb2.Timestamp, _Mapping]] = ..., counts: _Optional[_Union[CorrectionCounts, _Mapping]] = ..., sent_to: _Optional[_Iterable[_Union[PeerCopy, _Mapping]]] = ..., state_reason: _Optional[str] = ..., old_sha256: _Optional[str] = ..., new_sha256: _Optional[str] = ..., arrivals_allowed: _Optional[bool] = ..., undo_reason: _Optional[str] = ..., old_revision: _Optional[int] = ..., new_revision: _Optional[int] = ..., commit_time: _Optional[_Union[datetime.datetime, _timestamp_pb2.Timestamp, _Mapping]] = ..., dispositions: _Optional[_Iterable[_Union[StorageDisposition, _Mapping]]] = ..., unresolved: _Optional[bool] = ..., origin: _Optional[_Union[CorrectionOrigin, str]] = ..., retention_rule: _Optional[str] = ..., retention_due_time: _Optional[_Union[datetime.datetime, _timestamp_pb2.Timestamp, _Mapping]] = ..., override_retention: _Optional[bool] = ..., override_reason: _Optional[str] = ..., job: _Optional[str] = ..., copies_abandoned: _Optional[_Iterable[str]] = ..., holds: _Optional[_Iterable[_Union[HeldValue, _Mapping]]] = ..., swept: _Optional[bool] = ...) -> None: ...

class DeleteRequest(_message.Message):
    __slots__ = ("tenant", "target", "reason", "validate_only", "confirm", "override_retention", "override_reason")
    TENANT_FIELD_NUMBER: _ClassVar[int]
    TARGET_FIELD_NUMBER: _ClassVar[int]
    REASON_FIELD_NUMBER: _ClassVar[int]
    VALIDATE_ONLY_FIELD_NUMBER: _ClassVar[int]
    CONFIRM_FIELD_NUMBER: _ClassVar[int]
    OVERRIDE_RETENTION_FIELD_NUMBER: _ClassVar[int]
    OVERRIDE_REASON_FIELD_NUMBER: _ClassVar[int]
    tenant: str
    target: CorrectionTarget
    reason: str
    validate_only: bool
    confirm: str
    override_retention: bool
    override_reason: str
    def __init__(self, tenant: _Optional[str] = ..., target: _Optional[_Union[CorrectionTarget, _Mapping]] = ..., reason: _Optional[str] = ..., validate_only: _Optional[bool] = ..., confirm: _Optional[str] = ..., override_retention: _Optional[bool] = ..., override_reason: _Optional[str] = ...) -> None: ...

class DeleteResponse(_message.Message):
    __slots__ = ("preview", "correction")
    PREVIEW_FIELD_NUMBER: _ClassVar[int]
    CORRECTION_FIELD_NUMBER: _ClassVar[int]
    preview: DeletePreview
    correction: Correction
    def __init__(self, preview: _Optional[_Union[DeletePreview, _Mapping]] = ..., correction: _Optional[_Union[Correction, _Mapping]] = ...) -> None: ...

class RelabelRequest(_message.Message):
    __slots__ = ("tenant", "target", "to_label", "reason")
    TENANT_FIELD_NUMBER: _ClassVar[int]
    TARGET_FIELD_NUMBER: _ClassVar[int]
    TO_LABEL_FIELD_NUMBER: _ClassVar[int]
    REASON_FIELD_NUMBER: _ClassVar[int]
    tenant: str
    target: CorrectionTarget
    to_label: str
    reason: str
    def __init__(self, tenant: _Optional[str] = ..., target: _Optional[_Union[CorrectionTarget, _Mapping]] = ..., to_label: _Optional[str] = ..., reason: _Optional[str] = ...) -> None: ...

class RelabelResponse(_message.Message):
    __slots__ = ("correction",)
    CORRECTION_FIELD_NUMBER: _ClassVar[int]
    correction: Correction
    def __init__(self, correction: _Optional[_Union[Correction, _Mapping]] = ...) -> None: ...

class ReplaceRequest(_message.Message):
    __slots__ = ("tenant", "quarantined_file", "reason")
    TENANT_FIELD_NUMBER: _ClassVar[int]
    QUARANTINED_FILE_FIELD_NUMBER: _ClassVar[int]
    REASON_FIELD_NUMBER: _ClassVar[int]
    tenant: str
    quarantined_file: str
    reason: str
    def __init__(self, tenant: _Optional[str] = ..., quarantined_file: _Optional[str] = ..., reason: _Optional[str] = ...) -> None: ...

class ReplaceResponse(_message.Message):
    __slots__ = ("correction",)
    CORRECTION_FIELD_NUMBER: _ClassVar[int]
    correction: Correction
    def __init__(self, correction: _Optional[_Union[Correction, _Mapping]] = ...) -> None: ...

class EditChange(_message.Message):
    __slots__ = ("attribute", "value", "remove")
    ATTRIBUTE_FIELD_NUMBER: _ClassVar[int]
    VALUE_FIELD_NUMBER: _ClassVar[int]
    REMOVE_FIELD_NUMBER: _ClassVar[int]
    attribute: str
    value: str
    remove: bool
    def __init__(self, attribute: _Optional[str] = ..., value: _Optional[str] = ..., remove: _Optional[bool] = ...) -> None: ...

class EditRequest(_message.Message):
    __slots__ = ("tenant", "target", "changes", "reason", "validate_only", "confirm", "show_values")
    TENANT_FIELD_NUMBER: _ClassVar[int]
    TARGET_FIELD_NUMBER: _ClassVar[int]
    CHANGES_FIELD_NUMBER: _ClassVar[int]
    REASON_FIELD_NUMBER: _ClassVar[int]
    VALIDATE_ONLY_FIELD_NUMBER: _ClassVar[int]
    CONFIRM_FIELD_NUMBER: _ClassVar[int]
    SHOW_VALUES_FIELD_NUMBER: _ClassVar[int]
    tenant: str
    target: CorrectionTarget
    changes: _containers.RepeatedCompositeFieldContainer[EditChange]
    reason: str
    validate_only: bool
    confirm: str
    show_values: bool
    def __init__(self, tenant: _Optional[str] = ..., target: _Optional[_Union[CorrectionTarget, _Mapping]] = ..., changes: _Optional[_Iterable[_Union[EditChange, _Mapping]]] = ..., reason: _Optional[str] = ..., validate_only: _Optional[bool] = ..., confirm: _Optional[str] = ..., show_values: _Optional[bool] = ...) -> None: ...

class EditPreviewAttribute(_message.Message):
    __slots__ = ("attribute", "present", "known", "old_value", "new_value", "remove")
    ATTRIBUTE_FIELD_NUMBER: _ClassVar[int]
    PRESENT_FIELD_NUMBER: _ClassVar[int]
    KNOWN_FIELD_NUMBER: _ClassVar[int]
    OLD_VALUE_FIELD_NUMBER: _ClassVar[int]
    NEW_VALUE_FIELD_NUMBER: _ClassVar[int]
    REMOVE_FIELD_NUMBER: _ClassVar[int]
    attribute: str
    present: bool
    known: bool
    old_value: str
    new_value: str
    remove: bool
    def __init__(self, attribute: _Optional[str] = ..., present: _Optional[bool] = ..., known: _Optional[bool] = ..., old_value: _Optional[str] = ..., new_value: _Optional[str] = ..., remove: _Optional[bool] = ...) -> None: ...

class EditPreviewInstance(_message.Message):
    __slots__ = ("subject_label", "session_label", "scan_label", "instance_id", "revision", "attributes")
    SUBJECT_LABEL_FIELD_NUMBER: _ClassVar[int]
    SESSION_LABEL_FIELD_NUMBER: _ClassVar[int]
    SCAN_LABEL_FIELD_NUMBER: _ClassVar[int]
    INSTANCE_ID_FIELD_NUMBER: _ClassVar[int]
    REVISION_FIELD_NUMBER: _ClassVar[int]
    ATTRIBUTES_FIELD_NUMBER: _ClassVar[int]
    subject_label: str
    session_label: str
    scan_label: str
    instance_id: str
    revision: int
    attributes: _containers.RepeatedCompositeFieldContainer[EditPreviewAttribute]
    def __init__(self, subject_label: _Optional[str] = ..., session_label: _Optional[str] = ..., scan_label: _Optional[str] = ..., instance_id: _Optional[str] = ..., revision: _Optional[int] = ..., attributes: _Optional[_Iterable[_Union[EditPreviewAttribute, _Mapping]]] = ...) -> None: ...

class EditPreview(_message.Message):
    __slots__ = ("instances", "storages", "issue_time", "confirm")
    INSTANCES_FIELD_NUMBER: _ClassVar[int]
    STORAGES_FIELD_NUMBER: _ClassVar[int]
    ISSUE_TIME_FIELD_NUMBER: _ClassVar[int]
    CONFIRM_FIELD_NUMBER: _ClassVar[int]
    instances: _containers.RepeatedCompositeFieldContainer[EditPreviewInstance]
    storages: _containers.RepeatedScalarFieldContainer[str]
    issue_time: _timestamp_pb2.Timestamp
    confirm: str
    def __init__(self, instances: _Optional[_Iterable[_Union[EditPreviewInstance, _Mapping]]] = ..., storages: _Optional[_Iterable[str]] = ..., issue_time: _Optional[_Union[datetime.datetime, _timestamp_pb2.Timestamp, _Mapping]] = ..., confirm: _Optional[str] = ...) -> None: ...

class EditResponse(_message.Message):
    __slots__ = ("preview", "correction")
    PREVIEW_FIELD_NUMBER: _ClassVar[int]
    CORRECTION_FIELD_NUMBER: _ClassVar[int]
    preview: EditPreview
    correction: Correction
    def __init__(self, preview: _Optional[_Union[EditPreview, _Mapping]] = ..., correction: _Optional[_Union[Correction, _Mapping]] = ...) -> None: ...

class ListCorrectionsRequest(_message.Message):
    __slots__ = ("tenant", "kinds", "states", "page_size", "page_token", "origins")
    TENANT_FIELD_NUMBER: _ClassVar[int]
    KINDS_FIELD_NUMBER: _ClassVar[int]
    STATES_FIELD_NUMBER: _ClassVar[int]
    PAGE_SIZE_FIELD_NUMBER: _ClassVar[int]
    PAGE_TOKEN_FIELD_NUMBER: _ClassVar[int]
    ORIGINS_FIELD_NUMBER: _ClassVar[int]
    tenant: str
    kinds: _containers.RepeatedScalarFieldContainer[CorrectionKind]
    states: _containers.RepeatedScalarFieldContainer[CorrectionState]
    page_size: int
    page_token: str
    origins: _containers.RepeatedScalarFieldContainer[CorrectionOrigin]
    def __init__(self, tenant: _Optional[str] = ..., kinds: _Optional[_Iterable[_Union[CorrectionKind, str]]] = ..., states: _Optional[_Iterable[_Union[CorrectionState, str]]] = ..., page_size: _Optional[int] = ..., page_token: _Optional[str] = ..., origins: _Optional[_Iterable[_Union[CorrectionOrigin, str]]] = ...) -> None: ...

class ListCorrectionsResponse(_message.Message):
    __slots__ = ("corrections", "next_page_token")
    CORRECTIONS_FIELD_NUMBER: _ClassVar[int]
    NEXT_PAGE_TOKEN_FIELD_NUMBER: _ClassVar[int]
    corrections: _containers.RepeatedCompositeFieldContainer[Correction]
    next_page_token: str
    def __init__(self, corrections: _Optional[_Iterable[_Union[Correction, _Mapping]]] = ..., next_page_token: _Optional[str] = ...) -> None: ...

class GetCorrectionRequest(_message.Message):
    __slots__ = ("tenant", "id")
    TENANT_FIELD_NUMBER: _ClassVar[int]
    ID_FIELD_NUMBER: _ClassVar[int]
    tenant: str
    id: str
    def __init__(self, tenant: _Optional[str] = ..., id: _Optional[str] = ...) -> None: ...

class GetCorrectionResponse(_message.Message):
    __slots__ = ("correction",)
    CORRECTION_FIELD_NUMBER: _ClassVar[int]
    correction: Correction
    def __init__(self, correction: _Optional[_Union[Correction, _Mapping]] = ...) -> None: ...

class UndoCorrectionRequest(_message.Message):
    __slots__ = ("tenant", "id", "reason")
    TENANT_FIELD_NUMBER: _ClassVar[int]
    ID_FIELD_NUMBER: _ClassVar[int]
    REASON_FIELD_NUMBER: _ClassVar[int]
    tenant: str
    id: str
    reason: str
    def __init__(self, tenant: _Optional[str] = ..., id: _Optional[str] = ..., reason: _Optional[str] = ...) -> None: ...

class UndoCorrectionResponse(_message.Message):
    __slots__ = ("correction",)
    CORRECTION_FIELD_NUMBER: _ClassVar[int]
    correction: Correction
    def __init__(self, correction: _Optional[_Union[Correction, _Mapping]] = ...) -> None: ...

class PurgeCorrectionRequest(_message.Message):
    __slots__ = ("tenant", "id")
    TENANT_FIELD_NUMBER: _ClassVar[int]
    ID_FIELD_NUMBER: _ClassVar[int]
    tenant: str
    id: str
    def __init__(self, tenant: _Optional[str] = ..., id: _Optional[str] = ...) -> None: ...

class PurgeCorrectionResponse(_message.Message):
    __slots__ = ("correction",)
    CORRECTION_FIELD_NUMBER: _ClassVar[int]
    correction: Correction
    def __init__(self, correction: _Optional[_Union[Correction, _Mapping]] = ...) -> None: ...

class AllowArrivalsRequest(_message.Message):
    __slots__ = ("tenant", "id", "reason", "value")
    TENANT_FIELD_NUMBER: _ClassVar[int]
    ID_FIELD_NUMBER: _ClassVar[int]
    REASON_FIELD_NUMBER: _ClassVar[int]
    VALUE_FIELD_NUMBER: _ClassVar[int]
    tenant: str
    id: str
    reason: str
    value: HeldValue
    def __init__(self, tenant: _Optional[str] = ..., id: _Optional[str] = ..., reason: _Optional[str] = ..., value: _Optional[_Union[HeldValue, _Mapping]] = ...) -> None: ...

class AllowArrivalsResponse(_message.Message):
    __slots__ = ("correction",)
    CORRECTION_FIELD_NUMBER: _ClassVar[int]
    correction: Correction
    def __init__(self, correction: _Optional[_Union[Correction, _Mapping]] = ...) -> None: ...

class ScrubFinding(_message.Message):
    __slots__ = ("id", "kind", "storage", "key", "subject_label", "session_label", "scan_label", "instance_id", "expected_size", "found_size", "expected_sha256", "found_sha256", "job", "find_time", "clear_time", "clear_job", "revision", "replica_of", "repair_job", "switch_generation")
    ID_FIELD_NUMBER: _ClassVar[int]
    KIND_FIELD_NUMBER: _ClassVar[int]
    STORAGE_FIELD_NUMBER: _ClassVar[int]
    KEY_FIELD_NUMBER: _ClassVar[int]
    SUBJECT_LABEL_FIELD_NUMBER: _ClassVar[int]
    SESSION_LABEL_FIELD_NUMBER: _ClassVar[int]
    SCAN_LABEL_FIELD_NUMBER: _ClassVar[int]
    INSTANCE_ID_FIELD_NUMBER: _ClassVar[int]
    EXPECTED_SIZE_FIELD_NUMBER: _ClassVar[int]
    FOUND_SIZE_FIELD_NUMBER: _ClassVar[int]
    EXPECTED_SHA256_FIELD_NUMBER: _ClassVar[int]
    FOUND_SHA256_FIELD_NUMBER: _ClassVar[int]
    JOB_FIELD_NUMBER: _ClassVar[int]
    FIND_TIME_FIELD_NUMBER: _ClassVar[int]
    CLEAR_TIME_FIELD_NUMBER: _ClassVar[int]
    CLEAR_JOB_FIELD_NUMBER: _ClassVar[int]
    REVISION_FIELD_NUMBER: _ClassVar[int]
    REPLICA_OF_FIELD_NUMBER: _ClassVar[int]
    REPAIR_JOB_FIELD_NUMBER: _ClassVar[int]
    SWITCH_GENERATION_FIELD_NUMBER: _ClassVar[int]
    id: str
    kind: ScrubFindingKind
    storage: str
    key: str
    subject_label: str
    session_label: str
    scan_label: str
    instance_id: str
    expected_size: int
    found_size: int
    expected_sha256: str
    found_sha256: str
    job: str
    find_time: _timestamp_pb2.Timestamp
    clear_time: _timestamp_pb2.Timestamp
    clear_job: str
    revision: int
    replica_of: str
    repair_job: str
    switch_generation: int
    def __init__(self, id: _Optional[str] = ..., kind: _Optional[_Union[ScrubFindingKind, str]] = ..., storage: _Optional[str] = ..., key: _Optional[str] = ..., subject_label: _Optional[str] = ..., session_label: _Optional[str] = ..., scan_label: _Optional[str] = ..., instance_id: _Optional[str] = ..., expected_size: _Optional[int] = ..., found_size: _Optional[int] = ..., expected_sha256: _Optional[str] = ..., found_sha256: _Optional[str] = ..., job: _Optional[str] = ..., find_time: _Optional[_Union[datetime.datetime, _timestamp_pb2.Timestamp, _Mapping]] = ..., clear_time: _Optional[_Union[datetime.datetime, _timestamp_pb2.Timestamp, _Mapping]] = ..., clear_job: _Optional[str] = ..., revision: _Optional[int] = ..., replica_of: _Optional[str] = ..., repair_job: _Optional[str] = ..., switch_generation: _Optional[int] = ...) -> None: ...

class ListScrubFindingsRequest(_message.Message):
    __slots__ = ("tenant", "kinds", "storage", "include_cleared", "page_size", "page_token")
    TENANT_FIELD_NUMBER: _ClassVar[int]
    KINDS_FIELD_NUMBER: _ClassVar[int]
    STORAGE_FIELD_NUMBER: _ClassVar[int]
    INCLUDE_CLEARED_FIELD_NUMBER: _ClassVar[int]
    PAGE_SIZE_FIELD_NUMBER: _ClassVar[int]
    PAGE_TOKEN_FIELD_NUMBER: _ClassVar[int]
    tenant: str
    kinds: _containers.RepeatedScalarFieldContainer[ScrubFindingKind]
    storage: str
    include_cleared: bool
    page_size: int
    page_token: str
    def __init__(self, tenant: _Optional[str] = ..., kinds: _Optional[_Iterable[_Union[ScrubFindingKind, str]]] = ..., storage: _Optional[str] = ..., include_cleared: _Optional[bool] = ..., page_size: _Optional[int] = ..., page_token: _Optional[str] = ...) -> None: ...

class ListScrubFindingsResponse(_message.Message):
    __slots__ = ("findings", "next_page_token")
    FINDINGS_FIELD_NUMBER: _ClassVar[int]
    NEXT_PAGE_TOKEN_FIELD_NUMBER: _ClassVar[int]
    findings: _containers.RepeatedCompositeFieldContainer[ScrubFinding]
    next_page_token: str
    def __init__(self, findings: _Optional[_Iterable[_Union[ScrubFinding, _Mapping]]] = ..., next_page_token: _Optional[str] = ...) -> None: ...

class InstanceRef(_message.Message):
    __slots__ = ("instance_id", "revision")
    INSTANCE_ID_FIELD_NUMBER: _ClassVar[int]
    REVISION_FIELD_NUMBER: _ClassVar[int]
    instance_id: str
    revision: int
    def __init__(self, instance_id: _Optional[str] = ..., revision: _Optional[int] = ...) -> None: ...

class ResolvedInstance(_message.Message):
    __slots__ = ("member", "disposition", "current_revision", "sha256", "correction", "bytes_kept", "subject_label", "session_label", "scan_label")
    MEMBER_FIELD_NUMBER: _ClassVar[int]
    DISPOSITION_FIELD_NUMBER: _ClassVar[int]
    CURRENT_REVISION_FIELD_NUMBER: _ClassVar[int]
    SHA256_FIELD_NUMBER: _ClassVar[int]
    CORRECTION_FIELD_NUMBER: _ClassVar[int]
    BYTES_KEPT_FIELD_NUMBER: _ClassVar[int]
    SUBJECT_LABEL_FIELD_NUMBER: _ClassVar[int]
    SESSION_LABEL_FIELD_NUMBER: _ClassVar[int]
    SCAN_LABEL_FIELD_NUMBER: _ClassVar[int]
    member: InstanceRef
    disposition: InstanceDisposition
    current_revision: int
    sha256: str
    correction: str
    bytes_kept: bool
    subject_label: str
    session_label: str
    scan_label: str
    def __init__(self, member: _Optional[_Union[InstanceRef, _Mapping]] = ..., disposition: _Optional[_Union[InstanceDisposition, str]] = ..., current_revision: _Optional[int] = ..., sha256: _Optional[str] = ..., correction: _Optional[str] = ..., bytes_kept: _Optional[bool] = ..., subject_label: _Optional[str] = ..., session_label: _Optional[str] = ..., scan_label: _Optional[str] = ...) -> None: ...

class ResolveInstancesRequest(_message.Message):
    __slots__ = ("tenant", "members")
    TENANT_FIELD_NUMBER: _ClassVar[int]
    MEMBERS_FIELD_NUMBER: _ClassVar[int]
    tenant: str
    members: _containers.RepeatedCompositeFieldContainer[InstanceRef]
    def __init__(self, tenant: _Optional[str] = ..., members: _Optional[_Iterable[_Union[InstanceRef, _Mapping]]] = ...) -> None: ...

class ResolveInstancesResponse(_message.Message):
    __slots__ = ("instances",)
    INSTANCES_FIELD_NUMBER: _ClassVar[int]
    instances: _containers.RepeatedCompositeFieldContainer[ResolvedInstance]
    def __init__(self, instances: _Optional[_Iterable[_Union[ResolvedInstance, _Mapping]]] = ...) -> None: ...
