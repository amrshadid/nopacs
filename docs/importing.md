# Importing a directory of DICOM

[Documentation index](README.md) · [A reception, end to end](reception.md) · [Running NoPACs](running.md) · [Settings](settings.md)

An *import* hands every DICOM file of an existing directory to a tenant, so an
archive that lives on a disk (an old research archive, a PACS dump, a lab's own
folders) can be brought into NoPACs without sending it file by file over the
network. This page says what an import is, where it may read, how paths become
labels, how it resumes, and what its numbers mean. The last section is a recipe
for one source, an XNAT archive.

## What an import is

An import is a *job* (see [Sending data on](sending.md) for jobs in general) of
kind `import`, made over a *receiver* of type `import`. That receiver never
listens on a port. It holds what any receiver holds (the storage or routes, the
naming rule, the choice between de-identifying and archiving as received, the
quiet period, the largest file it takes) plus how a file's path is read for
labels.

The job reads a directory under a root, one file at a time, and hands each DICOM
file to the receiver. From there it is an ordinary reception
([A reception, end to end](reception.md)): files are grouped by study, named by
the naming rule, de-identified or archived as received, routed, and quarantined
for whatever the pipeline finds, with the same events. An import never writes,
moves or deletes anything in the directory it reads.

Which scan a file belongs to is decided by the file's own Study and Series
Instance UIDs, as for every reception. The path and the map file only *name*:
they supply label text. Every label still has to pass the label rule, or the
file is quarantined as `bad_label`; a label is never cleaned up.

## Where an import may read

A tenant's administrator must not be able to make the server read any file on the
machine. So where an import may read is decided by the installation, in two
settings:

- `import.roots` (installation only, empty by default): the directories an import
  may read, one per line as `name=/absolute/path`, such as `old=/mnt/old-archive`.
  A name is 1 to 63 characters of `a-z`, `0-9` and `-`, starting with a letter;
  there are at most 64 roots.
- `import.allowed-roots` (set on a tenant by the installation's admin only, empty
  by default): the root names a tenant's import receivers may read, such as
  `old`. Until a root is allotted, a tenant cannot add an import receiver.
  Allotting one root to several tenants means each may read all of it.

A root's path is resolved through its links when the setting is made, and kept
so: the value read back holds each path as it resolved then. The setting
is refused if a root, once resolved, is, contains or lies inside a storage
directory root, the server's data directory, its incoming area, the directory of
the key file, or another import root; and `storage.directory-roots` is refused
if one of its roots would come to touch an import root the same way. So an import
can never reach the archive's own files or its secrets. A job on a root whose
links now lead somewhere else than when the setting was made is refused, and a
running unit on such a root fails with the reason "root unavailable". Removing or
moving a root an import receiver reads, or taking it back from the receiver's
tenant, is refused until the receiver is removed or changed.

An import receiver names one root. A *job* names a path under it, relative to the
root and written with `/` between segments. Each segment is 1 to 255 bytes of
UTF-8, none is `.` or `..`, none holds `\`, `:` or a NUL byte, there is no empty
segment and no leading or trailing `/`, and the whole path is at most 2048 bytes:
the longest path the server keeps in an index of its database (a Postgres index
row holds at most 2704 bytes, and the path shares it with the job's id and more).
A file or directory met in the tree whose path is longer is skipped as
`path_too_long`, and one whose own name breaks the rule of a segment (it holds
`\` or `:`, is not UTF-8, or is longer than 255 bytes) as `bad_name`, a directory
without being entered. An empty path means the whole
root. The segments are opened one by one under the
root without following a link, so a link anywhere in the tree is skipped (as
`not_regular`) and nothing outside the root is ever opened. On Linux, macOS and
the other Unix systems, a file with more than one hard link is skipped as
`not_regular` too, since its other name may lie outside the root, and so is a
file or directory on another file system than the root's (a mount point, and
everything under it), which the root's owner may not have meant to expose. On
Windows neither can be told from what a directory read gives, and neither is
skipped. A tenant's readers see root names, never paths.

A unit in flight reads its root again every 30 seconds, while it waits on a read as
well as between the entries it judges: when the installation no longer names the root,
the tenant is no longer allotted it, or it now resolves to another directory, the
unit stops at once and fails with the reason "root unavailable", handing in
nothing more.

Making an import receiver needs an admin of the tenant; making an import job
needs an operator, like any job.

## Layouts: how a path becomes labels

A receiver's *layout* says whether and how a file's path is read.

- **`plain`**: the path is not read. Every file under the job's path is handed in
  and its labels come from the naming rule over the file's headers (by default,
  keyed hashes, as for any receiver). A unit of work is one folder at the first
  level under the root.
- **`pattern`**: a regular expression (RE2 syntax, at most 1024 bytes) over the
  file's path relative to the root, with at least one *named part*. It has to
  match the whole path (the server anchors it at both ends). Part names are a
  lowercase letter followed by lowercase letters, digits and `_`, each used once.
  `unit_depth` (1 to 16) says how many segments from the root make a *unit's
  folder*.

The naming rule uses a pattern's parts like any other part: `{project}`,
`{session}`, `{scan}`. A file whose path the pattern does not match is skipped as
`outside_layout`.

For example, a tree kept as `<study>/<session>/series/<scan>/<file>`:

```text
/mnt/old-archive/
  trial-a/
    visit1/
      series/
        3/  0001.dcm  0002.dcm ...
        4/  0001.dcm ...
    visit2/
      series/
        3/  ...
  trial-b/
    ...
```

is read by the pattern

```text
^(?P<study>[^/]+)/(?P<session>[^/]+)/series/(?P<scan>[^/]+)/[^/]+$
```

with `unit_depth` 2, so each `<study>/<session>` folder is one unit, and a naming
rule such as session `{study}_{session}`, scan `{scan}`. The study is part of the
session label because a session label that is unique only inside one study would
collide with another study's in the same tenant.

## The map file

A tree often names its sessions but not their subjects: the system that wrote it
kept that elsewhere. A *map* is a table that gives one part from others.

It is a CSV file (RFC 4180), UTF-8 (a byte order mark is allowed), whose first row
names the columns. Four settings describe how to read it; they are given together
or not at all:

| Setting | Meaning |
|---|---|
| `map_key_columns` | Header names of the key columns, matched byte for byte, 1 to 8, each once |
| `map_key_parts` | The layout's parts that form a file's key, in the same count and order as the columns |
| `map_value_column` | The header name of the column that holds the value (not one of the key columns) |
| `map_gives` | The part the map fills, such as `subject` |

Take the tree `<project>/<session>/<file>`, read by
`^(?P<project>[^/]+)/(?P<session>[^/]+)/[^/]+$`, and a map with columns `proj`,
`visit` and `participant`:

```text
proj,visit,participant
trial-a,visit1,P-017
trial-a,visit2,P-017
```

With `map_key_columns` `[proj, visit]`, `map_key_parts` `[project, session]`,
`map_value_column` `participant` and `map_gives` `subject`, each file gets the
`{subject}` of the row whose `proj` and `visit` equal the file's project and
session. The naming rule can then say subject `{project}_{subject}`.

Other columns are ignored. Every row has as many cells as the header, and its key
and value cells are not empty. A key may be listed twice with the same value; a
key with two different values refuses the whole map, because either choice would
file data under a label the source did not say.

The map is either **a file inside the root** (set on the receiver, or named by the
job) or **inline** with the job, at most 3 MiB either way, since a map read from a
file is kept with the job as an inline map is; a larger map file is refused by its
size before it is read. A job may give its
own four settings, which replace the receiver's whole. The map is read when the job
is made and kept with the job as it was read; its units and a retry use that copy
and never read the file again, so a map file changed or swapped after the job was
made changes nothing. A job that names a map file and keeps no copy of its map is
refused by its units, each failing with a reason that says to make the job again.

A refusal names the line at fault by its number and says what is wrong. It never
repeats a cell, the header or the file's path, because a map holds labels as the
source kept them, and they can be names.

A file whose key is not a row of the map is skipped as `unmapped_session`. That is
deliberate: a folder the source system deleted from its records is not brought
back to life.

A receiver is refused if its map names a part the layout does not give as a key,
or fills a part the layout already gives.

## Naming rules with given parts

A receiver's naming rule (see [Reception](reception.md) for naming in general) may
use parts from a header pattern, from the layout and from the map. The server
refuses a rule that uses a part nobody gives, or a part two sources would both
fill. A label from a header needs nothing special: `{project}_{PatientName}` is an
ordinary template.

A session that already has a label keeps it, whatever a path now says; such files
are counted as `label_differs` and are archived under the first label.

## Units, the cursor and resuming

A job's work is a *listing unit* and one unit per folder at the layout's unit
depth. The listing unit lists those folders a page at a time and queues one unit
for each; it also judges any file it meets above that depth. Each unit then walks
its folder one directory at a time, in an order fixed by comparing paths segment by
segment (so `a/b/c` comes before `a/b.d`).

A directory is read as names only, at most `import.max-dir-entries` of them
(installation only, default 200000, between 1000 and 10000000). A directory with
more entries is not read further and is skipped whole as `directory_too_wide`.
Nothing is lost: raise the setting (it applies at the next directory read, with no
restart) and retry the job. A name read costs about its own bytes and 25 more
(measured on 64-bit), so the default holds about 16 MiB of 60-byte names, and the
largest setting about 0.8 GiB of them, up to 2.6 GiB of 255-byte ones, for each
unit reading so wide a directory. A unit holds the names of the directory it reads
whole and at most 64 KiB of the names of each directory above it; it reads a
directory above again when those run out, so a directory of many subdirectories
is read again once every few hundred of them. A directory the unit is already
inside that grows past the setting fails the unit for now, spending an attempt:
it goes on once the setting is raised, and the job fails if the directory stays
too wide through every attempt.

A unit whose folder cannot be opened or read (no permission, too many open files,
an input or output error) fails for now, spending an attempt; the job fails if the
cause stays through every attempt, rather than end with that folder's files
uncounted. A folder that is no longer there is gone and its walk ends with nothing
found; one replaced by a link or a file is skipped as `not_regular`. A unit whose
walk makes no progress for ten minutes (no names read, no entry judged, no bytes
of a file read: a network share that stopped answering) stops renewing its lease
and fails for now, spending an attempt, and another claim takes it up.

Each unit keeps a *cursor*: the last path it handed in. The cursor, the file's
record and the unit's counts are written in one database transaction per file. So
after a crash or a lost lease the unit goes on after its cursor, and no file is
handed in twice. A unit pauses with its place kept, using no attempt, while the
receiver is disabled, the incoming area or storage is nearly full, or the tenant
is not ready.

Two jobs on one receiver whose paths overlap by whole segments (`a` and `a/b`, not
`a` and `ab`) are never queued or running together; the second is refused. Running
a whole job again is simply a new job: files already in the archive count as
`already_held`, and files added to the tree since count as new.

To keep an import from taking the pipeline away from live scanners, a tenant runs
at most `import.max-units` units at once (1 to 64, default 4, set by the
installation's admin) and the server at most `import.server-max-units` together
(1 to 256, default 8).

## What the counts mean

Handing a file in returns once the file is in the incoming area, before it is
identified, de-identified, archived or quarantined. So the job counts both what it
handed in and what became of it. `ImportCounts` (for the job, and for each unit
through `ListImportUnits`) holds:

| Count | Meaning |
|---|---|
| `found` | Entries the walk judged: each file, and each directory skipped whole |
| `skipped` | Files not handed in, by reason (below) |
| `handed_in` | Files landed in the incoming area |
| `handed_again` | Handed in a second time after a crash; always 0 in this version |
| `archived` | New instances in the catalog |
| `already_held` | Same SOP Instance UID and same checksum as the archive already has |
| `quarantined` | Held in quarantine, by reason |
| `lost` | The incoming copy was gone before the pipeline read it; a retry hands it in again |
| `failed` | The file's reception failed (`nopacs reception retry` can move it back) |
| `discarded` | The file's reception was discarded by a person |
| `processing` | Handed in, not yet decided |
| `label_differs` | Archived into a session that keeps a different, earlier label |

### The two identities

The counts reconcile at every answer, across a crash too:

```text
found + handed_again = skipped + handed_in
handed_in = archived + already_held + quarantined + lost + failed + discarded + processing
```

(`skipped` and `quarantined` are the sums of their counts by reason;
`label_differs` is in neither.) The first says every entry the walk judged was
either skipped or handed in. The second says every file handed in is either
decided or still being worked on. If a job's numbers do not satisfy them, that is a
bug to report.

A file already held under an older revision of an instance (one a correction
replaced) is counted `already_held` and tallied `superseded_revision`, so reading a
tree again after a correction does not bring an old version back.

### Skip codes

| Code | Why a file was not handed in |
|---|---|
| `not_part10` | The first 132 bytes are not a Part 10 preamble and `DICM` (PS3.10 7.1): not DICOM, or a data set without its file header. Never reaches quarantine, so a tree's notes, images and index files do not fill it |
| `outside_layout` | The path does not match the layout's pattern |
| `unmapped_session` | The file's key is not a row of the map |
| `directory_too_wide` | The directory has more entries than `import.max-dir-entries`; counted once |
| `unreadable` | Could not be opened or read (permission, input or output error, removed during the run) |
| `not_regular` | A symbolic link, device, socket or pipe; never followed. On Unix systems also a file with more than one hard link, and a file or directory on another file system than the root's |
| `too_large` | Larger than the receiver's largest file; not copied |
| `path_too_long` | Its path from the root is longer than 2048 bytes; a directory is not entered. Each such entry counts once, and they are listed in one row under the deepest folder above them whose path fits. Shorten the tree's paths (move or rename it) and make a job again to import them |
| `bad_name` | Its own name holds `\` or `:`, is not UTF-8, or is longer than 255 bytes; a directory is not entered. Each such entry counts once, and they are listed in one row under the folder that holds them, the name never kept. Rename them and make a job again to import them |

### Settling, and partial

A job does not end when its last unit has walked: files are still in receptions.
It stays `running` with the reason "settling" until `processing` is zero, and only
then ends. If files are still in a reception an hour past the receiver's longest
open time after the last walk, it ends `partial` with the reason "settling timed
out" and says how many were left; its counts keep moving until they are decided.

The final state is:

- **done**: every file found has an outcome and none was quarantined, lost, failed
  or discarded, or skipped as `unreadable`, `too_large`, `directory_too_wide`,
  `path_too_long` or `bad_name`. Files skipped by design (`not_part10`, `outside_layout`, `unmapped_session`,
  `not_regular`) leave it done.
- **partial**: otherwise.
- **failed**: a unit's attempts ran out, or its root was unavailable. What it had
  handed in is still counted.
- **canceled**: by `CancelJob`. Files already handed in are not withdrawn; they go
  on to their outcome and are counted. A unit still walking stops at its next
  file: the job takes no file handed in after the cancel. A job that reached its
  end while the cancel was on its way keeps that end, and the cancel is refused.

A file still undecided when its reception ends is counted `already_held` if the
reception ended done (the reception archived, held, or found the same file for
each of its files) and `failed` if it failed or was quarantined: the pipeline
quarantines a reception only after holding each of its files, so none should be
left, and one that is is not claimed held.

`ListImportFiles` lists, to an operator, each file that did not reach the archive
(skipped, quarantined, lost, failed or discarded) with its path and reason, up to
`job.failures-kept`.

## Paths can identify a person

A folder can be named after a patient. So a path is treated as possible patient
data: it is kept only in the job's cursor and skip rows and on a quarantined file's
row (`import_path`) in the tenant's database, and shown only to an operator or more
on the tenant. It is never written to a log, an event or an audit entry. Events
carry counts only. The audit records the root's name and the job's id. The same
holds for a map file's path and an inline map.

## The commands

The installation's admin names a root and allots it to a tenant:

```sh
nopacs setting set import.roots "old=/mnt/old-archive"
nopacs setting set import.allowed-roots old --tenant lab1
```

A tenant's admin adds an import receiver over it (`--layout plain`, or
`--layout pattern` with `--pattern` and `--unit-depth`; the four `--map-*` flags
and `--map-file` together describe a map):

```sh
nopacs receiver add lab1 old-tree --type import --storage main --root old \
    --layout pattern --pattern '^(?P<project>[^/]+)/(?P<session>[^/]+)/.+$' --unit-depth 2 \
    --naming-subject '{project}_{subject}' --naming-session '{project}_{session}' \
    --naming-scan '{#SeriesInstanceUID}' --map-file subjects.csv \
    --map-key-column project --map-key-column label \
    --map-key-part project --map-key-part session \
    --map-value-column subject_label --map-gives subject --as-received --enabled
```

The receiver's health is `idle`: it holds no listener. `nopacs receiver set`
changes one of its fields at a time (`--pattern`, `--map-file`, ...), and is
refused while one of its jobs is queued or running, as `nopacs receiver remove` is.

An operator makes the job, a path under the root (none for the whole root),
optionally with a map of its own: `--map-file` under the root, or `--map-csv`, a
file on the operator's computer sent with the job (at most 3 MiB). `--wait`
follows the job to its end as `nopacs job watch` does:

```sh
nopacs job import lab1 old-tree trial-a --wait
nopacs job import lab1 old-tree trial-b/visit2 --map-csv subjects.csv
```

`nopacs job show` gives the job's counts. `nopacs job files` lists each file that did
not reach the archive, by path, with why (`--kind skipped`, `quarantined`, `lost`,
`failed`, `discarded`; `--unit`), and `nopacs job units` lists each unit's folder,
state and counts (`--state`); both need the operator role, since they answer paths.
Each command takes `--json`. `nopacs job cancel` stops a job within one file.
`nopacs job retry` of an import job is not served yet (the server answers
`UNIMPLEMENTED`): make a new job over the same path instead, which counts what is
already archived as `already_held`.

A map read from a file is kept with the job as it was read, in the job's inline
map, so it is at most 3 MiB as an inline map is; a larger map file is refused by its
size when the job is made, before it is read. `--map-csv` must name a regular file
(not a pipe or a device) of at most 3 MiB.

The job is checked before it is made: the root opened, the path's folders and the
map file opened without following a link, the map read. The check is bounded to 30
seconds; a share that does not answer refuses the job as `UNAVAILABLE` rather than
holding the request, and holds back no change of the installation's settings or of
a receiver meanwhile. A receiver or root changed while the job was checked refuses
it as `UNAVAILABLE` too; ask again.

A removed import receiver's name stays held while any of its jobs, ended or not,
names it: a new receiver of that name is refused (`ALREADY_EXISTS`), as it is while
a reception or a quarantined file names it.

Where an import root may lie is compared by the directories themselves, not only by
how their paths are written: a second spelling of a directory (an accent written
decomposed on a file system that ignores Unicode normalization, as macOS's do) or
a bind mount of it is that directory, for import roots and storage roots alike.

## Recipes

A recipe is configuration only. NoPACs has no code specific to any product; a
layout is a pattern and a map is a CSV, and the export of any other tool is
described the same way.

### An XNAT archive

An XNAT archive keeps each session's files in a tree of this shape, as
publicly described:

```text
<project>/arc001/<session>/SCANS/<scan>/DICOM/<file>.dcm
<project>/arc001/<session>/RESOURCES/...
<project>/arc001/<session>/ASSESSORS/...
```

**Layout.** A pattern layout:

```text
^(?P<project>[^/]+)/arc[0-9]{3}/(?P<session>[^/]+)/SCANS/(?P<scan>[^/]+)/DICOM/[^/]+$
```

with `unit_depth` 3, so one unit is one session folder (`<project>/arc001/<session>`).
Files under `RESOURCES` and `ASSESSORS` fall outside the pattern and are skipped as
`outside_layout`. The `*_catalog.xml` files in a `DICOM` folder match the pattern
but are not DICOM, so they are counted `not_part10`; NoPACs does not read them.

**The subject.** XNAT keeps a session's subject in its database, not in the
folders. If you can export the experiments listing as a CSV, use it as the map:

| Setting | Value |
|---|---|
| `map_key_columns` | `[project, label]` |
| `map_key_parts` | `[project, session]` |
| `map_value_column` | `subject_label` |
| `map_gives` | `subject` |

**Naming rule.**

| Level | Template |
|---|---|
| subject | `{project}_{subject}` |
| session | `{project}_{session}` |
| scan | `{scan}` |

The project is in the subject and session labels because XNAT session labels are
unique only within a project, and two projects imported into one tenant could
share one. A site importing a single project per tenant can drop it.

**Without the export.** If the site's own anonymization put the subject into
Patient Name, use no map and the subject template `{project}_{PatientName}`.
Sessions then need no map entry, so none is skipped as `unmapped_session`.

**Not verified on a real archive.** The tree shape (`arc001`, `RESOURCES`,
`ASSESSORS`, `*_catalog.xml`) and the export's column names come from public
descriptions, not from a real archive. If the pattern does not match your tree,
every file is skipped as `outside_layout`; nothing is lost and nothing is
archived wrongly, and the pattern can be changed on the receiver and the job run
again. Check the column names against your own export before relying on the map.
Importing from an archive while XNAT is writing to it can catch a file
mid-write, which is then quarantined; import from a snapshot or a read-only
mount.
