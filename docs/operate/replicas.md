# Replica storages

[Documentation index](../README.md) · [Running NoPACs](running.md) · [Settings](settings.md) · [Moving off a storage](moving-off-a-storage.md) · [Scrub coverage](scrub-coverage.md) · [Correcting archived data](../use/correcting.md)

A **replica** is a second storage of a tenant that holds byte-identical copies of what
another storage, its **primary**, holds, under the same keys. A primary is the storage a
receiver or a route names. A replica is never named by a receiver, a route or a sender:
nothing is archived to it first, the server copies to it. This page is how to attach one,
how to tell it is keeping up, what happens when a copy is damaged, and what it costs. The
rules are in the comments of
[`replica.proto`](../../api/proto/nopacs/v1/replica.proto) and in
[replicate](../../internal/replicate/doc.go).

A replica protects only as far as it is apart from its primary. A replica on the same disk,
or in the same bucket or cloud account, is lost by whatever loses the primary: a failed
disk, a deleted bucket, a stolen credential. The server refuses a replica whose place
overlaps its primary's, and shows `shares bucket` for one in the same bucket under another
prefix, but it cannot know that two directories lie on one disk. Put a replica on another
machine, volume or account.

| What | Role on the tenant |
|---|---|
| list replicas, read a storage's status | reader |
| backfill a replica | operator |
| attach, detach, repair | admin |

## Attach a replica

A tenant's primary gets up to two replicas. The storage that becomes a replica must exist,
be empty (no archived file and no object under the tenant's keys), not be named by a
receiver or a route, and not be a replica or a primary with replicas already. Make it as
an ordinary storage first, then attach it:

```
nopacs storage add lab1 copy2 --type s3 --endpoint https://s3.example.org --region eu-central-1 \
  --bucket nopacs-lab1-copy --access-key-id AKIA... --credential-file /etc/nopacs/copy2.secret
nopacs storage test lab1 copy2
nopacs replica attach lab1 copy2 --primary main --wait
```

`storage add ... --replica-of main` does both in one step. `--wait` follows the copy job
until it ends, as `job watch` does. A storage that is a replica already is answered as it
stands, with its copy job if one runs, so repeating the command starts nothing more.
Attaching is refused while a purge on the primary runs or a switch of the primary is in
progress.

What the attach copies is **backfill**: every object the primary already holds, every
superseded revision not yet purged, every correction record and every hold record. Scans
archived from then on are copied as they arrive. To copy again what a replica lacks (it was
restored from elsewhere, or you want to be sure before a switch):

```
nopacs replica backfill lab1 copy2 --wait
```

`replica backfill` and the attach queue a `replicate` job; a job of the replica that is
queued or running is answered instead of a second one. At this tree, the server's method
behind `replica backfill` is in the contract but not served yet, so the command is refused as
unimplemented; the attach itself queues the same job.

## What "in step" means

The copy is a job that runs behind the reception, so a sender is never made to wait for a
replica. The same transaction that makes a scan visible records the scan as pending for each
replica, so a crash between the commit and the copy forgets nothing. The server then copies
each object the scan's manifest names that the replica lacks: it reads it through the
checked read (length and SHA-256 against the catalog), puts it on the replica, and verifies
it there (read back, or the store's own SHA-256 confirmation at the put where the store
gives one). The scan's manifest and metadata are put **last**, so a replica's scan is whole
once its manifest is there.

A replica is **in step** for a scan when it holds, verified, every object the scan's
current manifest names, and that manifest. Until a scan is in step its only copy is the
primary's. The copy holds an instance that was deleted but not yet purged too, since a
delete can be undone until its purge ([correcting](../use/correcting.md)); it never copies
what a purge has begun to remove.

```
nopacs replica list lab1
nopacs storage status lab1 copy2
```

`replica list` shows, for each replica, the scans in step, waiting and damaged, and how long
the oldest waiting scan has waited (REPLICA, PRIMARY, TIER, IN STEP, WAITING, DAMAGED, OLDEST
WAIT). `storage status` on the replica adds `Behind its primary`, such as `1204 scans in
step, 3 waiting, 1 being copied, behind by 12m`. A replica that does not answer leaves its
scans waiting; it never refuses a reception. The event `REPLICA_BEHIND` is written when a
replica's lag passes its alert threshold and again when it falls back
([events](../reference/events.md)).

Two settings shape this. `replica.lag-alert` (an hour by default) is the lag past which
`REPLICA_BEHIND` is written, looked at every minute. `replica.rate` is the most bytes a
second the replicator puts on one replica, shared by every copy to it; the default `0` is no
bound, so the copy runs as fast as the replica and the primary allow. Set either on a tenant:

```
nopacs setting set replica.lag-alert 6h --tenant lab1
nopacs setting set replica.rate 50MiB --tenant lab1
```

The lag is also in the metrics `nopacs_replica_lag_seconds` (the age of the oldest scan not
in step) and `nopacs_replica_scans_behind` ([metrics](../reference/metrics.md)).

## Reads fall over to a replica

The one place that opens an archived instance for every read (a download, DICOMweb, frames,
sending on, a de-identification job) tries the primary first. It turns to a replica **in
step for that scan** when the primary's object is marked damaged, is missing, is not
online, or its storage does not answer. It turns before the first byte, never midway: a
reader never gets one copy's bytes joined to another's. The replica's bytes are checked
against the catalog's SHA-256 exactly as the primary's are, so a reader sees no difference;
the audit entry and a metric say which storage served.

A replica that is not in step for a scan is never read for it: the reader gets the
primary's error. If damage is found midway through an answer, that answer still aborts, as
it does without a replica, and a check of the instance is queued at once; once the check
confirms the damage, later reads go to the replica.

## A replica's damage, and repair

[A scrub](scrub-coverage.md) reads a replica like a primary (`nopacs job scrub lab1
--storage copy2`). What it finds there marks the replica's scan damaged; it never marks the
instance damaged, since reads still have the primary. The server copies a replica's damaged
or missing object again from the primary, verified, without being asked: a replica is the
server's own copy.

A **primary's** damaged or missing object is mended from a replica, by a person or by the
server:

```
nopacs job findings lab1 --storage main
nopacs repair lab1 main --finding <id> --wait
nopacs repair lab1 main --all --from copy2 --wait
```

`nopacs repair` (admin) queues a repair job. For each finding it reads the object whole from
a replica in step for its scan (any replica, or the one `--from` names) and checks it
against the catalog's length and SHA-256; records the damaged object's size and checksum on
the finding first; puts exactly the bytes the catalog names at the same key; reads them
back; and clears the finding. An object no replica holds good is left as it is and counted
refused with the detail `no good copy`; a replace from a file sent again
([correcting](../use/correcting.md)) is then the way to mend it. A repair with the same
storage, replica and findings that is already queued or running is answered instead.

A repair and a change of the same scan exclude each other: while a repair of a scan's
object stands, a correction of that scan and a reception's commit to it are refused as
retryable, and the repair checks again before it finishes that the catalog still names
those bytes. A repair is short, so a sender's retry finds the way clear.

### Repair by itself: `replica.repair = auto`

`replica.repair` is `manual` by default, because damage is a person's to see before the
server writes to the storage again, and reads already fall over so waiting loses nothing.
With `auto` the server makes a repair job, within five minutes of a scrub's finding, for each
primary that has a replica:

```
nopacs setting set replica.repair auto --tenant lab1
```

### The repair lease, and a very large object

A repair holds its object under a **lease**, a claim that lasts `replica.repair-lease` (a
minute by default, `30s` to `1h`). Each put of a repair is bounded by the lease less 15
seconds, and every put's deadline is before the time the lease was last renewed to, so a
worker that was cut off cannot land bytes after a delete has gone ahead. A lease too short
for the largest object is therefore a repair that can never finish: a put cut short is tried
again and cut short again.

The server checks this before it runs any repair. It takes the larger of the largest
instance the catalog holds and the largest file any receiver of the tenant accepts, assumes
the storage puts 100 MiB a second (no real speed is measured yet), and refuses a lease that
leaves less time than that. The refusal is in the server's log with the lease that would do,
and no repair runs until you lengthen it. For a 20 GiB multi-frame object the put needs 205
seconds, so the lease must be at least 220 seconds:

```
nopacs setting set replica.repair-lease 4m --tenant lab1
```

A slow link to the primary needs a lease longer than the floor even where the check passes;
raise it if repairs of large objects keep being retried.

## Cold (archive-tier) replicas and recall

A replica on S3 may be marked **archive tier**: the site's own bucket rule moves the `data/`
objects to a cold storage class and leaves `meta/` online. NoPACs does not set a bucket's
storage class or lifecycle; it only knows the replica is cold.

```
nopacs replica attach lab1 cold --primary main --tier archive --recall-days 7 --recall-tier bulk
```

A primary is never archive tier. `--recall-days` (1 to 365) is how long a recalled object
stays readable and `--recall-tier` (`standard`, `bulk`, `expedited`) is how fast the store
brings it back; `expedited` is refused by a store that does not offer it for the object's
class, and the recall then fails.

"Not online" is a state, never missing or damaged: a read of such an object is answered
`UNAVAILABLE` with the reason `NOT_ONLINE` (also when a primary's object is missing and the
only copy in step is cold on a replica), and a scrub of an archive-tier replica is a
**presence scrub** (listing, sizes, and the store's own checksum where it kept one), not a
read of every byte ([scrub coverage](scrub-coverage.md)). A reader never starts a recall:
a reader's key would be a lever on the site's bill. A job's unit that meets a cold object (a
send, a de-identification job, a repair) asks for the recall and waits, in a state shown as
`recalling` that spends none of its attempts. An operator may recall on purpose, after a
preview and its confirm token (days above the storage's own need an admin), and there is one
recall per object however many requests wait for it.

At this tree the recall worker runs, but `ReplicaService.Recall` is in the contract and not
served yet, so there is no `nopacs recall` command: recalls start only from a job meeting a
cold object. A recall that is not online within `recall.wait` (48 hours by default, from
`1h` to `30d`, per tenant if you wish) is recorded as failed, and the server asks the store
nothing more for it. A recall is proven against a stub of an S3 store only; try it on your
own store before you rely on it.

## What it costs

- **Storage and requests.** A replica holds a full second copy: twice the bytes (a cold tier
  is cheaper to hold and dear to read), and every object put and read back is a request your
  store may charge for.
- **Reading.** A scrub reads **every** copy it covers, a replica's as much as a primary's,
  so each storage a tenant has costs its full size in reads (and, from a cloud store,
  egress) once every `scrub.interval` (30 days by default). [Scrub coverage](scrub-coverage.md)
  shows whether the scrub keeps up.
- **Asynchronous copy.** Until a scan is in step its only copy is the primary's. A primary
  lost inside that window loses the scans not yet copied. Watch `OLDEST WAIT`.
- **Same disk, same account.** See the first section: it protects little.
- **Not a backup of the catalog.** A replica holds the archive's files and records, and a
  catalog can be rebuilt from it, but replication of the tenant's database is the job of
  [the backup](backup.md).
