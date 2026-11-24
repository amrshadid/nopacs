"""Python client for the NoPACs research imaging archive's API.

    import nopacs
    from nopacs.v1 import tenant_pb2

    with nopacs.Client("nopacs.example.org:8443", key) as client:
        for tenant in client.pages(client.tenants.ListTenants, tenant_pb2.ListTenantsRequest(), "tenants"):
            print(tenant.name)
"""

from nopacs import v1
from nopacs.client import Client
from nopacs.errors import Error

__all__ = ["Client", "Error", "v1"]
