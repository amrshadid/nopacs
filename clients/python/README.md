# nopacs

The Python client of a NoPACs archive's API. Its code is generated from the
protobuf contract in `api/proto` with `grpcio-tools`, so it has every service
and message the contract has; a thin layer on top opens the connection, sends
the key and turns failures into one exception.

NoPACs' server speaks gRPC, and this client uses `grpcio`.

## Install

```sh
pip install ./clients/python
```

It needs Python 3.10 or later, `grpcio` and `protobuf`.

## Use

```python
import nopacs
from nopacs.v1 import tenant_pb2

with nopacs.Client("nopacs.example.org:8443", key) as client:
    # One page of tenants, and then every tenant, following the cursor.
    page = client.tenants.ListTenants(tenant_pb2.ListTenantsRequest(page_size=10))
    for tenant in client.pages(client.tenants.ListTenants, tenant_pb2.ListTenantsRequest(), "tenants"):
        print(tenant.name, tenant_pb2.TenantState.Name(tenant.state))
```

- `nopacs.Client(address, key, tls=True, root_certificates=None, timeout=60.0,
  allow_insecure=False)` opens the connection. `key` is the API key
  (`nopacs_<id>_<secret>`); it is sent with every call as
  `Authorization: Bearer <key>`. A key with a space, a line break or any other
  character a header cannot carry is refused with a `ValueError` that does not
  repeat it.
- `root_certificates` (PEM bytes) is for a server with a private certificate
  authority. It replaces the roots `grpcio` bundles, not the operating
  system's certificate store: pass the whole set you want trusted, and leave
  it out to use the bundled roots.
- `tls=False` is for a server on this machine (127.0.0.0/8, `::1`,
  `localhost` or a Unix socket) started without a certificate. For any other
  address the client refuses, because the key would travel in clear; pass
  `allow_insecure=True` only on a network you trust.
- `timeout` is the deadline of a unary call, 60 seconds by default and `None`
  for none. A server stream (`monitor.Watch`) has no deadline unless you pass
  `timeout=` to that call.
- One attribute per service: `access`, `audit`, `audit_seal`, `backups`,
  `catalog`, `corrections`, `data`, `jobs`,
  `metadata`, `monitor`, `profiles`, `quarantine`, `receivers`, `receptions`,
  `replicas`, `retention`, `senders`, `settings`, `storages`, `tenants`. Each method takes the request message and returns the
  response message; a server-streaming method returns an iterator of messages.
- `client.pages(call, request, field)` yields the items of a list call across
  all its pages, following `next_page_token` until it is empty. A server that
  answers the token it was just sent makes it raise `RuntimeError` instead of
  looping.
- A failed call raises `nopacs.Error`, with `code` (a `grpc.StatusCode`),
  `message` and `request_id` (the id to look for in the audit log; empty when
  the server sent none, for example when the connection failed).
- Requests and responses are the classes in `nopacs.v1` (`tenant_pb2`,
  `catalog_pb2`, ...). The contract's comments say what each method needs and
  answers.

## Develop

The generated code is `src/nopacs/v1/*_pb2.py`, `*_pb2.pyi` and `*_pb2_grpc.py`,
checked in and never edited by hand; `src/nopacs/v1/__init__.py` is hand-written.

```sh
pip install --require-hashes -r scripts/requirements/client-python.txt
pip install --no-deps --no-build-isolation ./clients/python
python clients/python/generate.py          # make it again
python clients/python/generate.py --check  # fail if it is out of date
python -m pytest clients/python/tests      # needs Go: builds and starts the server
```

The tests build the `nopacs` binary, start it on a temporary installation and
call it with a key made on that installation. The reading tests also send a
small public CT study (from `internal/receive/testdata`) with dcmtk's
`storescu`, found through `NOPACS_DCMTK` or the `PATH`, and skip when it is
missing; no other DICOM data is used.
