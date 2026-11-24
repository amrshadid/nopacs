# A reception, end to end

[Documentation index](../README.md) · [Architecture](../develop/architecture.md) · [Receiving through the API](../api/reception-walkthrough.md) · [Reading the archive](reading.md)

A *reception* is the files of one study arriving at one receiver. This page
follows one from a sender's first file to a verified scan, and says how the
parts named in [Architecture](../develop/architecture.md) hand it on. Each part's rules are
in its `doc.go`: [receive](../../internal/receive/doc.go) (the pipeline and what makes
a crash safe), [incoming](../../internal/receive/incoming/doc.go),
[dimse](../../internal/receive/dimse/doc.go), [stow](../../internal/receive/stow/doc.go),
[supervise](../../internal/receive/supervise/doc.go).

## The flow

```mermaid
sequenceDiagram
    participant S as Sender
    participant R as Receiver (dimse or stow)
    participant I as Incoming area
    participant P as Pipeline
    participant D as Tenant database
    participant B as Storage
    participant E as Event feed

    S->>R: C-STORE, or one part of a STOW-RS request
    Note over R: admitted only if the caller is listed
    R->>I: stream the file, then sync it
    R-->>S: success, only now
    R->>P: hand in the file (one entry for every source)
    P->>P: read the header
    alt not DICOM, a UID malformed, a SOP class not taken
        P->>D: hold in quarantine, with its reason and its bytes
    else
        P->>D: record the file as received (in batches)
    end
    Note over P: the receiver's quiet period passes with no new file
    P->>D: complete
    P->>D: identify: labels from the receiver's naming rule
    P->>B: archive: put each file, read it back, write sidecar and manifest
    P->>D: commit the scan to the catalog
    P->>B: verify: check every object the manifest lists
    P->>D: done
    D-->>E: each step's event, written in the same transaction
```

Every step after the first is a leased unit of work held in the tenant's database,
so a step can be run again after a crash and changes nothing more. A file the
receiver acknowledged is never lost and never archived twice; the order of effects
of each step is in [receive](../../internal/receive/doc.go).

## Its states

```mermaid
stateDiagram-v2
    [*] --> receiving
    state "before done" as live {
        receiving --> complete: quiet period passed, or longest open time reached
        complete --> identified: labels made
        identified --> archived: files stored and read back, catalog committed
        archived --> verified: every listed object checked
    }
    verified --> done
    live --> quarantined: nothing of it can go on
    live --> failed: a step failed
    done --> [*]
    quarantined --> [*]
    failed --> [*]
```

Done, quarantined and failed are ends. A file that arrives for a study whose
reception is past receiving does not reopen it: it starts a new reception of the
same study (a later arrival). So does running a quarantined or failed reception
again once its cause is fixed (`nopacs reception retry`), so a reception's history
is never rewritten. A single bad file is quarantined on its own and the reception
goes on with the others. A reception is complete when it is *quiet*, not when it
is whole: DICOM does not say how many files a study has
([core/reception](../../internal/core/reception/doc.go)).

When a reception is identified, a new subject's label comes from the receiver's
naming rule. The default rule's subject is the keyed hash of the file's subject
identity: its Patient ID with its Issuer of Patient ID and that issuer's
qualifiers, since a Patient ID is unique only within its issuer. A file that
names no issuer takes the receiver's namespace (`nopacs receiver add
--namespace site-b`). Use one namespace per population of Patient IDs that came
without an issuer: two receivers whose senders may reuse each other's numbers
get two namespaces, two receivers fed by one hospital system share one. A file
with no issuer under the default namespace is named by its Patient ID alone.

The namespace is fixed once the receiver has received anything (a reception or
a quarantined file names it): `receiver set --namespace` is then refused (exit
5), since the same patient's next study would become a new subject and a
deleted subject's files would no longer be held when sent again. A receiver for
another population is a new receiver. Before its first file the namespace can be
set, or returned to the default with `--namespace ""`. A study whose receiver is
removed before the study has a session is held as `unrouted`, cause
`receiver_removed`, rather than named under another namespace; it is rejected,
or sent again through a receiver that exists. At a receiver that
de-identifies, a file whose own subject label is not its session's subject (one
file of a study carries the issuer and another does not, or two Patient IDs) is
held, `deid_failed`, cause `subject_mismatch`, so one subject never holds dates
moved by two offsets; a release holds it again unless it now agrees
([Who a subject is](../concepts/deidentification.md#who-a-subject-is)).

## What a person can do about it

- Watch it: `nopacs reception list`, `nopacs reception show`, the tenant's status
  and event stream (`MonitorService`), shown step by step in
  [Receiving through the API](../api/reception-walkthrough.md).
- A quarantined file: `nopacs quarantine show --header`, then `release` or
  `reject` with a reason.
- A failed reception: `nopacs reception retry`, or `discard` with a reason.

Nothing is deleted from a storage by the pipeline, and a file leaves the incoming
area only after the database has recorded why. The commands are in
[the command line](command-line.md).
