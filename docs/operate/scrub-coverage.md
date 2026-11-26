# Scrub coverage

[Documentation index](../README.md) · [Replica storages](replicas.md) · [Settings](settings.md) · [Encryption at rest](encryption-at-rest.md) · [The command line](../use/command-line.md#scrub-and-rebuild)

A **scrub** reads every stored object again and compares its length and SHA-256 with the
catalog and the manifests. It finds a file that was changed, lost or damaged since it was
archived, long before a reader meets it. The server runs one for each of a tenant's storages
by itself, every `scrub.interval` (30 days by default; `0` is never), and a person may run
one at any time (`nopacs job scrub lab1 --storage main --wait`). It never changes a stored
byte. **Coverage** answers the question a scrub raises: does it keep up?

## What the numbers mean

`nopacs storage status lab1` shows, per storage, the share of objects verified (the COVERAGE
column); `nopacs storage status lab1 main` shows it in full as `Scrub coverage`, such as

```
Scrub coverage   97.3% of 4812004 objects read within the interval, 129210 overdue, the oldest unread for 41d
```

| Number | What it is |
|---|---|
| objects | the objects the catalog places on the storage; for a replica, those of its scans in step |
| verified, and the share | of those, the objects read whole within the last `scrub.interval`, and `verified` over `objects` (1 when the storage holds none) |
| overdue | the objects not read within one interval of their last read, or of their archiving when never read |
| oldest unverified | how long the object that has waited longest has gone unread; zero when every object is verified within the interval |
| checked by presence only | on an archive-tier replica, the objects checked by listing, size and the store's own checksum without being read; they are not counted in `verified` |

A share of 100% and no overdue object mean that every object was read whole within the
interval. A falling share, or an `oldest unverified` older than `scrub.interval` plus the time
one scrub takes, means the scrub does not finish inside its interval. Coverage is in the
status of a tenant, and in the server's metrics, which are updated when a scrub ends
([metrics](../reference/metrics.md)): `nopacs_scrub_verified_share`,
`nopacs_scrub_oldest_unverified_seconds` and `nopacs_scrub_overdue_objects`, for each
tenant and storage. Alert on a share that falls or an oldest age past the interval.

An archive-tier replica is not read whole by its scrub, since a read would bring objects out
of the cold tier at the site's cost. It is checked by presence: a listing, each object's
size, and the store's own SHA-256 where the store kept one. That trusts the store's
checksum, which a read of the bytes would not. It is a weaker check, and coverage says how
many objects had only it. `scrub.archive-sample` (`0%` by default) makes a scrub read whole
that share of the objects online on such a storage; the setting is read at the start of each
unit of a scrub, with no restart. A scrub never asks for a recall.

## The arithmetic

A scrub reads at most `scrub.rate` bytes a second from one storage (`64MiB` by default, from
`1MiB` to `64GiB`). The time one full scrub takes is the bytes held divided by that rate,
whatever the storage's size:

```
300 TB = 300 x 10^12 bytes
64 MiB/s = 64 x 1,048,576 = 67,108,864 bytes/s
300 x 10^12 / 67,108,864 = 4,470,348 seconds
4,470,348 s / 86,400 s per day = 51.7 days
```

At the default rate a storage of 300 TB takes about 51.7 days to scrub once, longer than the
default 30-day interval, so its coverage can never reach 100% and its overdue count grows
(with 300 TiB the time is about 56.9 days). The same arithmetic for other sizes:

| Bytes held | At 64 MiB/s | Rate needed for a 30-day interval |
|---|---|---|
| 10 TB | 1.7 days | 3.7 MiB/s |
| 100 TB | 17.2 days | 36.8 MiB/s |
| 300 TB | 51.7 days | 110.4 MiB/s |
| 1 PB | 172.5 days | 367.9 MiB/s |

To keep up, raise the rate, lengthen the interval, or both. Check both against the cost:

```
nopacs setting set scrub.rate 128MiB --tenant lab1
nopacs setting set scrub.interval 60d --tenant lab1
```

- The rate is **per storage**, so a tenant with a primary and two replicas reads three times
  what one storage does, at the same time if their scrubs overlap.
- A scrub reads every byte it covers: from a cloud bucket that is a request per object and
  egress for every byte, at each interval.
- The figure is a ceiling on the speed of the server's reads, not a measurement of any
  storage. The real time depends on the storage, the network and the load; no measured
  number is published yet. Read the oldest-unverified age after a scrub or two rather than
  trusting the division.

## When coverage is low

1. Run `nopacs job scrub lab1 --storage main --wait` and look at `nopacs job findings lab1`:
   damage that is found is reported there whatever the coverage.
2. If the scrub is simply too slow for the interval, change `scrub.rate` or
   `scrub.interval` as above. A setting is read when it is next used; no restart.
3. Scrub the replicas too: a copy that was never read is a copy you have not shown to
   exist ([replicas](replicas.md#a-replicas-damage-and-repair)).
