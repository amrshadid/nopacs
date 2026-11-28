# Searching a tenant

[Documentation index](../README.md) · [The command line](command-line.md) · [Routing studies with rule sets](routing.md) · [Reading the archive](reading.md) · [DICOMweb](../api/dicomweb.md)

A search finds the subjects, sessions or scans of a tenant by the header values
their files hold: every scan that is MR with a slice thickness under 1.5 mm,
every session that has a CT scan, every subject all of whose sessions have a
T1 scan. It is answered from the tenant's own database alone, by indexes, and
never reads a file from a storage.

```sh
nopacs search lab1 'Modality == "MR" and SliceThickness < 1.5' --count
nopacs search lab1 'has scan (Modality == "CT")' --level session
nopacs search lab1 'Manufacturer = "siemens"' --facet ManufacturerModelName
```

The same search is `SearchService.Search` of the API, and a reader's grant on
the tenant is all it needs. Series-level QIDO-RS searches the same index.

## What a search can name

A condition is written in the condition language that [routing](routing.md#conditions)
uses: `=` compares text with case and accents folded, `==` exactly, `!=`, `<`,
`<=`, `>`, `>=`, `in (...)`, `between A and B` (both ends included), `prefix`,
`like` (`*` for any run, `?` for one character), `contains`, `present` and
`absent`, joined by `and`, `or` and `not`; `nocase` folds the exact comparisons
(`Modality nocase in ("mr", "ct")`). A date is written `2024-01-31`, a date and
time `2024-01-31T10:30:00`, and a recent one `last 30 days`.

A search does not read every attribute a file holds. It reads the attributes the
tenant's search table has a column and an index for, and a condition that names
any other is refused, naming the attribute. A scan has one value for each (or none),
read from its file with the lowest SOP Instance UID, so the answer does not
depend on the order the files arrived in; an [edit](correcting.md) of that
file changes it.

| Attribute | Held as | Compared with |
|---|---|---|
| `Modality`, `BodyPartExamined`, `Manufacturer`, `ManufacturerModelName`, `StationName`, `SequenceName` | text | `=` and `==`, `in`, `prefix`, `like` |
| `SeriesDescription`, `ProtocolName`, `StudyDescription` | text | the same, and `contains`, `words` and a `like` that begins with `*` |
| `ImageType`, `ScanningSequence`, `OperatorsName` | text, with several values a scan | the same as the first row; a comparison holds when one value does |
| `PatientSex`, `PatientAge` | text | `==`, `in`, `prefix` (exact only) |
| `SeriesNumber`, `MagneticFieldStrength`, `SliceThickness`, `RepetitionTime`, `EchoTime`, `FlipAngle` | number | `==`, `in`, `<`, `<=`, `>`, `>=`, `between` |
| `AcquisitionDate` | date | the same, with dates |
| `AcquisitionDateTime` | date and time | the same, with dates and times |

The catalog fields `session.date`, `session.receiver`, `calling_ae` and `called_ae`
belong to the session, and `subject.label` to the subject. An attribute a file
does not hold makes a comparison false, never an error, so `not Modality == "CT"` holds for a
scan without a modality, as it does in routing. A new attribute is filled by a
backfill job for the scans already archived; until it is, a condition that names it
is refused as not ready.

## Levels, has and every

`--level` says what is listed: scans (the default), sessions or subjects. The
attributes of a scan are compared at the scan level; to use them from another level
a condition reaches across with `has` and `every`:

```text
has scan (Modality == "MR" and SliceThickness < 1.5)              # sessions
every scan (SeriesDescription present)                            # sessions
has session (has scan (Modality == "PT"))                         # subjects
every session (session.date >= 2023-01-01) and Modality == "CT"   # scans
```

`has LEVEL (...)` holds when one row of that level that belongs with this one
meets the condition, and `every LEVEL (...)` when at least one exists and all of them
do. They reach down (a subject's sessions and scans, a session's scans) and up (the
session or subject a scan is in), and nest.

## An index must serve the query

A search that no index serves would read every row of the tenant, so it is refused
with its reason rather than run slowly. The rule is made of the tree of the
condition:

- A comparison is served by the index its operator needs: an exact or a folded
  index for `=`, `==`, ranges and `prefix`, a trigram index for `contains`, `words`
  and a `like` that does not begin with a literal.
- An `and` is served when one of its children is, since the others only filter
  the rows that child finds; an `or` only when every child is.
- `contains` needs at least three characters in a row (a shorter term would read
  the whole index, or, in one database, match nothing); a `like` with a wildcard
  at the start needs three literal characters somewhere between wildcards.
- A query is refused as too complex when it would compile to more than 64 KiB
  of SQL or more than 999 values: deep nesting of `every` and `has`, or many long
  `in` and `words` lists, can reach it. Split such a query into smaller ones.
- A `not` alone is never served (`not Modality == "CT"` is every row but some): add
  a condition that is, as in `Modality == "MR" and not SliceThickness < 1.5`.
- `absent` of an attribute with several values is not served, and nor is `matches`,
  which is for routing.

A refused search exits 2 with the reason, for example `no index serves the search
on SeriesDescription: a term with three characters in a row the trigram index can
use`.

## Counts and facets

`--count` adds how many rows match. A count reads the rows it counts, so it stops
at the setting `search.facet-rows` (default 1,000,000; the installation's value is
every tenant's default, and a tenant's admin may set its own): a search that
matches more answers "at least" that many, which the command line writes as
`nopacs: at least 1000000 in all` and the API as `total_is_lower_bound`.

`--facet FIELD` lists the distinct values of an attribute of the rows that match
with how many rows have each, the most frequent first, at most 1,000 values. The
field must be held at the level searched. A facet reads every row that matches, so
it is refused, naming the bound, when the search matches more than
`search.facet-rows`; narrow the search.

```sh
nopacs setting set search.facet-rows 5000000 --tenant lab1
```

Counts, facets and searches across levels cost in proportion to the rows they
match; a page of an equality or a range does not (its first page is the same
whether the tenant holds a thousand scans or a million). Pages are in the order of
the rows' ids and resume from a cursor, so a row archived or removed between two
pages neither shifts one nor repeats a row.

## Masked keys

A key made with `--masked` may not learn the values a file held as it was
received. Its search therefore never returns a scan archived as received, nor a
session that has one: the rows are the values of files that were read before any
de-identification, so a row's presence would tell the key a value, whatever
the condition. This is stricter than the catalog, which lists such rows with
their values withheld. Counts and facets over a masked key's rows are exact over
the rows it may see. See [the security model](../concepts/security.md).

## The command

`nopacs search <tenant> [<condition>] [--level scan|session|subject] [--count] [--facet FIELD] [--limit N | --all]`.
Quote the condition for the shell. Without a condition every row of the level is
listed. `--json` writes the rows as `{"scans": [...], "more": ..., "totalCount": ...}`
(or `sessions`, `subjects`) and, with `--facet`, the facet as a second document
`{"facet": [{"value": ..., "count": ...}]}`. Every flag is in
[the command reference](../reference/commands.md#nopacs-search).
