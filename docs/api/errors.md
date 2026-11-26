# Reasons and retry delays

[Documentation index](../README.md) · [The API: an overview](overview.md#errors) · [Error kinds and codes](../reference/errors.md) · [The clients](clients.md) · [Troubleshooting](../operate/troubleshooting.md)

An error's **code** says the kind of failure and whether to try again. Some failures carry
more: a **reason**, a fixed word that says which of the cases of that kind it is, and for the
ones that pass by themselves, a **retry delay**. A program should read both, because the
right action differs: `STORAGE_SWITCHING` should be retried in half a minute, `HELD` should
not be retried at all. A client that reads neither still has the code and the message.

## Where they travel

The reason is a `google.rpc.ErrorInfo` detail of the error, with the domain `nopacs`, the
reason, and a few metadata pairs. The delay is a `google.rpc.RetryInfo` detail
(`retry_delay`). The same two messages are used by Google's APIs, so most gRPC libraries
already know them. The server never sends a delay without a reason. A plain HTTP answer
that has a delay (a STOW-RS store, a download) also carries it in the `Retry-After` header
in seconds.

| Reason | Code (HTTP) | When | Metadata | Retry delay |
|---|---|---|---|---|
| `STORAGE_SWITCHING` | `UNAVAILABLE` (503) | a write to a storage being [switched to its replica](../operate/moving-off-a-storage.md) waits for the swap | none | 30 seconds |
| `NOT_ONLINE` | `UNAVAILABLE` (503) | the storage holds the object in a cold tier it cannot read at once, and no copy in step on another storage is online | `recall_state`: `none`, `requested`, `in_progress` or `failed` | 60 seconds while a recall is requested or in progress; none otherwise |
| `HELD` | `FAILED_PRECONDITION` (400) | a delete of data a [hold](../use/retention.md#holds) covers | `hold`: the hold's id | none |
| `RETENTION_MINIMUM` | `FAILED_PRECONDITION` (400) | a person's delete of data a [retention rule](../use/retention.md#keep-at-least-and-lifting-it) keeps at least until a time to come | `keep_until`: that time, RFC 3339, UTC | none |
| `TENANT_RESTORING` | `UNAVAILABLE` (503) | the tenant is being reconciled after a restore | none | none |
| `TENANT_MOVED` | `FAILED_PRECONDITION` (400) | a change to a tenant that was handed to another installation, now read only | none | none |
| `TENANT_FENCED` | `FAILED_PRECONDITION` (400) | the tenant's storages name another installation or a later generation; it serves no data | none | none |
| `RELABELED` | `NOT_FOUND` (404) | a label a relabel moved, asked by an operator or more | `correction`, `to_label` | none |
| `CURSOR_EXPIRED`, `EVENT_HISTORY_CHANGED` | `OUT_OF_RANGE` (400) | an event cursor that can no longer be answered; start over from a status | none | none |

What to do:

- **`STORAGE_SWITCHING`**. Wait the delay and send again. A switch pauses writes for as long as
  its drain takes, from seconds to the drain timeout. DICOM senders see an association
  refused as a transient failure and STOW-RS clients a 503 with `Retry-After`, which the
  sender retries by itself; reads are not paused.
- **`NOT_ONLINE`**. The object is neither missing nor damaged. A reader's request never starts
  a recall (a reader's key would be a lever on the site's bill), so waiting brings nothing
  back unless someone recalls it: with `recall_state` `none`, an operator or a job must ask.
  While a recall is `requested` or `in_progress` the answer carries a retry delay of 60
  seconds (`Retry-After` on a plain HTTP download), and waiting that long is worth it; with
  `none` or `failed` it carries none, and a person must ask for a recall, or look at why it
  failed. The reason is also given when a primary's object is missing and the copy in step
  on a replica is held cold. See [cold replicas](../operate/replicas.md#cold-archive-tier-replicas-and-recall).
- **`HELD`**. Do not retry. A person releases the hold, or does not delete.
- **`RETENTION_MINIMUM`**. Do not retry. Wait until `keep_until`, or an admin on the
  installation deletes with a stated reason.
- **The tenant states.** `TENANT_RESTORING` passes by itself when the reconcile ends, so
  retry with a backoff. `TENANT_MOVED` and `TENANT_FENCED` do not pass: the tenant is read
  only (moved) or serves nothing (fenced) on this installation, and a person must act. Other
  `UNAVAILABLE` answers with no reason (a database or storage that does not answer, a tenant
  that is not ready) may pass; retry with a backoff.
- Every other `FAILED_PRECONDITION` fails again until the state it names changes.

Both `HELD` and `RETENTION_MINIMUM` also say so at the start of the message
(`HELD: ...`), as does every reason, so a person reading a log sees them without decoding.

## How the command line shows them

`nopacs` writes the reason and the delay into the message, and exits with the status of the
kind ([error kinds](../reference/errors.md)). The words after the reason are the server's
and differ by case; the shape is:

```
$ nopacs correction delete lab1 sub-3f9a --reason "consent withdrawn" --yes
nopacs: HELD: a hold covers this subject (request 8f1c...)
$ echo $?
5
$ nopacs switch lab1 main newb --confirm sw1_4Zq8fN2k
nopacs: STORAGE_SWITCHING: another switch is running (try again in 30s)
$ echo $?
1
```

The delay is capped at 24 hours, and a negative or absurd one is ignored, so a hostile
answer cannot park a client or make it retry at once. `nopacs` does not retry by itself; a
script should read the exit status and the text.

## Reading them from another language

The detail is the same protobuf message over every protocol the port speaks.

**gRPC.** The details are in the trailer `grpc-status-details-bin`, a `google.rpc.Status`.
With Python's `grpcio`, `grpcio-status` and `googleapis-common-protos`:

```python
import grpc
from grpc_status import rpc_status
from google.rpc import error_details_pb2

def reason_and_delay(err: grpc.RpcError):
    status = rpc_status.from_call(err)   # None when the error carries no details
    reason, delay = "", None
    for d in (status.details if status else []):
        info, retry = error_details_pb2.ErrorInfo(), error_details_pb2.RetryInfo()
        if d.Unpack(info) and info.domain == "nopacs":
            reason = info.reason          # info.metadata["keep_until"], and so on
        elif d.Unpack(retry):
            delay = retry.retry_delay.ToTimedelta().total_seconds()
    return reason, delay
```

The generated client of this repository turns a failure into one `nopacs.Error` that keeps
the code, the message and the request id but not the details; to read them, call the stub
(`client.replicas`) and catch `grpc.RpcError` as above, or read the message's first word.

**Connect and plain JSON.** The error is JSON with `code`, `message` and a `details` list.
Each entry has `type` (`google.rpc.ErrorInfo` or `google.rpc.RetryInfo`) and `value`, the
message's protobuf bytes in base64 without padding; a Connect server may add a `debug`
object holding the same message as JSON. Decode `value` with your language's
`error_details` classes:

```sh
curl -s https://archive.example.org:8443/nopacs.v1.CorrectionService/Delete \
  -H "Authorization: Bearer $NOPACS_KEY" -H 'content-type: application/json' -d '{...}'
# {"code":"failed_precondition","message":"HELD: ...",
#  "details":[{"type":"google.rpc.ErrorInfo","value":"..."}]}
```

In TypeScript, `ConnectError.findDetails` of `@connectrpc/connect` decodes details given the
message's schema, from a package that has the `google.rpc` error details.

Reasons are fixed words, and a reason added later is one your client does not know. Treat
an unknown reason as its code alone, and never fail on one.
