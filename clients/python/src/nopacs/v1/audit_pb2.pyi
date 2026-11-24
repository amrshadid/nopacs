import datetime

from google.protobuf import timestamp_pb2 as _timestamp_pb2
from nopacs.v1 import scope_pb2 as _scope_pb2
from google.protobuf.internal import containers as _containers
from google.protobuf.internal import enum_type_wrapper as _enum_type_wrapper
from google.protobuf import descriptor as _descriptor
from google.protobuf import message as _message
from collections.abc import Iterable as _Iterable, Mapping as _Mapping
from typing import ClassVar as _ClassVar, Optional as _Optional, Union as _Union

DESCRIPTOR: _descriptor.FileDescriptor

class AuditOutcome(int, metaclass=_enum_type_wrapper.EnumTypeWrapper):
    __slots__ = ()
    AUDIT_OUTCOME_UNSPECIFIED: _ClassVar[AuditOutcome]
    AUDIT_OUTCOME_DONE: _ClassVar[AuditOutcome]
    AUDIT_OUTCOME_REFUSED: _ClassVar[AuditOutcome]
    AUDIT_OUTCOME_FAILED: _ClassVar[AuditOutcome]
AUDIT_OUTCOME_UNSPECIFIED: AuditOutcome
AUDIT_OUTCOME_DONE: AuditOutcome
AUDIT_OUTCOME_REFUSED: AuditOutcome
AUDIT_OUTCOME_FAILED: AuditOutcome

class AuditEntry(_message.Message):
    __slots__ = ("id", "time", "key_id", "local_user_id", "client_address", "method", "tenant", "object", "outcome", "request_id", "details", "reason")
    class DetailsEntry(_message.Message):
        __slots__ = ("key", "value")
        KEY_FIELD_NUMBER: _ClassVar[int]
        VALUE_FIELD_NUMBER: _ClassVar[int]
        key: str
        value: str
        def __init__(self, key: _Optional[str] = ..., value: _Optional[str] = ...) -> None: ...
    ID_FIELD_NUMBER: _ClassVar[int]
    TIME_FIELD_NUMBER: _ClassVar[int]
    KEY_ID_FIELD_NUMBER: _ClassVar[int]
    LOCAL_USER_ID_FIELD_NUMBER: _ClassVar[int]
    CLIENT_ADDRESS_FIELD_NUMBER: _ClassVar[int]
    METHOD_FIELD_NUMBER: _ClassVar[int]
    TENANT_FIELD_NUMBER: _ClassVar[int]
    OBJECT_FIELD_NUMBER: _ClassVar[int]
    OUTCOME_FIELD_NUMBER: _ClassVar[int]
    REQUEST_ID_FIELD_NUMBER: _ClassVar[int]
    DETAILS_FIELD_NUMBER: _ClassVar[int]
    REASON_FIELD_NUMBER: _ClassVar[int]
    id: str
    time: _timestamp_pb2.Timestamp
    key_id: str
    local_user_id: str
    client_address: str
    method: str
    tenant: str
    object: str
    outcome: AuditOutcome
    request_id: str
    details: _containers.ScalarMap[str, str]
    reason: str
    def __init__(self, id: _Optional[str] = ..., time: _Optional[_Union[datetime.datetime, _timestamp_pb2.Timestamp, _Mapping]] = ..., key_id: _Optional[str] = ..., local_user_id: _Optional[str] = ..., client_address: _Optional[str] = ..., method: _Optional[str] = ..., tenant: _Optional[str] = ..., object: _Optional[str] = ..., outcome: _Optional[_Union[AuditOutcome, str]] = ..., request_id: _Optional[str] = ..., details: _Optional[_Mapping[str, str]] = ..., reason: _Optional[str] = ...) -> None: ...

class ListAuditEntriesRequest(_message.Message):
    __slots__ = ("scope", "key_id", "outcome", "start_time", "end_time", "page_size", "page_token")
    SCOPE_FIELD_NUMBER: _ClassVar[int]
    KEY_ID_FIELD_NUMBER: _ClassVar[int]
    OUTCOME_FIELD_NUMBER: _ClassVar[int]
    START_TIME_FIELD_NUMBER: _ClassVar[int]
    END_TIME_FIELD_NUMBER: _ClassVar[int]
    PAGE_SIZE_FIELD_NUMBER: _ClassVar[int]
    PAGE_TOKEN_FIELD_NUMBER: _ClassVar[int]
    scope: _scope_pb2.Scope
    key_id: str
    outcome: AuditOutcome
    start_time: _timestamp_pb2.Timestamp
    end_time: _timestamp_pb2.Timestamp
    page_size: int
    page_token: str
    def __init__(self, scope: _Optional[_Union[_scope_pb2.Scope, _Mapping]] = ..., key_id: _Optional[str] = ..., outcome: _Optional[_Union[AuditOutcome, str]] = ..., start_time: _Optional[_Union[datetime.datetime, _timestamp_pb2.Timestamp, _Mapping]] = ..., end_time: _Optional[_Union[datetime.datetime, _timestamp_pb2.Timestamp, _Mapping]] = ..., page_size: _Optional[int] = ..., page_token: _Optional[str] = ...) -> None: ...

class ListAuditEntriesResponse(_message.Message):
    __slots__ = ("entries", "next_page_token")
    ENTRIES_FIELD_NUMBER: _ClassVar[int]
    NEXT_PAGE_TOKEN_FIELD_NUMBER: _ClassVar[int]
    entries: _containers.RepeatedCompositeFieldContainer[AuditEntry]
    next_page_token: str
    def __init__(self, entries: _Optional[_Iterable[_Union[AuditEntry, _Mapping]]] = ..., next_page_token: _Optional[str] = ...) -> None: ...
