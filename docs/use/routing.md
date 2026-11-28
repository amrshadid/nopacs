# Routing studies with rule sets

[Documentation index](../README.md) · [A reception, end to end](receiving.md) · [De-identification](../concepts/deidentification.md) · [The command line](command-line.md)

## Rule sets

A receiver's routing is a **rule set**: a numbered version, never changed
once made. A change is a new version, and the receiver runs the version it is
pointed at, so the version a reception records always means the rules it was
routed by. A rule set has two phases:

- **Archive rules** run in order over the first file of a new session's first
  reception, as received (the first, by SOP Instance UID, of its first series,
  by Series Instance UID), before any byte of it is de-identified. The first
  whose condition holds chooses the session's storage and, optionally, a
  profile version that de-identifies it in place of the receiver's. No rule
  holds, and the rule set's default decides: the receiver's storage and
  profile, or, with `quarantineUnmatched`, every file held as `unrouted`.
- **Forward rules** all run, over each reception's first file. Each whose
  condition holds forwards the reception to its senders when it reaches done:
  one send job per series the reception brought, or, with a **series filter**,
  one per such series whose first file the filter holds for. A series that an
  earlier arrival of the session brought is never sent again by a later
  reception's rule. A rule may name a
  **destination profile**: each copy is de-identified by it, over the archived
  file, and checked after by the same check a reception runs, before it is
  sent (DIMSE senders only). A destination profile may only tighten what the
  archive's profile did: a rule of it that would keep more of an element is
  refused with `loosens_lower_layer`. The same holds for an archive rule's
  profile over the receiver's: a rule naming a profile that would keep more
  than the receiver's profile removes is refused, when the version is made, when
  it is activated and when the receiver's profile is changed.

The receiver's own `--forward-to` senders still receive every reception.

## Conditions

A condition is written in the condition language a search uses too:

```text
Modality in ("CT", "MR") and SeriesDescription contains "t1"
calling_ae == "SCANNER1" or called_ae prefix "ARCH"
ImageType[2] == "LOCALIZER"
ReferencedImageSequence.ReferencedSOPClassUID present
(0029,"SIEMENS CSA HEADER",10) present
(0018,0050) < 2.5
```

`=` compares text with case and accents folded, `==` exactly; `prefix`,
`like` (`*`, `?`), `contains`, `words`, `matches` (RE2), `in`, ranges and
`present`/`absent`, joined by `and`, `or`, `not`. A condition may read private
elements, whose contents are not known from their tags; values are never shown
without `--show-values`, which a masked key may not use. A field is a keyword, a tag,
a value index, a sequence path (any item), a private tag by its creator, or
the catalog fields `calling_ae` and `called_ae` (absent for a file not
received by DIMSE). **An attribute the file does not hold makes a comparison
false, never an error.** A value longer than 1024 bytes cannot be evaluated: an
archive rule that reads one holds the study as `unrouted` (`route_unusable`),
whatever rules come after it; a forward rule that reads one forwards nothing.
**No condition may read who the patient is**: Patient's Name and ID, other
patient IDs, birth date and time, addresses, telephone numbers and any
attribute whose VR is PN are refused, at any depth.

## Making and running a version

A rule set is a JSON file (the API's `RuleSet`):

```json
{
  "archiveRules": [
    {"name": "site-a", "when": "calling_ae == \"SITE_A\"", "storage": "lab-a"},
    {"name": "mr", "when": "Modality = \"MR\"", "storage": "lab-b",
     "profile": {"name": "strict", "version": 2}}
  ],
  "quarantineUnmatched": true,
  "forwardRules": [
    {"name": "t1-to-ai", "when": "Modality = \"MR\"", "series": "SeriesDescription contains \"t1\"",
     "senders": ["ai"], "destinationProfile": {"name": "ai-copy", "version": 1}}
  ],
  "comment": "split by site; T1 series to the AI service"
}
```

```sh
nopacs route create lab1 scanner1 rules.json --activate
nopacs route get lab1 scanner1                 # the version it runs
nopacs route list lab1 scanner1                # every version, newest first
nopacs route export lab1 scanner1 --to rules.json
nopacs route activate lab1 scanner1 2          # back to version 2; 0 runs none
```

An activation (with `create --activate`, or `activate`) is refused, and a new
version not stored, when a rule names a storage, sender or profile version the
tenant does not have, a storage that is a replica or of the archive tier, a
profile on a receiver that archives as received, or a destination profile that
loosens. A storage or a sender the running version names cannot be removed.
Making and activating need admin, and are refused to a masked key when they
could forward.

## Trying a rule set

- `nopacs route explain lab1 scanner1 --file f.dcm --calling-aet SCANNER1`
  (or `--instance SUBJECT/SESSION/SCAN[/UID]` for an archived instance) shows
  what a version decides, rule by rule, and the comparison that decided each:
  its tag, VR and whether the file holds it. The values it saw are hidden
  unless `--show-values`, which the audit log records and a masked key is
  refused.
- `nopacs route dry-run lab1 scanner1 --version 3 --sessions 200` tries a
  version on the receiver's latest sessions, one metadata file read per scan
  and the session's AE titles, beside the rules that routed them: `same`,
  `changed` or `unknown`. The archive's headers are de-identified, so a
  session whose result depends on an attribute its profile removed or
  replaced is `unknown`, never counted as matching or not. A masked key is
  refused a run that would read a session archived as received.
- `nopacs route hits lab1 scanner1` counts, per rule and version, the
  receptions it routed and the series its forwards sent, from the record
  each reception keeps.
- `nopacs profile try --receiver NAME` applies the receiver's archive rules,
  as before.

## What is kept

- **The archive choice is the session's.** The rule that chose, its version
  and profile, and the AE titles of the first file are kept with the reception
  and with the session; every later arrival goes where the first did. Should
  two receptions of a study be routed apart all the same (two receivers at
  the same moment), the files of the second to archive are held as `unrouted`
  (`session_elsewhere`).
- **Forwards are each reception's own.** A later arrival is forwarded by the
  rules the receiver runs when it is identified, over its own series.
- **A release** naming a storage (`nopacs quarantine release ... --storage`)
  chooses the storage only; the archive rules still choose the profile.
- **Limits.** 64 archive rules and 64 forward rules per version, 16 senders
  per forward rule; a condition at most 16 KiB, 16 levels and 64
  comparisons; a version's conditions at most 256 KiB together; a dry run
  at most 1000 sessions and 20,000 metadata files.
