# The lifecycle of a file

[Documentation index](../README.md) · [What an archive is made of](archive.md) · [A reception, end to end](../use/receiving.md) · [Correcting archived data](../use/correcting.md)

A file in NoPACs is not simply "stored" or "not stored". There are six questions
you can ask about an instance, each answered on its own, and no one word says they
all hold. This page names them, says what the archive answers to each, and where
you can see it.

## Three facts about an instance

| Fact | Means | Where you see it |
|---|---|---|
| **Landed** | The file reached the incoming area whole, was synced to disk, and only then was the sender told it was received. From now on a crash loses nothing. | `nopacs reception show`, the tenant's status (the incoming bytes) |
| **Archived** | The file was put in its storage, read back and compared, and the catalog committed it. It is listed, readable and checked on every read. | `nopacs catalog`, `nopacs reception show` (step *verified*, *done*) |
| **Protected** | The file is verified on a second storage. A replica storage holds a second copy, and a scan is protected once the replica is in step for it ([replicas](../operate/replicas.md)). | `nopacs replica list` |

The first two are in the pipeline described in [a reception, end to end](../use/receiving.md).
`nopacs reception show` and `MonitorService.GetStatus` say, for each reception, how
many files are received, archived and quarantined.

## Six dimensions

| Dimension | What the archive answers | What is built |
|---|---|---|
| **Reception** | Whether a file was refused, landed, held in quarantine (with its reason) or archived; which receiver, which caller, which import job | Built: receptions, quarantine, import outcome counts |
| **Identity and provenance** | Which tenant, subject, study, series and instance it is; which de-identification profile version made it, in what form and transfer syntax it was received, and by which version of the software | Built: the catalog, and a sidecar next to each scan on its storage |
| **Publication** | Which revision of an instance is the current one, with its length, transfer syntax and SHA-256, and the manifest that commits it | Built: one transaction makes a change visible; a replace publishes a new revision |
| **Preservation** | How many copies there are, when each was last checked, and whether an object is online, damaged or missing | Partly built: the server's scrub re-reads every stored object against its checksum and marks a damaged instance so a read fails as data loss, never returning wrong bytes. Replicas with their backfill, repair, recall from an archive tier and a verification sample for tools outside the server are built ([replicas](../operate/replicas.md)). |
| **Distribution** | Which revision a send delivered and what the peer answered | Built: send jobs say, for each instance, sent, sent with a warning or refused, and why ([sending](../use/sending.md)) |
| **Disposition** | Whether it is active, hidden, awaiting purge or removed, and why | Partly built: delete, relabel, replace, edit and move (a session to another subject, or a merge), with a grace period and a record ([correcting](../use/correcting.md)). Holds and retention rules are built ([retention rules and holds](../use/retention.md)). |

## What a person can rely on

- **A file acknowledged to a sender is never lost and never archived twice.** The
  incoming area is synced before the acknowledgement, and every later step is a
  unit of work held in the database that a crash runs again with no further effect.
- **A file the pipeline cannot take is held, not dropped.** It is in quarantine with
  its reason and its bytes until someone releases it or rejects it.
- **Nothing is archived de-identified wrongly.** A receiver that de-identifies
  checks the result for the values the profile removed, and holds the file when it
  cannot show they are gone.
- **A read is checked.** Every file read back is compared with the checksum in the
  manifest as it streams; a mismatch is an error (`DATA_LOSS`), not wrong bytes.
- **Nothing is deleted from a storage without a recorded reason** and, for a
  delete, a grace period in which it can be undone.

## What is weak

Replication and retention are not built, so an archive's only copy is on its one
storage until you copy it yourself ([backup](../operate/backup.md) says what a
backup holds and what it leaves to the storage's own tools). The scrub finds damage;
it never repairs it.
