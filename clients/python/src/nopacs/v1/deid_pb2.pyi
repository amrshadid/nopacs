from google.protobuf.internal import containers as _containers
from google.protobuf import descriptor as _descriptor
from google.protobuf import message as _message
from collections.abc import Iterable as _Iterable, Mapping as _Mapping
from typing import ClassVar as _ClassVar, Optional as _Optional, Union as _Union

DESCRIPTOR: _descriptor.FileDescriptor

class CodeRequest(_message.Message):
    __slots__ = ("tenant", "attribute", "value")
    TENANT_FIELD_NUMBER: _ClassVar[int]
    ATTRIBUTE_FIELD_NUMBER: _ClassVar[int]
    VALUE_FIELD_NUMBER: _ClassVar[int]
    tenant: str
    attribute: str
    value: str
    def __init__(self, tenant: _Optional[str] = ..., attribute: _Optional[str] = ..., value: _Optional[str] = ...) -> None: ...

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
    __slots__ = ("value", "code")
    VALUE_FIELD_NUMBER: _ClassVar[int]
    CODE_FIELD_NUMBER: _ClassVar[int]
    value: str
    code: str
    def __init__(self, value: _Optional[str] = ..., code: _Optional[str] = ...) -> None: ...

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
