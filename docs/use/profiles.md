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

### Text lists: names of staff, sites and departments

A profile can hold named lists of terms that `clean` and `redact` cut out of
free text, such as the names of a site's staff or its clinics. A list is part
of the version: changing a term makes a new version, as changing a rule does.

```
{"bases": ["annex-e/basic:2026d"],
 "textLists": [{"name": "staff", "version": 3,
                "terms": ["Ann Lee", "Okonkwo-Hale", "North Riverbend Clinic"]}],
 "rules": [
   {"name": "clean-desc", "selector": {"keyword": "StudyDescription"},
    "action": {"clean": {"lists": ["staff"]}}},
   {"name": "cut-comments", "selector": {"keyword": "ImageComments"},
    "action": {"redact": {"pattern": "MRN\\d{6}", "lists": ["staff"], "replacement": "[x]"}}}]}
```

- **A list** has a name (1 to 63 of `a-z`, `0-9` and `-`, starting with a
  letter), a version of its own from 1 (raise it when the terms change, so a
  description tells two lists of one name apart), and 1 to 10,000 terms. A
  profile has at most 16 lists and 20,000 terms in all, at most 512 KiB of
  terms folded and 3 MiB of their forms in every case and Unicode form, in
  UTF-8 and in the character set that writes them longest (`too_large` past
  either: 20,000 terms of ASCII of 25 bytes each take 0.5 MB, 10,000
  accented names of 10 letters 1.6 MB, 20,000 of 5 letters 1.5 MB; 20,000
  of 20 letters are past it). The second bound counts each term's forms
  exactly, written in each encoding the server reads, so a version it
  accepts never has a file refused for the set the file was received in; it
  bounds the automata a version's search holds to some tens of megabytes. In
  a set of several encodings (ISO 2022 code extensions), a form of a term
  written across them that would pass what was counted for the term is left
  to the search of the decoded text. A term is at most 256
  bytes of UTF-8, with no control
  character and no backslash (which separates the values of an element), and
  at least 2 characters, or 1 in a script written without spaces (Han,
  Hiragana, Katakana, Hangul). Two terms of a list that are one once folded
  are refused as `duplicate_term`. A list past a bound is refused whole,
  never cut to fit; a problem names the list's and the term's position
  (`textLists[1].terms[17]`), never the term.
- **Matching** ignores letter case and full-width forms, compares in Unicode
  NFC (an accented letter written decomposed is its composed letter), and
  takes a run of white space in the text for the one space of a term: "ann
  lee", "ANN  LEE" and full-width "ＡＮＮ ＬＥＥ" are "Ann Lee". A term is taken
  only where it stands as a word: on a side where the term ends in a letter or
  a digit, no letter or digit may stand beside it, so "Ann" is not cut out of
  "Annual" nor "Lee" out of "Leeds"; on a side where it ends in punctuation
  ("Dr."), anything may. A term of a script written without spaces is taken
  wherever it stands. Removing what does not identify is a failure as much as
  leaving what does (PS3.15 E.3.3 keeps a descriptor's text that does not
  identify; the MIDI-B evaluation scores over-removal as an error). A soft
  hyphen, a zero-width space, joiner or word joiner, or a zero-width
  no-break space inside a term does not hide it: they are dropped before
  comparing, in clean and in the check alike. Not folded, a limit: full case
  folding (`ß` is not `ss`), compatibility forms (the ligature `ﬃ` is not
  `ffi`) and the Turkish dotted `İ`; an accented letter is its own letter
  (`José` is not `Jose`). List each spelling a site writes.
- **`clean` with `lists`** cuts the terms of those lists, besides the file's
  own identifying values it always cuts, and keeps every other character.
  **`redact` with `lists`** cuts the terms with the matches of its pattern,
  each becoming the replacement, terms that overlap cut as one, a person's
  name component by component; a redact may name lists and no pattern. A rule
  names at most 16 lists, each a list of the profile (`unknown_list`). A
  redact's replacement, and a replace or set constant or template, holding a
  term of any list of the profile is refused (`bad_value`): the check after
  would hold every file the rule writes it into.
- **The check after** searches every value of every file the version makes for
  every term of every list, whether or not a rule named the list, in the text
  of each element and in the raw bytes, in UTF-8 and in the character set the
  file was received in, by the same rule of words. Only values that hold
  numbers are not searched for the terms, since names of four letters occur
  by chance in a megabyte of random bytes: a standard element whose VR, as
  sent and in the data dictionary, is one of numbers (OW, OF, OD, OL, OV, FL,
  FD, SL, SS, SV, UL, US, UV, AT), and pixel data, float and double float
  pixel data, overlay data (60xx,3000) and waveform data (5400,1010).
  Everything else is searched: OB and UN values, so an encapsulated document
  (0042,0011, a PDF or CDA) holding a term holds the file; a Series
  Description sent as OW, since the dictionary says it is text; a private
  value whatever its VR; and a value of a tag the dictionary does not know.
  `clean` does not rewrite an OB or OW value but removes it, and `redact`
  acts on text only, so a document kept whole with a term inside is held,
  never archived.
  A label a naming rule made from a value as received is searched too. A term
  found again, in a value the profile kept or anywhere else, holds the file as
  `residual_value` with the kind `list_term` and the element's path, never the term. So a
  list is also a list of words that must not leave the site: a term a profile
  keeps somewhere holds every file that has it.
- **Terms are never printed.** `profile try`, `explain`, `diff` and their
  `--json`, problems, errors, logs and audit entries never hold a term;
  `profile describe` gives each list's name, version, number of terms and the
  SHA-256 of its terms folded and sorted, each followed by a line feed, so two
  lists written apart are seen to be the same; `profile diff` lists each list
  added, removed or changed between two versions with those summaries on
  each side. The checksum is not keyed, so a list of a few terms could be
  found from it by hashing guesses: it is answered only to a key that may
  read the terms. A masked key (an outside model's) is never answered a term
  nor what confirms a guess of one. A version with lists is answered to it
  with its lists' names and versions and no term, and without its expanded
  form and its checksum, which covers the terms (`terms_withheld`; `profile
  export` refuses such a document); a list's summary without its checksum.
  It may not store a version with lists (the answer for a document equal to
  a stored version would confirm it), nor try or explain a stored version
  with lists (whether a value changes tells a guessed name in the list from
  one that is not); it may check a document of its own (`profile
  validate`). Masking bounds what the API answers, not what goes through the
  archive: a key that may both send files and read archived headers can send
  a file holding a guessed word and see whether it was held or cut, and so
  learn whether the word is a term. Give a masked key one of the two, not
  both. A private creator holding a term, or a value the profile removed
  from the file (the patient's name written as a block's creator), is
  withheld from the report of elements no rule named. A list's term is
  found in a creator by the same rule of words as the check after; the
  patient's name and identifiers are found anywhere inside a creator, as
  part of a longer word too.

Bounds of the work: the terms of every list are found in one pass over a text
whatever their number, and a file's searches for them share clean's bound of
work; past it the file is held (`too_large`). A list of 10,000 terms adds about
30 µs to clean a text of 1 KiB, and about 45 ms to the check after for each
MiB of text and binary values a file keeps outside its pixel data (measured
on a 10-core Apple silicon laptop). A stored version's automata are built
once, by one caller while the others wait, and kept in a cache all its
receptions, jobs and corrections share, bounded by the bytes of the automata
it holds (256 MiB by default, the setting `deid.list-cache-bytes`), the
least recently used dropped first; the
raw forms of up to 8 character sets per version are kept with them and
counted. A document tried, described or validated builds its own, uses them
and drops them, never entering the cache. Each file's check counts the
automata it searches with in the memory it takes from `deid.memory`.

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

A Patient ID is unique only within the issuer that gave it, so the archive
tells subjects apart by their **subject identity**: the Patient ID together with
its Issuer of Patient ID (0010,0021) and the identifying values of the Issuer of
Patient ID Qualifiers Sequence (0010,0024). The default subject label, the date
shift, a `hash` or `lookup` rule on the Patient ID, and the check against
deleted subjects all key on it. Give the issuer as the files give it:

```
nopacs deid code lab1 --patient-id - --issuer HOSPITAL_A
nopacs deid code lab1 --patient-id - --issuer-qualifier UniversalEntityID=2.16.840.1.113883.3.1 \
  --issuer-qualifier UniversalEntityIDType=ISO
nopacs deid code lab1 --patient-id - --namespace site-b   # a Patient ID that came with no issuer
```

A file with no issuer takes its receiver's namespace (`receiver add --namespace`)
in the issuer's place; each receiver's subject label is answered under its own
namespace, while `--namespace` chooses the one the keyed hash and lookup codes
are answered under. A Patient ID with no issuer under the default namespace
keeps the label, shift and codes of its value alone. A site's own naming rule
that writes `{#PatientID}` keeps hashing the Patient ID alone.

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
an LO value's). For a table a `lookup` rule on the Patient ID reads, a third
column `issuer` and a fourth `namespace` store each entry by its subject
identity, which is what the rule meets in a file from that issuer (or with no
issuer, through a receiver of that namespace); an entry with both empty is
stored by its value alone:

```
printf 'value,code,issuer,namespace\n98890234,SUBJ001,HOSPITAL_A,\n98890234,SUBJ002,,site-b\n' > cohort.csv
```
 An import is all or nothing, at most 10000 entries: one value
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

