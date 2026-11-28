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
  file was received in, by the same rule of words. A value of unknown kind
  (OB, UN, private) too long to decode whole (over 256 KiB) is decoded a
  piece at a time in the file's character set, the state of its ISO 2022
  escapes carried from piece to piece, so a Greek name in Title Case in ISO
  8859-7, or a name an escape or a shift splits, is found in it as in a
  short value, as a term and as a value the profile removed. A standard
  element the data dictionary gives binary VRs only (an ICC profile, a LUT,
  pixel, overlay and waveform data) is not searched for the terms, whatever
  VR it was sent with, since names of four letters occur by chance in a few
  hundred kilobytes of random bytes. An encapsulated document (0042,0011)
  is read by its MIME Type (0042,0012): one of text (`text/*`, such as a
  CDA's `text/XML`, `application/xml`, `application/json`, or a type ending
  in `+xml` or `+json`) is searched as text, and a term in it holds the
  file; any other (a PDF, whose text is compressed) cannot be searched.
  So under a profile with text lists, **every file that keeps an
  encapsulated document that is not text, a PDF report or one with no
  MIME Type, is held** as `residual_value` with the kind
  `list_unsearchable` and the element's path, whatever the document
  holds. A site that receives PDF reports removes the document, or
  replaces it with a dummy of two zero bytes (which keeps the Type 1
  element present), by a rule:

  ```
  {"name": "drop-pdf", "selector": {"keyword": "EncapsulatedDocument"},
   "action": {"dummy": {}}, "reason": "a PDF cannot be searched for the lists' terms"}
  ```

  (`{"remove": {}}` in place of `{"dummy": {}}` removes it), or receives
  such files with a profile without lists. Everything else is searched: a
  standard text element sent as OB (the dictionary says it is text), a
  value sent as UN, a private value whatever its VR, and a value of a tag
  the dictionary does not know.
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

### A tenant's base profile, and the effective profile of a receiver

A tenant may name a base, a profile version every receiver's profile must be at
least as strict as: `nopacs setting set deid.base-profile floor@1 --tenant lab1`
(the tenant's admin; a masked key never sets it). The actions are ordered by
strictness: keep, then hash, shift and generalize, then replace and dummy, then
empty, then remove, then quarantine. A rule that lets through more of an element
than the base decides for it is refused, with every such rule as a detail of the
error (`loosens_lower_layer`; `incomparable_actions` when a rule and the base's
have the same strictness and differ, such as a hash and a shift). It is judged
where a profile is pointed at: when the base is set, against every receiver that
de-identifies and every archive rule's and destination's profile of the rule set
each runs; when a receiver is added or its profile changed; when a rule set
that names profiles is created with activation or activated; and when a
quarantined file is released through a named profile. A later rule of a profile
for an element replaces its earlier one (an option after the basic profile), and
only the one that decides is judged. Pixel rules are not judged as layers.
An image is decided by the base's pixel rules when one matches it (the last that
does), and by the receiver's own only when no pixel rule of the base matches, so
a receiver's catch-all rule never replaces what the base does to an image.

`nopacs profile describe --effective lab1 scanner1` prints the receiver's profile
stacked over the base: one row for each selector with the action that decides
it, the layer it came from (`receiver` for what the receiver's profile names,
`tenant` for what only the base names) and that layer's profile version.

An element both layers decide is decided by the stricter of the two decisions,
each made as the layer would alone: a base's rule for one tag does not weaken a
receiver's rule for a whole VR, and the other way round.

The effective profile is what runs. A reception de-identifies a file by the
receiver's profile (or the archive rule's) stacked over the base, read for each
file, so an element the base decides and the receiver's profile does not name
is decided by the base, and a base set or changed is taken by the next file
received. A trial or an explanation as a named receiver runs the same stack.
Each instance's metadata line records the base beside the profile
(`base_profile`; the profile stays the receiver's, so the instance still has
its profile provenance and may be sent). If a base can no longer be stacked
(a profile of the receiver loosens it, or the base is gone) the file is held
as `deid_failed`, cause `profile_unusable`, never archived with less. Files
archived before a base was set are not changed by it. Use `deid coverage` below
to see what no rule named.

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


## See what no rule named: the coverage report

```
nopacs deid coverage lab1 --refresh
nopacs deid coverage lab1 --json
```

A profile decides each element a rule names; every other element is decided by
a default (a private element is removed, any other kept or removed as the
profile's `unnamed` says). `deid coverage` lists those: every standard element
and every private element, by its private creator and its element within the
creator's block, that files archived by a receiver that de-identifies held as
received and no rule named, with its VR as the file gave it, how many instances
held it and how many times in all. It is where to look for a vendor's private
elements. It holds no value of any file. A private creator is a vendor's name
written in the file; one that is not printable ASCII of at most 64 bytes, or
that holds a value the profile removed from the file or a term of its text
lists, is shown as `(withheld)` and its elements are counted together. A key
that is masked may read it. A name written as a private creator is
withheld only when the part of the name that matches is at least three
characters long, so a very short name used as a creator may be shown.

The report is built by a job of the kind `deid_coverage`, made by `--refresh`
(operator on the tenant), which reads each scan's metadata file once, line by
line, and adds up what each line records of its unnamed elements, a page of
scans at a time in the tenant's database: a restart goes on after the last
page, and no scan is counted twice. It reads no instance's file and writes no
storage. A scan whose metadata is missing or not in its form is counted skipped
and the report is marked partial. A line records the elements only when it was
written by a build that does, for a file a receiver de-identified; an instance
archived as received, or before that, is counted as read but not as recorded.
A line lists at most 1,024 distinct elements; the occurrences past it are
counted, not listed. A report lists at most 100,000 distinct elements. Only the
latest report is kept: a new job's end replaces it. Without `--refresh` the
command shows the stored report and says when there is none.

## Run a newer version over the archive

A profile version that takes more from a file than the one that archived it
(a newer, stricter version) can be run over data already archived, as a job of
the kind `deid` in the mode `rerun`:

```sh
nopacs deid rerun lab1 sub-0017 --profile research@5 --reason "the site now removes KVP" --preview --wait
nopacs deid rerun lab1 sub-0017 --profile research@5 --reason "the site now removes KVP" --wait
```

It needs the admin role on the tenant and is refused to a masked key. When the
tenant has a base profile, a version that does not stack over it is refused when
the job is made (`loosens_lower_layer`), and the files are judged and the new
revisions made by the version stacked over the base, as a reception runs it;
each new revision's line records the base in force. A target
is a subject, a session or a scan; with none it is the whole tenant. `--reason`
is recorded in the audit log with the request. `--preview` writes nothing: its
tallies count what would change, by element and action
(`rerun|element|00180060|remove`), the files left alone (`rerun|unchanged`) and
the files that would be held, with no value.

The archived file is the only original there is, and what the version that made
it decided is already done in it (a hash, a remapped UID, a shifted date), so a
re-run does not run the newer version whole. It does, to each archived file,
what the newer version decides otherwise than the older one and only where it
lets through less: an element it removes, empties, replaces by a constant,
generalizes or cleans, or adds. Each file that changes becomes a new revision
through the path `correction edit` uses: the superseded revision stays on every
copy until the purge, `correction undo` gives it back before then (the file's
profile version and the scan's catalog values with it), the scan's metadata and
checksums name the new revision, and the new revision's method record
(De-identification Method, 0012,0063) names the newer version. The check after
runs over each new revision before it is written, searching for the values the
newer version took out and for its text lists. A file the newer version changes
nothing in is skipped and not rewritten. An instance archived as received
passes with a note: a profile run over it is a reception's work.

A file is not written, and `job failures` lists it with a fixed detail, when:

- the check after holds it (`residual_value/...`, `deid_failed/...`);
- the newer version would hash, remap, shift or look up an element the older one
  left (`deid_failed/rerun_needs_original`): the value would have to be made
  from the original;
- the newer version changes an element the catalog is built on, such as a UID,
  the study date, the character set or how the pixel data is laid out
  (`deid_failed/rerun_catalog_element`);
- the file's character set would not be kept by a rewrite
  (`deid_failed/rerun_charset`), or the newer version has pixel rules
  (`deid_failed/rerun_pixel_rules`);
- the newer version lets through more than the older one, or cannot be compared
  with it, by the layer rule of [the tenant's base profile](#a-tenants-base-profile-and-the-effective-profile-of-a-receiver)
  (`loosens_lower_layer`, `incomparable_actions`): a looser version would need the
  original, which de-identified data does not keep;
- a hold covers the instance (`held_by_hold`).

Each instance is its own correction, in the name of the key that made the job
(which is checked again before each one), so holds, the overlap with other
corrections and the purge apply as to an edit, and the job goes on after a
restart: an instance already at the newer version is met again as unchanged.
A job stops being served when its key is revoked or loses the admin role: the
remaining instances are refused as `key_refused`.

### Try a version over a receiver's last receptions before pointing it there

`receiver set --profile` needs the new version tried first, whether the receiver runs another version or archives as received:

```sh
nopacs receiver try-profile lab1 scanner1 --profile research@5
nopacs receiver set lab1 scanner1 --profile research@5
```

The trial runs the version over the first archived file of each scan of the
receiver's newest sessions (the setting `deid.change-trial-sessions`, 20 by
default, 1 to 200), as `deid rerun --preview` does, and records how many files
it would change and hold. It answers the counts, each element it changes with
its action and in how many files, and why files would be held; no value of any
file. The command that points the receiver is refused when the version was
never tried for that receiver, when its trial read no file (a receiver that
has received nothing shows nothing), or when its trial found files it would
hold. `--force` with `--force-reason` makes the change anyway; the audit entry
of the change says it was forced, why, and what the trial had found. The same
gate stands in front of the other ways to run a version on a receiver: making
a rule set active (`CreateRuleSet` with `activate`) or activating one
(`ActivateRuleSet`) when an archive rule names a profile version the receiver
does not already run needs each such version tried, or `force` with a reason,
audited alike. A change of the tenant's base profile (`deid.base-profile`) is
not gated by trials, since a base applies to every receiver: it is checked
for strictness against every receiver and rule set, and audited. The trial reads
one file of each scan, not every instance: a file of the scan that differs
from the first is found by `deid rerun --preview` over the whole target.
