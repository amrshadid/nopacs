from google.protobuf import descriptor as _descriptor
from google.protobuf import message as _message
from collections.abc import Mapping as _Mapping
from typing import ClassVar as _ClassVar, Optional as _Optional, Union as _Union

DESCRIPTOR: _descriptor.FileDescriptor

class Scope(_message.Message):
    __slots__ = ("tenant", "installation")
    TENANT_FIELD_NUMBER: _ClassVar[int]
    INSTALLATION_FIELD_NUMBER: _ClassVar[int]
    tenant: str
    installation: Installation
    def __init__(self, tenant: _Optional[str] = ..., installation: _Optional[_Union[Installation, _Mapping]] = ...) -> None: ...

class Installation(_message.Message):
    __slots__ = ()
    def __init__(self) -> None: ...
