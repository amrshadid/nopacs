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
scan, keeps it whole, and serves it through an API with keys.

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
- Routing rule sets: each receiver runs an immutable, numbered rule set version
  (RouteService, `nopacs route`). Archive rules choose a new session's storage
  and profile, the first that holds; forward rules all fire, each with senders,
  an optional series filter and an optional destination profile applied to each
  copy and checked after (DIMSE senders); an archive rule's or destination's
  profile may only tighten the receiver's, checked at create, activate and on a
  receiver's profile change. A forward rule sends only the series of the
  reception it was judged on. Conditions use the condition language (keywords,
  tags, value indexes, sequence paths, private tags by creator,
  calling_ae/called_ae); an absent value is false; no condition may read a
  patient identifier. Each reception records the version and rules that routed
  it (hit counts); `route explain` shows the deciding comparison (values hidden
  unless --show-values, audited, refused to a masked key); `route dry-run` tries
  a version over the latest sessions, refused to a masked key over sessions
  archived as received. Replaces `receiver route` and Receiver.routing.

#### De-identification

- A subject is the Patient ID with its issuer. The default subject label
  (`sub-{#SubjectIdentity}`), the date shift, a profile's `hash`, `lookup` or
  template of the Patient ID, the forward lookup and the hold of a deleted
  subject's re-sent files key on the Patient ID with its Issuer of Patient ID
  (0010,0021) and the identifying values of the Issuer of Patient ID
  Qualifiers Sequence (0010,0024), or, when a file names no issuer, with its
  receiver's new `namespace`: two hospitals' patients sharing a Patient ID are
  two subjects. Each file's identity is read once from its header (an item's
  text under its own character set); a file whose identity cannot be read is
  held, never keyed by its Patient ID alone; at a de-identifying receiver a
  file whose own subject is not its session's (one file of a study carrying
  the issuer and another not, or two Patient IDs) is held `subject_mismatch`,
  so one subject never holds dates moved by two offsets (a subject a committed
  relabel renamed keeps receiving its files). A receiver's namespace is fixed
  once it has received data; a study whose receiver was removed before it had
  a session is held (`unrouted`, `receiver_removed`). New: `Receiver.namespace`
  (`receiver add --namespace`), `CodeRequest.issuer`/`namespace` (`deid code
  --issuer --issuer-qualifier --namespace`), `LookupEntry.issuer`/`namespace`
  (columns of `profile lookup import`). On upgrade a file with no issuer,
  through a receiver of the default namespace, keeps its label, date shift
  and codes to the byte; a file carrying an issuer gets a new label, shift and
  codes (nothing is released); a lookup table imported by Patient ID alone
  answers only files that name no issuer under the default namespace.
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
- De-identification text lists: named, versioned lists of terms in a profile
  (up to 10,000 terms each) that clean and redact cut from free text where a
  term stands as a word, ignoring case, width, Unicode form and invisible
  characters. The check after searches every value for the terms, in UTF-8 and
  the received character set, long values decoded as well as byte by byte; it
  leaves out standard elements that hold binary data (pixel, overlay,
  waveform, ICC profiles and the like), and searches private and UN values and
  a text tag sent with a binary VR. An encapsulated document is searched when
  its MIME type is text (a CDA, XML or JSON); one of any other type, such as a
  PDF, kept under a profile with text lists holds the file
  (list_unsearchable). A file or label holding a term is held (residual_value,
  kind list_term). The bound on a profile's raw forms counts each term exactly
  in the character set that writes it longest, so a profile that validates
  never has a file refused for the set it was received in. Terms are never
  printed: profile describe and diff show each list's name, version, count and
  SHA-256; a masked key gets no term, list checksum or version checksum of a
  version with lists, and may not store, try or explain one. The cache of
  compiled lists is bounded by the setting `deid.list-cache-bytes` (default
  256MiB, 64MiB to 16GiB).
- An order of strictness of de-identification actions and a layer rule
  (tenant, receiver, route, destination) that refuses a layer loosening what
  is below it (loosens_lower_layer, incomparable_actions), with the effective
  description per element.
- Each file's de-identification record lists the public tags and (private
  creator, element) no rule named, with VR and count, no values; a creator
  holding a removed value or a list term is withheld.
- A tenant may set a base profile (`deid.base-profile`). Every reception runs
  the receiver's profile stacked over it: an element both layers decide gets
  the stricter action, the base's pixel rules decide an image first, and the
  check after holds what either layer removes. Every receiver, archive-rule
  and destination profile must be at least as strict as the base, checked
  wherever a profile is pointed at, and a looser one is refused with the
  loosening listed; `nopacs profile describe --effective` shows each element's
  deciding layer. De-identification jobs judge by the same stack.
- `nopacs deid coverage` lists the standard and private elements archived
  files held that no rule named, with VR and counts and never a value (a
  private creator that held a removed value or a list term is withheld), from
  a job that reads each scan's metadata once and resumes after a restart.
- Profile re-run over the archive (`nopacs deid rerun`): a newer profile
  version runs over data already archived de-identified, and each file it
  takes more from becomes a new revision (the superseded one kept until the
  purge, undone before it, the check after first); a preview counts what would
  change by element and action, and a looser version, a version that does not
  stack over the base, a file that would need the original, and a held file
  are refused with a fixed reason.
- Change control: `nopacs receiver try-profile` tries a version over a
  receiver's newest `deid.change-trial-sessions` sessions. Giving a receiver
  another profile version, its first one, or an archive rule's version needs
  that version tried over files with none held, or `--force` with a reason,
  which is audited.
- Per-device pixel templates: a tenant keeps, for a device (manufacturer and
  model, case ignored) and an image size (rows and columns, exactly), and
  optionally a SOP class, the rectangles of burned-in text; a pixel rule
  `redact_by_template` fills them, and an image at risk of burned-in text that
  no template fits is held (`no_pixel_template`), never archived unfilled.
  Loaded by an admin with `nopacs deid template add|import|list|remove` (JSON
  or CSV); none is shipped.

#### Reading and sending

- The catalog of subjects, sessions and scans (`CatalogService`), each
  instance's header as DICOM JSON (`MetadataService`), the files streamed and
  checked against their manifest (`DataService`), a plain HTTP download, and
  DICOMweb (QIDO-RS, WADO-RS, STOW-RS) under `/tenants/<name>/dicom-web`,
  with CORS origins by setting.
- Senders (DIMSE C-STORE and DICOMweb STOW-RS) and send jobs, with
  destinations limited by setting (`SenderService`, `JobService`,
  `nopacs sender`, `nopacs job`). A sender sends only instances archived
  de-identified, with their profile version recorded and no accepted pixel
  risk; any other is refused per instance as it would be sent, with a fixed
  reason (`as_received`, `no_profile_provenance`, `pixel_risk_accepted`),
  unless an admin on the installation marks the sender `allow_identified`
  (`--allow-identified`), a change that is audited, reported as an event
  and refused to a masked key. Only that admin changes where a marked
  sender sends (host, port, AE titles, URL, TLS or account). A replace gives the
  instance its new revision's profile version and pixel risk, so the rule
  judges the bytes the catalog points to.
- Search: find subjects, sessions or scans by the header values their files
  hold, in the routing condition language over 22 indexed attributes kept in
  step with the catalog by every correction and rebuild, with `has` and `every`
  across levels (`SearchService`, `nopacs search`, and series-level QIDO-RS). A
  query must be served by an index, `contains` needs three characters, and a
  query compiling past 64 KiB of SQL or 999 values is refused; counts stop at
  `search.facet-rows` and say "at least", and facets are refused above it; a
  masked key never sees a row archived as received. A new attribute is filled
  by a backfill job (`nopacs search backfill`) that resumes after a restart.
  Checked against a pydicom reference filter and dicomweb-client.

#### Keeping the archive whole

- Corrections: delete a subject, session, scan or instance after a preview
  with a grace period (`delete.grace`), relabel, replace, undo and purge, each
  kept as a signed record on the storage (`CorrectionService`,
  `nopacs correction`).
- Edit a header value as a correction (`CorrectionService.Edit`, `nopacs
  correction edit`): set, change or remove named attributes of every
  instance of a subject, session, scan or instance, after a preview whose
  values are hidden unless asked (audited). Each instance gets a later
  revision written by go-dicom, judged first by the tenant's profile and its
  check after; the UIDs the catalog is built on, Pixel Data and what lays out
  its bytes, private attributes, binary VRs, the de-identification markers,
  a value the profile takes out or writes, a scan's values below the scan,
  and a file whose character set could not be kept are refused, and so is a
  masked key. The superseded revisions are kept until the purge and an undo
  puts them back; an undo that may not be published (a hold placed since) is
  taken back, audited, and the edit stays; a revision number is never used twice, a
  restore adopts an edit with what its publish owns, and a scrub expects what
  an edit keeps. A replace or an edit rolled back after a reception committed
  its scan keeps the digests of what the rollback wrote.
- Batch delete (`CorrectionService.DeleteBatch`, `nopacs correction
  delete-batch <tenant> (--file LIST | --from-search QUERY) --reason TEXT
  [--preview]`): up to the tenant's `correction.batch-max` (10000) subjects,
  sessions or scans with one reason. One preview lists every target with its
  holds, retention minimums, replicas and senders under one token, bound to
  the targets, their order, the reason and what each held. Confirmed, a job
  of kind `delete_batch` deletes each target in turn by an ordinary delete
  (records, grace, undo, purge on every copy), and skips, never forces, a
  target held, kept by a rule, changed since the preview, gone, or held by
  another correction, each with its reason (`ListDeleteBatchTargets`,
  `nopacs correction batch-targets`). It is followed, paused, resumed and
  canceled like other jobs (`JobService.PauseJob` and `ResumeJob`,
  `Job.paused`; `nopacs job pause|resume`); a restart goes on where it
  stopped and never deletes a target twice, two runners of one job never
  record a deleted target as skipped, and a storage that keeps failing is
  tried again after a wait that doubles from 5 seconds to 10 minutes. One
  batch runs per tenant at a time, and a masked key is refused. A delete's
  confirm token, and each batch target's check, also name the catalog's rows
  the delete takes and each scan's manifest checksum, so sessions of the
  same size swapped by relabels, or an instance replaced or edited, never
  pass as the data a person previewed.
- Move a session to another subject, or merge two subjects (`CorrectionService.Move` and `Merge`; `nopacs correction move <tenant> <subject>/<session> --to <subject>` and `nopacs correction merge <tenant> <subject> --into <subject>`, each `--reason TEXT [--preview] [--rewrite-identity]`): a correction of kind `move` built on the relabel's copy, every object copied under the other subject, read back and compared by SHA-256 on each primary and replica in step, the move switched at once with a `session_moved` or `subject_merged` event, the old tree kept until the purge; a merge hides the emptied subject and holds its label until it ends, and a lookup by old labels answers RELABELED. Its preview's token binds the rows and manifests that move and the destination; refused are a session label the destination holds, a hold on either subject, a correction on either not ended, a subject's last session and a masked key. It is undone until its purge (refused once what moved changed), and rebuild, restore and scrub read it from its records. With --rewrite-identity each moved session is edited after the publish to carry the destination's archived Patient ID and issuer, decided per session and tried again each pass until done or refused for good, a refusal recorded on the move and shown by `correction show`.
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
  Every method of the contract is served (`api/not-served.txt` lists none).
- The command line `nopacs` for every service, with `--json` output and named
  client contexts, and an interactive program (`nopacs` with no arguments).
- `nopacs job scrub`, `job rebuild` (`--mode compare|replace`; a replace needs
  `--yes` and says what it does), `job findings` and `job finding` make and
  read scrub and rebuild jobs; `job show`, `list` and `watch` follow them in
  their own words (read good / with a finding; cataloged / left out).
- Generated clients for Python (`nopacs`, `clients/python`) and TypeScript
  (`nopacs`, `clients/typescript`), tested against a real server; their one
  error type carries the reason, metadata and retry delay of the answer's
  error details.
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
- A backup of the running installation over the API
  (`BackupService/CreateBackup`, installation admin; `nopacs backup create
  --out FILE [--json]`): the same file as `nopacs-server backup create`, read
  database by database in read transactions that hold no writer or event
  back, encrypted by the server, streamed in chunks of at most 1 MiB, one
  backup or export at a time, the audit's sealing pass paused meanwhile,
  bounded as a whole by the setting `backup.max-duration` (6h by default, 1m
  to 7d), audited before the first read and when it ends.
- S3 storages keep `purge_versions`: AddStorage, GetStorage, ListStorages and
  SetStorage carry it (absent means true), `nopacs storage add|set
  --purge-versions=false` sets it, and a purge on a versioned bucket then
  leaves older versions and reports them hidden instead of deleting them. An
  object a store answers `InvalidObjectState` (an archive storage class)
  reads as unavailable `NOT_ONLINE`, never as refused credentials, and a
  scrub tallies it `unreadable|not_online` once and moves on.
- A key's reads of a tenant handed to another installation (moved or fenced)
  are served (catalog, metadata, downloads, DICOMweb, ReadFiles): the tenant's
  network rule was read as if for a change, so every keyed request answered
  500; the rule still applies, and a change is refused FAILED_PRECONDITION with
  TENANT_MOVED or TENANT_FENCED. An installation setting whose check visits
  every tenant is checked against such a tenant like a ready one, not refused
  outright.
- A subject, session or scan asked for by a label a committed relabel moved
  answers NOT_FOUND with a message starting "RELABELED:" that names the relabel
  and the labels the data has now, to callers with operator or more on the
  tenant (who may read corrections); a reader gets the same not found as for a
  label never used, so a relabel made to remove an identifying label stays
  done.
- A restore, an import or a rebuild that replaces the catalog begins a new
  event history for clients: a cursor of the old history (or one past the
  newest event) is refused OUT_OF_RANGE with a message beginning
  EVENT_HISTORY_CHANGED, the new history's first event is catalog_rebuilt and
  no event of the old history is served in it; GetStatus carries the history id
  (event_history) and every keep-alive carries it with each cursor it names; an
  expired cursor's message begins CURSOR_EXPIRED; cursors made before this
  version are refused once as of another history.
- The restore and import reports name, by file name only, each TLS file a
  receiver or sender names that the data directory lacks (a backup holds
  none), under "external credentials to re-enter", one that cannot be looked
  at as not checked, never failing the restore; and they say the network
  listener's certificate and key are not in a backup.
- Outside proofs of the archive kept whole (`make prove-whole`): purge
  dispositions on versioned and object-locked buckets, a restore onto a fresh
  host with its recovery time, the event history after a restore, one writer
  per storage across a move, sends meeting corrections, ResolveInstances and
  old labels, references after corrections, provenance of received bytes,
  what a client sees of a damaged instance, objects not online and the purge
  clock.
- A fenced tenant serves no data. After a hand-off the old installation kept
  serving its catalog, downloads, DICOMweb and ReadFiles from what it held at
  the export, including subjects the new owner had since deleted, whose files
  stay on the shared storage until that purge. Every read of a fenced tenant
  is refused FAILED_PRECONDITION with a message beginning TENANT_FENCED that
  names the installation owning the storage; only its state and reason stay
  visible. A moved tenant serves reads while its storages' owner records still
  name this installation; after an import the next owner-record check (within
  30 seconds, or at start) fences it.
- `nopacs-server backup restore --reconcile-only` reconciles an installation
  whose databases the site brought back by its own means (a Postgres
  point-in-time recovery, a copy of the SQLite files, a volume snapshot), in
  place, through the restore's own reconcile: every key suspended, each
  tenant's correction records newer than its rows applied, its catalog rebuilt
  from the storages under a new event history; a held quarantined file whose
  bytes are still in incoming stays held. It refuses a data directory a server
  holds, an empty database, another baseline and a master of another
  installation, each before anything is written; interrupted, it is run again.
- A restore and a reconcile read every storage's owner record before writing
  anything: a tenant handed to another installation is left fenced, serving no
  data, and the others are restored or reconciled; any other refusal stops the
  run before any key is suspended or storage taken; `--take-over` is offered
  for another installation's record only when its claim is known to be stale.
  A subject's delete brought back from its record holds data arriving under
  every label the subject had before a relabel made before the delete. A
  rebuild job a server had not ended no longer blocks a restore, a reconcile or
  an import: it is canceled and its shadow removed. `serve`, `backup create` and
  `tenant import` check the restore and reconcile markers again under the lock.
- Proof tools for archive tiers, replicas and retention: an archive-tier S3
  proxy (`scripts/prove/s3_stub.py`), a comparison of two storage trees
  (`tree_compare.py`), and a test-only clock (`internal/clock`, build tag
  `clock`) with its offset helper (`clock_offset.py`).
- The storage port tells whether an object reads at once (online, archived,
  restoring, with a recalled copy's expiry), recalls one from a cold S3 class
  (asked once, and only when the object is cold), gives the store's own SHA-256
  of an object and says when a put was confirmed by the store itself, both only
  on a server shown at open to refuse a wrong checksum, puts bytes only when
  they are the size and SHA-256 expected, and reports an S3 bucket's default
  encryption.
- The tenant database keeps replica storages (at most two per primary, named
  with `replica_of`) and each replica's state per scan, keyed by Series Instance
  UID, with the queue of copies written in the same transaction that publishes,
  corrects, purges or restores a scan; one shared recall per archive-tier
  object; the record of a primary's repair from a replica; and the switch that
  swaps a primary's and a replica's places in one transaction, refusing while
  a correction or a rebuild is open and taking each moved S3 secret sealed
  again for its new name. Storages gain a tier, recall days and speed, a
  directory's declared encryption at rest and a switch generation. Both
  database drivers.
- The replicator copies each queued scan of a primary to its replicas: every
  object the committed manifest names is streamed through a put checked
  against its SHA-256 and read back when the replica cannot confirm it; an
  object already there counts only when the replica's own verified manifest
  lists it; the sidecar and the manifest go last and are checked again on the
  primary; the lease is renewed on a timer and a lost lease writes nothing;
  a scan is recorded in step only at the manifest it copied.
- The tenant database keeps retention rules and each session's keep and due
  times, stored and indexed, clamped by `retention.shorten-notice`, on a clock
  that pauses on a forward jump until an admin confirms it (a step back only
  delays); holds that refuse deletes, stop purges (a relabel's too) and are
  owed as records on every storage their target reaches; and dispose jobs that
  delete due sessions through the ordinary delete. A rule change and a commit
  are ordered, so a keep is never lost; a delete a rule keeps needs
  `override_retention` with its own reason.
- Reads of archived instances (downloads, ReadFiles, DICOMweb with frames and
  metadata, senders, de-identification jobs) turn, before their first byte, to
  a replica in step for the scan when the primary's object is missing, marked
  damaged, not online, or its storage does not answer; the replica's bytes are
  checked against the catalog as the primary's are, and wrong or missing
  instance bytes are recorded as that replica's damage while the revision is
  still the catalog's. A range covering the whole instance is checked like a
  whole answer. The audit detail `served_by`, the metrics
  `nopacs_reads_served_by_replica_total` and
  `nopacs_replica_read_mismatches_total`, and a warning at most once a minute
  per storage say a read turned.
- The recall worker asks an archive-tier store once to bring a cold object
  back, polls until it reads, and records the answer in the tenant's shared
  recall row (online until a time, or failed with a fixed reason, never the
  store's text).
- `nopacs replica backfill` (ReplicaService.BackfillReplica) queues a
  replicate job that copies to a replica every object of its primary it does
  not hold verified, never one a purging delete withdraws; the job counts the
  scans its backfill owes, ends done once each is in step, says "replica not
  answering" while copies to the replica fail, and is answered again to a
  repeated call. Attaching a replica makes the same job for what the primary
  held. `nopacs recall` (ReplicaService.Recall) brings a subject's, session's
  or scan's objects back from archive-tier storages after a preview and its
  confirm token, joining recalls already asked for and asking nothing twice;
  days above a storage's own need an admin. `nopacs replica verify`
  (ReplicaService.VerifySample) lists a random sample of a storage's objects
  (key, size, SHA-256), repeatable with a seed, for checks with tools outside
  the server.
- The storage switch runs a primary's move onto its replica on the server's
  own goroutine: a preview names its blockers (a clean scrub of the replica
  and an empty compare from it, each the newest of its kind) and gives a
  confirm token bound to the tenant, both storages and the generation; once
  accepted it pauses every archive step and sidecar rewrite on the primary at
  one gate (UNAVAILABLE `STORAGE_SWITCHING`, reads go on), waits for writers in
  flight, drains until every scan is in step, takes the owner record, seals
  moved S3 secrets again for their new names and swaps both places in one
  transaction; a restart refuses an accepted switch and finishes a swapped one.
 
- Corrections reach every replica: a correction's record goes on each replica
  (owed and retried when one does not answer, and owed to a replica attached
  later); the purge removes each object from the primary and every replica,
  records one disposition per storage and switch generation, checks every
  removed key absent on every copy before it says purged, and stays purging
  while a copy does not answer or is owed its record; a purge and a switch
  exclude each other, and no replica is attached while its primary's purge
  runs; a relabel copies to replicas in step before its publish; detaching
  with `abandon_purges` records the replica's objects unknown; the delete
  preview lists replicas.
- Scrubs read replicas like primaries: a replica's damage is recorded on the
  replica, never on the instance, and findings a switch moved there clear
  once it reads good; a replica that fails reads is passed over for that run
  while its primary is still checked; objects no manifest names are recorded
  as strays, never removed; an archive-tier storage is checked by presence
  (sizes and the store's own SHA-256) with no recall; requests to scrub one
  instance are deduplicated, bounded and rate-limited, and a request that
  cannot be read never holds a scrub back; coverage counts only scans read
  whole.
- The repair job puts objects a scrub found missing or damaged back on a
  primary from a replica in step: each replica object is read whole and
  checked against the catalog's size and SHA-256, the damaged state is
  recorded before the put, the put goes through the storage's write gate and
  is read back, and the finding is cleared with an audit entry. While it runs
  a repair holds the object's scan, so a correction or a reception's commit
  to that scan is refused as retryable, and the repair is refused while a
  correction of the scan or a switch is under way; if the catalog changed by
  the end, the finding stays open. One repair per storage at a time, with
  counts, tallies and cancel like other jobs.
- A scrub of a replica reads its primary's scans on the replica; a scrub can
  name one scan, and a read that finds an instance's bytes wrong makes a scrub
  of that scan without moving the storage's scheduled scrub; when each scan
  was last read whole on each storage is kept for coverage; damage a scrub
  finds on a replica is listed among the scrub findings, naming its primary,
  and cleared with it.
- Each scan's metadata file keeps its SHA-256 beside the manifest's: replicas
  copy the file against it, scrubs and reads served by a replica check it,
  the pass that adds frame tables records the file it rewrites (a record
  lost is recovered on the next pass), and a delete's purge leaves the file
  unchecked rather than wrongly checked.
- A rebuild compare can read one named storage, a replica among them, alone,
  against that storage's primary only. Receivers refuse new associations
  with `storage_switching` while their storage switches, each counted.
- The server runs each tenant's replicator, recall worker, repair jobs (by
  hand, or automatically with `replica.repair = auto`), storage switches and
  retention: the clock checked hourly, due sessions disposed of daily through
  the ordinary delete, a failed pass tried again the next hour. Every write of
  this background work asks whether the tenant may still write, and an
  export's or rebuild's drain stops it first. A storage switch pauses every
  write to the primary: receptions, purges and repairs, each entering the
  gate before it opens the storage. New settings: `replica.repair`,
  `replica.repair-lease`, `retention.shorten-notice`,
  `retention.max-clock-step`, `retention.max-per-pass`, `quarantine.warn-age`,
  `quarantine.warn-bytes`.
- The API serves replicas (list, attach, detach, repair, switch with its
  preview and token) and retention (rules, due sessions, disposal with a
  server-keyed token bound to its rule, clock confirmation that names the
  tenants it left paused, hold reads); storages take a replica, the archive
  tier, recall settings and a directory's declared encryption; storage status
  shows tier, replica, switching, encryption at rest and scrub coverage;
  corrections show their dispositions on every copy. Every error the API
  answers carries a google.rpc.ErrorInfo (domain `nopacs`): the contract's
  reason with its metadata where a case names one (`TENANT_MOVED`, `HELD`,
  `NOT_ONLINE` with `recall_state`, `RELABELED` with the new labels, ...),
  else the code's general reason (`NOT_FOUND`, `UNAVAILABLE`, ...), in every
  protocol and in plain HTTP answers, with RetryInfo where a retry is meant
  (`STORAGE_SWITCHING`, `NOT_ONLINE` while a recall runs).
- A tenant's admin sets, changes and removes reader, writer and operator
  grants on its tenant and lists its keys; never an admin's grant (the
  tenant's or the installation's), never its own; the bounds are checked in
  the master's transaction. `override_retention` is taken only from an
  installation admin or the local operator, checked again when the delete
  commits.
- The command line gains `replica`, `repair`, `switch`, `retention` (rules,
  due sessions, disposal, clock), `hold` and `storage status`; storages take
  tier, recall and declared-encryption flags; a delete takes a retention
  override; `key list --tenant` serves a tenant's admin; errors show the
  server's reason and when to try again, and with `--json` are one JSON
  object on standard error with the reason, its metadata and the retry delay.
  The interactive program builds its
  menu and a tenant's keys from the caller's grants and gains screens for
  replicas, repair, switch, retention, holds, corrections, scrub findings,
  profiles, routes and the audit seal's verification, each destructive step
  confirmed by typing its target.
- The interactive program gains screens for header edits, batch deletes (marked
  rows, typed targets or a filter, with pause and resume), routing rule sets
  (view, activate, explain, dry run, hits), quotas, moves and merges, transfers,
  a sender's allow-identified mark, and replica recall and sample checks; each
  offered only to keys whose grants allow it, masked keys included, with values
  hidden by default.
- Holds outlive a lost database: placing a hold writes a sealed record,
  `meta/.holds/<id>.json` (under its own derived key, never the reason's
  text), on every primary its target touches before the hold commits, and
  owes it to replicas; a release first reserves its count in the database, so
  a release that does not commit never leaves a released record that
  outranks the hold; records are rewritten on relabel and when the
  installation's key version changes; a rebuild, a restore and every start
  make an active hold again of a record found with no row, and a record that
  does not verify is refused and reported, never adopted.
- A dispose job always ends: a session another correction holds, or whose
  delete is refused, is skipped with its reason; a canceled job deletes
  nothing more; a retention delete is refused when the session's labels name
  another study since it was listed. The repair lease is checked against the
  largest instance the tenant holds.
- Every write of a correction to a storage enters the storage's switch gate
  (a relabel and a replace hold it until their publish, an instance purge
  before the scan's lock), and replace, relabel and an instance purge keep
  the metadata file's digest. A switch's empty-compare check reads a
  rebuild compare made from the replica.
- PlaceHold and ReleaseHold are served (`nopacs hold place`, `hold release`):
  a hold's record is written on every primary before its row. The hold
  worker's writes are fenced; retention adopts holds found on storage before
  its first disposal after a start; a relabel writes the moved holds' records
  at once; a storage removed or a replica detached keeps no hold record rows;
  a restore adopts holds from every storage it can read.
- `replica.lag-alert` (1h): one replica-behind event when a replica's oldest
  queued copy passes it and one when it catches up, with the metrics
  `nopacs_replica_lag_seconds` and `nopacs_replica_scans_behind`;
  `replica.rate` paces the bytes put on each replica (default no bound);
  `recall.wait` (48h) records a recall that never comes online as failed;
  `scrub.archive-sample` applies without a restart; NOT_ONLINE carries the
  object's recall state and, while a recall runs, a retry delay (Retry-After
  on downloads); scrub coverage is exported as metrics.
- `retention.max-clock-step` is installation-only and
  `retention.max-per-pass` is set on a tenant by the installation's admin
  only.
- Documentation for replica storages, moving off a storage (with a check
  that reads the copy without NoPACs), scrub coverage, encryption at rest,
  retention and holds, and the error reasons and retry delays a client
  receives; the security model says what a tenant's admin may grant.
- One condition language for routing and search (internal/query): a bounded
  parser with one printed form; a three-valued evaluator over a DICOM data set
  (absent never matches, a removed attribute is unknown in a dry run, any
  sequence item and any value of a multi-valued attribute, private tags by
  creator, person names by any component group); a compiler to SQL from bound
  parameters and registered columns only, which refuses a search no index
  serves; and the QIDO-RS matching rules mapped onto it.
- Masked keys for AI agents (`nopacs key create --masked`): the server never
  answers a masked key an original value (held headers, quarantine and
  reception UIDs as received, trials and explanations of held files, value
  codes, import lookups and paths, data archived as received, backups and
  exports); a masked key rotates only masked keys and may not choose where
  data goes (send, replicas, storages, senders, forwarding routes); the mark
  is kept through rotation and on every audit entry. For every key, a held
  header and a trial answer no values unless `--show-values`, which is
  audited. `nopacs agent-guide` describes the commands and their JSON for an
  agent.
- `nopacs download` fetches a chosen set: references after the tenant, a file of
  references (`--refs-file`, one `subject[/session[/scan]]` a line), or the scans
  a catalog filter selects (`--modality`, `--description-prefix`, `--receiver`,
  `--archived-since`, `--archived-until`). It holds `--parallel` streams open
  (default 4, at most 16), each a run of consecutive files of one scan, and
  treats the server's refusal past its per-caller read bound as a cue to wait,
  hold fewer streams and try again, not as a failure. `--limit-rate` bounds the
  bytes per second of all streams together. One overall progress (files, bytes,
  rate, time left) shows on a terminal, and `--progress json` writes it as one
  JSON document per line on standard error. `--selection FILE` records the
  files that are whole and checked, replaced atomically, so a stopped download
  of thousands of scans resumes with the same command and skips them without
  reading them again. `--layout bagit` writes a BagIt bag (RFC 8493) with a
  SHA-256 manifest and tag manifest. The interactive program's download screen
  shows the same overall progress.
- Quarantine warnings: tenant status and the status screen warn when the
  oldest held file passes `quarantine.warn-age` or the bytes held pass
  `quarantine.warn-bytes`, with gauges and an event when a warning starts and
  when it ends.
- Storage quotas per tenant: `quota.bytes` and `quota.instances`, set on a
  tenant by the installation's admin only and never by a masked key, bound what
  a tenant holds on its storages, counted in its database as files are recorded
  and purged (deleted instances and superseded revisions count until their
  purge). At the bound a new file is refused as a temporary failure (C-STORE
  A700H, STOW-RS 507, an import waits with "quota reached"); a reception in
  progress finishes; an unreadable count refuses. `quota.warn` (90%) writes one
  `QUOTA` event per crossing and keeps the gauges `nopacs_quota_*`; `nopacs
  tenant show`, `tenant status`, `GetTenant` and `GetStatus` show use and bound.
- The catalog answers a masked key a session or scan archived as received without the values read from its files (UIDs, dates, modality, descriptions), with `values_withheld` set; a filter or order on such a value never matches or leads with those rows, and a DICOMweb search never finds them. The server's log carries no request or answer content at any level. A test starts a real server, receives synthetic files with unmistakable identifiers and drives the API, the DICOMweb and download routes and the command line as masked keys of every grant level, and requires that no original value appears in any answer, error, log, audit entry, event or command output.
- Copy or move a subject or a session to another tenant (`TransferService`,
  `nopacs transfer`): a job in the source tenant reads the archived bytes
  through the checked read and hands them to the target's receiver of type
  `job`, so the target's routing, profile, checks and quota apply; each
  instance is confirmed in the target by checksum, an instance archived as
  received is sent only to a receiver that de-identifies (checked as each file
  is accepted), and the target records its provenance (never a source UID). A
  move deletes the source by the ordinary delete once every instance is
  confirmed, the target is checked again and the delete covers exactly what was
  copied; a hold ends it partial with the copy kept; `transfer.wait` bounds
  waiting on a busy target.
- Proofs from outside the binary (`make prove` and its sections) with tools
  that are not NoPACs: pydicom, dcmtk, curl, openssl, sqlite3, psql,
  dicomweb-client and a viewer.
