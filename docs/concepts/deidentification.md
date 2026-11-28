# De-identification

[Documentation index](../README.md) · [The security model](security.md) · [Profiles, trials and lookup tables](../use/profiles.md) · [A reception, end to end](../use/receiving.md)

**De-identification** removes or replaces what identifies a person in a DICOM file:
names, identifiers, dates, UIDs and, where it can be found, text burned into the
pixels. NoPACs does it at the door: a receiver de-identifies every file before any
byte of it reaches a storage, then reads the result again for the values it
removed, and holds the file in quarantine when it cannot show they are gone. It
*fails closed*: nothing is archived de-identified that was not checked.

What a person writes is a **profile**: a JSON document made of *bases* (profiles the
server ships, such as `annex-e/basic:2026d`, the Basic Application Level
Confidentiality Profile of PS3.15 Annex E) and the person's own *rules*. A profile is
stored under a name; each document stored makes the next **version**, and a version
never changes, so a file's record says exactly which version made it. A receiver
names the version it uses (`--profile research@1`).

The steps, in order: [make a profile and try it](../use/profiles.md) on sample files
with `nopacs profile try` (nothing is stored, and the run uses keys of its own),
point a receiver at the version, and later check the archive against it
(`nopacs deid check`). Until you have tried a profile on test data, send only
synthetic or public data.

What it cannot do is as important: text burned into pixels is found only for the
cases a rule or the detector covers, and an image in a syntax whose segments are not
searched is held; the section below lists which. A site reviews what a profile leaves
before it relies on it.

## What a receiver does with what it receives

A receiver either de-identifies every file with a version of one of the
tenant's de-identification profiles, or archives it as received. A tenant made
now allows only the first: a receiver that archives as received, and so puts
the names and identifiers the scanner sent into the archive, is refused until
an admin on the installation sets `receiver.allow-as-received` to `true` on the
tenant (as above, for test data). A tenant that had receivers before receivers
could de-identify was given `true` when its database was upgraded, so its
receivers go on as they did. See [Settings](../operate/settings.md).

A receiver that de-identifies names its profile version (`"profile": {"name":
"research", "version": 1}` in `AddReceiver` or `SetReceiver`, mask
`deidentification`). Every file is de-identified before any byte of it reaches
the storage, checked again by a separate read for the identifying values the
profile removed or replaced (names, identifiers, dates and UIDs; not every
removed value: a number or a description a profile removes is not searched
for, though a name typed into it is, through the name; an identifier made of
words alone, such as an institution's or a station's name, with no digit and
in a script written with spaces, is found only as a whole word, `_` counted as
part of a word, so "TOSHIBA" is not found in a kept maker's name
"TOSHIBA_MEC"; every other identifier, and every one that identifies the
patient (Patient ID, Accession Number, issuers and the like), is found
wherever it stands, glued to other text included), and quarantined
(`deid_failed`, `residual_value`, `profile_refused`, `burned_in_risk`) when it
cannot be; the storage, the catalog and every list then hold only what the
profile leaves, and UIDs remapped by the tenant's key. A file held at such a
receiver is released only through a receiver that de-identifies (a release
through one that archives as received is refused, `FAILED_PRECONDITION`), and
one whose receiver was changed to archive as received after its release is
held again (`deid_failed`, cause `no_profile`): it is never archived as
received. A release may name the profile version it is de-identified with
(`nopacs quarantine release lab1 q_7Zr2 --profile research@2`, admin; the
version must pass the same checks as a receiver's), in place of the one its
route or receiver names, for that reception's files.
An image whose header says its pixels may carry burned-in text (Burned In
Annotation `YES`; or not `NO` on a secondary capture, an ultrasound, a visible
light (endoscopy, microscopy, photography) or ophthalmic image, or a modality
such as XA, RF, OP, SM, PX or IO; or a whole slide's label or overview image) is
held as `burned_in_risk` unless one of the profile's pixel rules matches it.
Radiographs (DX, CR) are not held by kind: their markers are rarely the person,
and a profile writes a pixel rule for a device known to stamp identity. An image
with only Float or Double Float Pixel Data is not judged by the gate. Pixel data
inside a sequence item, and the Icon Image Sequence, are always removed. A pixel
rule: `redact` fills rectangles in every frame
(the image is then written uncompressed), `detect` fills what the text detector
finds (opt-in, and slow: every frame is decoded and searched), `redact_by_template`
fills the rectangles the tenant keeps for the device and image size
([pixel templates](../use/pixel-templates.md); an image at risk with no template
is held as `no_pixel_template`), `accept` archives it as it is, with a reason. Comment and application segments of JPEG
pixel data, and comment segments of JPEG 2000 and HTJ2K pixel data (in the main
header and in every tile-part header, the tile-part's length and its TLM entry
lowered to match) and the main header's CPF and CRG segments, are always removed, the image data copied byte for byte; a
frame then holding any segment beyond those a decoder needs at their defined
length, a marker the walk does not know, a JP2 file instead of a bare
codestream, or anything but padding after its end, is held
(`pixel_text_unsearched`), whatever the profile says. An image left in a syntax
whose segments are not searched (JPEG 2000 Part 2, MPEG-2, H.264, HEVC, JPEG
XL, Deflated Image Frame Compression) is held unless the profile says
`"unsearchedSegments": "UNSEARCHED_SEGMENTS_KEEP"`. Work on pixels is bounded by `deid.pixel-limit`
([Settings](../operate/settings.md)). A profile version that keeps the UIDs a file is
placed by (the Retain UIDs option), and a naming rule that puts a value into a
label unhashed (`{PatientID}` rather than `{#PatientID}`) are refused for such a
receiver. A file whose Study, Series or SOP Instance UID lies under the
standard's own root (`1.2.840.10008.`) is held at such a receiver (`bad_uid`)
and cannot be released into the archive. A file in the Deflated Explicit VR
Little Endian transfer syntax is held at such a receiver (`deid_failed`, cause
`check_unreadable`): the check after reads the bytes as written, which a
deflated file holds compressed, so it cannot show the identifying values are
gone. A STOW-RS receiver that
de-identifies answers each stored instance by the SOP Instance UID it was sent
with and gives no Retrieve URL, neither the instance's nor the study's: the
archive holds it under remapped UIDs, and a URL by them would hand the sender
the mapping. How it works, step by step: [the reception pipeline](../../internal/receive/doc.go).

## Who a subject is

A Patient ID is unique only within the issuer that gave it (PS3.3 C.7.1.1): two
hospitals may give one number to two patients. The archive therefore tells
subjects apart by a **subject identity**, the Patient ID together with its
Issuer of Patient ID (0010,0021) and the values of the Issuer of Patient ID
Qualifiers Sequence (0010,0024) that name an issuer (Universal Entity ID and its
type, Identifier Type Code, the Assigning Facility's identifiers, and the codes
of the Assigning Jurisdiction and Agency; never a Code Meaning, which two senders
may word differently). A file that names no issuer takes the **namespace** of
the receiver it came through in the issuer's place (`receiver add --namespace`,
`Receiver.namespace`); empty, the default, is the tenant's one default
namespace.

The subject identity keys everything that must follow one person: the default
subject label (`sub-{#SubjectIdentity}`), the date shift (one offset per
subject, keyed by the tenant's date key over the identity), a profile's `hash`
or `lookup` of the Patient ID, the forward lookup (`nopacs deid code
--issuer`), the codes a lookup import stores, and the hold of a re-sent file
after its subject was deleted (a re-send from the same issuer is held, the same
Patient ID from another issuer is not). So two issuers of one Patient ID are two
subjects with two date shifts; one issuer is one subject through every
receiver; a Patient ID with no issuer through receivers of two namespaces is two
subjects. A Patient ID with no issuer under the default namespace is keyed by
its value alone, exactly as before issuers were told apart, so its label, its
date shift and its codes do not move.

Every other identity is written as a version byte and its fields each with its
length in front, so no two identities can be read as one whatever bytes their
values hold, and a later change of what an identity holds is a new version. A
naming rule a site writes keeps what it names: `{#PatientID}` hashes the
Patient ID alone. The issuer must arrive as the scanner and the hospital system
write it: an issuer spelled two ways is two issuers, and so two subjects.

Each file's identity is read once, from the file's header, and its date shift
and hashed Patient ID are keyed by it; a value inside the qualifiers sequence is
read under the item's own Specific Character Set when it declares one. A
session's subject label is made from one file of its first reception (or kept
from the catalog for a later arrival), so at a receiver that de-identifies every
file is also checked against its session: the subject label the receiver's rule
makes from the file itself must be the session's subject (or the subject a
committed relabel moved that label to). A file whose own subject is another is
held (`deid_failed`, cause `subject_mismatch`), so one label never holds dates
moved by two offsets, or two patients' files: a study mixing files that carry
the Issuer of Patient ID with files that do not holds the files that disagree
with the one the session was named from, and so does a study whose files carry
two Patient IDs. A release checks the file again and holds it again unless it
now agrees, as a file sent again with the issuer its study's other files carry
does. The same check holds a later arrival of a session through a receiver whose
naming rule or namespace would name it otherwise (its rule changed since, or a
second receiver with another rule or namespace), since its files would be
another subject's.

A file whose identity cannot be read (a Patient ID holding a NUL, an issuer
too long to read) is never keyed by its Patient ID alone: its label cannot be
made (`bad_label`), and a date shift, `hash`, `lookup` or template of its
Patient ID holds it (`deid_failed`, `no_subject`).

What this does not cover:

- A site's rule that writes `{#PatientID}` makes one label of two issuers'
  patients, while the date shift and a `hash` or `lookup` of the Patient ID
  still follow the issuer: two shifts can then sit under one label. Use the
  default subject template, or `{#SubjectIdentity}` in a rule of one's own, to
  keep the label and the shift together.
- A deleted subject's patient sent again **without** an issuer through a
  receiver of another namespace is another subject identity, so the delete
  does not hold it; a re-send from the same issuer, or with no issuer through
  a receiver of the same namespace, is held.
- A lookup table imported by Patient ID alone (no issuer, no namespace) answers
  only files that name no issuer under the default namespace.

