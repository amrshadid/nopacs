import datetime

from google.protobuf import timestamp_pb2 as _timestamp_pb2
from nopacs.v1 import audit_pb2 as _audit_pb2
from nopacs.v1 import scope_pb2 as _scope_pb2
from google.protobuf.internal import containers as _containers
from google.protobuf.internal import enum_type_wrapper as _enum_type_wrapper
from google.protobuf import descriptor as _descriptor
from google.protobuf import message as _message
from collections.abc import Iterable as _Iterable, Mapping as _Mapping
from typing import ClassVar as _ClassVar, Optional as _Optional, Union as _Union

DESCRIPTOR: _descriptor.FileDescriptor

class AuditVerifyProblemKind(int, metaclass=_enum_type_wrapper.EnumTypeWrapper):
    __slots__ = ()
    AUDIT_VERIFY_PROBLEM_KIND_UNSPECIFIED: _ClassVar[AuditVerifyProblemKind]
    AUDIT_VERIFY_PROBLEM_KIND_CHAIN_BROKEN: _ClassVar[AuditVerifyProblemKind]
    AUDIT_VERIFY_PROBLEM_KIND_ENTRY_MISSING: _ClassVar[AuditVerifyProblemKind]
    AUDIT_VERIFY_PROBLEM_KIND_BAD_SIGNATURE: _ClassVar[AuditVerifyProblemKind]
    AUDIT_VERIFY_PROBLEM_KIND_CHECKPOINT_OUT_OF_LINE: _ClassVar[AuditVerifyProblemKind]
    AUDIT_VERIFY_PROBLEM_KIND_ANCHOR_DISAGREES: _ClassVar[AuditVerifyProblemKind]
    AUDIT_VERIFY_PROBLEM_KIND_ROLLED_BACK: _ClassVar[AuditVerifyProblemKind]
    AUDIT_VERIFY_PROBLEM_KIND_SEALING_STALLED: _ClassVar[AuditVerifyProblemKind]
AUDIT_VERIFY_PROBLEM_KIND_UNSPECIFIED: AuditVerifyProblemKind
AUDIT_VERIFY_PROBLEM_KIND_CHAIN_BROKEN: AuditVerifyProblemKind
AUDIT_VERIFY_PROBLEM_KIND_ENTRY_MISSING: AuditVerifyProblemKind
AUDIT_VERIFY_PROBLEM_KIND_BAD_SIGNATURE: AuditVerifyProblemKind
AUDIT_VERIFY_PROBLEM_KIND_CHECKPOINT_OUT_OF_LINE: AuditVerifyProblemKind
AUDIT_VERIFY_PROBLEM_KIND_ANCHOR_DISAGREES: AuditVerifyProblemKind
AUDIT_VERIFY_PROBLEM_KIND_ROLLED_BACK: AuditVerifyProblemKind
AUDIT_VERIFY_PROBLEM_KIND_SEALING_STALLED: AuditVerifyProblemKind

class AuditCheckpoint(_message.Message):
    __slots__ = ("tenant", "from_seq", "to_seq", "entries", "head", "previous_digest", "digest", "time", "key_version", "signature", "signed_text", "installation_id", "log", "pruned")
    TENANT_FIELD_NUMBER: _ClassVar[int]
    FROM_SEQ_FIELD_NUMBER: _ClassVar[int]
    TO_SEQ_FIELD_NUMBER: _ClassVar[int]
    ENTRIES_FIELD_NUMBER: _ClassVar[int]
    HEAD_FIELD_NUMBER: _ClassVar[int]
    PREVIOUS_DIGEST_FIELD_NUMBER: _ClassVar[int]
    DIGEST_FIELD_NUMBER: _ClassVar[int]
    TIME_FIELD_NUMBER: _ClassVar[int]
    KEY_VERSION_FIELD_NUMBER: _ClassVar[int]
    SIGNATURE_FIELD_NUMBER: _ClassVar[int]
    SIGNED_TEXT_FIELD_NUMBER: _ClassVar[int]
    INSTALLATION_ID_FIELD_NUMBER: _ClassVar[int]
    LOG_FIELD_NUMBER: _ClassVar[int]
    PRUNED_FIELD_NUMBER: _ClassVar[int]
    tenant: str
    from_seq: int
    to_seq: int
    entries: int
    head: bytes
    previous_digest: bytes
    digest: bytes
    time: _timestamp_pb2.Timestamp
    key_version: int
    signature: bytes
    signed_text: bytes
    installation_id: str
    log: str
    pruned: bool
    def __init__(self, tenant: _Optional[str] = ..., from_seq: _Optional[int] = ..., to_seq: _Optional[int] = ..., entries: _Optional[int] = ..., head: _Optional[bytes] = ..., previous_digest: _Optional[bytes] = ..., digest: _Optional[bytes] = ..., time: _Optional[_Union[datetime.datetime, _timestamp_pb2.Timestamp, _Mapping]] = ..., key_version: _Optional[int] = ..., signature: _Optional[bytes] = ..., signed_text: _Optional[bytes] = ..., installation_id: _Optional[str] = ..., log: _Optional[str] = ..., pruned: _Optional[bool] = ...) -> None: ...

class AuditPublicKey(_message.Message):
    __slots__ = ("key_version", "public_key", "public_key_pem", "current", "installation_id")
    KEY_VERSION_FIELD_NUMBER: _ClassVar[int]
    PUBLIC_KEY_FIELD_NUMBER: _ClassVar[int]
    PUBLIC_KEY_PEM_FIELD_NUMBER: _ClassVar[int]
    CURRENT_FIELD_NUMBER: _ClassVar[int]
    INSTALLATION_ID_FIELD_NUMBER: _ClassVar[int]
    key_version: int
    public_key: bytes
    public_key_pem: str
    current: bool
    installation_id: str
    def __init__(self, key_version: _Optional[int] = ..., public_key: _Optional[bytes] = ..., public_key_pem: _Optional[str] = ..., current: _Optional[bool] = ..., installation_id: _Optional[str] = ...) -> None: ...

class AuditVerifyProblem(_message.Message):
    __slots__ = ("kind", "seq", "detail", "time")
    KIND_FIELD_NUMBER: _ClassVar[int]
    SEQ_FIELD_NUMBER: _ClassVar[int]
    DETAIL_FIELD_NUMBER: _ClassVar[int]
    TIME_FIELD_NUMBER: _ClassVar[int]
    kind: AuditVerifyProblemKind
    seq: int
    detail: str
    time: _timestamp_pb2.Timestamp
    def __init__(self, kind: _Optional[_Union[AuditVerifyProblemKind, str]] = ..., seq: _Optional[int] = ..., detail: _Optional[str] = ..., time: _Optional[_Union[datetime.datetime, _timestamp_pb2.Timestamp, _Mapping]] = ...) -> None: ...

class VerifyAuditRequest(_message.Message):
    __slots__ = ("scope", "from_seq", "to_seq", "expected_min_seq", "expected_checkpoint_digest")
    SCOPE_FIELD_NUMBER: _ClassVar[int]
    FROM_SEQ_FIELD_NUMBER: _ClassVar[int]
    TO_SEQ_FIELD_NUMBER: _ClassVar[int]
    EXPECTED_MIN_SEQ_FIELD_NUMBER: _ClassVar[int]
    EXPECTED_CHECKPOINT_DIGEST_FIELD_NUMBER: _ClassVar[int]
    scope: _scope_pb2.Scope
    from_seq: int
    to_seq: int
    expected_min_seq: int
    expected_checkpoint_digest: bytes
    def __init__(self, scope: _Optional[_Union[_scope_pb2.Scope, _Mapping]] = ..., from_seq: _Optional[int] = ..., to_seq: _Optional[int] = ..., expected_min_seq: _Optional[int] = ..., expected_checkpoint_digest: _Optional[bytes] = ...) -> None: ...

class VerifyAuditResponse(_message.Message):
    __slots__ = ("intact", "entries_checked", "checkpoints_checked", "unsealed_entries", "next_seq", "problems", "last_checkpoint")
    INTACT_FIELD_NUMBER: _ClassVar[int]
    ENTRIES_CHECKED_FIELD_NUMBER: _ClassVar[int]
    CHECKPOINTS_CHECKED_FIELD_NUMBER: _ClassVar[int]
    UNSEALED_ENTRIES_FIELD_NUMBER: _ClassVar[int]
    NEXT_SEQ_FIELD_NUMBER: _ClassVar[int]
    PROBLEMS_FIELD_NUMBER: _ClassVar[int]
    LAST_CHECKPOINT_FIELD_NUMBER: _ClassVar[int]
    intact: bool
    entries_checked: int
    checkpoints_checked: int
    unsealed_entries: int
    next_seq: int
    problems: _containers.RepeatedCompositeFieldContainer[AuditVerifyProblem]
    last_checkpoint: AuditCheckpoint
    def __init__(self, intact: _Optional[bool] = ..., entries_checked: _Optional[int] = ..., checkpoints_checked: _Optional[int] = ..., unsealed_entries: _Optional[int] = ..., next_seq: _Optional[int] = ..., problems: _Optional[_Iterable[_Union[AuditVerifyProblem, _Mapping]]] = ..., last_checkpoint: _Optional[_Union[AuditCheckpoint, _Mapping]] = ...) -> None: ...

class ListAuditCheckpointsRequest(_message.Message):
    __slots__ = ("scope", "page_size", "page_token")
    SCOPE_FIELD_NUMBER: _ClassVar[int]
    PAGE_SIZE_FIELD_NUMBER: _ClassVar[int]
    PAGE_TOKEN_FIELD_NUMBER: _ClassVar[int]
    scope: _scope_pb2.Scope
    page_size: int
    page_token: str
    def __init__(self, scope: _Optional[_Union[_scope_pb2.Scope, _Mapping]] = ..., page_size: _Optional[int] = ..., page_token: _Optional[str] = ...) -> None: ...

class ListAuditCheckpointsResponse(_message.Message):
    __slots__ = ("checkpoints", "next_page_token")
    CHECKPOINTS_FIELD_NUMBER: _ClassVar[int]
    NEXT_PAGE_TOKEN_FIELD_NUMBER: _ClassVar[int]
    checkpoints: _containers.RepeatedCompositeFieldContainer[AuditCheckpoint]
    next_page_token: str
    def __init__(self, checkpoints: _Optional[_Iterable[_Union[AuditCheckpoint, _Mapping]]] = ..., next_page_token: _Optional[str] = ...) -> None: ...

class ExportAuditRequest(_message.Message):
    __slots__ = ("scope", "from_seq", "to_seq")
    SCOPE_FIELD_NUMBER: _ClassVar[int]
    FROM_SEQ_FIELD_NUMBER: _ClassVar[int]
    TO_SEQ_FIELD_NUMBER: _ClassVar[int]
    scope: _scope_pb2.Scope
    from_seq: int
    to_seq: int
    def __init__(self, scope: _Optional[_Union[_scope_pb2.Scope, _Mapping]] = ..., from_seq: _Optional[int] = ..., to_seq: _Optional[int] = ...) -> None: ...

class AuditAnchor(_message.Message):
    __slots__ = ("log", "digest", "to_seq")
    LOG_FIELD_NUMBER: _ClassVar[int]
    DIGEST_FIELD_NUMBER: _ClassVar[int]
    TO_SEQ_FIELD_NUMBER: _ClassVar[int]
    log: str
    digest: bytes
    to_seq: int
    def __init__(self, log: _Optional[str] = ..., digest: _Optional[bytes] = ..., to_seq: _Optional[int] = ...) -> None: ...

class AuditExportHeader(_message.Message):
    __slots__ = ("installation_id", "tenant", "public_keys", "chain_before", "encoding_version", "log", "anchors", "from_seq")
    INSTALLATION_ID_FIELD_NUMBER: _ClassVar[int]
    TENANT_FIELD_NUMBER: _ClassVar[int]
    PUBLIC_KEYS_FIELD_NUMBER: _ClassVar[int]
    CHAIN_BEFORE_FIELD_NUMBER: _ClassVar[int]
    ENCODING_VERSION_FIELD_NUMBER: _ClassVar[int]
    LOG_FIELD_NUMBER: _ClassVar[int]
    ANCHORS_FIELD_NUMBER: _ClassVar[int]
    FROM_SEQ_FIELD_NUMBER: _ClassVar[int]
    installation_id: str
    tenant: str
    public_keys: _containers.RepeatedCompositeFieldContainer[AuditPublicKey]
    chain_before: bytes
    encoding_version: int
    log: str
    anchors: _containers.RepeatedCompositeFieldContainer[AuditAnchor]
    from_seq: int
    def __init__(self, installation_id: _Optional[str] = ..., tenant: _Optional[str] = ..., public_keys: _Optional[_Iterable[_Union[AuditPublicKey, _Mapping]]] = ..., chain_before: _Optional[bytes] = ..., encoding_version: _Optional[int] = ..., log: _Optional[str] = ..., anchors: _Optional[_Iterable[_Union[AuditAnchor, _Mapping]]] = ..., from_seq: _Optional[int] = ...) -> None: ...

class SealedAuditEntry(_message.Message):
    __slots__ = ("seq", "entry", "encoded", "chain")
    SEQ_FIELD_NUMBER: _ClassVar[int]
    ENTRY_FIELD_NUMBER: _ClassVar[int]
    ENCODED_FIELD_NUMBER: _ClassVar[int]
    CHAIN_FIELD_NUMBER: _ClassVar[int]
    seq: int
    entry: _audit_pb2.AuditEntry
    encoded: bytes
    chain: bytes
    def __init__(self, seq: _Optional[int] = ..., entry: _Optional[_Union[_audit_pb2.AuditEntry, _Mapping]] = ..., encoded: _Optional[bytes] = ..., chain: _Optional[bytes] = ...) -> None: ...

class ExportAuditResponse(_message.Message):
    __slots__ = ("header", "entry", "checkpoint")
    HEADER_FIELD_NUMBER: _ClassVar[int]
    ENTRY_FIELD_NUMBER: _ClassVar[int]
    CHECKPOINT_FIELD_NUMBER: _ClassVar[int]
    header: AuditExportHeader
    entry: SealedAuditEntry
    checkpoint: AuditCheckpoint
    def __init__(self, header: _Optional[_Union[AuditExportHeader, _Mapping]] = ..., entry: _Optional[_Union[SealedAuditEntry, _Mapping]] = ..., checkpoint: _Optional[_Union[AuditCheckpoint, _Mapping]] = ...) -> None: ...

class GetAuditPublicKeyRequest(_message.Message):
    __slots__ = ()
    def __init__(self) -> None: ...

class GetAuditPublicKeyResponse(_message.Message):
    __slots__ = ("installation_id", "public_keys")
    INSTALLATION_ID_FIELD_NUMBER: _ClassVar[int]
    PUBLIC_KEYS_FIELD_NUMBER: _ClassVar[int]
    installation_id: str
    public_keys: _containers.RepeatedCompositeFieldContainer[AuditPublicKey]
    def __init__(self, installation_id: _Optional[str] = ..., public_keys: _Optional[_Iterable[_Union[AuditPublicKey, _Mapping]]] = ...) -> None: ...

class AuditSealStatus(_message.Message):
    __slots__ = ("last_seq", "last_seal_time", "last_digest", "unsealed_entries")
    LAST_SEQ_FIELD_NUMBER: _ClassVar[int]
    LAST_SEAL_TIME_FIELD_NUMBER: _ClassVar[int]
    LAST_DIGEST_FIELD_NUMBER: _ClassVar[int]
    UNSEALED_ENTRIES_FIELD_NUMBER: _ClassVar[int]
    last_seq: int
    last_seal_time: _timestamp_pb2.Timestamp
    last_digest: bytes
    unsealed_entries: int
    def __init__(self, last_seq: _Optional[int] = ..., last_seal_time: _Optional[_Union[datetime.datetime, _timestamp_pb2.Timestamp, _Mapping]] = ..., last_digest: _Optional[bytes] = ..., unsealed_entries: _Optional[int] = ...) -> None: ...
