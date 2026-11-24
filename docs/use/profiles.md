# Profiles, trials and lookup tables

[Documentation index](../README.md) · [De-identification](../concepts/deidentification.md) · [Receiving](receiving.md) · [The command line](command-line.md)

How to make a de-identification profile, try it before any data depends on it,
describe what it does, find a subject from the identifier a site holds, fill a
lookup table, and check the archive against a profile. What de-identification is,
and what a receiver does with it, is in [the concept page](../concepts/deidentification.md).

## Make a profile, try it, then point a receiver at it

A profile is a JSON document: its bases (the profiles the server ships, listed
by `ProfileService.ListProfileBases`, such as `annex-e/basic:2026d`) and its
own rules. A version never changes; each document stored under a name makes its
next version.

```
cat > research.json <<'DOC'
{"bases": ["annex-e/basic:2026d"],
 "rules": [{"name": "keep-weight", "selector": {"keyword": "PatientWeight"},
            "action": {"keep": {}}, "reason": "dose calculations need the weight"}]}
DOC
nopacs profile create lab1 research --document research.json --note "basic, weight kept"
nopacs profile try lab1 --profile research@1 ./samples/ --changed-only
nopacs profile explain lab1 --profile research@1 ./samples/one.dcm
nopacs receiver set lab1 scanner1 --profile research@1
```

`profile try` runs the version over sample files (or held files, `--record`)
exactly as a receiver would, in memory, and shows each element's value before
and after with the rule that decided it, and whether the file would be archived
or held and why. Nothing is stored and no secret of the tenant is used: each
run has keys of its own, so its remapped UIDs, hashes, shifted dates and codes
never equal what the archive holds (only their form carries over), and it reads
no lookup table (every lookup shows `UNASSIGNED`). With `--receiver`, and for
held files, the receiver's own checks run too, so "passed" means that receiver
would archive the file; without one the verdict is de-identification's alone.
The audit entry keeps the counts per rule and reason as evidence the version
was tried, never a value. At most two trials run at once on a server. `profile explain` shows, for one
file, which rule or base decides each element and why the others do not, with
no value; `profile describe` says in plain words what a version does, with its
method record and every rule it runs;
`profile diff` shows what a new version changes. Trying needs the operator role
(it answers the files' values, and can read held files); making a version needs
admin. Every command takes `--json`.

A rule can also choose elements by their value, and cut only part of a value.
A `valuePattern` selector (an RE2 pattern of at most 256 bytes; the whole value
by default, anywhere in it with `"search": true`) selects the SH, LO, UC, ST,
LT, UT and PN elements whose text matches, at any depth, each value of a
multi-valued element apart with its padding removed. Matching is exact: case
counts unless the pattern says `(?i)`. A value is matched in Unicode NFC: an
accented letter the file wrote decomposed (`u` and a combining diaeresis) is
matched as its composed letter (`ü`), so a pattern writes accented letters
composed, as editors do (a pattern written decomposed is refused as
`bad_pattern`); redact writes what it keeps in NFC. Clean and the
check after compare names in the same form. A search takes the longest
match at each place, so `MRN|MRN\d+` finds the whole MRN. A rule naming an
element by its tag, path, private creator or group wins over it, and it wins
over a rule of a whole VR. The `redact` action cuts the matches of its pattern
out of a text value and keeps the rest: with `{"redact": {"pattern":
"MRN\\d{6}"}}` on Study Description, `Head CT MRN123456 follow-up` becomes
`Head CT  follow-up`. A name component is cut apart from the others, so a name
keeps its `^` and `=`; but a person's name is never partly kept, so redact on a
PN removes it as keep would.

Neither ever passes a value unread or half done: the file is held when a value
a pattern would read is longer than 64 KiB (`too_large`) or cannot be read as
text (`bad_value`), when a pattern finds more than 256 distinct texts in one
value (`too_many_matches`), when the value left after a cut would not be valid
for its VR or would not fit its element (`redact_invalid`), and when a file's
patterns would run past their bound of work (`too_large`: the bytes each reads
times its compiled size). Every distinct text a pattern rule removes or cuts is
searched for by the check after, as a whole word when it begins or ends with a
letter, so a copy of a redacted MRN kept elsewhere in the file holds it.
`profile try` shows each element's `redactions` and `patternMatches`, and
`profile explain` names the selector `value_pattern`. A version that uses
either is of language edition 2; versions that do not keep their checksums.

## Describe a version, and try it with no server

`nopacs profile describe lab1 research@1` says what the version does in plain
words: for each class of element (names of persons, identifiers and addresses,
dates, times and ages, UIDs, private elements, pixels, the rest), which
elements are removed, kept, emptied, remapped, shifted, hashed or looked up,
those under a condition marked; the defaults and what is never kept whatever
the rules say; and the method record every file it makes carries, the text a
DICOM Conformance Statement needs (PS3.15 E.1.1, E.3): the PS3.16 CID 7050
codes in De-identification Method Code Sequence (0012,0064) with their
meanings, the values of De-identification Method (0012,0063) this server
writes (the meanings, the version, NoPACs' and go-dicom's versions), Patient
Identity Removed (0012,0062) YES, and Longitudinal Temporal Information
Modified (0028,0303). A version whose own rules keep what an Annex E base
changes claims no code, and says why. Then every rule in order (`--no-rules`
leaves them out); `--json` gives all of it. The classes are judged from each
rule's selector and VRs, not from a file: an element's own decision is
`profile explain`'s. Needs the operator role.

`nopacs profile try --local --document draft.json ./samples/` runs the same
trial as `profile try` inside the command, with no server and no tenant: the
engine and the check after run in the process, under keys made for the run and
thrown away, every lookup `UNASSIGNED`, nothing sent and nothing written. The
document may be a version's export (`profile export`, or its `--json` answer,
which names the version; one whose checksum this build does not reach is
refused). Files are read one at a time, each up to 64 MiB, so a directory of
any size can be tried; the verdict is de-identification's alone, with
`receiver.header-limit` and `deid.pixel-limit` at their defaults. The answer
and `--json` are those of a server's trial with no receiver.

## Find a subject from its identifier, and fill lookup tables

The archive keeps no map from a label or a code back to the identifier it
replaces. When a person withdraws consent, an admin finds their data the
forward way, from the identifier the site holds:

```
nopacs deid code lab1 --patient-id -          # reads the ID from standard input
nopacs deid code lab1 --value - --json < id.txt
```

The answer is the keyed hash of the value under the tenant's naming key (what
`{#PatientID}` writes into a label and a `hash` rule into a file), the subject
label each receiver whose naming rule makes it from that hash alone gives the
value, whether the catalog holds that subject now, and the code each lookup
table holds for the value. `--attribute` names another attribute (a keyword or
eight hexadecimal digits); the value's padding is removed as its VR says,
exactly as at reception. It needs the admin role on the tenant, takes one value
per call, at most 60 calls a minute a key and 60 a minute into one tenant (exit
7 past either), and each call is in the tenant's audit log, written before the
answer (a call whose entry cannot be written is refused), and in the server's
log, with the key and the attribute, never the value. `--patient-id -` and
`--value -` read the value from standard input, keeping it out of the shell's
history and the process list. The call is never served over HTTP GET, so a
value never travels in a URL.

A `lookup` rule replaces a value by the code its table gives it. Codes are
assigned in order on first sight (`"onMissing": "LOOKUP_MISSING_ASSIGN"`), or
chosen by a person beforehand: then a file whose value has no code is held
(`deid_failed`, `lookup_missing`), and the codes are imported from a CSV of
value and code:

```
printf 'value,code\n98890234,SUBJ001\n' > cohort.csv
nopacs profile lookup import lab1 subjects cohort.csv
nopacs profile lookup list lab1
```

Each value is stored as its keyed hash, never as given (its padding removed as
an LO value's). An import is all or nothing, at most 10000 entries: one value
with two codes in the file, or one code for two values, refuses all of it,
naming the entry; values the table gives another code, or codes it gives
another value, refuse all of it too, answered by how many conflict and never
which, so an import cannot be used to learn whether a person is in the table.
An entry it already holds is counted unchanged. A code assigned later in order
passes over every imported code. A key makes at most 10 imports an hour,
stored or refused (exit 7 past it). Every import is audited before its answer,
with counts and no value or code. Import needs admin; `lookup list` (each
table's count, nothing else) needs operator.

## Check the archive, and dry-run a version over it

```
nopacs deid check lab1 --wait
nopacs deid dry-run lab1 research@2 sub-0017 --wait
nopacs job failures lab1 <job id> --warnings
```

Both are jobs of the kind deid that change nothing: no catalog row, no stored
byte, no lookup table. `deid check` verifies each archived instance against the
profile version its catalog row records, by what that version would leave of
each element the archive holds: no element it removes (a private element it
does not keep among them), no value in an element it empties
(`kept_value`), nothing but what the IOD keeps of an element it removes or
empties unless required; Patient Identity Removed (0012,0062) YES;
De-identification Method (0012,0063) naming the version; and no UID outside the
standard's registry that a file the tenant received carried, found by
remapping the file's UIDs with the tenant's UID key, as the reception did, and
looking them up in the catalog (the lookup answers only whether one was found;
a UID the remapping itself made, as in de-identified data received again, is
none). Only Study, Series and SOP Instance UIDs the catalog holds are
recognized: an original Frame of Reference UID, or a Referenced UID of an
instance never archived, is not. An instance archived as received passes with
a note. An archived file no longer holds the Patient ID a version that shifts
dates keyed each subject's shift by, so both jobs give the engine the
instance's subject label in its place; a check compares no shifted date, only
which elements remain. `deid dry-run` runs a version (NAME alone is the latest, fixed when the
job is made; `--own` runs each instance's own) over each archived file in
memory, as `profile try` does: keys made for the job, never the tenant's; no
lookup table read; the check after; no receiver's checks. It says what the
version would archive and what it would hold and why.

A rule's conditions read the instance's reception: its receiver, its calling AE
title when its files came with exactly one, and its receiver's called AE title
as configured now. A condition on Modality, SOP Class UID, Manufacturer, Model
Name or Software Versions reads the archived file, which says what the file
said only where the archiving version kept that element. In a check, an element
whose decision rests on such a condition, or on a reception fact not known, is
a note (`check|undecidable|<tag>`), not a finding; in a dry run, the version
decides on what the archive says.

With no target a job judges the whole tenant. `job watch`, `show`, `failures`,
`cancel` and `retry` work on them as on send jobs: a unit per scan, progress
recorded every 64 instances or second, a cancel seen within one batch, a
restart judging again at most one batch per unit. `job show` adds the counts
per rule and per reason (`rule|<rule>|<action>`, `hold|<reason>|<cause>`,
`check|<finding>|<tag>`, `class|<SOP class>|passed`), names and tags only,
never a value read from a file; `--json` gives them as `tallies`. A file larger
than 64 MiB is not read and passes with a note (`too_large`); one whose bytes
differ from the catalog's checksum is `damaged_in_storage`. A dry run holds
about three times the file it judges, and a pixel rule's image within the
lower of `deid.pixel-limit` and 256 MiB. The server works at most two units of deid
jobs at once, one per tenant, and their reads count in the same bound as the
API's and the senders'. Both need the operator role.

