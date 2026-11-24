# NoPACs documentation

[Back to the project's README](../README.md)

This directory holds what no single package can say: how the parts meet, the
flows through them, and the guides for people who run, call or extend NoPACs. What
one package holds, and its rules and why, is in that package's `doc.go` (open
the file, or run `go doc ./internal/receive`). The pages link to those instead of
copying them.

## If you run NoPACs

| Page | What it is for |
|---|---|
| [Running NoPACs](running.md) | Make an installation, run the server, make the first key, set a tenant up to receive |
| [The command line](command-line.md) | Every command, how it connects, its output and exit codes, and the interactive program |
| [Sending data on](sending.md) | Senders and jobs: send a subject, a session or a scan to a PACS or a DICOMweb service, or forward each reception, and follow it to its end |
| [Importing a directory](importing.md) | How a directory of DICOM files will be imported (defined in the API, not yet served): roots, layouts, the map file, counts, and a recipe for an XNAT archive |
| [Correcting archived data](correcting.md) | Delete, relabel or replace archived data: the preview, the grace period, undo, the purge and what it can claim, what stays held, what is out of reach |
| [Backup, restore and moving a tenant](backup.md) | Back up an installation, check a backup, restore it, and hand one tenant to another installation: what is and is not kept, the suspended keys, the owner records, reclaim, the fenced state, what stays weak |
| [Settings](settings.md) | Every setting with its default, who may set it, and whether a change needs a restart |

## If you build on its API

| Page | What it is for |
|---|---|
| [Receiving data through the API](api-walkthrough-reception.md) | Three uses, call by call: set a tenant up to receive, follow a study to archived, settle a quarantined file |
| [Reading the archive](reading.md) | Catalog, metadata, data and DICOMweb, and what each read checks |
| [A reception, end to end](reception.md) | What happens from a sender's first file to a verified scan, and its states |

The contract itself is `api/proto/nopacs/v1`; the generated clients are in
`clients/python` and `clients/typescript`.

## If you extend the code

| Page | What it is for |
|---|---|
| [Architecture](architecture.md) | The parts of an installation, how packages depend on each other, the path of a request, where state lives, the rules and what enforces each |
| [Testing](testing.md) | The kinds of test, how a slow one is marked, and every environment variable the tests and scripts read |
| [Gates and outside checks](gates.md) | Each `make` target a change passes, and the tools that check NoPACs from outside |
| [A database driver](guides/database-driver.md) | Implement the master and tenant ports for another database |
| [A storage driver](guides/storage-driver.md) | Implement `blob.Store` for another kind of storage |
| [An API method](guides/api-method.md) | From the contract to the handler, the command and the proof |
| [A migration](guides/migration.md) | Change a schema: the baseline until the first release, a forward migration after |
| [An event type](guides/event-type.md) | Report a new change on the live feed |

## Conventions

- Diagrams are Mermaid code in the Markdown; GitHub draws them.
- Commands shown here were checked against `nopacs --help`; if one differs from
  what you see, the binary is right and the page is a bug.
