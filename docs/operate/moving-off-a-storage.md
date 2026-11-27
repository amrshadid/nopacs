# Moving off a storage

[Documentation index](../README.md) · [Replica storages](replicas.md) · [Running NoPACs](running.md) · [Scrub coverage](scrub-coverage.md) · [Backup, restore and moving a tenant](backup.md)

To move a tenant's data from one storage to another (a directory to a bucket, an old disk to
a new one, one account to another) you make the new storage a **replica** of the old, prove
it holds the same data, and **switch**: the replica becomes the primary under the primary's
name. No catalog row changes, nothing is deleted, and a sender can keep sending the whole
time but for a short pause of writes. This is not the move of a tenant to another
installation, which is [on the backup page](backup.md). The rules are in the comments of
[`SwitchStorage`](../../api/proto/nopacs/v1/replica.proto) and in
[storageswitch](../../internal/storageswitch/doc.go).

There are five steps. Each is a command a person runs and reads, and the switch itself
refuses until the checks of steps 2 and 4 have been made.

| Step | Command | You are told |
|---|---|---|
| 1. Copy | `replica attach`, then `replica list` | every scan in step on the new storage |
| 2. Scrub the replica | `job scrub --storage NEW` | every object read there, no finding |
| 3. Check without NoPACs | a script reading straight from the store | sizes, SHA-256 and DICOM parse equal |
| 4. Rebuild compare from it alone | `job rebuild --mode compare --from-storage NEW` | no difference from the catalog |
| 5. Switch | `switch --validate`, then `switch --confirm` | the new place is the primary |

The examples move tenant `lab1` from `main` to `newb`.

## 1. Attach and backfill

```
nopacs storage add lab1 newb --type s3 --endpoint https://s3.example.org --region eu-central-1 \
  --bucket nopacs-lab1 --prefix nopacs/lab1/ --access-key-id AKIA... --credential-file /etc/nopacs/newb.secret
nopacs storage test lab1 newb
nopacs replica attach lab1 newb --primary main --wait
nopacs replica list lab1
```

The attach copies every current object, every superseded revision not yet purged, and every
correction and hold record, so the new storage can be read alone by a rebuild
([replicas](replicas.md#attach-a-replica) says what is copied and how it is verified). Wait
until `replica list` shows `WAITING` and `DAMAGED` at 0 and `OLDEST WAIT` empty. New scans
keep arriving on `main` and are copied as they come, so run `replica list` again just before
the switch.

A replica on the same machine's disk or the same cloud account protects little; for a move
this does not matter, but see [what it costs](replicas.md#what-it-costs).

## 2. Scrub the replica

```
nopacs job scrub lab1 --storage newb --wait
nopacs job findings lab1 --storage newb
```

A scrub reads every object of the replica and compares its length and SHA-256 with the
catalog. It reports and marks, and changes no stored byte. It must end with no finding that
holds. The switch is refused unless the replica has had a clean scrub since its backfill
ended. A scrub reads every byte, so for a large tenant it takes as long as copying did:
[scrub coverage](scrub-coverage.md) has the arithmetic.

## 3. Check without NoPACs

NoPACs checking its own copy is not proof enough for a move, so read a sample with tools
that are not NoPACs: hash each object, parse it as DICOM, and compare with the size and
SHA-256 the catalog holds. `nopacs replica verify` draws the sample
(`ReplicaService.VerifySample`): a random sample of the instances the storage holds (for a
replica, those of its scans in step), each with its key relative to the storage's directory
or prefix, its size and its SHA-256, as a tab-separated table. The server reads no object
for it. `--seed` makes the sample repeatable: the same seed over the same storage draws the
same objects again.

```
nopacs replica verify lab1 newb --count 200 --seed 7 > sample.tsv
```

The table's keys hold UIDs as received: keep the file with the care the archive itself
needs, and delete it after. The script below reads that file (lines starting with `#` and
the header line are skipped; the first three columns are `key`, `size`, `sha256`) and needs
pydicom (and boto3 for a bucket). It reads straight from the directory or the bucket, with
no NoPACs code:

```python
#!/usr/bin/env python3
"""Read sampled objects straight from a storage, without NoPACs.

usage: check_sample.py sample.tsv --dir /srv/nopacs/lab1-new
       check_sample.py sample.tsv --bucket nopacs-lab1 --prefix nopacs/lab1/ --endpoint URL

sample.tsv is what "nopacs replica verify" writes: one object per line, its
first columns key, size, sha256, tab separated; lines starting with "#" and the
header line are skipped.
"""
import argparse, csv, hashlib, io, os, sys
import pydicom

def reader(args):
    if args.dir:
        def read(key):
            with open(os.path.join(args.dir, key), "rb") as f:
                return f.read()
        return read
    import boto3
    s3 = boto3.client("s3", endpoint_url=args.endpoint)
    def read(key):
        return s3.get_object(Bucket=args.bucket, Key=args.prefix + key)["Body"].read()
    return read

def main():
    p = argparse.ArgumentParser()
    p.add_argument("sample")
    p.add_argument("--dir")
    p.add_argument("--bucket")
    p.add_argument("--prefix", default="")
    p.add_argument("--endpoint")
    args = p.parse_args()
    if bool(args.dir) == bool(args.bucket):
        sys.exit("give --dir, or --bucket")
    read, bad, n = reader(args), 0, 0
    with open(args.sample, newline="") as f:
        for row in csv.reader(f, delimiter="\t"):
            if not row or row[0].startswith("#") or row[0] == "key":
                continue
            key, size, sha = row[:3]
            n += 1
            data = read(key)
            why = None
            if len(data) != int(size):
                why = "size %d, expected %s" % (len(data), size)
            elif hashlib.sha256(data).hexdigest() != sha:
                why = "sha256 differs"
            else:
                try:
                    pydicom.dcmread(io.BytesIO(data)).SOPInstanceUID
                except Exception as e:
                    why = "pydicom cannot read it: %s" % e
            if why:
                bad += 1
                print("BAD", key, why)
    print("%d objects read, %d bad" % (n, bad))
    sys.exit(1 if bad else 0)

main()
```

Run it with `python3 -I check_sample.py sample.tsv --dir /srv/nopacs/lab1-new`; it exits 1
and names each object that differs. A key is a SOP Instance UID as received, so keep the
sample file private and do not paste it into a ticket.

## 4. Rebuild compare from the replica alone

```
nopacs job rebuild lab1 --mode compare --from-storage newb --wait
```

This builds a shadow catalog from the new storage alone, as if the tenant had no other
storage, tallies its differences from the catalog in use, and drops it. It changes nothing.
The tally must be empty; any difference means the new storage lacks something the
catalog names, and the move stops here. On Postgres add `--scratch-schema NAME`, an empty
schema to build in ([command line](../use/command-line.md#scrub-and-rebuild)). The switch is
refused unless the replica has had a compare from it alone that found no difference since
its backfill.

## 5. Switch

First ask what would happen, changing nothing:

```
nopacs switch lab1 main newb --validate
```

It lists `blockers` in words, such as `replica not in step: 12 scans`, `records owed to the
replica`, `no clean scrub of the replica since its backfill`, `no empty compare from the
replica` or `correction prepared`, the receivers that would pause, and a **confirm token**.
The command exits 5 when the switch is not ready. When it is:

```
nopacs switch lab1 main newb --confirm sw1_4Zq8fN2k --wait
```

The token digests both storages' places and the primary's switch generation and is good for
one hour and once; the switch is refused if either place changed. A used token answers the
switch it started, so a command that lost its answer can be run again. Only an admin on the
tenant may switch.

`--drain-timeout` (1 second to 1 hour, ten minutes by default) is how long the copy may take
to drain what is still pending before the switch is refused.

`--skip-verification --reason "..."` passes over the two checks of steps 2 and 4 and nothing
else. It is for the case where the check ran somewhere the server cannot see, such as a
scrub of the new volume made by the site's own tool. The reason goes in the audit entry.

### What pauses

From the moment the switch is accepted every write to the primary pauses; reads go on.

- Receivers, and routes that name `main`, refuse new data in a way a sender tries again: a
  DICOM association is refused with a transient failure, and STOW-RS answers 503 with
  `Retry-After`. An association already open has its next store refused the same way.
- A released quarantined file, an import's hand-off, a purge, a repair, a relabel's copy, a
  new correction, a scrub of the primary, and the hold records of a new or released hold wait,
  or are answered `UNAVAILABLE` with the reason `STORAGE_SWITCHING` and a retry delay of 30
  seconds ([errors](../api/errors.md)).
- A read begun before the swap finishes on the old place, whole.

Then the copy drains every scan still pending and every record owed to the replica; the
server takes the owner record of the new place; and one transaction swaps the two storages'
places and raises both storages' switch generation. The name `main`, which the catalog,
receivers and routes use, now names the new place; `newb` now names the old place, as a
replica of `main`. Open scrub findings move with their place. The command prints the pause
and the number of associations refused, each of which its sender retries.

The outcome is in `nopacs storage status lab1 main` (`Switching`, `Last switch`, `Switch
generation`) and in the `STORAGE_SWITCHED` event, made when the switch is done or refused.

### Refusals

A refused switch resumes every write on the old place, and nothing changed. The reasons:

| Refusal | What to do |
|---|---|
| the confirm token does not match, or is over an hour old | run `--validate` again |
| the replica is not a replica of that primary, or is of the archive tier | a cold replica cannot become a primary |
| no clean scrub, or no empty compare, of the replica | do steps 2 and 4, or skip with a reason |
| a scan or record still not on the replica when the drain timed out | wait for `replica list` to settle, or lengthen `--drain-timeout` |
| a correction of the primary is prepared, or a purge of either storage is unfinished | let it finish; a purge and a switch exclude each other |
| a rebuild of the tenant, or another switch, is running | wait |
| a scrub finding that holds on the replica | repair or look into it |
| the tenant moved or fenced (`TENANT_MOVED`, `TENANT_FENCED`) | the tenant is read only here |

### What a restart does

A switch is recorded in the tenant's database when it is accepted, with each step it
reaches. A call cut short, by its deadline or the client, stops nothing: `--wait` then reads
the storage's status until the switch is over and exits 0 only if the switch generation
rose. A server that restarts **before the swap committed** refuses the switch at start
(`server restarted`): writes resume on the old place and the event says it was refused. A
server that restarts **after** the swap committed keeps the swap and finishes what follows
it.

## After the switch

The old storage is now the replica `newb`. It keeps its files and keeps being copied to, so
you have a second copy, and NoPACs deletes nothing on it. When you are satisfied, make it an
ordinary storage again and then remove its configuration:

```
nopacs replica detach lab1 newb
nopacs storage remove lab1 newb --yes
```

Detach deletes no file, and removing the configuration deletes none either: the files stay
where they are for you to remove with the tools of that place. Detach is refused while a
committed or purging correction counts the replica among its copies, since the bytes that
correction withdraws would stay there unseen. `--abandon-purges --reason "..."` goes ahead
anyway; each such correction then records the replica's objects as unknown and names the
replica among its abandoned copies, so it never reads as if that copy had been removed.

A storage with replicas, or a replica, cannot change its place by `storage set`; the move is
the way.

## What is weak

The pause and the backfill time are not measured at scale. A switch with a very large
tenant drains for as long as the copy lags; start it when `OLDEST WAIT` is short. The sample
of step 3 cannot yet be drawn by a command here (see above).
