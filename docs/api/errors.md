# Reasons and retry delays

[Documentation index](../README.md) · [The API: an overview](overview.md#errors) · [Error kinds and codes](../reference/errors.md) · [The clients](clients.md) · [Troubleshooting](../operate/troubleshooting.md)

An error's **code** says the kind of failure and whether to try again. Every error also
carries a **reason**, a fixed word that says which case of that kind it is, and the ones
that pass by themselves a **retry delay**. A program should read both, because the right
action differs: `STORAGE_SWITCHING` should be retried in half a minute, `HELD` should not be
retried at all. A client that reads neither still has the code and the message.

## Where they travel

Every error the server answers, whatever the protocol and wherever it was refused (before a
call is routed, by the access check, by a handler), carries a `google.rpc.ErrorInfo` detail
with the domain `nopacs`, the reason, and the metadata pairs its case names. A failure that
is one of the cases in the table below carries that case's reason. Any other failure carries
its code's **general reason**, the code's name in capitals (`NOT_FOUND`, `UNAVAILABLE`,
`INVALID_ARGUMENT`, `PERMISSION_DENIED`, ...): it says no more than the code, so a program
can branch on the reason alone. No case is spelled like a general reason.

The delay is a `google.rpc.RetryInfo` detail (`retry_delay`). The same two messages are
used by Google's APIs (the gRPC richer error model), so most gRPC libraries already know
them. The server never sends a delay without a case reason. A plain HTTP answer carries the
same details in its JSON body, and when it has a delay (a STOW-RS store, a download) also
the `Retry-After` header in seconds. The full list of reasons, generated from the code, is
in [error kinds and codes](../reference/errors.md#reasons).

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

A general reason is not written into the line: the exit status already says the code.

The delay is capped at 24 hours, and a negative or absurd one is ignored, so a hostile
answer cannot park a client or make it retry at once. `nopacs` does not retry by itself; a
script should read the exit status and, better, the reason.

With `--json` the error is one JSON object on one line of standard error (standard output
stays empty), so a script reads the reason without parsing the sentence:

```
$ nopacs --json switch lab1 main newb --confirm sw1_4Zq8fN2k
{"schemaVersion":1,"error":{"message":"switch: STORAGE_SWITCHING: another switch is running (try again in 30s) (request id 8f1c...)","exitCode":1,"code":"unavailable","reason":"STORAGE_SWITCHING","domain":"nopacs","retryAfterSeconds":30,"requestId":"8f1c..."}}
```

`message` is the line `nopacs` writes without `--json`; `code` is the API code the server
answered; `metadata` holds the case's facts (`{"hold":"..."}` for `HELD`) and is left out
when there are none; `retryAfterSeconds` (rounded up) and `requestId` are left out when
there is no delay or no answer. A failure before any answer (a usage mistake, a server that
cannot be reached) has the general reason of its kind: `INVALID_ARGUMENT` for a usage
mistake (exit 2), `UNAVAILABLE` for a server that does not answer.

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

The Python client of this repository does this for you: the one `nopacs.Error` it raises
has `reason`, `metadata` (a dict) and `retry_delay` (a `datetime.timedelta`, or `None`) beside
`code`, `message` and `request_id`, decoded without `googleapis-common-protos`:

```python
try:
    client.corrections.Delete(request)
except nopacs.Error as err:
    if err.reason == "HELD":
        print("held by", err.metadata["hold"])
    elif err.retry_delay is not None:
        time.sleep(err.retry_delay.total_seconds())
```

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

In TypeScript, the client of this repository throws one `NopacsError` with `reason`,
`reasonMetadata` and `retryDelayMs` (milliseconds, or `undefined`), over Connect, gRPC and
gRPC-web alike; `errorDetailsOf(err)` reads the same from any `ConnectError`. Without it,
`ConnectError.findDetails` of `@connectrpc/connect` decodes details given the message's
schema, from a package that has the `google.rpc` error details.

```ts
try {
  await nopacs.corrections.delete(request);
} catch (err) {
  if (err instanceof NopacsError && err.reason === "HELD") console.log("held by", err.reasonMetadata.hold);
  else throw err;
}
```

Reasons are fixed words, and a reason added later is one your client does not know. Treat
an unknown reason as its code alone, and never fail on one.
