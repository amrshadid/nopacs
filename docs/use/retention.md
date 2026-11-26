# Retention rules and holds

[Documentation index](../README.md) · [Correcting archived data](correcting.md) · [Settings](../operate/settings.md) · [The security model](../concepts/security.md) · [Replica storages](../operate/replicas.md)

A **retention rule** says how long a tenant's data is kept at least and when it becomes due
to go. A **hold** is a mark, with a reason, that stops any data it covers from going,
whatever the rules say. Both act on the tenant's **sessions** (a scan session of a subject)
and on nothing above them: there is no grouping by label, modality or collection. A platform
that has its own collections places holds and deletes by subject through the API.

A rule does what it says and nothing more. Whether it meets a law, a consent form or a
contract is the institution's to judge; NoPACs makes no legal claim for any rule. The rules
are in the comments of
[`retention.proto`](../../api/proto/nopacs/v1/retention.proto) and in
[tenant](../../internal/tenant/doc.go).

| What | Role on the tenant |
|---|---|
| add, change, remove a rule; dispose; place and release a hold; confirm the clock | admin |
| show a hold with its reason | operator |
| list rules, list what is due | reader |
| lift a rule's keep-at-least for one delete | admin **on the installation** |

## Rules

```
nopacs retention rule add lab1 trial7 --receiver scanner1 --keep-at-least 10y --dispose-after 15y --action review
nopacs retention rule list lab1
nopacs retention rule set lab1 trial7 --dispose-after 20y
nopacs retention rule remove lab1 trial7
```

A rule has a name and:

- a **selector**: no flag for every session of the tenant; `--receiver R` for sessions
  archived through that receiver, with `--route RT` for only one route of it; or `--storage
  S` for sessions first archived to that primary storage. A receiver, route or storage that
  is removed later keeps matching what was archived through it. A replica is never a
  selector, since nothing is archived to it first;
- **`--keep-at-least`**, how long a matched session is kept at least, `7y` or `90d`, or
  `none`;
- **`--dispose-after`**, how long after its start a matched session is due, never shorter
  than keep-at-least, or `none`. At least one of the two periods is needed; a period is
  between 1 day and 100 years;
- an **action**: `review` (the default: due sessions are listed for a person) or `delete`
  (the daily pass deletes them).

A session's scans may have come through different receivers or routes, so the gentler
reading wins: a rule's keep-at-least matches a session when **any** of its scans came
through the selector, its dispose-after only when **all** of them did.

### When a session is due

A period counts from the session's **start time**: when it was first archived, by the
server's clock, kept as the earliest arrival of its scans. **No date inside a DICOM file ever
sets it**, shifted or not, so a study dated 1990 or 2030, or whose dates de-identification
moved, is due on the same day as one dated today. A scan that arrives later joins its
session's start time. An import job may carry a start time an admin gives (never before
1970), audited; such a session is never due before it was archived plus the notice below.

Of the rules that match a session, the latest keep-at-least and the earliest dispose-after
apply: the session is due when its start plus that dispose-after has passed and no matching
rule still keeps it. A session no rule's dispose-after matches is never due. Due times are
stored when a session is archived, when a scan joins it and when a rule changes, so a
listing computes nothing.

### Changing a rule, and the shortening notice

Every change applies at once, but a change that would make data due **sooner**, or keep it
for **less**, is clamped: nothing is due, and nothing is let go of, before the change's time
plus `retention.shorten-notice` (30 days by default, from `1d` to `365d`; an installation
admin sets a tenant's own). A mistyped rule cannot make a year's data due tonight, and a
change that keeps data longer takes effect now. A rule that is removed keeps what it kept
until the notice ends; adding it again keeps it at once.

## What is due

```
nopacs retention due lab1
nopacs retention due lab1 --rule trial7 --held
```

This lists the sessions due now, oldest first. Held sessions are left out unless `--held`,
and then marked. It changes nothing.

## The trusted clock

Retention depends on the server's clock, so a wrong clock must not delete data. NoPACs asks
no clock outside the server, since asking would send data out. It trusts the clock **forward
in small steps**: once a day, and before any purge started by its time, and at least every
hour, it compares the clock with the last time it trusted. If the clock moved forward by
more than `retention.max-clock-step` (2 days by default, from `1h` to `30d`; one value for
the installation, since the clock is the server's) the tenant's retention **pauses** with the reason "clock jumped" and a
`RETENTION_CLOCK` event is written ([events](../reference/events.md)). While paused nothing
becomes due, the daily disposal does not run, and no purge starts because its time came
(only a purge a person asks for goes on). A clock moved **back** never pauses it: the trusted
time stays the latest one, so going back only delays what is due. That the clock slowly
drifts is not caught, only jumps are.

You see the state, and end a pause after reading the time from a clock you trust:

```
nopacs retention clock lab1
nopacs retention clock lab1 --confirm 2026-11-26T14:03:00+01:00 --reason "ntp was off after the power cut; read from the wall clock"
```

Type the time yourself, with its offset; do not copy it from the server's answer, or the
check confirms nothing. A time that differs from the server's clock by more than 5 minutes
is refused: the server's time is wrong, or you misread yours. Through the API an admin on
the installation may confirm every paused tenant at once; each is audited.

## Disposal

Due data is deleted by a **dispose job** that deletes each due, unheld session through the
ordinary [delete](correcting.md#delete): its records, its grace period, undo, a purge on
every copy, and the values a later arrival is checked against. There is no second way to delete. A
retention delete is marked with the rule's name.

By hand, after a preview that gives a confirm token:

```
nopacs retention dispose lab1 --rule trial7 --reason "trial 7 closed, approved by the data office" --preview
nopacs retention dispose lab1 --rule trial7 --reason "trial 7 closed, approved by the data office" --confirm <token> --wait
```

The preview lists the sessions, at most `--max-sessions` (1 to 1000; 100 by default), oldest
due first, none held. The token digests the rule, the sessions and their counts and the time
of the preview, and is good for an hour. The disposal is refused if a preview made now would
list others (data that became due, a hold placed, a rule changed), so a disposal deletes
exactly what a person saw. Each delete carries your key and your reason.

**The daily pass** handles rules whose action is `delete`: once a day, for each such rule
with due sessions and no dispose job already queued or running, it queues one that deletes up
to `retention.max-per-pass` sessions (1000 by default, from 1 to 100000; set on a tenant by an
admin on the installation only). Its deletes carry no key and the
reason `retention rule <rule>, due <date>`. A rule with action `review` never deletes by
itself.

- If a person **undoes** a retention delete, that session is left out of that rule's daily
  pass until a person disposes of it with `dispose` or the rule changes: the pass never
  deletes again what a person brought back.
- A dispose job is not retried.
- A held session is never disposed of, and no DICOM date changes when anything goes.
- Disposal is refused while retention is paused for a clock jump, and while a rebuild of the
  tenant runs.

## Keep-at-least, and lifting it

A person's delete of data a rule keeps at least until a time to come is refused with the
reason `RETENTION_MINIMUM`, and the time in `keep_until` (RFC 3339, UTC). A consent withdrawn
must stay possible, so one delete may lift the rule, with its own reason, which the
correction and its audit entry keep. Only an **admin on the installation**, or the local
operator, may do it: a tenant's admin is refused with `PERMISSION_DENIED`. Whoever wants
the delete is not the one who lifts the rule that refuses it, and a keep-at-least is often a
legal duty whose lifting should be rare and seen:

```
nopacs correction delete lab1 sub-3f9a --reason "consent withdrawn" \
  --override-retention --override-reason "legal advice of 2026-11-03"
```

The delete's preview and its token cover what retention keeps, so lifting it is a decision
about exactly what was shown.

## Holds

```
nopacs hold place lab1 sub-3f9a --reason "litigation hold, case 24-117"
nopacs hold place lab1 sub-3f9a/ses-01/scan-4 --reason "pending audit"
nopacs hold place lab1 --whole-tenant --reason "regulator's request"
nopacs hold list lab1 --state active
nopacs hold show lab1 <id>
nopacs hold release lab1 <id> --reason "case closed"
```

A hold is on a subject, a session, a scan or the whole tenant, and has a reason of 1 to 1024
bytes in your own words. It has **no expiry**: it lasts until a person releases it. It covers
what it names now and what arrives under it later, and follows a relabel.

`hold place` writes the hold's record (below) on every primary it touches before the hold
commits, so a hold that is placed is already safe from a lost database. `hold release`
reserves its count first, then rewrites the record as released.

### What a hold stops

- A new **delete** of what it covers is refused: failed precondition, reason `HELD`, with the
  hold's id.
- A **purge** of it waits, with the state reason "held", before its next object. A delete
  committed before the hold stays committed and may still be undone.
- **Disposal** skips it.
- A **replace** or a **relabel** under a hold is allowed, since nothing is lost; the purge of
  what it superseded waits.
- A storage's **object lock** is not a hold. A purge that an object lock refuses is
  recorded as `retained` until the time the store states ([correcting](correcting.md#what-purged-means)).

That a hold exists, its id and what it covers are shown wherever it stops something (a
delete's refusal and preview, a due session marked held, the `HOLD_CHANGED` event). Its
reason, which may say something about a person, is shown only to an operator or more.

### Holds outlive a lost database

A hold is a row in the tenant's database, and also a **hold record**,
`meta/.holds/<id>.json`, put on every storage the target touches and on every replica of
them, **before** the row commits as active, and rewritten when the hold is released. The
record is sealed with a MAC under the installation key, as a correction's record is. So a
rebuild of the catalog from the storages, a restore from a backup older than the hold, and
every server start read the records; one found **active with no row** makes an active hold
again. Keeping data longer is the lesser error, so a restore from a backup made before a hold
was placed still keeps what the hold protects. A replica that does not answer when a hold is
placed is owed the record, kept in the database in the same transaction, and put there when
it answers; until then no purge removes anything from it and no switch to it swaps. A
primary that does not answer leaves no hold placed.

A relabel writes the records of the holds it moves at once. A storage that is removed, or a
replica that is detached, keeps no hold records of the tenant. After each start, retention
adopts the holds it finds on the storages (an active record with no row) before its first
disposal, so a disposal never runs ahead of a hold the database lost.

A **refused record** is a hold record whose MAC is missing, does not verify for this tenant,
or is not in the record's form. It makes no hold: anyone who can write to a storage could
put a file there. It is not passed over quietly. It is counted, named by its key, and
shown by a rebuild's tally, by a restore's report and by the server's log at start. Treat
one as a sign that somebody wrote to the storage, or that a storage holds files of another
installation, and look into it. The weakness runs the other way too: a hold record is lost
with a storage rolled back to before it was written.

## Quarantine

A tenant's status warns when held files age in quarantine past `quarantine.warn-age` (30
days) or grow past `quarantine.warn-bytes`; the settings only warn and remove nothing. A
retention rule that removes held files by age is not offered yet.
