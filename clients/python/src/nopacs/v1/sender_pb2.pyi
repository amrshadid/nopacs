import datetime

from google.protobuf import duration_pb2 as _duration_pb2
from google.protobuf import field_mask_pb2 as _field_mask_pb2
from google.protobuf import timestamp_pb2 as _timestamp_pb2
from google.protobuf.internal import containers as _containers
from google.protobuf.internal import enum_type_wrapper as _enum_type_wrapper
from google.protobuf import descriptor as _descriptor
from google.protobuf import message as _message
from collections.abc import Iterable as _Iterable, Mapping as _Mapping
from typing import ClassVar as _ClassVar, Optional as _Optional, Union as _Union

DESCRIPTOR: _descriptor.FileDescriptor

class StowAuthentication(int, metaclass=_enum_type_wrapper.EnumTypeWrapper):
    __slots__ = ()
    STOW_AUTHENTICATION_UNSPECIFIED: _ClassVar[StowAuthentication]
    STOW_AUTHENTICATION_NONE: _ClassVar[StowAuthentication]
    STOW_AUTHENTICATION_BASIC: _ClassVar[StowAuthentication]
    STOW_AUTHENTICATION_BEARER: _ClassVar[StowAuthentication]
    STOW_AUTHENTICATION_HEADER: _ClassVar[StowAuthentication]

class SenderState(int, metaclass=_enum_type_wrapper.EnumTypeWrapper):
    __slots__ = ()
    SENDER_STATE_UNSPECIFIED: _ClassVar[SenderState]
    SENDER_STATE_UNKNOWN: _ClassVar[SenderState]
    SENDER_STATE_UP: _ClassVar[SenderState]
    SENDER_STATE_DOWN: _ClassVar[SenderState]

class SenderDownReason(int, metaclass=_enum_type_wrapper.EnumTypeWrapper):
    __slots__ = ()
    SENDER_DOWN_REASON_UNSPECIFIED: _ClassVar[SenderDownReason]
    SENDER_DOWN_REASON_DISABLED: _ClassVar[SenderDownReason]
    SENDER_DOWN_REASON_PEER_FAILING: _ClassVar[SenderDownReason]
    SENDER_DOWN_REASON_TENANT_NOT_READY: _ClassVar[SenderDownReason]
    SENDER_DOWN_REASON_STOPPING: _ClassVar[SenderDownReason]
    SENDER_DOWN_REASON_OTHER: _ClassVar[SenderDownReason]

class SenderFault(int, metaclass=_enum_type_wrapper.EnumTypeWrapper):
    __slots__ = ()
    SENDER_FAULT_UNSPECIFIED: _ClassVar[SenderFault]
    SENDER_FAULT_UNREACHABLE: _ClassVar[SenderFault]
    SENDER_FAULT_ADDRESS_NOT_ALLOWED: _ClassVar[SenderFault]
    SENDER_FAULT_TLS_FAILED: _ClassVar[SenderFault]
    SENDER_FAULT_REJECTED: _ClassVar[SenderFault]
    SENDER_FAULT_NOT_ACCEPTED: _ClassVar[SenderFault]
    SENDER_FAULT_WRONG_ANSWER: _ClassVar[SenderFault]

class SenderTestStep(int, metaclass=_enum_type_wrapper.EnumTypeWrapper):
    __slots__ = ()
    SENDER_TEST_STEP_UNSPECIFIED: _ClassVar[SenderTestStep]
    SENDER_TEST_STEP_RESOLVE: _ClassVar[SenderTestStep]
    SENDER_TEST_STEP_CONNECT: _ClassVar[SenderTestStep]
    SENDER_TEST_STEP_TLS: _ClassVar[SenderTestStep]
    SENDER_TEST_STEP_ASSOCIATE: _ClassVar[SenderTestStep]
    SENDER_TEST_STEP_REQUEST: _ClassVar[SenderTestStep]
    SENDER_TEST_STEP_RELEASE: _ClassVar[SenderTestStep]

class SenderTestOutcome(int, metaclass=_enum_type_wrapper.EnumTypeWrapper):
    __slots__ = ()
    SENDER_TEST_OUTCOME_UNSPECIFIED: _ClassVar[SenderTestOutcome]
    SENDER_TEST_OUTCOME_PASSED: _ClassVar[SenderTestOutcome]
    SENDER_TEST_OUTCOME_FAILED: _ClassVar[SenderTestOutcome]
    SENDER_TEST_OUTCOME_SKIPPED: _ClassVar[SenderTestOutcome]
STOW_AUTHENTICATION_UNSPECIFIED: StowAuthentication
STOW_AUTHENTICATION_NONE: StowAuthentication
STOW_AUTHENTICATION_BASIC: StowAuthentication
STOW_AUTHENTICATION_BEARER: StowAuthentication
STOW_AUTHENTICATION_HEADER: StowAuthentication
SENDER_STATE_UNSPECIFIED: SenderState
SENDER_STATE_UNKNOWN: SenderState
SENDER_STATE_UP: SenderState
SENDER_STATE_DOWN: SenderState
SENDER_DOWN_REASON_UNSPECIFIED: SenderDownReason
SENDER_DOWN_REASON_DISABLED: SenderDownReason
SENDER_DOWN_REASON_PEER_FAILING: SenderDownReason
SENDER_DOWN_REASON_TENANT_NOT_READY: SenderDownReason
SENDER_DOWN_REASON_STOPPING: SenderDownReason
SENDER_DOWN_REASON_OTHER: SenderDownReason
SENDER_FAULT_UNSPECIFIED: SenderFault
SENDER_FAULT_UNREACHABLE: SenderFault
SENDER_FAULT_ADDRESS_NOT_ALLOWED: SenderFault
SENDER_FAULT_TLS_FAILED: SenderFault
SENDER_FAULT_REJECTED: SenderFault
SENDER_FAULT_NOT_ACCEPTED: SenderFault
SENDER_FAULT_WRONG_ANSWER: SenderFault
SENDER_TEST_STEP_UNSPECIFIED: SenderTestStep
SENDER_TEST_STEP_RESOLVE: SenderTestStep
SENDER_TEST_STEP_CONNECT: SenderTestStep
SENDER_TEST_STEP_TLS: SenderTestStep
SENDER_TEST_STEP_ASSOCIATE: SenderTestStep
SENDER_TEST_STEP_REQUEST: SenderTestStep
SENDER_TEST_STEP_RELEASE: SenderTestStep
SENDER_TEST_OUTCOME_UNSPECIFIED: SenderTestOutcome
SENDER_TEST_OUTCOME_PASSED: SenderTestOutcome
SENDER_TEST_OUTCOME_FAILED: SenderTestOutcome
SENDER_TEST_OUTCOME_SKIPPED: SenderTestOutcome

class Sender(_message.Message):
    __slots__ = ("name", "dimse", "stow", "enabled", "max_associations", "max_bytes_per_second", "connect_timeout", "response_timeout", "health", "create_time", "update_time", "allow_identified")
    NAME_FIELD_NUMBER: _ClassVar[int]
    DIMSE_FIELD_NUMBER: _ClassVar[int]
    STOW_FIELD_NUMBER: _ClassVar[int]
    ENABLED_FIELD_NUMBER: _ClassVar[int]
    MAX_ASSOCIATIONS_FIELD_NUMBER: _ClassVar[int]
    MAX_BYTES_PER_SECOND_FIELD_NUMBER: _ClassVar[int]
    CONNECT_TIMEOUT_FIELD_NUMBER: _ClassVar[int]
    RESPONSE_TIMEOUT_FIELD_NUMBER: _ClassVar[int]
    HEALTH_FIELD_NUMBER: _ClassVar[int]
    CREATE_TIME_FIELD_NUMBER: _ClassVar[int]
    UPDATE_TIME_FIELD_NUMBER: _ClassVar[int]
    ALLOW_IDENTIFIED_FIELD_NUMBER: _ClassVar[int]
    name: str
    dimse: DimseSender
    stow: StowSender
    enabled: bool
    max_associations: int
    max_bytes_per_second: int
    connect_timeout: _duration_pb2.Duration
    response_timeout: _duration_pb2.Duration
    health: SenderHealth
    create_time: _timestamp_pb2.Timestamp
    update_time: _timestamp_pb2.Timestamp
    allow_identified: bool
    def __init__(self, name: _Optional[str] = ..., dimse: _Optional[_Union[DimseSender, _Mapping]] = ..., stow: _Optional[_Union[StowSender, _Mapping]] = ..., enabled: _Optional[bool] = ..., max_associations: _Optional[int] = ..., max_bytes_per_second: _Optional[int] = ..., connect_timeout: _Optional[_Union[datetime.timedelta, _duration_pb2.Duration, _Mapping]] = ..., response_timeout: _Optional[_Union[datetime.timedelta, _duration_pb2.Duration, _Mapping]] = ..., health: _Optional[_Union[SenderHealth, _Mapping]] = ..., create_time: _Optional[_Union[datetime.datetime, _timestamp_pb2.Timestamp, _Mapping]] = ..., update_time: _Optional[_Union[datetime.datetime, _timestamp_pb2.Timestamp, _Mapping]] = ..., allow_identified: _Optional[bool] = ...) -> None: ...

class DimseSender(_message.Message):
    __slots__ = ("host", "port", "called_ae_title", "calling_ae_title", "tls")
    HOST_FIELD_NUMBER: _ClassVar[int]
    PORT_FIELD_NUMBER: _ClassVar[int]
    CALLED_AE_TITLE_FIELD_NUMBER: _ClassVar[int]
    CALLING_AE_TITLE_FIELD_NUMBER: _ClassVar[int]
    TLS_FIELD_NUMBER: _ClassVar[int]
    host: str
    port: int
    called_ae_title: str
    calling_ae_title: str
    tls: SenderTls
    def __init__(self, host: _Optional[str] = ..., port: _Optional[int] = ..., called_ae_title: _Optional[str] = ..., calling_ae_title: _Optional[str] = ..., tls: _Optional[_Union[SenderTls, _Mapping]] = ...) -> None: ...

class StowSender(_message.Message):
    __slots__ = ("url", "authentication", "username", "header_name", "credential", "credential_set", "tls", "max_instances_per_request", "max_request_bytes")
    URL_FIELD_NUMBER: _ClassVar[int]
    AUTHENTICATION_FIELD_NUMBER: _ClassVar[int]
    USERNAME_FIELD_NUMBER: _ClassVar[int]
    HEADER_NAME_FIELD_NUMBER: _ClassVar[int]
    CREDENTIAL_FIELD_NUMBER: _ClassVar[int]
    CREDENTIAL_SET_FIELD_NUMBER: _ClassVar[int]
    TLS_FIELD_NUMBER: _ClassVar[int]
    MAX_INSTANCES_PER_REQUEST_FIELD_NUMBER: _ClassVar[int]
    MAX_REQUEST_BYTES_FIELD_NUMBER: _ClassVar[int]
    url: str
    authentication: StowAuthentication
    username: str
    header_name: str
    credential: str
    credential_set: bool
    tls: SenderTls
    max_instances_per_request: int
    max_request_bytes: int
    def __init__(self, url: _Optional[str] = ..., authentication: _Optional[_Union[StowAuthentication, str]] = ..., username: _Optional[str] = ..., header_name: _Optional[str] = ..., credential: _Optional[str] = ..., credential_set: _Optional[bool] = ..., tls: _Optional[_Union[SenderTls, _Mapping]] = ..., max_instances_per_request: _Optional[int] = ..., max_request_bytes: _Optional[int] = ...) -> None: ...

class SenderTls(_message.Message):
    __slots__ = ("ca_file", "certificate_file", "private_key_file", "server_name")
    CA_FILE_FIELD_NUMBER: _ClassVar[int]
    CERTIFICATE_FILE_FIELD_NUMBER: _ClassVar[int]
    PRIVATE_KEY_FILE_FIELD_NUMBER: _ClassVar[int]
    SERVER_NAME_FIELD_NUMBER: _ClassVar[int]
    ca_file: str
    certificate_file: str
    private_key_file: str
    server_name: str
    def __init__(self, ca_file: _Optional[str] = ..., certificate_file: _Optional[str] = ..., private_key_file: _Optional[str] = ..., server_name: _Optional[str] = ...) -> None: ...

class SenderHealth(_message.Message):
    __slots__ = ("state", "down_reason", "fault", "detail", "since", "failed_connections", "next_try_time", "last_success_time")
    STATE_FIELD_NUMBER: _ClassVar[int]
    DOWN_REASON_FIELD_NUMBER: _ClassVar[int]
    FAULT_FIELD_NUMBER: _ClassVar[int]
    DETAIL_FIELD_NUMBER: _ClassVar[int]
    SINCE_FIELD_NUMBER: _ClassVar[int]
    FAILED_CONNECTIONS_FIELD_NUMBER: _ClassVar[int]
    NEXT_TRY_TIME_FIELD_NUMBER: _ClassVar[int]
    LAST_SUCCESS_TIME_FIELD_NUMBER: _ClassVar[int]
    state: SenderState
    down_reason: SenderDownReason
    fault: SenderFault
    detail: str
    since: _timestamp_pb2.Timestamp
    failed_connections: int
    next_try_time: _timestamp_pb2.Timestamp
    last_success_time: _timestamp_pb2.Timestamp
    def __init__(self, state: _Optional[_Union[SenderState, str]] = ..., down_reason: _Optional[_Union[SenderDownReason, str]] = ..., fault: _Optional[_Union[SenderFault, str]] = ..., detail: _Optional[str] = ..., since: _Optional[_Union[datetime.datetime, _timestamp_pb2.Timestamp, _Mapping]] = ..., failed_connections: _Optional[int] = ..., next_try_time: _Optional[_Union[datetime.datetime, _timestamp_pb2.Timestamp, _Mapping]] = ..., last_success_time: _Optional[_Union[datetime.datetime, _timestamp_pb2.Timestamp, _Mapping]] = ...) -> None: ...

class SenderTestResult(_message.Message):
    __slots__ = ("step", "outcome", "duration", "fault", "detail")
    STEP_FIELD_NUMBER: _ClassVar[int]
    OUTCOME_FIELD_NUMBER: _ClassVar[int]
    DURATION_FIELD_NUMBER: _ClassVar[int]
    FAULT_FIELD_NUMBER: _ClassVar[int]
    DETAIL_FIELD_NUMBER: _ClassVar[int]
    step: SenderTestStep
    outcome: SenderTestOutcome
    duration: _duration_pb2.Duration
    fault: SenderFault
    detail: str
    def __init__(self, step: _Optional[_Union[SenderTestStep, str]] = ..., outcome: _Optional[_Union[SenderTestOutcome, str]] = ..., duration: _Optional[_Union[datetime.timedelta, _duration_pb2.Duration, _Mapping]] = ..., fault: _Optional[_Union[SenderFault, str]] = ..., detail: _Optional[str] = ...) -> None: ...

class AddSenderRequest(_message.Message):
    __slots__ = ("tenant", "sender")
    TENANT_FIELD_NUMBER: _ClassVar[int]
    SENDER_FIELD_NUMBER: _ClassVar[int]
    tenant: str
    sender: Sender
    def __init__(self, tenant: _Optional[str] = ..., sender: _Optional[_Union[Sender, _Mapping]] = ...) -> None: ...

class AddSenderResponse(_message.Message):
    __slots__ = ("sender",)
    SENDER_FIELD_NUMBER: _ClassVar[int]
    sender: Sender
    def __init__(self, sender: _Optional[_Union[Sender, _Mapping]] = ...) -> None: ...

class GetSenderRequest(_message.Message):
    __slots__ = ("tenant", "name")
    TENANT_FIELD_NUMBER: _ClassVar[int]
    NAME_FIELD_NUMBER: _ClassVar[int]
    tenant: str
    name: str
    def __init__(self, tenant: _Optional[str] = ..., name: _Optional[str] = ...) -> None: ...

class GetSenderResponse(_message.Message):
    __slots__ = ("sender",)
    SENDER_FIELD_NUMBER: _ClassVar[int]
    sender: Sender
    def __init__(self, sender: _Optional[_Union[Sender, _Mapping]] = ...) -> None: ...

class ListSendersRequest(_message.Message):
    __slots__ = ("tenant", "page_size", "page_token")
    TENANT_FIELD_NUMBER: _ClassVar[int]
    PAGE_SIZE_FIELD_NUMBER: _ClassVar[int]
    PAGE_TOKEN_FIELD_NUMBER: _ClassVar[int]
    tenant: str
    page_size: int
    page_token: str
    def __init__(self, tenant: _Optional[str] = ..., page_size: _Optional[int] = ..., page_token: _Optional[str] = ...) -> None: ...

class ListSendersResponse(_message.Message):
    __slots__ = ("senders", "next_page_token")
    SENDERS_FIELD_NUMBER: _ClassVar[int]
    NEXT_PAGE_TOKEN_FIELD_NUMBER: _ClassVar[int]
    senders: _containers.RepeatedCompositeFieldContainer[Sender]
    next_page_token: str
    def __init__(self, senders: _Optional[_Iterable[_Union[Sender, _Mapping]]] = ..., next_page_token: _Optional[str] = ...) -> None: ...

class SetSenderRequest(_message.Message):
    __slots__ = ("tenant", "sender", "update_mask")
    TENANT_FIELD_NUMBER: _ClassVar[int]
    SENDER_FIELD_NUMBER: _ClassVar[int]
    UPDATE_MASK_FIELD_NUMBER: _ClassVar[int]
    tenant: str
    sender: Sender
    update_mask: _field_mask_pb2.FieldMask
    def __init__(self, tenant: _Optional[str] = ..., sender: _Optional[_Union[Sender, _Mapping]] = ..., update_mask: _Optional[_Union[_field_mask_pb2.FieldMask, _Mapping]] = ...) -> None: ...

class SetSenderResponse(_message.Message):
    __slots__ = ("sender",)
    SENDER_FIELD_NUMBER: _ClassVar[int]
    sender: Sender
    def __init__(self, sender: _Optional[_Union[Sender, _Mapping]] = ...) -> None: ...

class RemoveSenderRequest(_message.Message):
    __slots__ = ("tenant", "name", "cancel_jobs")
    TENANT_FIELD_NUMBER: _ClassVar[int]
    NAME_FIELD_NUMBER: _ClassVar[int]
    CANCEL_JOBS_FIELD_NUMBER: _ClassVar[int]
    tenant: str
    name: str
    cancel_jobs: bool
    def __init__(self, tenant: _Optional[str] = ..., name: _Optional[str] = ..., cancel_jobs: _Optional[bool] = ...) -> None: ...

class RemoveSenderResponse(_message.Message):
    __slots__ = ("sender", "jobs_canceled")
    SENDER_FIELD_NUMBER: _ClassVar[int]
    JOBS_CANCELED_FIELD_NUMBER: _ClassVar[int]
    sender: Sender
    jobs_canceled: int
    def __init__(self, sender: _Optional[_Union[Sender, _Mapping]] = ..., jobs_canceled: _Optional[int] = ...) -> None: ...

class TestSenderRequest(_message.Message):
    __slots__ = ("tenant", "name")
    TENANT_FIELD_NUMBER: _ClassVar[int]
    NAME_FIELD_NUMBER: _ClassVar[int]
    tenant: str
    name: str
    def __init__(self, tenant: _Optional[str] = ..., name: _Optional[str] = ...) -> None: ...

class TestSenderResponse(_message.Message):
    __slots__ = ("passed", "results")
    PASSED_FIELD_NUMBER: _ClassVar[int]
    RESULTS_FIELD_NUMBER: _ClassVar[int]
    passed: bool
    results: _containers.RepeatedCompositeFieldContainer[SenderTestResult]
    def __init__(self, passed: _Optional[bool] = ..., results: _Optional[_Iterable[_Union[SenderTestResult, _Mapping]]] = ...) -> None: ...
