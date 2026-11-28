import datetime

from google.protobuf import duration_pb2 as _duration_pb2
from google.protobuf import field_mask_pb2 as _field_mask_pb2
from google.protobuf import timestamp_pb2 as _timestamp_pb2
from nopacs.v1 import profile_pb2 as _profile_pb2
from google.protobuf.internal import containers as _containers
from google.protobuf.internal import enum_type_wrapper as _enum_type_wrapper
from google.protobuf import descriptor as _descriptor
from google.protobuf import message as _message
from collections.abc import Iterable as _Iterable, Mapping as _Mapping
from typing import ClassVar as _ClassVar, Optional as _Optional, Union as _Union

DESCRIPTOR: _descriptor.FileDescriptor

class ImportLayout(int, metaclass=_enum_type_wrapper.EnumTypeWrapper):
    __slots__ = ()
    IMPORT_LAYOUT_UNSPECIFIED: _ClassVar[ImportLayout]
    IMPORT_LAYOUT_PLAIN: _ClassVar[ImportLayout]
    IMPORT_LAYOUT_PATTERN: _ClassVar[ImportLayout]

class ReceiverState(int, metaclass=_enum_type_wrapper.EnumTypeWrapper):
    __slots__ = ()
    RECEIVER_STATE_UNSPECIFIED: _ClassVar[ReceiverState]
    RECEIVER_STATE_STARTING: _ClassVar[ReceiverState]
    RECEIVER_STATE_UP: _ClassVar[ReceiverState]
    RECEIVER_STATE_DOWN: _ClassVar[ReceiverState]
    RECEIVER_STATE_IDLE: _ClassVar[ReceiverState]

class ReceiverDownReason(int, metaclass=_enum_type_wrapper.EnumTypeWrapper):
    __slots__ = ()
    RECEIVER_DOWN_REASON_UNSPECIFIED: _ClassVar[ReceiverDownReason]
    RECEIVER_DOWN_REASON_DISABLED: _ClassVar[ReceiverDownReason]
    RECEIVER_DOWN_REASON_ADDRESS_IN_USE: _ClassVar[ReceiverDownReason]
    RECEIVER_DOWN_REASON_PERMISSION_DENIED: _ClassVar[ReceiverDownReason]
    RECEIVER_DOWN_REASON_ADDRESS_NOT_AVAILABLE: _ClassVar[ReceiverDownReason]
    RECEIVER_DOWN_REASON_TLS: _ClassVar[ReceiverDownReason]
    RECEIVER_DOWN_REASON_NEARLY_FULL: _ClassVar[ReceiverDownReason]
    RECEIVER_DOWN_REASON_TENANT_NOT_READY: _ClassVar[ReceiverDownReason]
    RECEIVER_DOWN_REASON_STOPPING: _ClassVar[ReceiverDownReason]
    RECEIVER_DOWN_REASON_OTHER: _ClassVar[ReceiverDownReason]
    RECEIVER_DOWN_REASON_INCOMING_LIMIT: _ClassVar[ReceiverDownReason]
IMPORT_LAYOUT_UNSPECIFIED: ImportLayout
IMPORT_LAYOUT_PLAIN: ImportLayout
IMPORT_LAYOUT_PATTERN: ImportLayout
RECEIVER_STATE_UNSPECIFIED: ReceiverState
RECEIVER_STATE_STARTING: ReceiverState
RECEIVER_STATE_UP: ReceiverState
RECEIVER_STATE_DOWN: ReceiverState
RECEIVER_STATE_IDLE: ReceiverState
RECEIVER_DOWN_REASON_UNSPECIFIED: ReceiverDownReason
RECEIVER_DOWN_REASON_DISABLED: ReceiverDownReason
RECEIVER_DOWN_REASON_ADDRESS_IN_USE: ReceiverDownReason
RECEIVER_DOWN_REASON_PERMISSION_DENIED: ReceiverDownReason
RECEIVER_DOWN_REASON_ADDRESS_NOT_AVAILABLE: ReceiverDownReason
RECEIVER_DOWN_REASON_TLS: ReceiverDownReason
RECEIVER_DOWN_REASON_NEARLY_FULL: ReceiverDownReason
RECEIVER_DOWN_REASON_TENANT_NOT_READY: ReceiverDownReason
RECEIVER_DOWN_REASON_STOPPING: ReceiverDownReason
RECEIVER_DOWN_REASON_OTHER: ReceiverDownReason
RECEIVER_DOWN_REASON_INCOMING_LIMIT: ReceiverDownReason

class Receiver(_message.Message):
    __slots__ = ("name", "dimse", "stow", "importer", "job", "enabled", "storage", "naming", "namespace", "quiet_period", "max_open_time", "max_file_bytes", "health", "create_time", "update_time", "forward_to", "as_received", "profile", "routing", "rule_set_version")
    NAME_FIELD_NUMBER: _ClassVar[int]
    DIMSE_FIELD_NUMBER: _ClassVar[int]
    STOW_FIELD_NUMBER: _ClassVar[int]
    IMPORTER_FIELD_NUMBER: _ClassVar[int]
    JOB_FIELD_NUMBER: _ClassVar[int]
    ENABLED_FIELD_NUMBER: _ClassVar[int]
    STORAGE_FIELD_NUMBER: _ClassVar[int]
    NAMING_FIELD_NUMBER: _ClassVar[int]
    NAMESPACE_FIELD_NUMBER: _ClassVar[int]
    QUIET_PERIOD_FIELD_NUMBER: _ClassVar[int]
    MAX_OPEN_TIME_FIELD_NUMBER: _ClassVar[int]
    MAX_FILE_BYTES_FIELD_NUMBER: _ClassVar[int]
    HEALTH_FIELD_NUMBER: _ClassVar[int]
    CREATE_TIME_FIELD_NUMBER: _ClassVar[int]
    UPDATE_TIME_FIELD_NUMBER: _ClassVar[int]
    FORWARD_TO_FIELD_NUMBER: _ClassVar[int]
    AS_RECEIVED_FIELD_NUMBER: _ClassVar[int]
    PROFILE_FIELD_NUMBER: _ClassVar[int]
    ROUTING_FIELD_NUMBER: _ClassVar[int]
    RULE_SET_VERSION_FIELD_NUMBER: _ClassVar[int]
    name: str
    dimse: DimseReceiver
    stow: StowReceiver
    importer: ImportReceiver
    job: JobReceiver
    enabled: bool
    storage: str
    naming: NamingRule
    namespace: str
    quiet_period: _duration_pb2.Duration
    max_open_time: _duration_pb2.Duration
    max_file_bytes: int
    health: ReceiverHealth
    create_time: _timestamp_pb2.Timestamp
    update_time: _timestamp_pb2.Timestamp
    forward_to: _containers.RepeatedScalarFieldContainer[str]
    as_received: AsReceived
    profile: ProfileVersionRef
    routing: ReceiverRouting
    rule_set_version: int
    def __init__(self, name: _Optional[str] = ..., dimse: _Optional[_Union[DimseReceiver, _Mapping]] = ..., stow: _Optional[_Union[StowReceiver, _Mapping]] = ..., importer: _Optional[_Union[ImportReceiver, _Mapping]] = ..., job: _Optional[_Union[JobReceiver, _Mapping]] = ..., enabled: _Optional[bool] = ..., storage: _Optional[str] = ..., naming: _Optional[_Union[NamingRule, _Mapping]] = ..., namespace: _Optional[str] = ..., quiet_period: _Optional[_Union[datetime.timedelta, _duration_pb2.Duration, _Mapping]] = ..., max_open_time: _Optional[_Union[datetime.timedelta, _duration_pb2.Duration, _Mapping]] = ..., max_file_bytes: _Optional[int] = ..., health: _Optional[_Union[ReceiverHealth, _Mapping]] = ..., create_time: _Optional[_Union[datetime.datetime, _timestamp_pb2.Timestamp, _Mapping]] = ..., update_time: _Optional[_Union[datetime.datetime, _timestamp_pb2.Timestamp, _Mapping]] = ..., forward_to: _Optional[_Iterable[str]] = ..., as_received: _Optional[_Union[AsReceived, _Mapping]] = ..., profile: _Optional[_Union[ProfileVersionRef, _Mapping]] = ..., routing: _Optional[_Union[ReceiverRouting, _Mapping]] = ..., rule_set_version: _Optional[int] = ...) -> None: ...

class ReceiverRouting(_message.Message):
    __slots__ = ("routes", "quarantine_unmatched")
    ROUTES_FIELD_NUMBER: _ClassVar[int]
    QUARANTINE_UNMATCHED_FIELD_NUMBER: _ClassVar[int]
    routes: _containers.RepeatedCompositeFieldContainer[Route]
    quarantine_unmatched: bool
    def __init__(self, routes: _Optional[_Iterable[_Union[Route, _Mapping]]] = ..., quarantine_unmatched: _Optional[bool] = ...) -> None: ...

class Route(_message.Message):
    __slots__ = ("name", "conditions", "storage", "profile", "forward_to")
    NAME_FIELD_NUMBER: _ClassVar[int]
    CONDITIONS_FIELD_NUMBER: _ClassVar[int]
    STORAGE_FIELD_NUMBER: _ClassVar[int]
    PROFILE_FIELD_NUMBER: _ClassVar[int]
    FORWARD_TO_FIELD_NUMBER: _ClassVar[int]
    name: str
    conditions: _containers.RepeatedCompositeFieldContainer[_profile_pb2.ProfileCondition]
    storage: str
    profile: ProfileVersionRef
    forward_to: RouteForwardTo
    def __init__(self, name: _Optional[str] = ..., conditions: _Optional[_Iterable[_Union[_profile_pb2.ProfileCondition, _Mapping]]] = ..., storage: _Optional[str] = ..., profile: _Optional[_Union[ProfileVersionRef, _Mapping]] = ..., forward_to: _Optional[_Union[RouteForwardTo, _Mapping]] = ...) -> None: ...

class RouteForwardTo(_message.Message):
    __slots__ = ("senders",)
    SENDERS_FIELD_NUMBER: _ClassVar[int]
    senders: _containers.RepeatedScalarFieldContainer[str]
    def __init__(self, senders: _Optional[_Iterable[str]] = ...) -> None: ...

class AsReceived(_message.Message):
    __slots__ = ()
    def __init__(self) -> None: ...

class ProfileVersionRef(_message.Message):
    __slots__ = ("name", "version")
    NAME_FIELD_NUMBER: _ClassVar[int]
    VERSION_FIELD_NUMBER: _ClassVar[int]
    name: str
    version: int
    def __init__(self, name: _Optional[str] = ..., version: _Optional[int] = ...) -> None: ...

class DimseReceiver(_message.Message):
    __slots__ = ("port", "bind_address", "ae_title", "allowed_callers", "any_caller", "tls", "sop_classes", "transfer_syntaxes")
    PORT_FIELD_NUMBER: _ClassVar[int]
    BIND_ADDRESS_FIELD_NUMBER: _ClassVar[int]
    AE_TITLE_FIELD_NUMBER: _ClassVar[int]
    ALLOWED_CALLERS_FIELD_NUMBER: _ClassVar[int]
    ANY_CALLER_FIELD_NUMBER: _ClassVar[int]
    TLS_FIELD_NUMBER: _ClassVar[int]
    SOP_CLASSES_FIELD_NUMBER: _ClassVar[int]
    TRANSFER_SYNTAXES_FIELD_NUMBER: _ClassVar[int]
    port: int
    bind_address: str
    ae_title: str
    allowed_callers: AllowedCallers
    any_caller: bool
    tls: ReceiverTls
    sop_classes: _containers.RepeatedScalarFieldContainer[str]
    transfer_syntaxes: _containers.RepeatedScalarFieldContainer[str]
    def __init__(self, port: _Optional[int] = ..., bind_address: _Optional[str] = ..., ae_title: _Optional[str] = ..., allowed_callers: _Optional[_Union[AllowedCallers, _Mapping]] = ..., any_caller: _Optional[bool] = ..., tls: _Optional[_Union[ReceiverTls, _Mapping]] = ..., sop_classes: _Optional[_Iterable[str]] = ..., transfer_syntaxes: _Optional[_Iterable[str]] = ...) -> None: ...

class StowReceiver(_message.Message):
    __slots__ = ("key_ids", "max_request_bytes")
    KEY_IDS_FIELD_NUMBER: _ClassVar[int]
    MAX_REQUEST_BYTES_FIELD_NUMBER: _ClassVar[int]
    key_ids: _containers.RepeatedScalarFieldContainer[str]
    max_request_bytes: int
    def __init__(self, key_ids: _Optional[_Iterable[str]] = ..., max_request_bytes: _Optional[int] = ...) -> None: ...

class ImportReceiver(_message.Message):
    __slots__ = ("root", "layout", "path_pattern", "unit_depth", "map_file", "map_key_columns", "map_value_column", "map_key_parts", "map_gives")
    ROOT_FIELD_NUMBER: _ClassVar[int]
    LAYOUT_FIELD_NUMBER: _ClassVar[int]
    PATH_PATTERN_FIELD_NUMBER: _ClassVar[int]
    UNIT_DEPTH_FIELD_NUMBER: _ClassVar[int]
    MAP_FILE_FIELD_NUMBER: _ClassVar[int]
    MAP_KEY_COLUMNS_FIELD_NUMBER: _ClassVar[int]
    MAP_VALUE_COLUMN_FIELD_NUMBER: _ClassVar[int]
    MAP_KEY_PARTS_FIELD_NUMBER: _ClassVar[int]
    MAP_GIVES_FIELD_NUMBER: _ClassVar[int]
    root: str
    layout: ImportLayout
    path_pattern: str
    unit_depth: int
    map_file: str
    map_key_columns: _containers.RepeatedScalarFieldContainer[str]
    map_value_column: str
    map_key_parts: _containers.RepeatedScalarFieldContainer[str]
    map_gives: str
    def __init__(self, root: _Optional[str] = ..., layout: _Optional[_Union[ImportLayout, str]] = ..., path_pattern: _Optional[str] = ..., unit_depth: _Optional[int] = ..., map_file: _Optional[str] = ..., map_key_columns: _Optional[_Iterable[str]] = ..., map_value_column: _Optional[str] = ..., map_key_parts: _Optional[_Iterable[str]] = ..., map_gives: _Optional[str] = ...) -> None: ...

class AllowedCallers(_message.Message):
    __slots__ = ("rules",)
    RULES_FIELD_NUMBER: _ClassVar[int]
    rules: _containers.RepeatedCompositeFieldContainer[CallerRule]
    def __init__(self, rules: _Optional[_Iterable[_Union[CallerRule, _Mapping]]] = ...) -> None: ...

class CallerRule(_message.Message):
    __slots__ = ("calling_ae_title", "address_range")
    CALLING_AE_TITLE_FIELD_NUMBER: _ClassVar[int]
    ADDRESS_RANGE_FIELD_NUMBER: _ClassVar[int]
    calling_ae_title: str
    address_range: str
    def __init__(self, calling_ae_title: _Optional[str] = ..., address_range: _Optional[str] = ...) -> None: ...

class ReceiverTls(_message.Message):
    __slots__ = ("certificate_file", "private_key_file")
    CERTIFICATE_FILE_FIELD_NUMBER: _ClassVar[int]
    PRIVATE_KEY_FILE_FIELD_NUMBER: _ClassVar[int]
    certificate_file: str
    private_key_file: str
    def __init__(self, certificate_file: _Optional[str] = ..., private_key_file: _Optional[str] = ...) -> None: ...

class NamingRule(_message.Message):
    __slots__ = ("subject", "session", "scan", "pattern")
    SUBJECT_FIELD_NUMBER: _ClassVar[int]
    SESSION_FIELD_NUMBER: _ClassVar[int]
    SCAN_FIELD_NUMBER: _ClassVar[int]
    PATTERN_FIELD_NUMBER: _ClassVar[int]
    subject: str
    session: str
    scan: str
    pattern: NamingPattern
    def __init__(self, subject: _Optional[str] = ..., session: _Optional[str] = ..., scan: _Optional[str] = ..., pattern: _Optional[_Union[NamingPattern, _Mapping]] = ...) -> None: ...

class NamingPattern(_message.Message):
    __slots__ = ("attribute", "expression")
    ATTRIBUTE_FIELD_NUMBER: _ClassVar[int]
    EXPRESSION_FIELD_NUMBER: _ClassVar[int]
    attribute: str
    expression: str
    def __init__(self, attribute: _Optional[str] = ..., expression: _Optional[str] = ...) -> None: ...

class ReceiverHealth(_message.Message):
    __slots__ = ("state", "down_reason", "detail", "since")
    STATE_FIELD_NUMBER: _ClassVar[int]
    DOWN_REASON_FIELD_NUMBER: _ClassVar[int]
    DETAIL_FIELD_NUMBER: _ClassVar[int]
    SINCE_FIELD_NUMBER: _ClassVar[int]
    state: ReceiverState
    down_reason: ReceiverDownReason
    detail: str
    since: _timestamp_pb2.Timestamp
    def __init__(self, state: _Optional[_Union[ReceiverState, str]] = ..., down_reason: _Optional[_Union[ReceiverDownReason, str]] = ..., detail: _Optional[str] = ..., since: _Optional[_Union[datetime.datetime, _timestamp_pb2.Timestamp, _Mapping]] = ...) -> None: ...

class AddReceiverRequest(_message.Message):
    __slots__ = ("tenant", "receiver")
    TENANT_FIELD_NUMBER: _ClassVar[int]
    RECEIVER_FIELD_NUMBER: _ClassVar[int]
    tenant: str
    receiver: Receiver
    def __init__(self, tenant: _Optional[str] = ..., receiver: _Optional[_Union[Receiver, _Mapping]] = ...) -> None: ...

class AddReceiverResponse(_message.Message):
    __slots__ = ("receiver",)
    RECEIVER_FIELD_NUMBER: _ClassVar[int]
    receiver: Receiver
    def __init__(self, receiver: _Optional[_Union[Receiver, _Mapping]] = ...) -> None: ...

class GetReceiverRequest(_message.Message):
    __slots__ = ("tenant", "name")
    TENANT_FIELD_NUMBER: _ClassVar[int]
    NAME_FIELD_NUMBER: _ClassVar[int]
    tenant: str
    name: str
    def __init__(self, tenant: _Optional[str] = ..., name: _Optional[str] = ...) -> None: ...

class GetReceiverResponse(_message.Message):
    __slots__ = ("receiver",)
    RECEIVER_FIELD_NUMBER: _ClassVar[int]
    receiver: Receiver
    def __init__(self, receiver: _Optional[_Union[Receiver, _Mapping]] = ...) -> None: ...

class ListReceiversRequest(_message.Message):
    __slots__ = ("tenant", "page_size", "page_token")
    TENANT_FIELD_NUMBER: _ClassVar[int]
    PAGE_SIZE_FIELD_NUMBER: _ClassVar[int]
    PAGE_TOKEN_FIELD_NUMBER: _ClassVar[int]
    tenant: str
    page_size: int
    page_token: str
    def __init__(self, tenant: _Optional[str] = ..., page_size: _Optional[int] = ..., page_token: _Optional[str] = ...) -> None: ...

class ListReceiversResponse(_message.Message):
    __slots__ = ("receivers", "next_page_token")
    RECEIVERS_FIELD_NUMBER: _ClassVar[int]
    NEXT_PAGE_TOKEN_FIELD_NUMBER: _ClassVar[int]
    receivers: _containers.RepeatedCompositeFieldContainer[Receiver]
    next_page_token: str
    def __init__(self, receivers: _Optional[_Iterable[_Union[Receiver, _Mapping]]] = ..., next_page_token: _Optional[str] = ...) -> None: ...

class SetReceiverRequest(_message.Message):
    __slots__ = ("tenant", "receiver", "update_mask", "expected_update_time", "force", "force_reason")
    TENANT_FIELD_NUMBER: _ClassVar[int]
    RECEIVER_FIELD_NUMBER: _ClassVar[int]
    UPDATE_MASK_FIELD_NUMBER: _ClassVar[int]
    EXPECTED_UPDATE_TIME_FIELD_NUMBER: _ClassVar[int]
    FORCE_FIELD_NUMBER: _ClassVar[int]
    FORCE_REASON_FIELD_NUMBER: _ClassVar[int]
    tenant: str
    receiver: Receiver
    update_mask: _field_mask_pb2.FieldMask
    expected_update_time: _timestamp_pb2.Timestamp
    force: bool
    force_reason: str
    def __init__(self, tenant: _Optional[str] = ..., receiver: _Optional[_Union[Receiver, _Mapping]] = ..., update_mask: _Optional[_Union[_field_mask_pb2.FieldMask, _Mapping]] = ..., expected_update_time: _Optional[_Union[datetime.datetime, _timestamp_pb2.Timestamp, _Mapping]] = ..., force: _Optional[bool] = ..., force_reason: _Optional[str] = ...) -> None: ...

class SetReceiverResponse(_message.Message):
    __slots__ = ("receiver",)
    RECEIVER_FIELD_NUMBER: _ClassVar[int]
    receiver: Receiver
    def __init__(self, receiver: _Optional[_Union[Receiver, _Mapping]] = ...) -> None: ...

class RemoveReceiverRequest(_message.Message):
    __slots__ = ("tenant", "name")
    TENANT_FIELD_NUMBER: _ClassVar[int]
    NAME_FIELD_NUMBER: _ClassVar[int]
    tenant: str
    name: str
    def __init__(self, tenant: _Optional[str] = ..., name: _Optional[str] = ...) -> None: ...

class RemoveReceiverResponse(_message.Message):
    __slots__ = ("receiver",)
    RECEIVER_FIELD_NUMBER: _ClassVar[int]
    receiver: Receiver
    def __init__(self, receiver: _Optional[_Union[Receiver, _Mapping]] = ...) -> None: ...

class JobReceiver(_message.Message):
    __slots__ = ()
    def __init__(self) -> None: ...

class TryReceiverProfileRequest(_message.Message):
    __slots__ = ("tenant", "receiver", "profile", "profile_version")
    TENANT_FIELD_NUMBER: _ClassVar[int]
    RECEIVER_FIELD_NUMBER: _ClassVar[int]
    PROFILE_FIELD_NUMBER: _ClassVar[int]
    PROFILE_VERSION_FIELD_NUMBER: _ClassVar[int]
    tenant: str
    receiver: str
    profile: str
    profile_version: int
    def __init__(self, tenant: _Optional[str] = ..., receiver: _Optional[str] = ..., profile: _Optional[str] = ..., profile_version: _Optional[int] = ...) -> None: ...

class TryReceiverProfileResponse(_message.Message):
    __slots__ = ("profile", "profile_version", "tried", "sessions", "files", "changed", "held", "unchanged", "skipped", "elements", "holds")
    PROFILE_FIELD_NUMBER: _ClassVar[int]
    PROFILE_VERSION_FIELD_NUMBER: _ClassVar[int]
    TRIED_FIELD_NUMBER: _ClassVar[int]
    SESSIONS_FIELD_NUMBER: _ClassVar[int]
    FILES_FIELD_NUMBER: _ClassVar[int]
    CHANGED_FIELD_NUMBER: _ClassVar[int]
    HELD_FIELD_NUMBER: _ClassVar[int]
    UNCHANGED_FIELD_NUMBER: _ClassVar[int]
    SKIPPED_FIELD_NUMBER: _ClassVar[int]
    ELEMENTS_FIELD_NUMBER: _ClassVar[int]
    HOLDS_FIELD_NUMBER: _ClassVar[int]
    profile: str
    profile_version: int
    tried: _timestamp_pb2.Timestamp
    sessions: int
    files: int
    changed: int
    held: int
    unchanged: int
    skipped: int
    elements: _containers.RepeatedCompositeFieldContainer[ChangeElement]
    holds: _containers.RepeatedCompositeFieldContainer[ChangeHold]
    def __init__(self, profile: _Optional[str] = ..., profile_version: _Optional[int] = ..., tried: _Optional[_Union[datetime.datetime, _timestamp_pb2.Timestamp, _Mapping]] = ..., sessions: _Optional[int] = ..., files: _Optional[int] = ..., changed: _Optional[int] = ..., held: _Optional[int] = ..., unchanged: _Optional[int] = ..., skipped: _Optional[int] = ..., elements: _Optional[_Iterable[_Union[ChangeElement, _Mapping]]] = ..., holds: _Optional[_Iterable[_Union[ChangeHold, _Mapping]]] = ...) -> None: ...

class ChangeElement(_message.Message):
    __slots__ = ("tag", "action", "files")
    TAG_FIELD_NUMBER: _ClassVar[int]
    ACTION_FIELD_NUMBER: _ClassVar[int]
    FILES_FIELD_NUMBER: _ClassVar[int]
    tag: str
    action: str
    files: int
    def __init__(self, tag: _Optional[str] = ..., action: _Optional[str] = ..., files: _Optional[int] = ...) -> None: ...

class ChangeHold(_message.Message):
    __slots__ = ("reason", "files")
    REASON_FIELD_NUMBER: _ClassVar[int]
    FILES_FIELD_NUMBER: _ClassVar[int]
    reason: str
    files: int
    def __init__(self, reason: _Optional[str] = ..., files: _Optional[int] = ...) -> None: ...
