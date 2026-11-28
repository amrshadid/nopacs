# NoPACs documentation

[Back to the project's README](../README.md)

NoPACs is a research imaging archive: it receives DICOM, de-identifies it, catalogs it
as subject, session and scan, keeps it whole, and serves it through an API with keys.
These pages hold what no single package can say: how the parts meet, how to run and
use the archive, and how to build on or extend it. What one package holds, and its
rules and why, is in that package's `doc.go` (open the file, or run `go doc
./internal/receive`); the pages link to those instead of copying them.

The pages are grouped by what you came to do:

| Folder | You come to | Pages |
|---|---|---|
| [start](start/install.md) | get going | install the two programs; a first archive end to end |
| [concepts](concepts/archive.md) | understand | what an archive is made of; the lifecycle of a file; the security model; de-identification |
| [operate](operate/running.md) | run a site | running; settings; backup and moving a tenant; replicas; moving off a storage; scrub coverage; encryption at rest; choosing a database; upgrading; troubleshooting |
| [use](use/command-line.md) | work in the archive | the command line; receiving; routing; reading; sending; importing; correcting; retention and holds; profiles |
| [api](api/overview.md) | build on it | the overview; reasons and retry delays; a walkthrough; DICOMweb; the clients |
| [reference](reference/settings.md) | look a fact up | settings, commands, errors, events, metrics, environment variables, all generated from the code |
| [develop](develop/architecture.md) | change the code | architecture; testing; gates; guides |

## By reader

**You run NoPACs for a site.**
[Install](start/install.md), make [a first archive](start/first-archive.md), then
[run it](operate/running.md), set [its settings](operate/settings.md), [back it up](operate/backup.md),
[choose a database](operate/database.md), keep a second copy on [a replica](operate/replicas.md) (and [move off a storage](operate/moving-off-a-storage.md) with it), watch [scrub coverage](operate/scrub-coverage.md), know what [encryption at rest](operate/encryption-at-rest.md) records, and keep [troubleshooting](operate/troubleshooting.md) and
[upgrading](operate/upgrading.md) at hand. The security side is [the security model](concepts/security.md).

**You work with the data.**
[The command line](use/command-line.md) (with contexts for many installations, and the
interactive program), [receiving](use/receiving.md), [reading](use/reading.md),
[sending on](use/sending.md), [importing](use/importing.md), [copying and moving between tenants](use/transferring.md), [correcting](use/correcting.md) and
[retention rules and holds](use/retention.md).

**You build on the API.**
[The overview](api/overview.md), [reasons and retry delays](api/errors.md), [receiving through the API](api/reception-walkthrough.md),
[DICOMweb](api/dicomweb.md) and [the clients](api/clients.md). The contract is `api/proto/nopacs/v1`; the
generated clients are in `clients/python` and `clients/typescript`.

**You change the code.**
[Architecture](develop/architecture.md), [testing](develop/testing.md), [gates and outside checks](develop/gates.md)
and the guides: [a database driver](develop/guides/database-driver.md), [a storage driver](develop/guides/storage-driver.md),
[an API method](develop/guides/api-method.md), [a migration](develop/guides/migration.md) and
[an event type](develop/guides/event-type.md).

## By task

| I want to | Go to |
|---|---|
| install the programs | [Install the two programs](start/install.md) |
| see the whole thing work once | [A first archive](start/first-archive.md) |
| understand the words (installation, tenant, subject, session, scan) | [What an archive is made of](concepts/archive.md) |
| make an installation and a server | [Running NoPACs](operate/running.md) |
| give a person or a program a key | [Running NoPACs: the first key](operate/running.md#the-first-key), [the security model](concepts/security.md) |
| work with several installations from one computer | [Contexts](use/command-line.md#contexts) |
| set a tenant up to receive scans | [Running NoPACs: make a tenant that receives](operate/running.md#make-a-tenant-that-receives) |
| remove identifying data on the way in | [De-identification](concepts/deidentification.md), [profiles](use/profiles.md) |
| send each study to a storage by where it comes from | [Routing](use/routing.md) |
| see what arrived, and why a file is held | [A reception, end to end](use/receiving.md) |
| read the archive back | [Reading the archive](use/reading.md), [DICOMweb](api/dicomweb.md) |
| send archived data on to a PACS | [Sending data on](use/sending.md) |
| bring in a directory of existing files | [Importing a directory](use/importing.md) |
| copy or move a subject or a session to another tenant | [Copying and moving data between tenants](use/transferring.md) |
| delete, relabel or replace archived data | [Correcting archived data](use/correcting.md) |
| back up, restore, or move a tenant | [Backup, restore and moving a tenant](operate/backup.md) |
| keep a second copy of a tenant's files | [Replica storages](operate/replicas.md) |
| move a tenant to another disk or bucket | [Moving off a storage](operate/moving-off-a-storage.md) |
| tell whether scrubs keep up with the archive's size | [Scrub coverage](operate/scrub-coverage.md) |
| see which storages are said to be encrypted | [Encryption at rest](operate/encryption-at-rest.md) |
| keep data for a time, or stop it going | [Retention rules and holds](use/retention.md) |
| let a tenant's admin give keys access | [What a tenant admin may grant](concepts/security.md#what-a-tenant-admin-may-grant) |
| read a refusal's reason and retry delay in code | [Reasons and retry delays](api/errors.md) |
| check that the audit log was not changed | [Running NoPACs](operate/running.md#check-that-the-audit-log-was-not-changed) |
| pick SQLite or Postgres | [Choosing a database](operate/database.md) |
| find out why something fails | [Troubleshooting](operate/troubleshooting.md), [error kinds and codes](reference/errors.md) |
| look up a setting, a flag, a metric | [settings](reference/settings.md), [commands](reference/commands.md), [metrics](reference/metrics.md), [events](reference/events.md), [environment](reference/environment.md) |
| call the API from code | [The API overview](api/overview.md), [the clients](api/clients.md) |
| add a driver, a method or a migration | [The guides](develop/architecture.md#next) |

## What is not written yet

These are planned and have no page: the DICOM Conformance Statement and the
de-identification support matrix (what is checked per kind of image and transfer
syntax), the requirements map (which feature meets which technical requirement of
HIPAA and GDPR, and how it is proven), the operations guide (signals and thresholds
with examples), the originals policy, a recipe for a viewer behind a login, the
page on using the archive with an AI assistant, and the published service levels.
The pages above say plainly what each feature does not do yet.

## Conventions

- Diagrams are Mermaid code in the Markdown; GitHub draws them.
- Commands shown here were checked against the programs' `--help`; if one differs
  from what you see, the program is right and the page is a bug.
- The pages in [reference](reference/settings.md) are written by `make docs` from the
  code, and `make check-docs` fails when one is out of date. Do not edit them by hand.
- `make check-doc-links` fails when a link or an anchor in these pages does not resolve.
