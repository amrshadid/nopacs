# The clients

[Documentation index](../README.md) · [The API: an overview](overview.md) · [Receiving through the API](reception-walkthrough.md)

Two clients are in the repository, both **generated from the contract** in
`api/proto`, so each has every service and message the API has. A thin layer on top
opens the connection, sends the key, and turns every failure into one error type
that carries the code, the message, the request id and what the error details say:
the reason of its `google.rpc.ErrorInfo`, the case's metadata and the retry delay
([reasons and retry delays](errors.md)).
Neither is published to a package index yet: you install them from the checkout.

| | Python | TypeScript |
|---|---|---|
| Folder | `clients/python` | `clients/typescript` |
| Protocol | gRPC (`grpcio`) | Connect by default; gRPC and gRPC-web on Node |
| Needs | Python 3.10 or later | Node 22 or later, or a browser |
| Install | `pip install ./clients/python` | `npm ci --ignore-scripts && npm pack` in the folder, then `npm install` the packed file |

```python
import nopacs
from nopacs.v1 import tenant_pb2

with nopacs.Client("archive.example.org:8443", key) as client:
    for tenant in client.pages(client.tenants.ListTenants, tenant_pb2.ListTenantsRequest(), "tenants"):
        print(tenant.name)
```

`client.pages` follows `next_page_token` for you. The key is sent as
`Authorization: Bearer` with every call; a key a header cannot carry is refused
without repeating it. For a server with a private certificate authority pass its
certificate (`root_certificates`).

Each client's `README.md` has its full use, with examples in its `examples` folder
and its tests in `tests` or `test`. Other languages: any gRPC or Connect library
generates a client from the same `api/proto` files; or call the methods as JSON over
HTTP ([the overview](overview.md)).

## What is weak

The clients are version 0.0.0 and follow the contract as it stands, which is not yet
frozen. The contract is checked for breaking changes against the previous commit on
each change (`make proto-breaking`), but no release has promised a version yet.
