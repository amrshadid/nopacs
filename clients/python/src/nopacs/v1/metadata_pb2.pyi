from nopacs.v1 import profile_pb2 as _profile_pb2
from google.protobuf.internal import containers as _containers
from google.protobuf import descriptor as _descriptor
from google.protobuf import message as _message
from collections.abc import Iterable as _Iterable, Mapping as _Mapping
from typing import ClassVar as _ClassVar, Optional as _Optional, Union as _Union

DESCRIPTOR: _descriptor.FileDescriptor

class InstanceMetadata(_message.Message):
    __slots__ = ("instance_id", "sop_instance_uid", "dicom_json", "json_too_large", "transfer_syntax_uid", "bulk_data_omitted", "profile", "revision")
    INSTANCE_ID_FIELD_NUMBER: _ClassVar[int]
    SOP_INSTANCE_UID_FIELD_NUMBER: _ClassVar[int]
    DICOM_JSON_FIELD_NUMBER: _ClassVar[int]
    JSON_TOO_LARGE_FIELD_NUMBER: _ClassVar[int]
    TRANSFER_SYNTAX_UID_FIELD_NUMBER: _ClassVar[int]
    BULK_DATA_OMITTED_FIELD_NUMBER: _ClassVar[int]
    PROFILE_FIELD_NUMBER: _ClassVar[int]
    REVISION_FIELD_NUMBER: _ClassVar[int]
    instance_id: str
    sop_instance_uid: str
    dicom_json: str
    json_too_large: bool
    transfer_syntax_uid: str
    bulk_data_omitted: bool
    profile: _profile_pb2.ProfileVersionName
    revision: int
    def __init__(self, instance_id: _Optional[str] = ..., sop_instance_uid: _Optional[str] = ..., dicom_json: _Optional[str] = ..., json_too_large: _Optional[bool] = ..., transfer_syntax_uid: _Optional[str] = ..., bulk_data_omitted: _Optional[bool] = ..., profile: _Optional[_Union[_profile_pb2.ProfileVersionName, _Mapping]] = ..., revision: _Optional[int] = ...) -> None: ...

class GetScanMetadataRequest(_message.Message):
    __slots__ = ("tenant", "subject_label", "session_label", "scan_label", "page_size", "page_token")
    TENANT_FIELD_NUMBER: _ClassVar[int]
    SUBJECT_LABEL_FIELD_NUMBER: _ClassVar[int]
    SESSION_LABEL_FIELD_NUMBER: _ClassVar[int]
    SCAN_LABEL_FIELD_NUMBER: _ClassVar[int]
    PAGE_SIZE_FIELD_NUMBER: _ClassVar[int]
    PAGE_TOKEN_FIELD_NUMBER: _ClassVar[int]
    tenant: str
    subject_label: str
    session_label: str
    scan_label: str
    page_size: int
    page_token: str
    def __init__(self, tenant: _Optional[str] = ..., subject_label: _Optional[str] = ..., session_label: _Optional[str] = ..., scan_label: _Optional[str] = ..., page_size: _Optional[int] = ..., page_token: _Optional[str] = ...) -> None: ...

class GetScanMetadataResponse(_message.Message):
    __slots__ = ("instances", "next_page_token", "manifest_sha256")
    INSTANCES_FIELD_NUMBER: _ClassVar[int]
    NEXT_PAGE_TOKEN_FIELD_NUMBER: _ClassVar[int]
    MANIFEST_SHA256_FIELD_NUMBER: _ClassVar[int]
    instances: _containers.RepeatedCompositeFieldContainer[InstanceMetadata]
    next_page_token: str
    manifest_sha256: str
    def __init__(self, instances: _Optional[_Iterable[_Union[InstanceMetadata, _Mapping]]] = ..., next_page_token: _Optional[str] = ..., manifest_sha256: _Optional[str] = ...) -> None: ...

class GetInstanceMetadataRequest(_message.Message):
    __slots__ = ("tenant", "subject_label", "session_label", "scan_label", "instance_id")
    TENANT_FIELD_NUMBER: _ClassVar[int]
    SUBJECT_LABEL_FIELD_NUMBER: _ClassVar[int]
    SESSION_LABEL_FIELD_NUMBER: _ClassVar[int]
    SCAN_LABEL_FIELD_NUMBER: _ClassVar[int]
    INSTANCE_ID_FIELD_NUMBER: _ClassVar[int]
    tenant: str
    subject_label: str
    session_label: str
    scan_label: str
    instance_id: str
    def __init__(self, tenant: _Optional[str] = ..., subject_label: _Optional[str] = ..., session_label: _Optional[str] = ..., scan_label: _Optional[str] = ..., instance_id: _Optional[str] = ...) -> None: ...

class GetInstanceMetadataResponse(_message.Message):
    __slots__ = ("instance",)
    INSTANCE_FIELD_NUMBER: _ClassVar[int]
    instance: InstanceMetadata
    def __init__(self, instance: _Optional[_Union[InstanceMetadata, _Mapping]] = ...) -> None: ...
