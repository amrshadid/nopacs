# Changelog

All notable changes to NoPACs are written here. The form is
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions follow
[Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Initial release

NoPACs is a research imaging archive in Go, built on the go-dicom library,
as two static binaries (pure Go, `CGO_ENABLED=0`): `nopacs-server`, which runs
an installation, and `nopacs`, the command line, an API client that links no
database or storage driver. It receives DICOM,
de-identifies it, routes it to a storage, catalogs it as subject / session /
scan, keeps it whole, and serves it through an API with keys. The history of
the code before this release is in github.com/amrshadid/coria.

#### Installation, tenants and databases

- `nopacs-server init` makes an installation (its key file and master
  database) and `nopacs-server serve` serves it over TLS and on a local socket (`nopacs.sock`),
  where the user running the server is its operator without a key.
- Tenants, each with its own database. The master and every tenant database
  live in SQLite or in Postgres; both drivers pass the same contract suites.
- One baseline schema per database (the master, a tenant) and driver,
  `0001_baseline.sql`, each table written once with what it holds and why.
  A database whose recorded baseline differs from the binary's is refused at
  open ("made by an earlier development build ... make it again") and never
  migrated across; a backup and a tenant's export record the fingerprint of
  their baselines, and a restore or an import refuses one made from other
  baselines before it writes anything.
- Settings with defaults, bounds and per-tenant overrides where a setting
  allows one (`nopacs setting`), read again while the server runs.

#### Keys, grants and audit

- API keys `nopacs_<id>_<secret>`, stored hashed and shown once, with an
  expiry, a holder, rotation with an overlap, revocation with successors,
  network rules and an optional TLS client certificate. Grants give a key
  roles on one tenant or on the installation; a request without a grant for
  its tenant is refused.
- After a restore every key is suspended, since a key revoked after the
  backup would otherwise work again; the local operator reinstates the ones
  that should work (`AccessService/ReinstateKeys`, `nopacs key reinstate
  (--all | <id>...)`), by id (at most 1000) or all at once, with one audit
  entry per key reinstated. Any key is refused, and a revoked or expired key
  stays refused. `nopacs key list` and `key show` mark a suspended key.
- A whole key given where a key id goes is refused without being repeated:
  it never reaches an answer, a stored configuration or the audit log.
- Limits on failed calls per caller and per key, which hold under a flood.
- An audit log of every request and every change of configuration, in the
  master and in each tenant, written with the change it records. The logs are
  sealed: a SHA-256 hash chain over a canonical entry encoding, signed at
  intervals with Ed25519 under a key derived from the installation key
  (`AuditSealService`, `nopacs audit export`, verified outside by
  `scripts/prove/audit_verify.py`).
- Secrets (storage credentials, database passwords, naming and date-shift
  secrets) sealed at rest with AES-256-GCM under the installation key file
  (`nopacs-key-v1`); keys for each purpose derived with HKDF-SHA-256
  (RFC 5869) under `nopacs/...` labels.

#### Storage and reception

- Storages per tenant: a directory or an S3 bucket, behind one storage port,
  with capacity, a probe, owner records and the rules for where a tenant's
  data may be.
- DICOM receivers (C-STORE, closed to the callers they do not list, with TLS
  available), STOW-RS receivers (PS3.18 10.5) for the keys they list, and
  import of a directory as a job.
- Each study archived as subject / session / scan with a manifest and
  SHA-256 checksums, verified against what was sent, and scan metadata
  sidecars that record each instance's syntax and each frame's place. A
  crash at any step loses no file a sender was told was received; what
  cannot be archived is quarantined with its reason, and can be released.
- Routing of each study per receiver, a status and a live event stream per
  tenant (`MonitorService`, `nopacs watch`), and metrics (`nopacs_*`).

#### De-identification

- Profiles per tenant, versioned, built on the composite bases
  `nopacs/longitudinal:1` and `nopacs/strict:1` (PS3.15 Annex E), with rules
  by tag, path, private element, value pattern and condition; lookup tables;
  pixel rules that redact regions or detect burned-in text; trial runs and a
  plain description of a version (`ProfileService`, `nopacs profile`).
- A receiver de-identifies what it receives with a profile version, or keeps
  it as received when told so. The check after de-identification searches
  every written file for each removed value and fails closed: a file the
  profile cannot process, or in which a removed value is found again, is
  quarantined.
- De-identification jobs check archived instances against a profile, and the
  forward lookup of a value's codes serves a withdrawal of consent.

#### Reading and sending

- The catalog of subjects, sessions and scans (`CatalogService`), each
  instance's header as DICOM JSON (`MetadataService`), the files streamed and
  checked against their manifest (`DataService`), a plain HTTP download, and
  DICOMweb (QIDO-RS, WADO-RS, STOW-RS) under `/tenants/<name>/dicom-web`,
  with CORS origins by setting.
- Senders (DIMSE C-STORE and DICOMweb STOW-RS) and send jobs, with
  destinations limited by setting (`SenderService`, `JobService`,
  `nopacs sender`, `nopacs job`).

#### Keeping the archive whole

- Corrections: delete a subject, session, scan or instance after a preview
  with a grace period (`delete.grace`), relabel, replace, undo and purge, each
  kept as a signed record on the storage (`CorrectionService`,
  `nopacs correction`).
- Scrub jobs read every stored object back against its checksum. Rebuild
  jobs make a tenant's catalog again from its storages into a shadow
  catalog, reading each correction record as the tenant's database says it
  stands: `compare` (operator) lists the differences; `replace` (admin)
  makes the shadow the catalog in use in one transaction with a
  `catalog_rebuilt` event. From the moment a replace is queued until it
  ends, however it ends and across a restart, the tenant's receivers refuse
  new files as a temporary failure (association rejected as transient,
  STOW-RS 503 with Retry-After); before it builds, the tenant's receptions
  and correction requests in flight end and corrections left half done are
  rolled back. While any rebuild is queued or running, corrections and every
  step of a purge are refused.
- Backup and restore of an installation (`nopacs-server backup
  create|restore|verify`, `.nopacs` files in a chunked AES-256-GCM format),
  and the hand-off of a tenant to another installation (`nopacs tenant
  export|reclaim`, `nopacs-server tenant import`,
  `.nopacs-tenant` files), with storages fenced when another installation
  owns them.

#### API, command line and clients

- The API contract `nopacs.v1` in `api/proto/nopacs/v1`, served over gRPC,
  gRPC-web and Connect, every reply carrying a `Nopacs-Request-Id` header.
  Defined and not yet served: `BackupService/CreateBackup`,
  `ReplicaService` and `RetentionService`.
- The command line `nopacs` for every service, with `--json` output and named
  client contexts, and an interactive program (`nopacs` with no arguments).
- `nopacs job scrub`, `job rebuild` (`--mode compare|replace`; a replace needs
  `--yes` and says what it does), `job findings` and `job finding` make and
  read scrub and rebuild jobs; `job show`, `list` and `watch` follow them in
  their own words (read good / with a finding; cataloged / left out).
- Generated clients for Python (`nopacs`, `clients/python`) and TypeScript
  (`nopacs`, `clients/typescript`), tested against a real server.
- Documentation regrouped by what a reader comes to do (start, concepts,
  operate, use, api, reference, develop); the reference pages for settings,
  commands, errors, events, metrics and environment variables are generated
  from the code (`make docs`) and checked (`make check-docs`), and every
  link and anchor is checked (`make check-doc-links`).
- A server removes what a crash left in a storage once, at its first open of
  the storage, whoever opens it; every later store of the same storage (a
  pipeline reopened, a tenant's corrections, an owner record, a storage test)
  opens beside it, so it never removes another write's temporary file. A
  directory is known by its identity, not the spelling of its root; an S3
  sweep of old uploads that was refused, cut short or left an abort failed
  runs again at the next open.
- A DICOMweb answer that fails within its first 64 KiB (a scan's metadata
  file missing from its storage, an instance's file short or changed) is
  answered with its error status (500 data_loss) instead of a connection
  closed with no answer; one cut after it began ends short; the server logs
  the request with the failure's code and cause, never "ok" (the plain
  download too).
- A restore reads each verified correction record by the rule a rebuild
  reads a record with no row by: a delete abandoned or undone after the
  backup shows its data (one the backup holds committed but undone since is
  undone); a delete committed, purged or still prepared stays hidden and
  unresolved, an undo after the restore shows its data again, and its purge
  removes all of it; a correction the backup holds prepared is moved to the
  state its record says, and a replace published since gets what its publish
  writes, so it keeps its new revision and its purge removes the old one.
- After a restore every delete the backup holds committed is unresolved and
  none is purged until an admin undoes or purges it (`nopacs correction undo`
  / `purge`): an undo whose record was not rewritten cannot be told apart,
  and a purge cannot be undone; the report counts them.
- A test build of the server (build tag `interrupt`) stops at a named durable
  step of a correction or a rebuild; `make prove-interrupt` kills the real
  server at every step and checks reads, the catalog, the storage, a restore
  and a send from outside.
- Proofs from outside the binary (`make prove` and its sections) with tools
  that are not NoPACs: pydicom, dcmtk, curl, openssl, sqlite3, psql,
  dicomweb-client and a viewer.
