# The API: an overview

[Documentation index](../README.md) · [Receiving through the API](reception-walkthrough.md) · [DICOMweb](dicomweb.md) · [The clients](clients.md) · [Error kinds and codes](../reference/errors.md) · [Reasons and retry delays](errors.md)

Everything `nopacs` does, it does through this API, and so can any program. The
contract is the protobuf files in `api/proto/nopacs/v1`: every method has a comment
that names the role it needs, what it writes to the audit log, and the error kinds it
can answer. This page is what holds for every method.

## Ways to call it

One port serves three protocols from the same methods, so use the simplest your
caller has:

| Protocol | For | Notes |
|---|---|---|
| **gRPC** | programs with a gRPC library (Python, Go, Java...) | `clients/python` uses it |
| **Connect** | browsers and `fetch`; the default of `clients/typescript` | works over HTTP/1.1 |
| **gRPC-web** | browsers that speak it | accepted by the server directly |
| **Plain JSON over HTTP** | `curl`, shell scripts | `POST /nopacs.v1.<Service>/<Method>` with `Content-Type: application/json` |

A call by `curl` on a server's local socket, with no key:

```sh
curl --unix-socket /var/lib/nopacs/run/nopacs.sock \
  -X POST http://local/nopacs.v1.AccessService/WhoAmI \
  -H 'content-type: application/json' -d '{}'
# {"principal":{"localOperator":{"userId":"502"}}}
```

Over the network the same call is made to `https://host:port` with the key as a
bearer token:

```sh
curl https://archive.example.org:8443/nopacs.v1.AccessService/WhoAmI \
  -H "Authorization: Bearer $NOPACS_KEY" -H 'content-type: application/json' -d '{}'
```

JSON names are the fields' lower camel case (`pageSize`), and enum values are their
full names (`TENANT_STATE_READY`). A 64-bit number is a string, as protobuf JSON says.

## Who is calling

A request on the network carries a key (`nopacs_<id>_<secret>`) in the
`Authorization: Bearer` header. A request on the local socket carries none: its caller
is the operating system user that runs the server. What each may do is in
[the security model](../concepts/security.md).

The checks run in a fixed order and the first to fail answers: the caller is
authenticated (`UNAUTHENTICATED`), holds the role the method needs
(`PERMISSION_DENIED`), and the request is valid (`INVALID_ARGUMENT`). So a caller
that may not call a method learns nothing about what a valid request looks like. A
request that is not even a well-formed call at the transport level is answered by the
protocol before any of this, and reveals nothing.

## Errors

Every failure has a **kind**, and a kind has one **code** and one **exit status** of
the command line. The codes and the HTTP status each travels with are in
[error kinds and codes](../reference/errors.md). A JSON error is:

```json
{"code":"not_found","message":"no tenant has this name",
 "details":[{"type":"google.rpc.ErrorInfo","value":"CglOT1RfRk9VTkQSBm5vcGFjcw"}]}
```

Every error carries a *reason* as a `google.rpc.ErrorInfo` detail with the domain
`nopacs` and a fixed word: the case's own where a kind names one (a relabeled label, a
moved tenant, a held delete, a cursor that expired), else the code's general reason
(`NOT_FOUND` above); a client that does not read details still has the code and the
message. A refusal that passes by itself (a storage being switched, an object
being recalled) may add a `google.rpc.RetryInfo` delay. The reasons, the delays and how to
read them in code are on [one page](errors.md). The message of an `INTERNAL` answer says nothing of the cause; the
cause is in the server's log under the request id.

## Request ids

The server makes an id for every request and returns it in the response header
`Nopacs-Request-Id`, on success and on error alike. A client cannot set it. The same
id is in the server's log line and in the request's audit entry, so quote it when you
report a problem. The command line prints it with an error.

## Paging

A list method takes `page_size` and `page_token` and answers `next_page_token`. It
never takes an offset. A page has 100 items unless `page_size` asks for another
number, and at most 1000; more is invalid. A page token is opaque (at most 1 KiB),
valid only with the request it came from, and an empty `nextPageToken` ends the
list. `nopacs` follows it for you with `--all`.

## Limits and time

A message over 4 MB is refused (`RESOURCE_EXHAUSTED`). Every call has a deadline;
when the client sends none, the server applies 30 seconds, except to the streams that
may legitimately run long (`MonitorService.Watch`, `DataService.ReadFiles`, the audit
export and the backup and tenant export streams). Times are UTC. Ids are opaque
strings.

## Changing a configuration: masks

A `Set` method that changes a configuration of several fields (`SetStorage`,
`SetReceiver`, `SetSender`) takes an `update_mask` naming the fields it changes. It is
required, and not empty. A named field takes the value given, its zero value
included, so a list can be emptied; a field not named keeps its value. A secret is
the exception: it is kept unless named and never set to empty.

## Secrets

A secret crosses the API in six fields only: a tenant database's address (in), an S3
secret key (in), a STOW-RS sender's credential (in), and a key's secret, a rotated
key's secret and a tenant export's transfer key (out, once). None is ever in an
answer a second time. The same rules keep them out of logs, events and audit entries.

## Repeating a call

Every `Get` and `List` method, `WhoAmI`, `Watch` and `ReadFiles` have no side effect
and may be repeated. Most changes that can safely be repeated are marked idempotent
in the contract and say what a repeat does. A change that makes a new thing (a
tenant, a key, a storage, a receiver, a sender, a job) is not repeated plainly.

## What is served

Every method of the contract is served but those `api/not-served.txt` in the repository
lists (at this writing, none). Calling one of those, as an authenticated caller, answers
`UNIMPLEMENTED`, and the tests keep the list true.

The services: access (keys and grants), audit and audit seal, backup (a backup of
the installation while it runs, and a tenant's export), catalog, correction, data (the files of a scan), deid, job, metadata,
monitor (status and the live event feed), profile, quarantine, receiver, reception,
replica, retention, sender, settings, storage and tenant. Each `*.proto` file holds one.

## Walkthroughs and clients

- [Receiving data through the API](reception-walkthrough.md): three uses, call by call.
- [DICOMweb](dicomweb.md): QIDO-RS, WADO-RS and STOW-RS, under `/tenants/<name>/dicom-web`.
- [The clients](clients.md): Python and TypeScript, generated from the contract.
- [Reading the archive](../use/reading.md): which call reads what.
