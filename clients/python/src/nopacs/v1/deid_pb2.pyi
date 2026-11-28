import datetime

from google.protobuf import timestamp_pb2 as _timestamp_pb2
from nopacs.v1 import job_pb2 as _job_pb2
from nopacs.v1 import profile_pb2 as _profile_pb2
from google.protobuf.internal import containers as _containers
from google.protobuf import descriptor as _descriptor
from google.protobuf import message as _message
from collections.abc import Iterable as _Iterable, Mapping as _Mapping
from typing import ClassVar as _ClassVar, Optional as _Optional, Union as _Union

DESCRIPTOR: _descriptor.FileDescriptor

class PixelTemplate(_message.Message):
    __slots__ = ("manufacturer", "model", "rows", "columns", "sop_class", "rectangles")
    MANUFACTURER_FIELD_NUMBER: _ClassVar[int]
    MODEL_FIELD_NUMBER: _ClassVar[int]
    ROWS_FIELD_NUMBER: _ClassVar[int]
    COLUMNS_FIELD_NUMBER: _ClassVar[int]
    SOP_CLASS_FIELD_NUMBER: _ClassVar[int]
    RECTANGLES_FIELD_NUMBER: _ClassVar[int]
    manufacturer: str
    model: str
    rows: int
    columns: int
    sop_class: str
    rectangles: _containers.RepeatedCompositeFieldContainer[_profile_pb2.PixelRectangle]
    def __init__(self, manufacturer: _Optional[str] = ..., model: _Optional[str] = ..., rows: _Optional[int] = ..., columns: _Optional[int] = ..., sop_class: _Optional[str] = ..., rectangles: _Optional[_Iterable[_Union[_profile_pb2.PixelRectangle, _Mapping]]] = ...) -> None: ...

class AddPixelTemplatesRequest(_message.Message):
    __slots__ = ("tenant", "templates")
    TENANT_FIELD_NUMBER: _ClassVar[int]
    TEMPLATES_FIELD_NUMBER: _ClassVar[int]
    tenant: str
    templates: _containers.RepeatedCompositeFieldContainer[PixelTemplate]
    def __init__(self, tenant: _Optional[str] = ..., templates: _Optional[_Iterable[_Union[PixelTemplate, _Mapping]]] = ...) -> None: ...

class AddPixelTemplatesResponse(_message.Message):
    __slots__ = ("added",)
    ADDED_FIELD_NUMBER: _ClassVar[int]
    added: int
    def __init__(self, added: _Optional[int] = ...) -> None: ...

class RemovePixelTemplateRequest(_message.Message):
    __slots__ = ("tenant", "manufacturer", "model", "rows", "columns", "sop_class")
    TENANT_FIELD_NUMBER: _ClassVar[int]
    MANUFACTURER_FIELD_NUMBER: _ClassVar[int]
    MODEL_FIELD_NUMBER: _ClassVar[int]
    ROWS_FIELD_NUMBER: _ClassVar[int]
    COLUMNS_FIELD_NUMBER: _ClassVar[int]
    SOP_CLASS_FIELD_NUMBER: _ClassVar[int]
    tenant: str
    manufacturer: str
    model: str
    rows: int
    columns: int
    sop_class: str
    def __init__(self, tenant: _Optional[str] = ..., manufacturer: _Optional[str] = ..., model: _Optional[str] = ..., rows: _Optional[int] = ..., columns: _Optional[int] = ..., sop_class: _Optional[str] = ...) -> None: ...

class RemovePixelTemplateResponse(_message.Message):
    __slots__ = ()
    def __init__(self) -> None: ...

class ListPixelTemplatesRequest(_message.Message):
    __slots__ = ("tenant", "page_size", "page_token")
    TENANT_FIELD_NUMBER: _ClassVar[int]
    PAGE_SIZE_FIELD_NUMBER: _ClassVar[int]
    PAGE_TOKEN_FIELD_NUMBER: _ClassVar[int]
    tenant: str
    page_size: int
    page_token: str
    def __init__(self, tenant: _Optional[str] = ..., page_size: _Optional[int] = ..., page_token: _Optional[str] = ...) -> None: ...

class ListPixelTemplatesResponse(_message.Message):
    __slots__ = ("templates", "next_page_token")
    TEMPLATES_FIELD_NUMBER: _ClassVar[int]
    NEXT_PAGE_TOKEN_FIELD_NUMBER: _ClassVar[int]
    templates: _containers.RepeatedCompositeFieldContainer[PixelTemplate]
    next_page_token: str
    def __init__(self, templates: _Optional[_Iterable[_Union[PixelTemplate, _Mapping]]] = ..., next_page_token: _Optional[str] = ...) -> None: ...

class GetCoverageRequest(_message.Message):
    __slots__ = ("tenant", "page_size", "page_token")
    TENANT_FIELD_NUMBER: _ClassVar[int]
    PAGE_SIZE_FIELD_NUMBER: _ClassVar[int]
    PAGE_TOKEN_FIELD_NUMBER: _ClassVar[int]
    tenant: str
    page_size: int
    page_token: str
    def __init__(self, tenant: _Optional[str] = ..., page_size: _Optional[int] = ..., page_token: _Optional[str] = ...) -> None: ...

class GetCoverageResponse(_message.Message):
    __slots__ = ("report", "refreshing", "elements", "next_page_token")
    REPORT_FIELD_NUMBER: _ClassVar[int]
    REFRESHING_FIELD_NUMBER: _ClassVar[int]
    ELEMENTS_FIELD_NUMBER: _ClassVar[int]
    NEXT_PAGE_TOKEN_FIELD_NUMBER: _ClassVar[int]
    report: CoverageReport
    refreshing: _job_pb2.Job
    elements: _containers.RepeatedCompositeFieldContainer[CoverageElement]
    next_page_token: str
    def __init__(self, report: _Optional[_Union[CoverageReport, _Mapping]] = ..., refreshing: _Optional[_Union[_job_pb2.Job, _Mapping]] = ..., elements: _Optional[_Iterable[_Union[CoverageElement, _Mapping]]] = ..., next_page_token: _Optional[str] = ...) -> None: ...

class CoverageReport(_message.Message):
    __slots__ = ("job_id", "end_time", "scans_read", "scans_skipped", "instances", "instances_recorded", "instances_capped", "elements_not_listed", "distinct_elements")
    JOB_ID_FIELD_NUMBER: _ClassVar[int]
    END_TIME_FIELD_NUMBER: _ClassVar[int]
    SCANS_READ_FIELD_NUMBER: _ClassVar[int]
    SCANS_SKIPPED_FIELD_NUMBER: _ClassVar[int]
    INSTANCES_FIELD_NUMBER: _ClassVar[int]
    INSTANCES_RECORDED_FIELD_NUMBER: _ClassVar[int]
    INSTANCES_CAPPED_FIELD_NUMBER: _ClassVar[int]
    ELEMENTS_NOT_LISTED_FIELD_NUMBER: _ClassVar[int]
    DISTINCT_ELEMENTS_FIELD_NUMBER: _ClassVar[int]
    job_id: str
    end_time: _timestamp_pb2.Timestamp
    scans_read: int
    scans_skipped: int
    instances: int
    instances_recorded: int
    instances_capped: int
    elements_not_listed: int
    distinct_elements: int
    def __init__(self, job_id: _Optional[str] = ..., end_time: _Optional[_Union[datetime.datetime, _timestamp_pb2.Timestamp, _Mapping]] = ..., scans_read: _Optional[int] = ..., scans_skipped: _Optional[int] = ..., instances: _Optional[int] = ..., instances_recorded: _Optional[int] = ..., instances_capped: _Optional[int] = ..., elements_not_listed: _Optional[int] = ..., distinct_elements: _Optional[int] = ...) -> None: ...

class CoverageElement(_message.Message):
    __slots__ = ("tag", "private", "private_creator", "creator_withheld", "vr", "instances", "occurrences")
    TAG_FIELD_NUMBER: _ClassVar[int]
    PRIVATE_FIELD_NUMBER: _ClassVar[int]
    PRIVATE_CREATOR_FIELD_NUMBER: _ClassVar[int]
    CREATOR_WITHHELD_FIELD_NUMBER: _ClassVar[int]
    VR_FIELD_NUMBER: _ClassVar[int]
    INSTANCES_FIELD_NUMBER: _ClassVar[int]
    OCCURRENCES_FIELD_NUMBER: _ClassVar[int]
    tag: str
    private: bool
    private_creator: str
    creator_withheld: bool
    vr: str
    instances: int
    occurrences: int
    def __init__(self, tag: _Optional[str] = ..., private: _Optional[bool] = ..., private_creator: _Optional[str] = ..., creator_withheld: _Optional[bool] = ..., vr: _Optional[str] = ..., instances: _Optional[int] = ..., occurrences: _Optional[int] = ...) -> None: ...

class RefreshCoverageRequest(_message.Message):
    __slots__ = ("tenant",)
    TENANT_FIELD_NUMBER: _ClassVar[int]
    tenant: str
    def __init__(self, tenant: _Optional[str] = ...) -> None: ...

class RefreshCoverageResponse(_message.Message):
    __slots__ = ("job",)
    JOB_FIELD_NUMBER: _ClassVar[int]
    job: _job_pb2.Job
    def __init__(self, job: _Optional[_Union[_job_pb2.Job, _Mapping]] = ...) -> None: ...

class CodeRequest(_message.Message):
    __slots__ = ("tenant", "attribute", "value", "issuer", "namespace")
    TENANT_FIELD_NUMBER: _ClassVar[int]
    ATTRIBUTE_FIELD_NUMBER: _ClassVar[int]
    VALUE_FIELD_NUMBER: _ClassVar[int]
    ISSUER_FIELD_NUMBER: _ClassVar[int]
    NAMESPACE_FIELD_NUMBER: _ClassVar[int]
    tenant: str
    attribute: str
    value: str
    issuer: PatientIDIssuer
    namespace: str
    def __init__(self, tenant: _Optional[str] = ..., attribute: _Optional[str] = ..., value: _Optional[str] = ..., issuer: _Optional[_Union[PatientIDIssuer, _Mapping]] = ..., namespace: _Optional[str] = ...) -> None: ...

class PatientIDIssuer(_message.Message):
    __slots__ = ("issuer", "universal_entity_id", "universal_entity_id_type", "identifier_type_code", "assigning_facility", "assigning_jurisdiction", "assigning_agency")
    ISSUER_FIELD_NUMBER: _ClassVar[int]
    UNIVERSAL_ENTITY_ID_FIELD_NUMBER: _ClassVar[int]
    UNIVERSAL_ENTITY_ID_TYPE_FIELD_NUMBER: _ClassVar[int]
    IDENTIFIER_TYPE_CODE_FIELD_NUMBER: _ClassVar[int]
    ASSIGNING_FACILITY_FIELD_NUMBER: _ClassVar[int]
    ASSIGNING_JURISDICTION_FIELD_NUMBER: _ClassVar[int]
    ASSIGNING_AGENCY_FIELD_NUMBER: _ClassVar[int]
    issuer: str
    universal_entity_id: str
    universal_entity_id_type: str
    identifier_type_code: str
    assigning_facility: IssuerDesignator
    assigning_jurisdiction: IssuerCode
    assigning_agency: IssuerCode
    def __init__(self, issuer: _Optional[str] = ..., universal_entity_id: _Optional[str] = ..., universal_entity_id_type: _Optional[str] = ..., identifier_type_code: _Optional[str] = ..., assigning_facility: _Optional[_Union[IssuerDesignator, _Mapping]] = ..., assigning_jurisdiction: _Optional[_Union[IssuerCode, _Mapping]] = ..., assigning_agency: _Optional[_Union[IssuerCode, _Mapping]] = ...) -> None: ...

class IssuerDesignator(_message.Message):
    __slots__ = ("local_namespace_entity_id", "universal_entity_id", "universal_entity_id_type")
    LOCAL_NAMESPACE_ENTITY_ID_FIELD_NUMBER: _ClassVar[int]
    UNIVERSAL_ENTITY_ID_FIELD_NUMBER: _ClassVar[int]
    UNIVERSAL_ENTITY_ID_TYPE_FIELD_NUMBER: _ClassVar[int]
    local_namespace_entity_id: str
    universal_entity_id: str
    universal_entity_id_type: str
    def __init__(self, local_namespace_entity_id: _Optional[str] = ..., universal_entity_id: _Optional[str] = ..., universal_entity_id_type: _Optional[str] = ...) -> None: ...

class IssuerCode(_message.Message):
    __slots__ = ("code_value", "coding_scheme_designator", "long_code_value", "urn_code_value")
    CODE_VALUE_FIELD_NUMBER: _ClassVar[int]
    CODING_SCHEME_DESIGNATOR_FIELD_NUMBER: _ClassVar[int]
    LONG_CODE_VALUE_FIELD_NUMBER: _ClassVar[int]
    URN_CODE_VALUE_FIELD_NUMBER: _ClassVar[int]
    code_value: str
    coding_scheme_designator: str
    long_code_value: str
    urn_code_value: str
    def __init__(self, code_value: _Optional[str] = ..., coding_scheme_designator: _Optional[str] = ..., long_code_value: _Optional[str] = ..., urn_code_value: _Optional[str] = ...) -> None: ...

class CodeResponse(_message.Message):
    __slots__ = ("attribute", "code", "subjects", "lookups")
    ATTRIBUTE_FIELD_NUMBER: _ClassVar[int]
    CODE_FIELD_NUMBER: _ClassVar[int]
    SUBJECTS_FIELD_NUMBER: _ClassVar[int]
    LOOKUPS_FIELD_NUMBER: _ClassVar[int]
    attribute: str
    code: str
    subjects: _containers.RepeatedCompositeFieldContainer[SubjectLabelCode]
    lookups: _containers.RepeatedCompositeFieldContainer[LookupTableCode]
    def __init__(self, attribute: _Optional[str] = ..., code: _Optional[str] = ..., subjects: _Optional[_Iterable[_Union[SubjectLabelCode, _Mapping]]] = ..., lookups: _Optional[_Iterable[_Union[LookupTableCode, _Mapping]]] = ...) -> None: ...

class SubjectLabelCode(_message.Message):
    __slots__ = ("receiver", "subject_label", "held")
    RECEIVER_FIELD_NUMBER: _ClassVar[int]
    SUBJECT_LABEL_FIELD_NUMBER: _ClassVar[int]
    HELD_FIELD_NUMBER: _ClassVar[int]
    receiver: str
    subject_label: str
    held: bool
    def __init__(self, receiver: _Optional[str] = ..., subject_label: _Optional[str] = ..., held: _Optional[bool] = ...) -> None: ...

class LookupTableCode(_message.Message):
    __slots__ = ("table", "code")
    TABLE_FIELD_NUMBER: _ClassVar[int]
    CODE_FIELD_NUMBER: _ClassVar[int]
    table: str
    code: str
    def __init__(self, table: _Optional[str] = ..., code: _Optional[str] = ...) -> None: ...

class ImportLookupCodesRequest(_message.Message):
    __slots__ = ("tenant", "table", "entries")
    TENANT_FIELD_NUMBER: _ClassVar[int]
    TABLE_FIELD_NUMBER: _ClassVar[int]
    ENTRIES_FIELD_NUMBER: _ClassVar[int]
    tenant: str
    table: str
    entries: _containers.RepeatedCompositeFieldContainer[LookupEntry]
    def __init__(self, tenant: _Optional[str] = ..., table: _Optional[str] = ..., entries: _Optional[_Iterable[_Union[LookupEntry, _Mapping]]] = ...) -> None: ...

class LookupEntry(_message.Message):
    __slots__ = ("value", "code", "issuer", "namespace")
    VALUE_FIELD_NUMBER: _ClassVar[int]
    CODE_FIELD_NUMBER: _ClassVar[int]
    ISSUER_FIELD_NUMBER: _ClassVar[int]
    NAMESPACE_FIELD_NUMBER: _ClassVar[int]
    value: str
    code: str
    issuer: PatientIDIssuer
    namespace: str
    def __init__(self, value: _Optional[str] = ..., code: _Optional[str] = ..., issuer: _Optional[_Union[PatientIDIssuer, _Mapping]] = ..., namespace: _Optional[str] = ...) -> None: ...

class ImportLookupCodesResponse(_message.Message):
    __slots__ = ("table", "added", "unchanged", "codes")
    TABLE_FIELD_NUMBER: _ClassVar[int]
    ADDED_FIELD_NUMBER: _ClassVar[int]
    UNCHANGED_FIELD_NUMBER: _ClassVar[int]
    CODES_FIELD_NUMBER: _ClassVar[int]
    table: str
    added: int
    unchanged: int
    codes: int
    def __init__(self, table: _Optional[str] = ..., added: _Optional[int] = ..., unchanged: _Optional[int] = ..., codes: _Optional[int] = ...) -> None: ...

class ListLookupTablesRequest(_message.Message):
    __slots__ = ("tenant", "page_size", "page_token")
    TENANT_FIELD_NUMBER: _ClassVar[int]
    PAGE_SIZE_FIELD_NUMBER: _ClassVar[int]
    PAGE_TOKEN_FIELD_NUMBER: _ClassVar[int]
    tenant: str
    page_size: int
    page_token: str
    def __init__(self, tenant: _Optional[str] = ..., page_size: _Optional[int] = ..., page_token: _Optional[str] = ...) -> None: ...

class ListLookupTablesResponse(_message.Message):
    __slots__ = ("tables", "next_page_token")
    TABLES_FIELD_NUMBER: _ClassVar[int]
    NEXT_PAGE_TOKEN_FIELD_NUMBER: _ClassVar[int]
    tables: _containers.RepeatedCompositeFieldContainer[LookupTable]
    next_page_token: str
    def __init__(self, tables: _Optional[_Iterable[_Union[LookupTable, _Mapping]]] = ..., next_page_token: _Optional[str] = ...) -> None: ...

class LookupTable(_message.Message):
    __slots__ = ("name", "codes")
    NAME_FIELD_NUMBER: _ClassVar[int]
    CODES_FIELD_NUMBER: _ClassVar[int]
    name: str
    codes: int
    def __init__(self, name: _Optional[str] = ..., codes: _Optional[int] = ...) -> None: ...
