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

class RulePhase(int, metaclass=_enum_type_wrapper.EnumTypeWrapper):
    __slots__ = ()
    RULE_PHASE_UNSPECIFIED: _ClassVar[RulePhase]
    RULE_PHASE_ARCHIVE: _ClassVar[RulePhase]
    RULE_PHASE_FORWARD: _ClassVar[RulePhase]
    RULE_PHASE_SERIES: _ClassVar[RulePhase]

class RouteTruth(int, metaclass=_enum_type_wrapper.EnumTypeWrapper):
    __slots__ = ()
    ROUTE_TRUTH_UNSPECIFIED: _ClassVar[RouteTruth]
    ROUTE_TRUTH_TRUE: _ClassVar[RouteTruth]
    ROUTE_TRUTH_FALSE: _ClassVar[RouteTruth]
    ROUTE_TRUTH_UNKNOWN: _ClassVar[RouteTruth]
    ROUTE_TRUTH_UNUSABLE: _ClassVar[RouteTruth]

class DryRunOutcome(int, metaclass=_enum_type_wrapper.EnumTypeWrapper):
    __slots__ = ()
    DRY_RUN_OUTCOME_UNSPECIFIED: _ClassVar[DryRunOutcome]
    DRY_RUN_OUTCOME_SAME: _ClassVar[DryRunOutcome]
    DRY_RUN_OUTCOME_CHANGED: _ClassVar[DryRunOutcome]
    DRY_RUN_OUTCOME_UNKNOWN: _ClassVar[DryRunOutcome]
RULE_PHASE_UNSPECIFIED: RulePhase
RULE_PHASE_ARCHIVE: RulePhase
RULE_PHASE_FORWARD: RulePhase
RULE_PHASE_SERIES: RulePhase
ROUTE_TRUTH_UNSPECIFIED: RouteTruth
ROUTE_TRUTH_TRUE: RouteTruth
ROUTE_TRUTH_FALSE: RouteTruth
ROUTE_TRUTH_UNKNOWN: RouteTruth
ROUTE_TRUTH_UNUSABLE: RouteTruth
DRY_RUN_OUTCOME_UNSPECIFIED: DryRunOutcome
DRY_RUN_OUTCOME_SAME: DryRunOutcome
DRY_RUN_OUTCOME_CHANGED: DryRunOutcome
DRY_RUN_OUTCOME_UNKNOWN: DryRunOutcome

class RuleSet(_message.Message):
    __slots__ = ("receiver", "version", "archive_rules", "quarantine_unmatched", "forward_rules", "comment", "create_time", "created_by_key", "active")
    RECEIVER_FIELD_NUMBER: _ClassVar[int]
    VERSION_FIELD_NUMBER: _ClassVar[int]
    ARCHIVE_RULES_FIELD_NUMBER: _ClassVar[int]
    QUARANTINE_UNMATCHED_FIELD_NUMBER: _ClassVar[int]
    FORWARD_RULES_FIELD_NUMBER: _ClassVar[int]
    COMMENT_FIELD_NUMBER: _ClassVar[int]
    CREATE_TIME_FIELD_NUMBER: _ClassVar[int]
    CREATED_BY_KEY_FIELD_NUMBER: _ClassVar[int]
    ACTIVE_FIELD_NUMBER: _ClassVar[int]
    receiver: str
    version: int
    archive_rules: _containers.RepeatedCompositeFieldContainer[ArchiveRule]
    quarantine_unmatched: bool
    forward_rules: _containers.RepeatedCompositeFieldContainer[ForwardRule]
    comment: str
    create_time: _timestamp_pb2.Timestamp
    created_by_key: str
    active: bool
    def __init__(self, receiver: _Optional[str] = ..., version: _Optional[int] = ..., archive_rules: _Optional[_Iterable[_Union[ArchiveRule, _Mapping]]] = ..., quarantine_unmatched: _Optional[bool] = ..., forward_rules: _Optional[_Iterable[_Union[ForwardRule, _Mapping]]] = ..., comment: _Optional[str] = ..., create_time: _Optional[_Union[datetime.datetime, _timestamp_pb2.Timestamp, _Mapping]] = ..., created_by_key: _Optional[str] = ..., active: _Optional[bool] = ...) -> None: ...

class ArchiveRule(_message.Message):
    __slots__ = ("name", "when", "storage", "profile")
    NAME_FIELD_NUMBER: _ClassVar[int]
    WHEN_FIELD_NUMBER: _ClassVar[int]
    STORAGE_FIELD_NUMBER: _ClassVar[int]
    PROFILE_FIELD_NUMBER: _ClassVar[int]
    name: str
    when: str
    storage: str
    profile: _receiver_pb2.ProfileVersionRef
    def __init__(self, name: _Optional[str] = ..., when: _Optional[str] = ..., storage: _Optional[str] = ..., profile: _Optional[_Union[_receiver_pb2.ProfileVersionRef, _Mapping]] = ...) -> None: ...

class ForwardRule(_message.Message):
    __slots__ = ("name", "when", "series", "senders", "destination_profile")
    NAME_FIELD_NUMBER: _ClassVar[int]
    WHEN_FIELD_NUMBER: _ClassVar[int]
    SERIES_FIELD_NUMBER: _ClassVar[int]
    SENDERS_FIELD_NUMBER: _ClassVar[int]
    DESTINATION_PROFILE_FIELD_NUMBER: _ClassVar[int]
    name: str
    when: str
    series: str
    senders: _containers.RepeatedScalarFieldContainer[str]
    destination_profile: _receiver_pb2.ProfileVersionRef
    def __init__(self, name: _Optional[str] = ..., when: _Optional[str] = ..., series: _Optional[str] = ..., senders: _Optional[_Iterable[str]] = ..., destination_profile: _Optional[_Union[_receiver_pb2.ProfileVersionRef, _Mapping]] = ...) -> None: ...

class CreateRuleSetRequest(_message.Message):
    __slots__ = ("tenant", "receiver", "rule_set", "activate")
    TENANT_FIELD_NUMBER: _ClassVar[int]
    RECEIVER_FIELD_NUMBER: _ClassVar[int]
    RULE_SET_FIELD_NUMBER: _ClassVar[int]
    ACTIVATE_FIELD_NUMBER: _ClassVar[int]
    tenant: str
    receiver: str
    rule_set: RuleSet
    activate: bool
    def __init__(self, tenant: _Optional[str] = ..., receiver: _Optional[str] = ..., rule_set: _Optional[_Union[RuleSet, _Mapping]] = ..., activate: _Optional[bool] = ...) -> None: ...

class CreateRuleSetResponse(_message.Message):
    __slots__ = ("rule_set",)
    RULE_SET_FIELD_NUMBER: _ClassVar[int]
    rule_set: RuleSet
    def __init__(self, rule_set: _Optional[_Union[RuleSet, _Mapping]] = ...) -> None: ...

class GetRuleSetRequest(_message.Message):
    __slots__ = ("tenant", "receiver", "version")
    TENANT_FIELD_NUMBER: _ClassVar[int]
    RECEIVER_FIELD_NUMBER: _ClassVar[int]
    VERSION_FIELD_NUMBER: _ClassVar[int]
    tenant: str
    receiver: str
    version: int
    def __init__(self, tenant: _Optional[str] = ..., receiver: _Optional[str] = ..., version: _Optional[int] = ...) -> None: ...

class GetRuleSetResponse(_message.Message):
    __slots__ = ("rule_set",)
    RULE_SET_FIELD_NUMBER: _ClassVar[int]
    rule_set: RuleSet
    def __init__(self, rule_set: _Optional[_Union[RuleSet, _Mapping]] = ...) -> None: ...

class ListRuleSetsRequest(_message.Message):
    __slots__ = ("tenant", "receiver", "page_size", "page_token")
    TENANT_FIELD_NUMBER: _ClassVar[int]
    RECEIVER_FIELD_NUMBER: _ClassVar[int]
    PAGE_SIZE_FIELD_NUMBER: _ClassVar[int]
    PAGE_TOKEN_FIELD_NUMBER: _ClassVar[int]
    tenant: str
    receiver: str
    page_size: int
    page_token: str
    def __init__(self, tenant: _Optional[str] = ..., receiver: _Optional[str] = ..., page_size: _Optional[int] = ..., page_token: _Optional[str] = ...) -> None: ...

class ListRuleSetsResponse(_message.Message):
    __slots__ = ("rule_sets", "next_page_token")
    RULE_SETS_FIELD_NUMBER: _ClassVar[int]
    NEXT_PAGE_TOKEN_FIELD_NUMBER: _ClassVar[int]
    rule_sets: _containers.RepeatedCompositeFieldContainer[RuleSet]
    next_page_token: str
    def __init__(self, rule_sets: _Optional[_Iterable[_Union[RuleSet, _Mapping]]] = ..., next_page_token: _Optional[str] = ...) -> None: ...

class ActivateRuleSetRequest(_message.Message):
    __slots__ = ("tenant", "receiver", "version")
    TENANT_FIELD_NUMBER: _ClassVar[int]
    RECEIVER_FIELD_NUMBER: _ClassVar[int]
    VERSION_FIELD_NUMBER: _ClassVar[int]
    tenant: str
    receiver: str
    version: int
    def __init__(self, tenant: _Optional[str] = ..., receiver: _Optional[str] = ..., version: _Optional[int] = ...) -> None: ...

class ActivateRuleSetResponse(_message.Message):
    __slots__ = ("rule_set",)
    RULE_SET_FIELD_NUMBER: _ClassVar[int]
    rule_set: RuleSet
    def __init__(self, rule_set: _Optional[_Union[RuleSet, _Mapping]] = ...) -> None: ...

class ExplainRouteRequest(_message.Message):
    __slots__ = ("tenant", "receiver", "version", "file", "instance", "calling_ae_title", "called_ae_title", "show_values")
    TENANT_FIELD_NUMBER: _ClassVar[int]
    RECEIVER_FIELD_NUMBER: _ClassVar[int]
    VERSION_FIELD_NUMBER: _ClassVar[int]
    FILE_FIELD_NUMBER: _ClassVar[int]
    INSTANCE_FIELD_NUMBER: _ClassVar[int]
    CALLING_AE_TITLE_FIELD_NUMBER: _ClassVar[int]
    CALLED_AE_TITLE_FIELD_NUMBER: _ClassVar[int]
    SHOW_VALUES_FIELD_NUMBER: _ClassVar[int]
    tenant: str
    receiver: str
    version: int
    file: bytes
    instance: RouteInstance
    calling_ae_title: str
    called_ae_title: str
    show_values: bool
    def __init__(self, tenant: _Optional[str] = ..., receiver: _Optional[str] = ..., version: _Optional[int] = ..., file: _Optional[bytes] = ..., instance: _Optional[_Union[RouteInstance, _Mapping]] = ..., calling_ae_title: _Optional[str] = ..., called_ae_title: _Optional[str] = ..., show_values: _Optional[bool] = ...) -> None: ...

class RouteInstance(_message.Message):
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

class ExplainRouteResponse(_message.Message):
    __slots__ = ("version", "decision", "rules")
    VERSION_FIELD_NUMBER: _ClassVar[int]
    DECISION_FIELD_NUMBER: _ClassVar[int]
    RULES_FIELD_NUMBER: _ClassVar[int]
    version: int
    decision: RouteDecision
    rules: _containers.RepeatedCompositeFieldContainer[RuleResult]
    def __init__(self, version: _Optional[int] = ..., decision: _Optional[_Union[RouteDecision, _Mapping]] = ..., rules: _Optional[_Iterable[_Union[RuleResult, _Mapping]]] = ...) -> None: ...

class RouteDecision(_message.Message):
    __slots__ = ("archive_rule", "storage", "profile", "unrouted", "archive_unknown", "forward_rules", "forward_unknown")
    ARCHIVE_RULE_FIELD_NUMBER: _ClassVar[int]
    STORAGE_FIELD_NUMBER: _ClassVar[int]
    PROFILE_FIELD_NUMBER: _ClassVar[int]
    UNROUTED_FIELD_NUMBER: _ClassVar[int]
    ARCHIVE_UNKNOWN_FIELD_NUMBER: _ClassVar[int]
    FORWARD_RULES_FIELD_NUMBER: _ClassVar[int]
    FORWARD_UNKNOWN_FIELD_NUMBER: _ClassVar[int]
    archive_rule: str
    storage: str
    profile: _receiver_pb2.ProfileVersionRef
    unrouted: str
    archive_unknown: bool
    forward_rules: _containers.RepeatedScalarFieldContainer[str]
    forward_unknown: _containers.RepeatedScalarFieldContainer[str]
    def __init__(self, archive_rule: _Optional[str] = ..., storage: _Optional[str] = ..., profile: _Optional[_Union[_receiver_pb2.ProfileVersionRef, _Mapping]] = ..., unrouted: _Optional[str] = ..., archive_unknown: _Optional[bool] = ..., forward_rules: _Optional[_Iterable[str]] = ..., forward_unknown: _Optional[_Iterable[str]] = ...) -> None: ...

class RuleResult(_message.Message):
    __slots__ = ("phase", "rule", "truth", "unusable", "decided_by")
    PHASE_FIELD_NUMBER: _ClassVar[int]
    RULE_FIELD_NUMBER: _ClassVar[int]
    TRUTH_FIELD_NUMBER: _ClassVar[int]
    UNUSABLE_FIELD_NUMBER: _ClassVar[int]
    DECIDED_BY_FIELD_NUMBER: _ClassVar[int]
    phase: RulePhase
    rule: str
    truth: RouteTruth
    unusable: str
    decided_by: DecidingComparison
    def __init__(self, phase: _Optional[_Union[RulePhase, str]] = ..., rule: _Optional[str] = ..., truth: _Optional[_Union[RouteTruth, str]] = ..., unusable: _Optional[str] = ..., decided_by: _Optional[_Union[DecidingComparison, _Mapping]] = ...) -> None: ...

class DecidingComparison(_message.Message):
    __slots__ = ("comparison", "field", "tag", "vr", "present", "truth", "values")
    COMPARISON_FIELD_NUMBER: _ClassVar[int]
    FIELD_FIELD_NUMBER: _ClassVar[int]
    TAG_FIELD_NUMBER: _ClassVar[int]
    VR_FIELD_NUMBER: _ClassVar[int]
    PRESENT_FIELD_NUMBER: _ClassVar[int]
    TRUTH_FIELD_NUMBER: _ClassVar[int]
    VALUES_FIELD_NUMBER: _ClassVar[int]
    comparison: str
    field: str
    tag: str
    vr: str
    present: bool
    truth: RouteTruth
    values: _containers.RepeatedScalarFieldContainer[str]
    def __init__(self, comparison: _Optional[str] = ..., field: _Optional[str] = ..., tag: _Optional[str] = ..., vr: _Optional[str] = ..., present: _Optional[bool] = ..., truth: _Optional[_Union[RouteTruth, str]] = ..., values: _Optional[_Iterable[str]] = ...) -> None: ...

class DryRunRoutesRequest(_message.Message):
    __slots__ = ("tenant", "receiver", "version", "sessions")
    TENANT_FIELD_NUMBER: _ClassVar[int]
    RECEIVER_FIELD_NUMBER: _ClassVar[int]
    VERSION_FIELD_NUMBER: _ClassVar[int]
    SESSIONS_FIELD_NUMBER: _ClassVar[int]
    tenant: str
    receiver: str
    version: int
    sessions: int
    def __init__(self, tenant: _Optional[str] = ..., receiver: _Optional[str] = ..., version: _Optional[int] = ..., sessions: _Optional[int] = ...) -> None: ...

class DryRunRoutesResponse(_message.Message):
    __slots__ = ("version", "sessions", "same", "changed", "unknown")
    VERSION_FIELD_NUMBER: _ClassVar[int]
    SESSIONS_FIELD_NUMBER: _ClassVar[int]
    SAME_FIELD_NUMBER: _ClassVar[int]
    CHANGED_FIELD_NUMBER: _ClassVar[int]
    UNKNOWN_FIELD_NUMBER: _ClassVar[int]
    version: int
    sessions: _containers.RepeatedCompositeFieldContainer[DryRunSession]
    same: int
    changed: int
    unknown: int
    def __init__(self, version: _Optional[int] = ..., sessions: _Optional[_Iterable[_Union[DryRunSession, _Mapping]]] = ..., same: _Optional[int] = ..., changed: _Optional[int] = ..., unknown: _Optional[int] = ...) -> None: ...

class DryRunSession(_message.Message):
    __slots__ = ("subject_label", "session_label", "recorded_version", "recorded_archive_rule", "recorded_forward_rules", "archive_rule", "archive_unknown", "forward_rules", "forward_unknown", "outcome", "unreadable")
    SUBJECT_LABEL_FIELD_NUMBER: _ClassVar[int]
    SESSION_LABEL_FIELD_NUMBER: _ClassVar[int]
    RECORDED_VERSION_FIELD_NUMBER: _ClassVar[int]
    RECORDED_ARCHIVE_RULE_FIELD_NUMBER: _ClassVar[int]
    RECORDED_FORWARD_RULES_FIELD_NUMBER: _ClassVar[int]
    ARCHIVE_RULE_FIELD_NUMBER: _ClassVar[int]
    ARCHIVE_UNKNOWN_FIELD_NUMBER: _ClassVar[int]
    FORWARD_RULES_FIELD_NUMBER: _ClassVar[int]
    FORWARD_UNKNOWN_FIELD_NUMBER: _ClassVar[int]
    OUTCOME_FIELD_NUMBER: _ClassVar[int]
    UNREADABLE_FIELD_NUMBER: _ClassVar[int]
    subject_label: str
    session_label: str
    recorded_version: int
    recorded_archive_rule: str
    recorded_forward_rules: _containers.RepeatedScalarFieldContainer[str]
    archive_rule: str
    archive_unknown: bool
    forward_rules: _containers.RepeatedScalarFieldContainer[str]
    forward_unknown: _containers.RepeatedScalarFieldContainer[str]
    outcome: DryRunOutcome
    unreadable: str
    def __init__(self, subject_label: _Optional[str] = ..., session_label: _Optional[str] = ..., recorded_version: _Optional[int] = ..., recorded_archive_rule: _Optional[str] = ..., recorded_forward_rules: _Optional[_Iterable[str]] = ..., archive_rule: _Optional[str] = ..., archive_unknown: _Optional[bool] = ..., forward_rules: _Optional[_Iterable[str]] = ..., forward_unknown: _Optional[_Iterable[str]] = ..., outcome: _Optional[_Union[DryRunOutcome, str]] = ..., unreadable: _Optional[str] = ...) -> None: ...

class GetRouteHitsRequest(_message.Message):
    __slots__ = ("tenant", "receiver", "version")
    TENANT_FIELD_NUMBER: _ClassVar[int]
    RECEIVER_FIELD_NUMBER: _ClassVar[int]
    VERSION_FIELD_NUMBER: _ClassVar[int]
    tenant: str
    receiver: str
    version: int
    def __init__(self, tenant: _Optional[str] = ..., receiver: _Optional[str] = ..., version: _Optional[int] = ...) -> None: ...

class GetRouteHitsResponse(_message.Message):
    __slots__ = ("hits",)
    HITS_FIELD_NUMBER: _ClassVar[int]
    hits: _containers.RepeatedCompositeFieldContainer[RouteHit]
    def __init__(self, hits: _Optional[_Iterable[_Union[RouteHit, _Mapping]]] = ...) -> None: ...

class RouteHit(_message.Message):
    __slots__ = ("version", "phase", "rule", "receptions", "series", "last_time")
    VERSION_FIELD_NUMBER: _ClassVar[int]
    PHASE_FIELD_NUMBER: _ClassVar[int]
    RULE_FIELD_NUMBER: _ClassVar[int]
    RECEPTIONS_FIELD_NUMBER: _ClassVar[int]
    SERIES_FIELD_NUMBER: _ClassVar[int]
    LAST_TIME_FIELD_NUMBER: _ClassVar[int]
    version: int
    phase: RulePhase
    rule: str
    receptions: int
    series: int
    last_time: _timestamp_pb2.Timestamp
    def __init__(self, version: _Optional[int] = ..., phase: _Optional[_Union[RulePhase, str]] = ..., rule: _Optional[str] = ..., receptions: _Optional[int] = ..., series: _Optional[int] = ..., last_time: _Optional[_Union[datetime.datetime, _timestamp_pb2.Timestamp, _Mapping]] = ...) -> None: ...
