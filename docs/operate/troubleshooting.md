# Troubleshooting

[Documentation index](../README.md) · [Running NoPACs](running.md) · [Error kinds and codes](../reference/errors.md) · [Settings](settings.md)

The message a command prints is the first thing to read: it says what is wrong in
words, with the request id. The request id is also in the server's log and in the
audit entry of the request, so a person on the server can find the same call. The
exit status says the kind of failure (the table is in
[error kinds and codes](../reference/errors.md)).

## The command cannot reach the server

**`cannot reach the server at .../run/nopacs.sock: connect: invalid argument`.** The
path of the data directory is too long. A local socket's path is limited to about a
hundred characters, and the socket is `<data-dir>/run/nopacs.sock`. Use a shorter
data directory.

**`connect: no such file or directory` or `connection refused` on the socket.** The
server is not running, or runs on another data directory. Check `--data-dir` or
`NOPACS_DATA_DIR`.

**`cannot reach the server at https://...: connection refused`.** The server was
started without `--listen`, or the address or port is wrong.

**A certificate is refused.** The server's certificate must be one the client trusts.
For a private certificate authority give `--ca-file` (or `NOPACS_CA_FILE`) to every
command. A key that works only with a TLS client certificate also needs
`--tls-client-cert` and `--tls-client-key`.

**`the server address http://... is refused: http is allowed only for a loopback host`.** Use
`https://`.

**`the key file ... may be opened by other users (mode 0644)`.** Make it private:
`chmod 600`. A key file other users can read is not accepted.

## A write is refused and retried, or a read says NOT_ONLINE

`STORAGE_SWITCHING`, `NOT_ONLINE`, `HELD` and `RETENTION_MINIMUM` are reasons, each with its own
cause and whether to retry: [reasons and retry delays](../api/errors.md). A switch pauses writes
until its drain ends ([moving off a storage](moving-off-a-storage.md#what-pauses)); a cold replica's
object needs a recall ([replicas](replicas.md#cold-archive-tier-replicas-and-recall)); a held or kept
delete needs a person ([retention rules and holds](../use/retention.md)). A repair that never runs
may be refused for a lease that is too short ([replicas](replicas.md#the-repair-lease-and-a-very-large-object)).

## Refused (exit 3)

All of these are one answer on purpose, so a caller cannot tell which was wrong: no key,
a wrong secret, a key that has expired, been revoked or been suspended by a restore, a
key used from a network it is not limited to, and a key with no grant for what it asks.

- `nopacs whoami --server ...` says who the server takes you to be and what you are
  granted. If it works and another command is refused, the key lacks the role
  ([the security model](../concepts/security.md)).
- A key a restore suspended works again only after the local operator runs
  `nopacs key reinstate` ([backup](backup.md#keys-come-back-suspended)).
- After repeated failures an address or a key is slowed, then blocked for
  `access.block-time`: the audit log (`nopacs audit list --outcome refused`) says why.
- On the server's own socket there is no key and no network rule.

`nopacs: ...: no tenant has this name` is answered to a caller who may list every
tenant; a caller without that right gets the answer for "no grant", which is the same
whether the tenant exists or not.

## The server will not start

| The message says | What it means |
|---|---|
| `no installation in ...; make one with: nopacs-server init` | The data directory is not an installation. Check the path, or run `init`. |
| `another nopacs-server serve is running on the data directory ... (process N)` | One server per data directory. Stop the first, or use another directory. |
| `the key file is not the one this installation was made with; nothing was changed` | Put the installation's own key file back in the data directory. |
| `the master database is not the one this installation was made with` | `--master-env` points at an empty database or another installation's. Nothing was changed. |
| `the database was made by an earlier development build ... make it again` | See [upgrading](upgrading.md). |
| A message about an interrupted restore | See [backup](backup.md#restore): empty the data directory except the key file, and run the restore again. |
| A message about an interrupted `init` | They say which files to remove before running `init` again. |

## A receiver does not receive

Start with `nopacs tenant status <tenant>` (receiver health, callers, storage space)
and `nopacs receiver show <tenant> <name>`.

- **Health is down with a port reason.** The port is used by another program, or not
  allotted: `the port 11112 is not among the ports the installation allotted to this
  tenant` is answered when the receiver is made. Set `receiver.ports` on the tenant.
- **The sender is rejected.** A receiver admits only the callers it lists, by
  Calling AE title, by address range, or both. The tenant's events say why
  (`nopacs watch <tenant> --type association-failed`): caller not allowed, called AE
  title not recognized, no acceptable presentation context, nearly full, or the
  incoming limit.
- **Rejected as nearly full.** The incoming area or the storage has less free space than
  `storage.reserve`, or the tenant holds as many incoming bytes as `incoming.limit`
  allows. Free space, or raise the setting.
- **The file is accepted and never arrives in the catalog.** A reception is complete
  only after its quiet period without a file (default a minute). `nopacs reception
  list <tenant>` shows its step. A file the pipeline cannot take is in
  **quarantine** with its reason: `nopacs quarantine list <tenant>`, then `show
  --header`, `release` or `reject`.
- **A reception failed.** `nopacs reception show` names the step; once the cause is
  mended, `nopacs reception retry`.

## Reads fail

- **`DATA_LOSS` (exit 1).** A stored file is missing or its bytes do not match the
  checksum recorded when it was archived. The message names the scan. Mend it from
  another copy of the storage; NoPACs never repairs it itself.
- **`RESOURCE_EXHAUSTED` (exit 7).** Too many reads are open (`read.per-caller`,
  `read.per-server`); try later, or raise the setting.
- **`NOT_FOUND` for a label that used to work.** A relabel moved it; the error's
  detail names the label it has now ([correcting](../use/correcting.md)).

## When none of this explains it

Read the server's log (JSON lines on standard error; `--log-format text` for a
person). Every request has a line with its request id, the method and the code.
`/metrics` on the local socket shows the counts of requests by code
([metrics](../reference/metrics.md)). Do not paste a key, a database address or
anything from a patient's file into a report: none of them is needed to find the
request.
