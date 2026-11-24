# Receiving data through the API: a walk-through

[Documentation index](../README.md) · [A reception, end to end](../use/receiving.md) · [Settings](../operate/settings.md) · [Reading the archive](../use/reading.md)

This walks three uses of the API contract in `api/proto/nopacs/v1`, call by
call, with nothing but the contract in hand:

1. setting a tenant up to receive DICOM;
2. following one study from its first file to archived, through `Watch` and
   `GetStatus`, including a second arrival of the same study later;
3. reviewing a quarantined file that differs from the one archived under the
   same SOP Instance UID, and settling it.

The calls are shown as the JSON a plain HTTP client posts with the Connect
protocol: `POST https://<server>/<service>/<method>` with
`Content-Type: application/json` and `Authorization: Bearer nopacs_<id>_<secret>`.
A gRPC or Connect client sends the same messages. Field names are in the JSON
form of protobuf: `lowerCamelCase`, 64-bit integers as strings, durations as
`"300s"`, enum values by name, and a field mask as one string of
`lowerCamelCase` paths separated by commas.

The examples use a tenant named `lab`, a server at `archive.example.org`, and
made-up UIDs under the `2.25` root (PS3.5 B.2). Ids, cursors and hashes are
shortened. Answers are cut to the fields that matter.

## Who may do what

| Service | Methods | Role |
|---|---|---|
| `StorageService` | `AddStorage`, `SetStorage`, `RemoveStorage`, `TestStorage` | admin on the tenant |
| | `GetStorage`, `ListStorages` | reader on the tenant |
| `ReceiverService` | `AddReceiver`, `SetReceiver`, `RemoveReceiver` | admin on the tenant |
| | `GetReceiver`, `ListReceivers` | reader on the tenant |
| `ReceptionService` | `RetryReception`, `DiscardReception`; `ListReceptions` filtered by Study Instance UID | operator on the tenant |
| | `ListReceptions`, `GetReception` | reader on the tenant |
| `QuarantineService` | `ReleaseQuarantinedFile`, `RejectQuarantinedFile` | operator on the tenant |
| | `GetQuarantinedFile` with its header; a list filtered by UID | operator on the tenant |
| | `ListQuarantinedFiles`, `GetQuarantinedFile` | reader on the tenant |
| `MonitorService` | `GetStatus`, `Watch` on one tenant | reader on the tenant |
| | `GetStatus`, `Watch` on every tenant | operator on the installation |

A request is checked in a fixed order: the key first (`UNAUTHENTICATED`), then
the role (`PERMISSION_DENIED`), then the request itself (`INVALID_ARGUMENT`).
A caller with no grant on a tenant gets the same `PERMISSION_DENIED` whether
the tenant exists or not. A request that is well formed but names something
in a state that forbids it (a port the tenant was not allotted, a storage
that holds files) is answered `FAILED_PRECONDITION`.

## Use 1: setting a tenant up to receive

The tenant `lab` exists. An admin on the installation prepares what only the
installation may decide (the settings in [Settings](../operate/settings.md)); then an admin
on the tenant does the rest.

### What the installation allows

```
POST /nopacs.v1.SettingsService/SetSetting
{"setting": {"name": "storage.directory-roots", "scope": {"installation": {}},
             "value": "fast=/srv/archive"}}

POST /nopacs.v1.SettingsService/SetSetting
{"setting": {"name": "receiver.ports", "scope": {"tenant": "lab"},
             "value": "11112,11120-11129"}}

POST /nopacs.v1.SettingsService/SetSetting
{"setting": {"name": "receiver.allow-as-received", "scope": {"tenant": "lab"},
             "value": "true"}}
```

The third lets the receivers below archive what they receive as received,
which a tenant made now refuses (`FAILED_PRECONDITION`, naming the setting):
this walk-through uses test data. A receiver for real data de-identifies
instead; see "De-identify instead" below.

The second is set on the tenant, but only an admin on the installation may
set it: the tenant's own admin is answered `PERMISSION_DENIED`. A range that
overlaps another tenant's allotment is answered `FAILED_PRECONDITION`, so two
tenants' receivers never contend for a port.

### Add a storage and test it

```
POST /nopacs.v1.StorageService/AddStorage
{"tenant": "lab",
 "storage": {"name": "main", "directory": {"root": "fast", "path": "mri"}}}
```

The directory is `/srv/archive/lab/mri`: the root's path, the tenant's name,
then the path given. A tenant can only name a root and a path below its own
directory, so it cannot reach another tenant's files or the server's. The
server makes the directory, which must be absent or empty; a path that
passes through a symbolic link is refused. The answer shows `root` and `path`,
never the root's own location.

```
POST /nopacs.v1.StorageService/TestStorage
{"tenant": "lab", "name": "main"}

{"passed": true,
 "results": [{"step": "STORAGE_TEST_STEP_WRITE",   "outcome": "STORAGE_TEST_OUTCOME_PASSED", "duration": "0.004s"},
             {"step": "STORAGE_TEST_STEP_READ",    "outcome": "STORAGE_TEST_OUTCOME_PASSED", "duration": "0.001s"},
             {"step": "STORAGE_TEST_STEP_COMPARE", "outcome": "STORAGE_TEST_OUTCOME_PASSED", "duration": "0s"},
             {"step": "STORAGE_TEST_STEP_REMOVE",  "outcome": "STORAGE_TEST_OUTCOME_PASSED", "duration": "0.001s"}]}
```

A storage that fails is still a successful call: `passed` is false, and the
failed step's `fault` is one of a fixed list (`STORAGE_FAULT_UNREACHABLE`,
`STORAGE_FAULT_TLS_FAILED`, `STORAGE_FAULT_REFUSED`, `STORAGE_FAULT_NOT_FOUND`,
`STORAGE_FAULT_WRONG_ANSWER`, `STORAGE_FAULT_ADDRESS_NOT_ALLOWED`). The
store's own words are never answered.

An S3 bucket is added with `s3` in place of `directory`, at an endpoint
`storage.s3-endpoints` lists. Its secret access key is sent once and never
answered again; a read shows only `secretAccessKeySet: true`. Credentials are
rotated together, both named in the mask:

```
POST /nopacs.v1.StorageService/SetStorage
{"tenant": "lab",
 "storage": {"name": "cold", "s3": {"accessKeyId": "AKIA...NEW", "secretAccessKey": "..."}},
 "updateMask": "s3.accessKeyId,s3.secretAccessKey"}
```

A mask that names only one of the two is invalid. A mask that names neither
keeps the secret held.

### Add a receiver

The scanner calls itself `PRISMA1` and sits on `10.20.0.0/24`:

```
POST /nopacs.v1.ReceiverService/AddReceiver
{"tenant": "lab",
 "receiver": {"name": "scanner1",
              "enabled": true,
              "storage": "main",
              "dimse": {"port": 11112,
                        "aeTitle": "NOPACS",
                        "allowedCallers": {"rules": [{"callingAeTitle": "PRISMA1",
                                                      "addressRange": "10.20.0.0/24"}]}}}}
```

What the answer shows:

- `quietPeriod: "60s"`, `maxOpenTime: "3600s"` and `maxFileBytes: "4294967296"`:
  the defaults, filled in and stored as the receiver's own values.
- `naming` absent: the default rule, subject `sub-{#PatientID}`, session
  `ses-{#StudyInstanceUID}`, scan `scan-{#SeriesInstanceUID}`. `{#...}` is a
  keyed hash of the value under a secret the tenant's server keeps, so no
  label (and so no list, event or directory name on the storage) carries what
  the scanner sent. A file's own name on the storage is its SOP Instance UID,
  and the file holds everything the scanner sent: this receiver archives data
  as received (a receiver that de-identifies remaps those UIDs; below).
  A site that writes `{PatientID}` into its own rule (refused for a receiver
  that de-identifies) gets readable labels
  that are exactly as identifying as the patient's identifier.
- `dimse.sopClasses` and `dimse.transferSyntaxes` empty: the server's default
  sets, every Storage SOP Class of PS3.4 Table B.5-1 and every transfer syntax
  of PS3.5 Annex A that the server knows.
- `health`: `RECEIVER_STATE_STARTING` or already `RECEIVER_STATE_UP`.

Things that are easy to get wrong, and what the contract does about them:

- `enabled` left out is false: the receiver is stored but does not listen.
- `allowedCallers` left out admits nobody. Admitting every caller takes
  `"anyCaller": true`, and `"anyCaller": false` is refused, so it cannot be
  set by accident.
- A rule with neither an AE title nor an address range is refused: it would
  admit everyone.
- A port outside `receiver.ports` is answered `FAILED_PRECONDITION`. A port
  inside it that another program on the server holds is not refused; the
  receiver is down with `RECEIVER_DOWN_REASON_ADDRESS_IN_USE`, and its
  `detail` is a fixed phrase, never the operating system's text.

### De-identify instead

A receiver that names a version of one of the tenant's profiles de-identifies
every file before any byte of it reaches the storage, and needs no setting:

```
POST /nopacs.v1.ReceiverService/AddReceiver
{"tenant": "lab",
 "receiver": {"name": "scanner2", "enabled": true, "storage": "main",
              "dimse": {"port": 11120, "aeTitle": "NOPACS",
                        "allowedCallers": {"rules": [{"callingAeTitle": "PRISMA1"}]}},
              "profile": {"name": "research", "version": 1}}}
```

The answer carries `"profile"`; a receiver added without either member of the
oneof is answered `"asReceived": {}`. `SetReceiver` changes the choice with the
mask `deidentification`. Refused as `FAILED_PRECONDITION`, each with a message
that says why: a version the tenant does not have; a version that keeps the UIDs a file is
placed by (the Retain UIDs option); and a naming rule with an unhashed
placeholder such as `{PatientID}`, since labels are kept before any file is
de-identified. What then happens to each file, and the four quarantine
reasons it may end with (`deid_failed`, `residual_value`, `profile_refused`,
`burned_in_risk`),
are in [the reception pipeline](../../internal/receive/doc.go) and
`quarantine.proto`.

### Add a stow receiver

A program that stores over HTTP rather than DICOM uses the tenant's STOW-RS
service, `POST /tenants/lab/dicom-web/studies` (PS3.18 10.5). It is a
receiver like any other, of type `stow`, and takes requests only from the
keys it lists; each key must also hold writer or more on the tenant:

```
POST /nopacs.v1.ReceiverService/AddReceiver
{"tenant": "lab",
 "receiver": {"name": "web",
              "enabled": true,
              "storage": "main",
              "stow": {"keyIds": ["7QmXc2LpZ4"]}}}
```

What the answer shows, beside the defaults every receiver has:

- `stow.maxRequestBytes: "17179869184"`: the default largest request, 16 GiB;
  each part is also bounded by `maxFileBytes`.
- No port and no AE title: the service is the API's own address.

Things that are easy to get wrong:

- `keyIds` left out takes no key: the receiver is closed until one is listed.
  The local operator holds no key and is refused too.
- A key rotated from a listed one (`AccessService.RotateKey`) is taken
  without changing the receiver, during the overlap and after it, up to 16
  rotations back from the key listed.
- A key is listed by one stow receiver of a tenant at most; a second is a
  precondition failure, so which receiver (and so which storage and naming
  rule) a request goes to is never a choice.
- The body is `multipart/related; type="application/dicom"`, one DICOM file a
  part. Metadata sent as `application/dicom+json` with its bulk data in other
  parts is refused with 415 in this version.
- The answer is written once every part is synced on the server's disk: 200
  when all were stored, 202 when some were, 409 when none was, each refused
  instance in the Failed SOP Sequence with its reason (A900H of another study
  than the path names, A700H too large or the archive full, C000H not a file
  whose UIDs can be read). What the pipeline finds later is quarantined after
  the answer, as for a C-STORE answered success: a different file under a UID
  the tenant holds is answered stored, then held as `different_file` (or
  `uid_elsewhere`); a file whose placing UIDs read whole differ from those in
  its first bytes, by which it was checked against the path's study, is held
  as `uid_mismatch`.
- A request holds at most 10,000 parts (413 past it) and each part's header
  at most 16 KiB (400 past it). A body that ends inside a part is 400 and
  that part is not kept. When a request is refused part way, the parts sent
  whole before are kept and archived; the plain-text answer does not list
  them, and a search of the study shows them. A receiver that stops lets a
  request run 30 seconds, then answers it 503 with `Retry-After`.

### Change the quiet period

The scanner pauses for minutes between series, so a reception should wait
five minutes of quiet before it is complete:

```
POST /nopacs.v1.ReceiverService/SetReceiver
{"tenant": "lab",
 "receiver": {"name": "scanner1", "quietPeriod": "300s"},
 "updateMask": "quietPeriod"}
```

Only `quietPeriod` changes. A field of the DIMSE part is named with its
prefix, as `dimse.port`; naming `dimse` whole is refused.

### Check it is up

```
POST /nopacs.v1.MonitorService/GetStatus
{"scope": {"tenant": "lab"}}

{"tenants": [{"tenant": "lab",
              "databaseState": "TENANT_STATE_READY",
              "receivers": [{"receiver": "scanner1",
                             "health": {"state": "RECEIVER_STATE_UP", "since": "..."}}],
              "storages": [{"storage": "main", "usedBytes": "0", "freeBytes": "1834013425664"}],
              "counts": {"incomingBytes": "0", ...},
              "incomingLimitBytes": "0"}],
 "time": "..."}
```

The tenant is ready to receive. A C-ECHO from the scanner's host,
`echoscu -aet PRISMA1 -aec NoPACs archive.example.org 11112` with dcmtk, is
answered. From a calling AE title or an address no rule admits, the
association is rejected.

## Use 2: following one study to archived

A monitoring client wants to see a study arrive and know when it is safe on
the storage, missing nothing in between.

### Start from a status, then watch from its cursor

```
POST /nopacs.v1.MonitorService/GetStatus
{"scope": {"tenant": "lab"}}

{"tenants": [{"tenant": "lab",
              "eventCursor": "c1",
              "counts": {"subjects": "41", "sessions": "57", "scans": "612",
                         "instances": "88310", "quarantinedFiles": "0"}, ...}]}
```

The counts and the cursor come from one snapshot of the tenant's database:
every event after `c1` is a change the counts do not hold yet, and none
before it is. The client keeps the counts and opens a stream:

```
POST /nopacs.v1.MonitorService/Watch
{"scope": {"tenant": "lab"}, "cursor": "c1"}
```

`Watch` is a server stream. Its first message is a keep-alive, sent at once;
after that a keep-alive comes whenever 15 seconds pass without an event. The
client stores the cursor of every message it receives. If the stream drops,
it calls `Watch` again with the last cursor and loses nothing, as long as that
cursor is younger than `events.retention`. An older cursor is answered
`OUT_OF_RANGE` with a message that begins `CURSOR_EXPIRED`; the client then
calls `GetStatus` again and watches from the new `eventCursor`.

The status and every keep-alive also carry `eventHistory`, the id of the
tenant's event history (`tenantEventHistories` by tenant, with every tenant).
A restore, an import, a load, or a rebuild that replaces the catalog begins a
new history: its first event is `catalog_rebuilt`, no event of the old history
is answered in it, and a cursor of the old history (or one after the newest
event) is answered `OUT_OF_RANGE` with a message that begins
`EVENT_HISTORY_CHANGED`. The client then reads the catalog again, not only the
counts, and watches from the new status's `eventCursor`. A client keys what it
has seen by history and cursor together. A stream opened without a cursor can
end the same way at once, when the new history begins between the moment it
takes the tenant's position and its first read; the client does the same. The
old history's events stay in the tenant's database, never answered, until the
next pass of retention (at most an hour) removes them.

A client that watches every tenant (the installation as scope) keeps the
`tenantCursors` of the last keep-alive, which names every tenant the stream
follows, and resumes with them and `"resume": true`: a tenant they name goes
on from its cursor, and a tenant they do not name is one the client has not
seen yet (created after that keep-alive) and starts from its first event, so
nothing is lost between two streams. A keep-alive also names, in
`unansweredTenants`, the tenants whose database is not answering; the stream
goes on with the others and picks them up where they stopped.

The stream has no deadline unless the client sets one. The server keeps no
queue for the client: a client that reads slowly only falls behind.

### The study arrives

The scanner sends a study of three series. An accepted association makes no
event; the first event is the reception's:

```
{"event": {"cursor": "c2", "type": "EVENT_TYPE_RECEPTION_STEP",
           "receptionStep": {"reception": "r-7f3a", "receiver": "scanner1",
             "arrival": 1, "step": "RECEPTION_STEP_RECEIVING", "counts": {}}}}
```

It is written when the reception opens, before its first file is recorded,
so its counts are all zero, and it has no `previousStep`: receiving is the
reception's first step.

The event carries the reception's id, never the Study Instance UID or another
value the scanner sent. The client reads the reception to know which study
it is:

```
POST /nopacs.v1.ReceptionService/GetReception
{"tenant": "lab", "id": "r-7f3a"}

{"reception": {"id": "r-7f3a", "receiver": "scanner1",
               "studyInstanceUid": "2.25.1234567890123456789",
               "arrival": 1, "step": "RECEPTION_STEP_RECEIVING",
               "counts": {"filesReceived": "212", "bytesReceived": "112074304"},
               "associations": "1", "callingAeTitles": ["PRISMA1"],
               "startTime": "...", "lastFileTime": "..."},
 "arrivals": [{"id": "r-7f3a", "arrival": 1, "step": "RECEPTION_STEP_RECEIVING"}]}
```

`studyInstanceUid` is as received. While files arrive there is no event per
file. Progress during receiving is read from `GetReception` (its counts) or
from `GetStatus` (the tenant's `throughput` and `receptionsInProgress`).

### Complete, identified, archived, verified, done

Five minutes after the last file (the quiet period set in use 1), the
reception moves through its steps, one event each, each with the totals so
far and the step it left, so a client that keeps the receptions in progress
by step from `GetStatus` moves one reception from `previousStep` to `step`
on each event, whatever step a reception fails from:

```
{"event": {"cursor": "c3", "receptionStep": {"reception": "r-7f3a", "arrival": 1,
           "step": "RECEPTION_STEP_COMPLETE", "previousStep": "RECEPTION_STEP_RECEIVING",
           "counts": {"filesReceived": "480", "bytesReceived": "253771776"}}}}
{"event": {"cursor": "c4", "receptionStep": {"reception": "r-7f3a", "arrival": 1,
           "step": "RECEPTION_STEP_IDENTIFIED", "previousStep": "RECEPTION_STEP_COMPLETE",
           "subjectLabel": "sub-k2m7q4xwacb3ndfa", "sessionLabel": "ses-pzt2vz6yh4jr5wem", ...}}}
{"event": {"cursor": "c5", "receptionStep": {..., "step": "RECEPTION_STEP_ARCHIVED", "previousStep": "RECEPTION_STEP_IDENTIFIED",
           "counts": {"filesReceived": "480", "filesArchived": "480", ...}}}}
{"event": {"cursor": "c6", "receptionStep": {..., "step": "RECEPTION_STEP_VERIFIED", "previousStep": "RECEPTION_STEP_ARCHIVED", ...}}}
{"event": {"cursor": "c7", "receptionStep": {..., "step": "RECEPTION_STEP_DONE", "previousStep": "RECEPTION_STEP_VERIFIED",
           "counts": {"filesReceived": "480", "bytesReceived": "253771776",
                      "filesArchived": "480", "bytesArchived": "253771776"}}}}
```

`RECEPTION_STEP_VERIFIED` is the point the client was waiting for: every
archived file was read back from the storage and its SHA-256 matched the one
computed when it was received. At `RECEPTION_STEP_DONE`,
`filesReceived = filesArchived + filesDuplicate + filesQuarantined`.

The scans it archived:

```
POST /nopacs.v1.ReceptionService/GetReception
{"tenant": "lab", "id": "r-7f3a"}

{"reception": {..., "step": "RECEPTION_STEP_DONE", "storage": "main"},
 "scans": [{"label": "scan-a4c7...", "seriesInstanceUid": "2.25.111", "files": "1"},
           {"label": "scan-f2r6...", "seriesInstanceUid": "2.25.222", "files": "176"},
           {"label": "scan-w7k3...", "seriesInstanceUid": "2.25.333", "files": "303"}]}
```

`files` counts instances. A new `GetStatus` shows `sessions` up by one,
`scans` by three and `instances` by 480, and an event replayed at the
boundary counts nothing twice, because a reception's events carry totals,
not changes.

Had the storage stopped answering, the reception would have ended in
`RECEPTION_STEP_FAILED` with its files still in the server's incoming area.
Once the storage answers again, `RetryReception` (operator) runs them in a
new reception linked by `retryOf`; `DiscardReception`, with a reason, removes
them instead.

### A second arrival, later

Forty minutes later the scanner sends one more series of the same study. The
first reception has ended, so these files start a new reception: the second
arrival of the same session.

```
{"event": {"cursor": "c11", "receptionStep": {"reception": "r-80c1", "receiver": "scanner1",
           "arrival": 2, "step": "RECEPTION_STEP_RECEIVING", ...}}}
...
{"event": {"cursor": "c13", "receptionStep": {"reception": "r-80c1", "arrival": 2,
           "step": "RECEPTION_STEP_IDENTIFIED",
           "subjectLabel": "sub-k2m7q4xwacb3ndfa", "sessionLabel": "ses-pzt2vz6yh4jr5wem", ...}}}
...
{"event": {"cursor": "c16", "receptionStep": {"reception": "r-80c1", "arrival": 2,
           "step": "RECEPTION_STEP_DONE",
           "counts": {"filesReceived": "96", "filesArchived": "96", ...}}}}
```

The session label is the same as the first arrival's: a file is matched to its
session by its Study Instance UID before any naming rule runs, so a late file
always lands where the first ones did, on the same storage, even if the
receiver has been pointed at another storage since. `arrival: 2` tells a
consumer that this is an addition, and the counts are this arrival's own, so
it acts on the 96 new files only.

```
POST /nopacs.v1.ReceptionService/GetReception
{"tenant": "lab", "id": "r-80c1"}

{"reception": {"id": "r-80c1", "arrival": 2, "step": "RECEPTION_STEP_DONE", ...},
 "scans": [{"label": "scan-m5d2...", "seriesInstanceUid": "2.25.444", "files": "96"}],
 "arrivals": [{"id": "r-7f3a", "arrival": 1, "step": "RECEPTION_STEP_DONE"},
              {"id": "r-80c1", "arrival": 2, "step": "RECEPTION_STEP_DONE"}]}
```

Every arrival of a study can also be listed with
`ListReceptions {"tenant": "lab", "studyInstanceUid": "2.25.1234567890123456789"}`, which needs operator on the tenant, as any search by an identifier the caller already holds does.

## Use 3: reviewing a different file under an archived UID

The scanner's operator corrected an attribute and sent one image again under
the same SOP Instance UID. The archive already holds that UID with other
bytes, so it keeps the archived file, answers the sender success, and holds
the new file in quarantine.

### See it

On the stream:

```
{"event": {"cursor": "c25", "type": "EVENT_TYPE_FILE_QUARANTINED",
           "fileQuarantined": {"file": "q-19d2", "reception": "r-8a77", "receiver": "scanner1",
             "reason": "QUARANTINE_REASON_DIFFERENT_FILE", "sizeBytes": "528650",
             "heldFiles": "1", "heldBytes": "528650"}}}
```

`heldFiles` and `heldBytes` are the tenant's quarantine totals after the
event, so a client keeps them from the stream alone. Someone who was not
watching lists what is held:

```
POST /nopacs.v1.QuarantineService/ListQuarantinedFiles
{"tenant": "lab", "state": "QUARANTINE_STATE_HELD"}
```

### Review it

Reading the header takes operator on the tenant; the record alone needs
reader.

```
POST /nopacs.v1.QuarantineService/GetQuarantinedFile
{"tenant": "lab", "id": "q-19d2", "includeHeader": true}

{"file": {"id": "q-19d2",
          "reason": "QUARANTINE_REASON_DIFFERENT_FILE",
          "state": "QUARANTINE_STATE_HELD",
          "receiver": "scanner1", "reception": "r-8a77",
          "caller": {"callingAeTitle": "PRISMA1", "calledAeTitle": "NOPACS",
                     "address": "10.20.0.31:50288"},
          "sizeBytes": "528650",
          "sha256": "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08",
          "keptSha256": "60303ae22b998861bce3b28f33eec1be758a213c86c93c076dbe9f558c11c752",
          "sopClassUid": "1.2.840.10008.5.1.4.1.1.4",
          "sopInstanceUid": "2.25.987654321",
          "studyInstanceUid": "2.25.1234567890123456789",
          "seriesInstanceUid": "2.25.222",
          "transferSyntaxUid": "1.2.840.10008.1.2.1",
          "contentKept": true,
          "quarantineTime": "..."},
 "headerJson": "{\"00100030\":{\"vr\":\"DA\",\"Value\":[\"19800102\"]}, ...}",
 "headerTooLarge": false}
```

The person reviewing it sees why (`reason`), what is held (`sha256`) and the
checksum of the archived file (`keptSha256`), who sent it, and the file's header in the
DICOM JSON model (PS3.18 Annex F). Pixel data and every long value are left
out entirely; a header over 1 MiB is not cut but left out, with
`headerTooLarge` set. The UIDs are as received. The read is recorded in the
tenant's audit log.

The header is read through the tenant's running reception pipeline, which owns the
incoming area the held bytes are in, within the `receiver.header-limit` bound. A
value of exactly 1 KiB or less stays; a longer one, and Pixel Data, are left out. The
answer carries the record alone, with neither `headerJson` nor `headerTooLarge`,
which is what the contract answers for a header that could not be read: a file whose
bytes are not kept (rejected, or lost from the disk), whose header runs past the
bound (`header_too_large`), or a tenant whose pipeline does not run now.
The record's checksums and UIDs are answered in full.

This contract has no call that reads the archived file itself, so the
comparison with the kept file rests on what the person reviewing it knows of
the correction and on the two checksums.

### Settle it

A file quarantined as `QUARANTINE_REASON_DIFFERENT_FILE` is not released: a release
runs it through the pipeline again, and while the tenant holds the archived instance
under the UID, the file would be quarantined again, so a release is refused with
`FAILED_PRECONDITION` for that reason. It is rejected, or, when it is the corrected
file for the archived instance, it replaces that instance with
`CorrectionService.Replace` (`nopacs correction replace`). Replacing an archived
instance deletes archived data, so it is an operation of its own with a recorded
reason and proof of the copy that replaces it ([correcting](../use/correcting.md#replace)).
Here the file is rejected:

```
POST /nopacs.v1.QuarantineService/RejectQuarantinedFile
{"tenant": "lab", "id": "q-19d2", "reason": "re-sent by mistake; the archived file is correct"}

{"file": {"id": "q-19d2", "state": "QUARANTINE_STATE_REJECTED",
          "rejectReason": "re-sent by mistake; the archived file is correct",
          "contentKept": false, "decideTime": "...", ...}}
```

The stream shows the decision:

```
{"event": {"cursor": "c26", "type": "EVENT_TYPE_QUARANTINE_DECIDED",
           "quarantineDecided": {"file": "q-19d2", "state": "QUARANTINE_STATE_REJECTED",
             "heldFiles": "0", "heldBytes": "0"}}}
```

The held bytes are removed for good; the record stays, with its reason, its
checksums and who sent it, and the rejection is in the tenant's audit log
with the caller's key.

What the contract settles about this call:

- Rejecting again answers the record as it stands, with its first reason, so
  a client may retry after a lost answer.
- A released file cannot be rejected: `FAILED_PRECONDITION`.
- The only release mode is `RELEASE_MODE_AS_IS`; any other is refused as
  invalid. For the other reasons a release is how a file is run again once its
  cause is mended (a naming rule fixed, say), through the receiver it came to
  or, if that one was removed, through one the request names:
  `"receiver": "scanner2"`.
- A file released that fails again is quarantined under a new record whose
  `releasedFrom` names the first; that record stays released.
- A release, like a retry of a failed reception, runs through the tenant's
  reception pipeline, which puts the file's bytes back in line in an order a
  crash cannot break. While that pipeline is not running (the tenant's
  database not ready, or the pipeline restarting) the request is refused with
  `UNAVAILABLE` and nothing is recorded; it can be sent again. A rejection is
  recorded at once, and its bytes are removed by the pipeline, at once or at
  its next start.

## What this contract does not do

Said plainly, so a client does not look for it:

- It does not read archived files or their headers back; the catalog,
  metadata and data services do that, and DICOMweb
  ([reading the archive](../use/reading.md)).
- Quarantined files are released or rejected one at a time, by id.
- A receiver's TLS certificate and key are files placed on the server by a
  person with access to it; the API names them and never carries their
  content.
- The UIDs it answers are the ones received. When identifiers are replaced at
  reception, the archive holds remapped UIDs, which these answers do not carry.
