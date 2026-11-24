import datetime

from google.protobuf import timestamp_pb2 as _timestamp_pb2
from google.protobuf.internal import containers as _containers
from google.protobuf.internal import enum_type_wrapper as _enum_type_wrapper
from google.protobuf import descriptor as _descriptor
from google.protobuf import message as _message
from collections.abc import Iterable as _Iterable, Mapping as _Mapping
from typing import ClassVar as _ClassVar, Optional as _Optional, Union as _Union

DESCRIPTOR: _descriptor.FileDescriptor

class TenantState(int, metaclass=_enum_type_wrapper.EnumTypeWrapper):
    __slots__ = ()
    TENANT_STATE_UNSPECIFIED: _ClassVar[TenantState]
    TENANT_STATE_READY: _ClassVar[TenantState]
    TENANT_STATE_UNREACHABLE: _ClassVar[TenantState]
    TENANT_STATE_UPGRADING: _ClassVar[TenantState]
    TENANT_STATE_FAILED: _ClassVar[TenantState]
    TENANT_STATE_RESTORING: _ClassVar[TenantState]
    TENANT_STATE_MOVED: _ClassVar[TenantState]
    TENANT_STATE_FENCED: _ClassVar[TenantState]

class DatabaseType(int, metaclass=_enum_type_wrapper.EnumTypeWrapper):
    __slots__ = ()
    DATABASE_TYPE_UNSPECIFIED: _ClassVar[DatabaseType]
    DATABASE_TYPE_SQLITE: _ClassVar[DatabaseType]
    DATABASE_TYPE_POSTGRES: _ClassVar[DatabaseType]
TENANT_STATE_UNSPECIFIED: TenantState
TENANT_STATE_READY: TenantState
TENANT_STATE_UNREACHABLE: TenantState
TENANT_STATE_UPGRADING: TenantState
TENANT_STATE_FAILED: TenantState
TENANT_STATE_RESTORING: TenantState
TENANT_STATE_MOVED: TenantState
TENANT_STATE_FENCED: TenantState
DATABASE_TYPE_UNSPECIFIED: DatabaseType
DATABASE_TYPE_SQLITE: DatabaseType
DATABASE_TYPE_POSTGRES: DatabaseType

class Tenant(_message.Message):
    __slots__ = ("name", "database_type", "state", "state_reason", "create_time", "state_time")
    NAME_FIELD_NUMBER: _ClassVar[int]
    DATABASE_TYPE_FIELD_NUMBER: _ClassVar[int]
    STATE_FIELD_NUMBER: _ClassVar[int]
    STATE_REASON_FIELD_NUMBER: _ClassVar[int]
    CREATE_TIME_FIELD_NUMBER: _ClassVar[int]
    STATE_TIME_FIELD_NUMBER: _ClassVar[int]
    name: str
    database_type: DatabaseType
    state: TenantState
    state_reason: str
    create_time: _timestamp_pb2.Timestamp
    state_time: _timestamp_pb2.Timestamp
    def __init__(self, name: _Optional[str] = ..., database_type: _Optional[_Union[DatabaseType, str]] = ..., state: _Optional[_Union[TenantState, str]] = ..., state_reason: _Optional[str] = ..., create_time: _Optional[_Union[datetime.datetime, _timestamp_pb2.Timestamp, _Mapping]] = ..., state_time: _Optional[_Union[datetime.datetime, _timestamp_pb2.Timestamp, _Mapping]] = ...) -> None: ...

class CreateTenantRequest(_message.Message):
    __slots__ = ("name", "database_type", "database_address")
    NAME_FIELD_NUMBER: _ClassVar[int]
    DATABASE_TYPE_FIELD_NUMBER: _ClassVar[int]
    DATABASE_ADDRESS_FIELD_NUMBER: _ClassVar[int]
    name: str
    database_type: DatabaseType
    database_address: str
    def __init__(self, name: _Optional[str] = ..., database_type: _Optional[_Union[DatabaseType, str]] = ..., database_address: _Optional[str] = ...) -> None: ...

class CreateTenantResponse(_message.Message):
    __slots__ = ("tenant",)
    TENANT_FIELD_NUMBER: _ClassVar[int]
    tenant: Tenant
    def __init__(self, tenant: _Optional[_Union[Tenant, _Mapping]] = ...) -> None: ...

class GetTenantRequest(_message.Message):
    __slots__ = ("name",)
    NAME_FIELD_NUMBER: _ClassVar[int]
    name: str
    def __init__(self, name: _Optional[str] = ...) -> None: ...

class GetTenantResponse(_message.Message):
    __slots__ = ("tenant",)
    TENANT_FIELD_NUMBER: _ClassVar[int]
    tenant: Tenant
    def __init__(self, tenant: _Optional[_Union[Tenant, _Mapping]] = ...) -> None: ...

class ListTenantsRequest(_message.Message):
    __slots__ = ("page_size", "page_token")
    PAGE_SIZE_FIELD_NUMBER: _ClassVar[int]
    PAGE_TOKEN_FIELD_NUMBER: _ClassVar[int]
    page_size: int
    page_token: str
    def __init__(self, page_size: _Optional[int] = ..., page_token: _Optional[str] = ...) -> None: ...

class ListTenantsResponse(_message.Message):
    __slots__ = ("tenants", "next_page_token")
    TENANTS_FIELD_NUMBER: _ClassVar[int]
    NEXT_PAGE_TOKEN_FIELD_NUMBER: _ClassVar[int]
    tenants: _containers.RepeatedCompositeFieldContainer[Tenant]
    next_page_token: str
    def __init__(self, tenants: _Optional[_Iterable[_Union[Tenant, _Mapping]]] = ..., next_page_token: _Optional[str] = ...) -> None: ...

class RemoveTenantRequest(_message.Message):
    __slots__ = ("name", "confirm_name")
    NAME_FIELD_NUMBER: _ClassVar[int]
    CONFIRM_NAME_FIELD_NUMBER: _ClassVar[int]
    name: str
    confirm_name: str
    def __init__(self, name: _Optional[str] = ..., confirm_name: _Optional[str] = ...) -> None: ...

class RemoveTenantResponse(_message.Message):
    __slots__ = ("database_type", "data_location")
    DATABASE_TYPE_FIELD_NUMBER: _ClassVar[int]
    DATA_LOCATION_FIELD_NUMBER: _ClassVar[int]
    database_type: DatabaseType
    data_location: str
    def __init__(self, database_type: _Optional[_Union[DatabaseType, str]] = ..., data_location: _Optional[str] = ...) -> None: ...

class ReclaimTenantRequest(_message.Message):
    __slots__ = ("name",)
    NAME_FIELD_NUMBER: _ClassVar[int]
    name: str
    def __init__(self, name: _Optional[str] = ...) -> None: ...

class ReclaimTenantResponse(_message.Message):
    __slots__ = ("tenant",)
    TENANT_FIELD_NUMBER: _ClassVar[int]
    tenant: Tenant
    def __init__(self, tenant: _Optional[_Union[Tenant, _Mapping]] = ...) -> None: ...
