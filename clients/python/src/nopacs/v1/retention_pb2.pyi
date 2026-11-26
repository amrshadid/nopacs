import datetime

from google.protobuf import duration_pb2 as _duration_pb2
from google.protobuf import field_mask_pb2 as _field_mask_pb2
from google.protobuf import timestamp_pb2 as _timestamp_pb2
from nopacs.v1 import correction_pb2 as _correction_pb2
from nopacs.v1 import job_pb2 as _job_pb2
from nopacs.v1 import scope_pb2 as _scope_pb2
from google.protobuf.internal import containers as _containers
from google.protobuf.internal import enum_type_wrapper as _enum_type_wrapper
from google.protobuf import descriptor as _descriptor
from google.protobuf import message as _message
from collections.abc import Iterable as _Iterable, Mapping as _Mapping
from typing import ClassVar as _ClassVar, Optional as _Optional, Union as _Union

DESCRIPTOR: _descriptor.FileDescriptor

class RetentionAction(int, metaclass=_enum_type_wrapper.EnumTypeWrapper):
    __slots__ = ()
    RETENTION_ACTION_UNSPECIFIED: _ClassVar[RetentionAction]
    RETENTION_ACTION_REVIEW: _ClassVar[RetentionAction]
    RETENTION_ACTION_DELETE: _ClassVar[RetentionAction]

class HoldState(int, metaclass=_enum_type_wrapper.EnumTypeWrapper):
    __slots__ = ()
    HOLD_STATE_UNSPECIFIED: _ClassVar[HoldState]
    HOLD_STATE_ACTIVE: _ClassVar[HoldState]
    HOLD_STATE_RELEASED: _ClassVar[HoldState]
RETENTION_ACTION_UNSPECIFIED: RetentionAction
RETENTION_ACTION_REVIEW: RetentionAction
RETENTION_ACTION_DELETE: RetentionAction
HOLD_STATE_UNSPECIFIED: HoldState
HOLD_STATE_ACTIVE: HoldState
HOLD_STATE_RELEASED: HoldState

class RetentionPeriod(_message.Message):
    __slots__ = ("years", "days")
    YEARS_FIELD_NUMBER: _ClassVar[int]
    DAYS_FIELD_NUMBER: _ClassVar[int]
    years: int
    days: int
    def __init__(self, years: _Optional[int] = ..., days: _Optional[int] = ...) -> None: ...

class RetentionSelector(_message.Message):
    __slots__ = ("receiver", "route", "storage")
    RECEIVER_FIELD_NUMBER: _ClassVar[int]
    ROUTE_FIELD_NUMBER: _ClassVar[int]
    STORAGE_FIELD_NUMBER: _ClassVar[int]
    receiver: str
    route: str
    storage: str
    def __init__(self, receiver: _Optional[str] = ..., route: _Optional[str] = ..., storage: _Optional[str] = ...) -> None: ...

class RetentionRule(_message.Message):
    __slots__ = ("name", "selector", "keep_at_least", "dispose_after", "action", "create_time", "update_time", "notice_until", "sessions_due")
    NAME_FIELD_NUMBER: _ClassVar[int]
    SELECTOR_FIELD_NUMBER: _ClassVar[int]
    KEEP_AT_LEAST_FIELD_NUMBER: _ClassVar[int]
    DISPOSE_AFTER_FIELD_NUMBER: _ClassVar[int]
    ACTION_FIELD_NUMBER: _ClassVar[int]
    CREATE_TIME_FIELD_NUMBER: _ClassVar[int]
    UPDATE_TIME_FIELD_NUMBER: _ClassVar[int]
    NOTICE_UNTIL_FIELD_NUMBER: _ClassVar[int]
    SESSIONS_DUE_FIELD_NUMBER: _ClassVar[int]
    name: str
    selector: RetentionSelector
    keep_at_least: RetentionPeriod
    dispose_after: RetentionPeriod
    action: RetentionAction
    create_time: _timestamp_pb2.Timestamp
    update_time: _timestamp_pb2.Timestamp
    notice_until: _timestamp_pb2.Timestamp
    sessions_due: int
    def __init__(self, name: _Optional[str] = ..., selector: _Optional[_Union[RetentionSelector, _Mapping]] = ..., keep_at_least: _Optional[_Union[RetentionPeriod, _Mapping]] = ..., dispose_after: _Optional[_Union[RetentionPeriod, _Mapping]] = ..., action: _Optional[_Union[RetentionAction, str]] = ..., create_time: _Optional[_Union[datetime.datetime, _timestamp_pb2.Timestamp, _Mapping]] = ..., update_time: _Optional[_Union[datetime.datetime, _timestamp_pb2.Timestamp, _Mapping]] = ..., notice_until: _Optional[_Union[datetime.datetime, _timestamp_pb2.Timestamp, _Mapping]] = ..., sessions_due: _Optional[int] = ...) -> None: ...

class DueSession(_message.Message):
    __slots__ = ("subject_label", "session_label", "rule", "action", "start_time", "due_time", "held", "instances", "bytes", "undone")
    SUBJECT_LABEL_FIELD_NUMBER: _ClassVar[int]
    SESSION_LABEL_FIELD_NUMBER: _ClassVar[int]
    RULE_FIELD_NUMBER: _ClassVar[int]
    ACTION_FIELD_NUMBER: _ClassVar[int]
    START_TIME_FIELD_NUMBER: _ClassVar[int]
    DUE_TIME_FIELD_NUMBER: _ClassVar[int]
    HELD_FIELD_NUMBER: _ClassVar[int]
    INSTANCES_FIELD_NUMBER: _ClassVar[int]
    BYTES_FIELD_NUMBER: _ClassVar[int]
    UNDONE_FIELD_NUMBER: _ClassVar[int]
    subject_label: str
    session_label: str
    rule: str
    action: RetentionAction
    start_time: _timestamp_pb2.Timestamp
    due_time: _timestamp_pb2.Timestamp
    held: bool
    instances: int
    bytes: int
    undone: bool
    def __init__(self, subject_label: _Optional[str] = ..., session_label: _Optional[str] = ..., rule: _Optional[str] = ..., action: _Optional[_Union[RetentionAction, str]] = ..., start_time: _Optional[_Union[datetime.datetime, _timestamp_pb2.Timestamp, _Mapping]] = ..., due_time: _Optional[_Union[datetime.datetime, _timestamp_pb2.Timestamp, _Mapping]] = ..., held: _Optional[bool] = ..., instances: _Optional[int] = ..., bytes: _Optional[int] = ..., undone: _Optional[bool] = ...) -> None: ...

class RetentionStatus(_message.Message):
    __slots__ = ("rules", "sessions_due", "sessions_due_review", "sessions_due_held", "next_due_time", "last_pass_time", "paused", "pause_reason", "clock_step", "active_holds", "hold_records_owed")
    RULES_FIELD_NUMBER: _ClassVar[int]
    SESSIONS_DUE_FIELD_NUMBER: _ClassVar[int]
    SESSIONS_DUE_REVIEW_FIELD_NUMBER: _ClassVar[int]
    SESSIONS_DUE_HELD_FIELD_NUMBER: _ClassVar[int]
    NEXT_DUE_TIME_FIELD_NUMBER: _ClassVar[int]
    LAST_PASS_TIME_FIELD_NUMBER: _ClassVar[int]
    PAUSED_FIELD_NUMBER: _ClassVar[int]
    PAUSE_REASON_FIELD_NUMBER: _ClassVar[int]
    CLOCK_STEP_FIELD_NUMBER: _ClassVar[int]
    ACTIVE_HOLDS_FIELD_NUMBER: _ClassVar[int]
    HOLD_RECORDS_OWED_FIELD_NUMBER: _ClassVar[int]
    rules: int
    sessions_due: int
    sessions_due_review: int
    sessions_due_held: int
    next_due_time: _timestamp_pb2.Timestamp
    last_pass_time: _timestamp_pb2.Timestamp
    paused: bool
    pause_reason: str
    clock_step: _duration_pb2.Duration
    active_holds: int
    hold_records_owed: int
    def __init__(self, rules: _Optional[int] = ..., sessions_due: _Optional[int] = ..., sessions_due_review: _Optional[int] = ..., sessions_due_held: _Optional[int] = ..., next_due_time: _Optional[_Union[datetime.datetime, _timestamp_pb2.Timestamp, _Mapping]] = ..., last_pass_time: _Optional[_Union[datetime.datetime, _timestamp_pb2.Timestamp, _Mapping]] = ..., paused: _Optional[bool] = ..., pause_reason: _Optional[str] = ..., clock_step: _Optional[_Union[datetime.timedelta, _duration_pb2.Duration, _Mapping]] = ..., active_holds: _Optional[int] = ..., hold_records_owed: _Optional[int] = ...) -> None: ...

class HoldTarget(_message.Message):
    __slots__ = ("subject_label", "session_label", "scan_label", "whole_tenant")
    SUBJECT_LABEL_FIELD_NUMBER: _ClassVar[int]
    SESSION_LABEL_FIELD_NUMBER: _ClassVar[int]
    SCAN_LABEL_FIELD_NUMBER: _ClassVar[int]
    WHOLE_TENANT_FIELD_NUMBER: _ClassVar[int]
    subject_label: str
    session_label: str
    scan_label: str
    whole_tenant: bool
    def __init__(self, subject_label: _Optional[str] = ..., session_label: _Optional[str] = ..., scan_label: _Optional[str] = ..., whole_tenant: _Optional[bool] = ...) -> None: ...

class Hold(_message.Message):
    __slots__ = ("id", "target", "reason", "state", "placed_by_key_id", "place_time", "release_reason", "released_by_key_id", "release_time", "found_on_storage", "storages", "storages_pending")
    ID_FIELD_NUMBER: _ClassVar[int]
    TARGET_FIELD_NUMBER: _ClassVar[int]
    REASON_FIELD_NUMBER: _ClassVar[int]
    STATE_FIELD_NUMBER: _ClassVar[int]
    PLACED_BY_KEY_ID_FIELD_NUMBER: _ClassVar[int]
    PLACE_TIME_FIELD_NUMBER: _ClassVar[int]
    RELEASE_REASON_FIELD_NUMBER: _ClassVar[int]
    RELEASED_BY_KEY_ID_FIELD_NUMBER: _ClassVar[int]
    RELEASE_TIME_FIELD_NUMBER: _ClassVar[int]
    FOUND_ON_STORAGE_FIELD_NUMBER: _ClassVar[int]
    STORAGES_FIELD_NUMBER: _ClassVar[int]
    STORAGES_PENDING_FIELD_NUMBER: _ClassVar[int]
    id: str
    target: HoldTarget
    reason: str
    state: HoldState
    placed_by_key_id: str
    place_time: _timestamp_pb2.Timestamp
    release_reason: str
    released_by_key_id: str
    release_time: _timestamp_pb2.Timestamp
    found_on_storage: bool
    storages: _containers.RepeatedScalarFieldContainer[str]
    storages_pending: _containers.RepeatedScalarFieldContainer[str]
    def __init__(self, id: _Optional[str] = ..., target: _Optional[_Union[HoldTarget, _Mapping]] = ..., reason: _Optional[str] = ..., state: _Optional[_Union[HoldState, str]] = ..., placed_by_key_id: _Optional[str] = ..., place_time: _Optional[_Union[datetime.datetime, _timestamp_pb2.Timestamp, _Mapping]] = ..., release_reason: _Optional[str] = ..., released_by_key_id: _Optional[str] = ..., release_time: _Optional[_Union[datetime.datetime, _timestamp_pb2.Timestamp, _Mapping]] = ..., found_on_storage: _Optional[bool] = ..., storages: _Optional[_Iterable[str]] = ..., storages_pending: _Optional[_Iterable[str]] = ...) -> None: ...

class AddRetentionRuleRequest(_message.Message):
    __slots__ = ("tenant", "rule")
    TENANT_FIELD_NUMBER: _ClassVar[int]
    RULE_FIELD_NUMBER: _ClassVar[int]
    tenant: str
    rule: RetentionRule
    def __init__(self, tenant: _Optional[str] = ..., rule: _Optional[_Union[RetentionRule, _Mapping]] = ...) -> None: ...

class AddRetentionRuleResponse(_message.Message):
    __slots__ = ("rule",)
    RULE_FIELD_NUMBER: _ClassVar[int]
    rule: RetentionRule
    def __init__(self, rule: _Optional[_Union[RetentionRule, _Mapping]] = ...) -> None: ...

class GetRetentionRuleRequest(_message.Message):
    __slots__ = ("tenant", "name")
    TENANT_FIELD_NUMBER: _ClassVar[int]
    NAME_FIELD_NUMBER: _ClassVar[int]
    tenant: str
    name: str
    def __init__(self, tenant: _Optional[str] = ..., name: _Optional[str] = ...) -> None: ...

class GetRetentionRuleResponse(_message.Message):
    __slots__ = ("rule",)
    RULE_FIELD_NUMBER: _ClassVar[int]
    rule: RetentionRule
    def __init__(self, rule: _Optional[_Union[RetentionRule, _Mapping]] = ...) -> None: ...

class ListRetentionRulesRequest(_message.Message):
    __slots__ = ("tenant", "page_size", "page_token")
    TENANT_FIELD_NUMBER: _ClassVar[int]
    PAGE_SIZE_FIELD_NUMBER: _ClassVar[int]
    PAGE_TOKEN_FIELD_NUMBER: _ClassVar[int]
    tenant: str
    page_size: int
    page_token: str
    def __init__(self, tenant: _Optional[str] = ..., page_size: _Optional[int] = ..., page_token: _Optional[str] = ...) -> None: ...

class ListRetentionRulesResponse(_message.Message):
    __slots__ = ("rules", "next_page_token")
    RULES_FIELD_NUMBER: _ClassVar[int]
    NEXT_PAGE_TOKEN_FIELD_NUMBER: _ClassVar[int]
    rules: _containers.RepeatedCompositeFieldContainer[RetentionRule]
    next_page_token: str
    def __init__(self, rules: _Optional[_Iterable[_Union[RetentionRule, _Mapping]]] = ..., next_page_token: _Optional[str] = ...) -> None: ...

class SetRetentionRuleRequest(_message.Message):
    __slots__ = ("tenant", "rule", "update_mask")
    TENANT_FIELD_NUMBER: _ClassVar[int]
    RULE_FIELD_NUMBER: _ClassVar[int]
    UPDATE_MASK_FIELD_NUMBER: _ClassVar[int]
    tenant: str
    rule: RetentionRule
    update_mask: _field_mask_pb2.FieldMask
    def __init__(self, tenant: _Optional[str] = ..., rule: _Optional[_Union[RetentionRule, _Mapping]] = ..., update_mask: _Optional[_Union[_field_mask_pb2.FieldMask, _Mapping]] = ...) -> None: ...

class SetRetentionRuleResponse(_message.Message):
    __slots__ = ("rule",)
    RULE_FIELD_NUMBER: _ClassVar[int]
    rule: RetentionRule
    def __init__(self, rule: _Optional[_Union[RetentionRule, _Mapping]] = ...) -> None: ...

class RemoveRetentionRuleRequest(_message.Message):
    __slots__ = ("tenant", "name")
    TENANT_FIELD_NUMBER: _ClassVar[int]
    NAME_FIELD_NUMBER: _ClassVar[int]
    tenant: str
    name: str
    def __init__(self, tenant: _Optional[str] = ..., name: _Optional[str] = ...) -> None: ...

class RemoveRetentionRuleResponse(_message.Message):
    __slots__ = ("rule",)
    RULE_FIELD_NUMBER: _ClassVar[int]
    rule: RetentionRule
    def __init__(self, rule: _Optional[_Union[RetentionRule, _Mapping]] = ...) -> None: ...

class ListDueSessionsRequest(_message.Message):
    __slots__ = ("tenant", "rule", "include_held", "page_size", "page_token")
    TENANT_FIELD_NUMBER: _ClassVar[int]
    RULE_FIELD_NUMBER: _ClassVar[int]
    INCLUDE_HELD_FIELD_NUMBER: _ClassVar[int]
    PAGE_SIZE_FIELD_NUMBER: _ClassVar[int]
    PAGE_TOKEN_FIELD_NUMBER: _ClassVar[int]
    tenant: str
    rule: str
    include_held: bool
    page_size: int
    page_token: str
    def __init__(self, tenant: _Optional[str] = ..., rule: _Optional[str] = ..., include_held: _Optional[bool] = ..., page_size: _Optional[int] = ..., page_token: _Optional[str] = ...) -> None: ...

class ListDueSessionsResponse(_message.Message):
    __slots__ = ("sessions", "next_page_token")
    SESSIONS_FIELD_NUMBER: _ClassVar[int]
    NEXT_PAGE_TOKEN_FIELD_NUMBER: _ClassVar[int]
    sessions: _containers.RepeatedCompositeFieldContainer[DueSession]
    next_page_token: str
    def __init__(self, sessions: _Optional[_Iterable[_Union[DueSession, _Mapping]]] = ..., next_page_token: _Optional[str] = ...) -> None: ...

class DisposeRequest(_message.Message):
    __slots__ = ("tenant", "rule", "reason", "validate_only", "confirm", "max_sessions")
    TENANT_FIELD_NUMBER: _ClassVar[int]
    RULE_FIELD_NUMBER: _ClassVar[int]
    REASON_FIELD_NUMBER: _ClassVar[int]
    VALIDATE_ONLY_FIELD_NUMBER: _ClassVar[int]
    CONFIRM_FIELD_NUMBER: _ClassVar[int]
    MAX_SESSIONS_FIELD_NUMBER: _ClassVar[int]
    tenant: str
    rule: str
    reason: str
    validate_only: bool
    confirm: str
    max_sessions: int
    def __init__(self, tenant: _Optional[str] = ..., rule: _Optional[str] = ..., reason: _Optional[str] = ..., validate_only: _Optional[bool] = ..., confirm: _Optional[str] = ..., max_sessions: _Optional[int] = ...) -> None: ...

class DisposalPreview(_message.Message):
    __slots__ = ("rule", "sessions", "sessions_not_listed", "sessions_held", "counts", "confirm")
    RULE_FIELD_NUMBER: _ClassVar[int]
    SESSIONS_FIELD_NUMBER: _ClassVar[int]
    SESSIONS_NOT_LISTED_FIELD_NUMBER: _ClassVar[int]
    SESSIONS_HELD_FIELD_NUMBER: _ClassVar[int]
    COUNTS_FIELD_NUMBER: _ClassVar[int]
    CONFIRM_FIELD_NUMBER: _ClassVar[int]
    rule: str
    sessions: _containers.RepeatedCompositeFieldContainer[DueSession]
    sessions_not_listed: int
    sessions_held: int
    counts: _correction_pb2.CorrectionCounts
    confirm: str
    def __init__(self, rule: _Optional[str] = ..., sessions: _Optional[_Iterable[_Union[DueSession, _Mapping]]] = ..., sessions_not_listed: _Optional[int] = ..., sessions_held: _Optional[int] = ..., counts: _Optional[_Union[_correction_pb2.CorrectionCounts, _Mapping]] = ..., confirm: _Optional[str] = ...) -> None: ...

class DisposeResponse(_message.Message):
    __slots__ = ("preview", "job")
    PREVIEW_FIELD_NUMBER: _ClassVar[int]
    JOB_FIELD_NUMBER: _ClassVar[int]
    preview: DisposalPreview
    job: _job_pb2.Job
    def __init__(self, preview: _Optional[_Union[DisposalPreview, _Mapping]] = ..., job: _Optional[_Union[_job_pb2.Job, _Mapping]] = ...) -> None: ...

class ConfirmClockRequest(_message.Message):
    __slots__ = ("scope", "observed_time", "reason")
    SCOPE_FIELD_NUMBER: _ClassVar[int]
    OBSERVED_TIME_FIELD_NUMBER: _ClassVar[int]
    REASON_FIELD_NUMBER: _ClassVar[int]
    scope: _scope_pb2.Scope
    observed_time: _timestamp_pb2.Timestamp
    reason: str
    def __init__(self, scope: _Optional[_Union[_scope_pb2.Scope, _Mapping]] = ..., observed_time: _Optional[_Union[datetime.datetime, _timestamp_pb2.Timestamp, _Mapping]] = ..., reason: _Optional[str] = ...) -> None: ...

class ConfirmClockResponse(_message.Message):
    __slots__ = ("tenants", "status", "tenants_refused")
    TENANTS_FIELD_NUMBER: _ClassVar[int]
    STATUS_FIELD_NUMBER: _ClassVar[int]
    TENANTS_REFUSED_FIELD_NUMBER: _ClassVar[int]
    tenants: _containers.RepeatedScalarFieldContainer[str]
    status: RetentionStatus
    tenants_refused: _containers.RepeatedScalarFieldContainer[str]
    def __init__(self, tenants: _Optional[_Iterable[str]] = ..., status: _Optional[_Union[RetentionStatus, _Mapping]] = ..., tenants_refused: _Optional[_Iterable[str]] = ...) -> None: ...

class PlaceHoldRequest(_message.Message):
    __slots__ = ("tenant", "target", "reason")
    TENANT_FIELD_NUMBER: _ClassVar[int]
    TARGET_FIELD_NUMBER: _ClassVar[int]
    REASON_FIELD_NUMBER: _ClassVar[int]
    tenant: str
    target: HoldTarget
    reason: str
    def __init__(self, tenant: _Optional[str] = ..., target: _Optional[_Union[HoldTarget, _Mapping]] = ..., reason: _Optional[str] = ...) -> None: ...

class PlaceHoldResponse(_message.Message):
    __slots__ = ("hold",)
    HOLD_FIELD_NUMBER: _ClassVar[int]
    hold: Hold
    def __init__(self, hold: _Optional[_Union[Hold, _Mapping]] = ...) -> None: ...

class ReleaseHoldRequest(_message.Message):
    __slots__ = ("tenant", "id", "reason")
    TENANT_FIELD_NUMBER: _ClassVar[int]
    ID_FIELD_NUMBER: _ClassVar[int]
    REASON_FIELD_NUMBER: _ClassVar[int]
    tenant: str
    id: str
    reason: str
    def __init__(self, tenant: _Optional[str] = ..., id: _Optional[str] = ..., reason: _Optional[str] = ...) -> None: ...

class ReleaseHoldResponse(_message.Message):
    __slots__ = ("hold",)
    HOLD_FIELD_NUMBER: _ClassVar[int]
    hold: Hold
    def __init__(self, hold: _Optional[_Union[Hold, _Mapping]] = ...) -> None: ...

class GetHoldRequest(_message.Message):
    __slots__ = ("tenant", "id")
    TENANT_FIELD_NUMBER: _ClassVar[int]
    ID_FIELD_NUMBER: _ClassVar[int]
    tenant: str
    id: str
    def __init__(self, tenant: _Optional[str] = ..., id: _Optional[str] = ...) -> None: ...

class GetHoldResponse(_message.Message):
    __slots__ = ("hold",)
    HOLD_FIELD_NUMBER: _ClassVar[int]
    hold: Hold
    def __init__(self, hold: _Optional[_Union[Hold, _Mapping]] = ...) -> None: ...

class ListHoldsRequest(_message.Message):
    __slots__ = ("tenant", "states", "subject_label", "page_size", "page_token")
    TENANT_FIELD_NUMBER: _ClassVar[int]
    STATES_FIELD_NUMBER: _ClassVar[int]
    SUBJECT_LABEL_FIELD_NUMBER: _ClassVar[int]
    PAGE_SIZE_FIELD_NUMBER: _ClassVar[int]
    PAGE_TOKEN_FIELD_NUMBER: _ClassVar[int]
    tenant: str
    states: _containers.RepeatedScalarFieldContainer[HoldState]
    subject_label: str
    page_size: int
    page_token: str
    def __init__(self, tenant: _Optional[str] = ..., states: _Optional[_Iterable[_Union[HoldState, str]]] = ..., subject_label: _Optional[str] = ..., page_size: _Optional[int] = ..., page_token: _Optional[str] = ...) -> None: ...

class ListHoldsResponse(_message.Message):
    __slots__ = ("holds", "next_page_token")
    HOLDS_FIELD_NUMBER: _ClassVar[int]
    NEXT_PAGE_TOKEN_FIELD_NUMBER: _ClassVar[int]
    holds: _containers.RepeatedCompositeFieldContainer[Hold]
    next_page_token: str
    def __init__(self, holds: _Optional[_Iterable[_Union[Hold, _Mapping]]] = ..., next_page_token: _Optional[str] = ...) -> None: ...
