from nopacs.v1 import scope_pb2 as _scope_pb2
from google.protobuf.internal import containers as _containers
from google.protobuf.internal import enum_type_wrapper as _enum_type_wrapper
from google.protobuf import descriptor as _descriptor
from google.protobuf import message as _message
from collections.abc import Iterable as _Iterable, Mapping as _Mapping
from typing import ClassVar as _ClassVar, Optional as _Optional, Union as _Union

DESCRIPTOR: _descriptor.FileDescriptor

class SettingSource(int, metaclass=_enum_type_wrapper.EnumTypeWrapper):
    __slots__ = ()
    SETTING_SOURCE_UNSPECIFIED: _ClassVar[SettingSource]
    SETTING_SOURCE_BUILT_IN: _ClassVar[SettingSource]
    SETTING_SOURCE_INSTALLATION: _ClassVar[SettingSource]
    SETTING_SOURCE_TENANT: _ClassVar[SettingSource]
SETTING_SOURCE_UNSPECIFIED: SettingSource
SETTING_SOURCE_BUILT_IN: SettingSource
SETTING_SOURCE_INSTALLATION: SettingSource
SETTING_SOURCE_TENANT: SettingSource

class Setting(_message.Message):
    __slots__ = ("name", "scope", "value", "source")
    NAME_FIELD_NUMBER: _ClassVar[int]
    SCOPE_FIELD_NUMBER: _ClassVar[int]
    VALUE_FIELD_NUMBER: _ClassVar[int]
    SOURCE_FIELD_NUMBER: _ClassVar[int]
    name: str
    scope: _scope_pb2.Scope
    value: str
    source: SettingSource
    def __init__(self, name: _Optional[str] = ..., scope: _Optional[_Union[_scope_pb2.Scope, _Mapping]] = ..., value: _Optional[str] = ..., source: _Optional[_Union[SettingSource, str]] = ...) -> None: ...

class GetSettingRequest(_message.Message):
    __slots__ = ("name", "scope")
    NAME_FIELD_NUMBER: _ClassVar[int]
    SCOPE_FIELD_NUMBER: _ClassVar[int]
    name: str
    scope: _scope_pb2.Scope
    def __init__(self, name: _Optional[str] = ..., scope: _Optional[_Union[_scope_pb2.Scope, _Mapping]] = ...) -> None: ...

class GetSettingResponse(_message.Message):
    __slots__ = ("setting",)
    SETTING_FIELD_NUMBER: _ClassVar[int]
    setting: Setting
    def __init__(self, setting: _Optional[_Union[Setting, _Mapping]] = ...) -> None: ...

class SetSettingRequest(_message.Message):
    __slots__ = ("setting",)
    SETTING_FIELD_NUMBER: _ClassVar[int]
    setting: Setting
    def __init__(self, setting: _Optional[_Union[Setting, _Mapping]] = ...) -> None: ...

class SetSettingResponse(_message.Message):
    __slots__ = ("setting",)
    SETTING_FIELD_NUMBER: _ClassVar[int]
    setting: Setting
    def __init__(self, setting: _Optional[_Union[Setting, _Mapping]] = ...) -> None: ...

class UnsetSettingRequest(_message.Message):
    __slots__ = ("name", "scope")
    NAME_FIELD_NUMBER: _ClassVar[int]
    SCOPE_FIELD_NUMBER: _ClassVar[int]
    name: str
    scope: _scope_pb2.Scope
    def __init__(self, name: _Optional[str] = ..., scope: _Optional[_Union[_scope_pb2.Scope, _Mapping]] = ...) -> None: ...

class UnsetSettingResponse(_message.Message):
    __slots__ = ("setting",)
    SETTING_FIELD_NUMBER: _ClassVar[int]
    setting: Setting
    def __init__(self, setting: _Optional[_Union[Setting, _Mapping]] = ...) -> None: ...

class ListSettingsRequest(_message.Message):
    __slots__ = ("scope", "page_size", "page_token")
    SCOPE_FIELD_NUMBER: _ClassVar[int]
    PAGE_SIZE_FIELD_NUMBER: _ClassVar[int]
    PAGE_TOKEN_FIELD_NUMBER: _ClassVar[int]
    scope: _scope_pb2.Scope
    page_size: int
    page_token: str
    def __init__(self, scope: _Optional[_Union[_scope_pb2.Scope, _Mapping]] = ..., page_size: _Optional[int] = ..., page_token: _Optional[str] = ...) -> None: ...

class ListSettingsResponse(_message.Message):
    __slots__ = ("settings", "next_page_token")
    SETTINGS_FIELD_NUMBER: _ClassVar[int]
    NEXT_PAGE_TOKEN_FIELD_NUMBER: _ClassVar[int]
    settings: _containers.RepeatedCompositeFieldContainer[Setting]
    next_page_token: str
    def __init__(self, settings: _Optional[_Iterable[_Union[Setting, _Mapping]]] = ..., next_page_token: _Optional[str] = ...) -> None: ...
