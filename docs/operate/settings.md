# Settings

[Documentation index](../README.md) · [Running NoPACs](running.md) · [The command line](../use/command-line.md) · [Every setting](../reference/settings.md)

The installation's settings, and the tenants' overrides of them. The list of every
setting, with its default, its range, who may set it and where, is generated from
the server's own catalog: [the settings reference](../reference/settings.md). This
page says what the table has no room for: how to read and change them, what the
important ones decide, and when a change takes effect.

## Reading and changing

```sh
nopacs setting list                              # the installation's settings
nopacs setting list --tenant lab1                # what applies to a tenant, with where each value comes from
nopacs setting get receiver.ports --tenant lab1
nopacs setting set delete.grace 14d
nopacs setting unset delete.grace
```

The same calls are `SettingsService` (`GetSetting`, `SetSetting`, `UnsetSetting`,
`ListSettings`). A value is checked by the setting's own rule when it is set, and a
value that would take away something in use (a destination a sender uses, a port a
receiver holds) is refused. A tenant's own value, where the setting allows one,
replaces the installation's for that tenant; unsetting it goes back to the
installation's. A tenant's admin may set only the settings the reference marks so;
the others, and every value that decides what the installation's disk, network or
ports are used for, are the installation admin's.

## Where data and receivers may go

Where a tenant's data may go is the installation's decision, not the tenant's.
Four settings make it, and a tenant can have neither storage nor receiver until
they are set:

- `storage.directory-roots`: the directories (`name=path`, one per line) that
  directory storages may lie under. No two roots may lead to one directory once their
  symbolic links are followed. A caller who is not an admin on the installation reads
  the names only.
- `storage.s3-endpoints`: the S3 endpoints storages may use. An endpoint at a
  `private-address` needs the storage to use path-style requests.
- `receiver.ports`: the ports a tenant's DICOM receivers may listen on, set on each tenant
  by the installation's admin only.
- `receiver.bind-addresses`: the addresses receivers may listen on.

`storage.reserve` (the free space kept on every disk or store) and `incoming.limit`
(how many bytes a tenant may hold in the incoming area) stop a receiver from filling
a disk: a receiver turns callers away with a transient rejection (PS3.8) when either is
reached, and the sender may try again later.

### Where a sender may send

`sender.destinations` is a list, one destination per line: a host name (letters,
digits, hyphens and dots; a name whose last label is a number or `0x...` is refused,
since a resolver reads it as an address), a range of addresses (CIDR, never `/0`) or
one address, then optionally `ports=104,11112-11120` (every port when left out) and,
for a host name, `private-address` and `link-local`.

Without either flag a host name must resolve to a global unicast address. With
`private-address` it may also resolve to loopback (`127.0.0.0/8`, `::1`), private use
(RFC 1918, `fc00::/7`) or shared address space (`100.64.0.0/10`); with `link-local`,
to a link-local address (`169.254.0.0/16`, `fe80::/10`); and to nothing else
special-purpose. Link-local has its own flag because cloud hosts answer their
instance metadata, credentials included, there. The unspecified (`0.0.0.0`, `::`),
multicast and broadcast addresses, and the cloud metadata addresses
`169.254.169.254` and `fd00:ec2::254`, are never a destination, whatever a line says.
A value that would take away a destination a sender uses is refused, as is unsetting
it.

## Reads, de-identification and memory

- `read.per-caller` and `read.per-server` bound the reads of archived bytes (a
  download, a `ReadFiles` stream, a DICOMweb retrieve or search that reads a storage). A
  viewer keeps about three open, so the default leaves room for several windows; raise
  it for a viewer that opens more. Each read holds a storage connection or file and two
  buffers of 1 MiB, so memory is about 2 MiB a read. A read past the bound is refused,
  never queued (`RESOURCE_EXHAUSTED`; HTTP 429 with `Retry-After`).
- `deid.pixel-limit` bounds the decoded image bytes that de-identification works on; a
  larger image is quarantined (`deid_failed`, cause `pixels_too_large`) with its bytes
  kept, and archived by a release once the setting is raised.
- `deid.memory` bounds the memory of all de-identification work at once, across every
  tenant. Work that does not fit waits for room; a file whose check alone needs more
  than the whole is quarantined (`deid_failed`, cause `check_too_large`) with its bytes
  kept, and archived by a release once the setting is raised.
- `deid.list-cache-bytes` bounds the memory the text lists of stored profile versions
  are kept in (256 MiB by default): every reception, job and correction of a version
  shares one copy of their automata, and the least recently used version's are dropped
  past it, built again when next needed.
- `receiver.header-limit` bounds the header a file may have before it is quarantined.
- `receiver.allow-as-received` is `false` by default: every receiver de-identifies. A
  tenant that had receivers before receivers could de-identify was given `true` when
  its database was upgraded.

## Keys and where they may be used from

`access.*` and `key.*` say where a request may come from, how a caller that keeps
failing is slowed and then blocked, and how long a key lives. A request on the local
socket is never refused by a network rule or a limit. `access.forwarded-header` is
one header only, because a proxy that appends to one commonly passes a client's copy
of the other through. A key expiring within `key.expiry-warning` is listed in the
status of every tenant and reported in the server's log once a day. See
[the security model](../concepts/security.md).

## How the audit is sealed and how long it is kept

`audit.seal-interval` is how often the server seals every audit log, and once more
when it stops. An entry not yet sealed can be removed without a trace for at most
that long. `audit.retention` is `0` (keep every entry) by default. When set, whole
sealed checkpoints older than it lose their entries at the next pass; every
checkpoint stays, so the chain is still checked across them, and each pruning writes
its own entry, sealed in the same pass. **Export the log before setting it: what is
pruned is gone.** The age is counted back from the earlier of the clock and the log's
latest checkpoint, so a clock set forward does not prune early.

No pass seals while a backup over the API runs; the first pass after it seals
what was written meanwhile. `backup.max-duration` (installation only, `6h` by
default, from `1m` to `7d`) bounds how long such a backup may run, and with it
how long it holds a database's read transaction, this pause and the server's one
place for a backup or an export ([Backup](backup.md)).

## Corrections, scrubs and imports

`delete.grace` is how long a delete's bytes stay on the storage, unserved and
undoable, before the server purges them; it is fixed for each delete when it is made,
so a later change moves no purge already set ([correcting](../use/correcting.md)).
`scrub.rate` and `scrub.interval` pace the server's own check of stored bytes.
`import.*` say which directories an import may read and how much of the server it may
take ([importing](../use/importing.md)). `job.failures-kept` is how many refused
instances a job lists; the rest are counted, not listed. `events.retention` is how
long a tenant's events are kept.

## Replicas, retention and the quarantine

The pages that use these settings are [replica storages](replicas.md) (with the repair lease worked through),
[retention rules and holds](../use/retention.md) and [scrub coverage](scrub-coverage.md) (for `scrub.rate` and `scrub.interval`).

`replica.repair` is `manual` by default: a primary's object a scrub found damaged
or missing is put back from a replica in step only when a person runs `nopacs
repair`, since reads already turn to the replica. With `auto`, the server makes an
automatic repair job, within five minutes of a scrub's finding, for each primary
that has a replica. `replica.repair-lease` (a minute by default, from `30s` to
`1h`) is how long a repair's claim holds its job, which bounds one put of an
object: 15 seconds less than the lease. A lease too short to put the largest file a
receiver of the tenant accepts at 100 MiB a second is refused, the server's log says
which lease would do, and no repair runs until it is lengthened. No speed of the
storage is measured yet; the check assumes that floor.

`replica.lag-alert` (an hour by default, from `1m` to `30d`) is how long a replica's
oldest scan not in step may wait before the server writes one `REPLICA_BEHIND` event,
and one more when the replica is back under it; the same lag is in the metrics
`nopacs_replica_lag_seconds` and `nopacs_replica_scans_behind`. `replica.rate` is the
most bytes a second the replicator puts on one replica; `0`, the default, is no bound.
`recall.wait` (48 hours by default, from `1h` to `30d`) is how long a recall may stay
requested or in progress; past it the recall is recorded as failed. `scrub.archive-sample`
is the share of the online objects of an archive-tier storage that a scrub reads whole
(`0%` by default: presence only). Each of the four may be set on the installation and
overridden for a tenant, by that tenant's admin too.

`retention.shorten-notice` and `retention.max-clock-step` guard retention against a
mistyped rule and a clock set wrong. `retention.max-clock-step` has one value, the
installation's, since the clock is the server's. `retention.shorten-notice` and
`retention.max-per-pass` may be set for one tenant, by an admin on the installation
only. Once a day the server disposes of what each delete rule makes due,
at most `retention.max-per-pass` sessions a rule, each through an ordinary delete, and
works the disposals a person queued within the hour. `quarantine.warn-age` and
`quarantine.warn-bytes` make a tenant's status warn of files held too long, or too
many bytes held. They warn and nothing more: only a quarantine retention rule
removes a held file.

## Storage quotas

One tenant must not fill a storage that others share. `quota.bytes` and
`quota.instances` bound what a tenant holds on its storages: empty, the default, is
no bound. Only an admin on the installation sets them, for the installation (every
tenant's default) or for one tenant; a tenant's own admin cannot, and neither can a
masked key, whatever its grants.

What a tenant holds is counted in its database as files are recorded and purged,
never by reading the storages: the instance files only (a scan's manifest and
metadata files are the archive's own), each revision of an instance its own file. A
file counts from the moment its reception records it, so files received and not yet
archived count, and until its purge removes it, so a deleted instance, a revision a
replace or an edit superseded, and a relabel's old key count until their purge.
`nopacs tenant show` and `nopacs tenant status` print the use against the bounds,
with what the purges will give back.

The bound is checked as each new file starts, against the database's count (read at
most once a second) plus the files taken since, one tenant's files decided one after
the other, so two files at the edge never both see the same room. A file that cannot
be checked, because the bounds or the count cannot be read, is refused the same way.
A file can briefly count twice just after its batch is written, once in the database
and once among the files taken since, which can only refuse early. A relabel's
copies count only from their publish, not while they are being made. At the
bound a new file is refused as a temporary failure: a C-STORE is answered `A700`
(out of resources), a STOW-RS request whose parts are all refused for it is
answered `507` with each part `A700`, and an import job waits, its place kept, with
the reason `quota reached`. Nothing received is lost: a file taken before the bound
is archived, so a reception in progress finishes what it was sent. An instance bound
is never passed; a bytes bound can be passed by the files being received when it is
reached, at most one for each store then in flight, each at most its receiver's
largest file, since a C-STORE does not say its size first.

`quota.warn` (`90%` by default) is the share of a bound past which the tenant is
warned: every minute the server measures each tenant, writes one `QUOTA` event when
the state changes (under, past the warning, at a bound, or back), and keeps the
metrics `nopacs_quota_used_bytes`, `nopacs_quota_bound_bytes`,
`nopacs_quota_used_instances`, `nopacs_quota_bound_instances` and
`nopacs_quota_state`. A tenant's own admin may set `quota.warn` for the tenant. After
a restart a state other than under that still stands is reported once more.

A tenant at its bound gets room back by a purge (a delete's bytes count until its
grace ends and its purge runs; `nopacs correction purge` runs it sooner) or by a
raised bound.

A period is a whole number followed by `s`, `m`, `h` or `d`; a size is a whole number
of bytes, or a number followed by `KiB`, `MiB`, `GiB`, `TiB` or `PiB`.

## Which changes take effect without a restart

No setting needs a restart: each is read from the database when it is used, or kept
for at most the few seconds the table says, so a value set through `SetSetting`
applies without one. What "next use" means differs by setting, and the table gives
it for the settings whose timing matters. The files and flags the server reads
when it starts do not change until it is started again.

| What | When a change takes effect |
|---|---|
| `storage.directory-roots` | At the next use of a directory storage: adding or changing a storage, and every time a storage is opened |
| `storage.s3-endpoints` | At the next use of an S3 storage, in the same places |
| `receiver.ports` | At the next `AddReceiver` or `SetReceiver` on the tenant; a listener that is running keeps its port, and a value that would take the port from a receiver is refused |
| `receiver.bind-addresses` | When a receiver that names no address of its own is started or started again (the first address listed is used); a listener that is running keeps the address it has |
| `storage.reserve` | Within five seconds, by default: the free space of the incoming area and of each storage a receiver writes to is checked on that interval |
| `incoming.limit` | Within five seconds, by default, on the same check |
| `quota.bytes`, `quota.instances` | Within a second, for the next file received; within a minute for the warning, the event and the metrics |
| `quota.warn` | Within a minute, at the next measure of the tenant's use |
| `receiver.header-limit` | Within a second, for the next file read |
| `deid.pixel-limit` | Within a second, for the next file de-identified; a file held as `pixels_too_large` is archived by its release once the setting is raised |
| `deid.memory` | Within a second, for the next file checked or tried; work that waits for room reads it again each second, and a file held as `check_too_large` is archived by its release once the setting is raised |
| `deid.list-cache-bytes` | Within ten seconds: the server reads it again on that interval, and drops the least recently used lists at once when it is lowered |
| `receiver.allow-as-received` | At the next `AddReceiver`, or `SetReceiver` that changes the receiver's de-identification; a receiver that archives as received keeps doing so when it is turned off |
| `api.cors-origins` | Within ten seconds: the server keeps the value it read for that long |
| `sender.destinations`, `sender.allow-plain-http` | At the next `AddSender` or `SetSender` that changes a destination; a sender that is configured already is checked again when the value is changed |
| `read.per-caller`, `read.per-server` | Within a second, for the next read: a read already open keeps its place, so a lowered value refuses new reads until enough have ended and cuts none |
| `key.default-lifetime` | At the next key made without an expiry; keys already made keep theirs |
| `key.max-lifetime` | At the next key made or rotated; keys already made keep theirs |
| `key.expiry-warning` | At the next status, and within an hour for the report in the log |
| `access.denied-networks`, `access.allowed-networks`, `access.trusted-proxies`, `access.forwarded-header`, `access.failure-limit`, `access.failure-window`, `access.block-time` | Within five seconds: the server keeps the values it read for that long |
| `access.tenant-networks` | Within five seconds, on the same terms, for each tenant |
| `replica.repair-lease` | At the next claim of a repair job; a job running keeps the lease it claimed with |
| `replica.repair` | Within five minutes, at the next look for damage a scrub found |
| `replica.lag-alert` | At the next look at the replicas, at most a minute away |
| `replica.rate` | At the next claim of scans to copy |
| `recall.wait` | At the next pass of the recall worker |
| `scrub.archive-sample` | At the start of the next unit of a scrub |
| `retention.max-clock-step`, `retention.shorten-notice` | At the next check of the tenant's clock, at most an hour away, and at the next change of a rule |
| `retention.max-per-pass` | At the next daily pass |
| the server's `--listen` address | Needs a restart: the address is opened once, when the server starts |
| the server's `--tls-cert` and `--tls-key` | A certificate renewed in the same files: within five seconds, at the next connection; files that change into a pair that does not load are logged and the last good pair is still served. Other paths need a restart |
| the key file | Needs a restart: it is read once, when the server starts, and the keys are held in memory |
| the data directory (`--data-dir` or `NOPACS_DATA_DIR`) | Needs a restart: it is read once, when the command starts |
| the server's `--master-env`, `--log-format`, `--log-level`, `--drain` | Need a restart: they are read once, when the command starts |
| the certificate and private key files of a receiver's TLS | When the receiver's listener is started or started again: a change to the receiver does that, as does a retry after a failure; a listener that is running keeps the pair it loaded |
