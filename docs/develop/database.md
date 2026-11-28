# The databases

[Documentation index](../README.md) · [Architecture](architecture.md) · [Testing](testing.md) · [Gates and outside checks](gates.md) · [Changing a schema](guides/migration.md)

The tables of NoPACs' two kinds of database, drawn. The schema itself is the
baseline file of each database and driver (`0001_baseline.sql` in
`internal/driver/sqlite/migrations/<master|tenant>/` and
`internal/driver/postgres/migrations/<master|tenant>/`); each table has a comment there
that says what it holds and why its constraints exist. These pages show how the tables
relate, which the file's order does not.

## Two kinds of database

An installation keeps one **master database** and one database for each tenant
([where state lives](architecture.md#where-state-lives)).

- The master database holds the tenants, the keys (as hashes), the grants, the
  installation's settings and its audit log
  ([what an installation is](../concepts/archive.md#installation)).
- A tenant database holds everything else about one tenant: its storages, receivers
  and senders, its receptions, the catalog, the quarantine, the work queue and the
  events, its profiles, and its own audit log
  ([what a tenant is](../concepts/archive.md#tenant)).

They are apart because a tenant is an independent archive: what one tenant does never
reads or changes another's, and its database can be copied or handed over whole. For
that, no tenant table holds who may enter the tenant: access lives only in the master
(the tenant schema's own header says so). Each kind can be SQLite or Postgres, chosen
when the database is opened ([choosing a database](../operate/database.md)); the two
drivers have the same tables, with the differences listed [at the end](#where-sqlite-and-postgres-differ).

In the diagrams, a column type is only a hint (`int` is an integer or a `bigint`, `text`
is text). A solid line is a foreign key the database enforces. A dotted line is a column
that names another table's key, with no foreign key behind it: the change that writes it
checks it, inside its transaction. A table drawn with no columns is only there to show
a link from another area. A table's key columns come first; the other columns shown are
the few that explain the table, not all of them.

## The master database

Seven tables. The only foreign keys in either kind of database are the two of `grants`:
a grant refers to a key, and, for a grant on one tenant, to that tenant. `audit` names a
key and a tenant by value with no foreign key, so an entry outlives both.

```mermaid
erDiagram
    tenants {
        text id PK
        text name UK "unique"
        text driver
        blob address "sealed"
        text state "ready, unreachable, upgrading, failed, moved, fenced"
        text state_reason
        int created
    }
    api_keys {
        text id PK
        blob hash "32 bytes, the key is never kept"
        text holder "who or what holds it"
        int created
        int expires
        int revoked
        int suspended
        text successor "a key that replaces this one"
        text networks
        text client_cert
        int masked
    }
    grants {
        text key_id PK, FK
        text scope PK "empty for the installation, else the tenant id"
        text tenant_id FK "null for the installation"
        text role "reader, writer, operator, admin"
    }
    settings {
        text name PK
        text value
    }
    audit {
        text id PK
        int at
        text key_id "names api_keys.id"
        text tenant_name "names tenants.name"
        text method
        text object
        text outcome "done, refused, failed"
        text request_id
        int seq "null until sealed"
    }
    audit_checkpoints {
        int to_seq PK
        int from_seq UK
        blob head
        blob previous
        blob digest UK
        blob signature
        int pruned
    }
    audit_anchors {
        text log PK
        int to_seq
        blob digest
    }
    api_keys ||--o{ grants : "key_id"
    tenants |o--o{ grants : "tenant_id"
    api_keys ||..o{ audit : "key_id, no foreign key"
    tenants ||..o{ audit : "tenant_name, no foreign key"
```

A grant's primary key is `(key_id, scope)`: one role per key and scope, so setting a grant
again replaces the role. `scope` is empty for the installation and the tenant's id
otherwise, and a check keeps `tenant_id` null exactly when `scope` is empty, so the foreign
key can hold for the installation too. `audit.seq` is the entry's place in the log's hash
chain; it is null until a pass seals the entry, and a unique index keeps each place once.
`audit_checkpoints` holds the signed checkpoints of that chain, kept after retention removes
the entries they cover (`pruned`), and `audit_anchors` holds the latest checkpoint of
another log this one keeps.

## A tenant database

A tenant database has 110 tables across the two drivers (109 in each; see
[the differences](#where-sqlite-and-postgres-differ)). **No tenant table refers to another
by a foreign key**: a reception and a quarantine record keep the name of a receiver that
was removed, a job keeps the name of a sender that was removed, and every other reference
is checked by the change that makes it, inside its transaction. So every line in the
diagrams below is dotted. The migration runner also keeps a `schema_migrations` table in
each database; no baseline file creates it, so it is not drawn.

The tenant's tables are drawn in fourteen areas. Every table is in exactly one.

| Area | Tables |
|---|---|
| [Identity, settings and audit](#identity-settings-and-audit) | 6 |
| [Storages, receivers, senders and routing](#storages-receivers-senders-and-routing) | 8 |
| [Receptions and the quarantine](#receptions-and-the-quarantine) | 7 |
| [The catalog](#the-catalog) | 8 |
| [Work and events](#work-and-events) | 5 |
| [De-identification](#de-identification) | 8 |
| [Send jobs](#send-jobs) | 6 |
| [Imports and transfers](#imports-and-transfers) | 8 |
| [Scrub and rebuild](#scrub-and-rebuild) | 5 |
| [Replicas and repair](#replicas-and-repair) | 10 |
| [Corrections](#corrections) | 13 |
| [Rows a delete hides](#rows-a-delete-hides) | 6 |
| [Retention, holds and batch deletes](#retention-holds-and-batch-deletes) | 9 |
| [Search](#search) | 11 |

### Identity, settings and audit

```mermaid
erDiagram
    tenant_identity {
        text id "Postgres only"
    }
    settings {
        text name PK
        text value
    }
    tenant_secrets {
        text name PK
        blob sealed "1 to 1024 bytes"
        int created
    }
    audit {
        text id PK
        int at
        text key_id "a key of the master"
        text method
        text outcome "done, refused, failed"
        int seq "null until sealed"
    }
    audit_checkpoints {
        int to_seq PK
        int from_seq UK
        blob digest UK
        blob signature
        int pruned
    }
    audit_anchors {
        text log PK "master"
        int to_seq
        blob digest
    }
```

A setting here overrides the installation's setting of the same name; a setting absent
here takes the installation's. `tenant_secrets` holds sealed values (a storage's credential,
for one), never plain ones. The audit tables are the master's, with the same columns, so
one entry type and one listing serve both logs; an entry names keys that live in the master,
with no foreign key. `audit.seq` is null until the entry is sealed and never changes once
given, and a unique index keeps each place once; `audit_checkpoints.from_seq` is unique
too, so two seals racing for the same place cannot both be kept. `tenant_identity` exists
only on Postgres: it holds at most one row, the tenant's id, which the driver compares each
time it opens the schema so two tenants cannot share one.

### Storages, receivers, senders and routing

```mermaid
erDiagram
    storages {
        text name PK
        text type "directory or S3"
        text replica_of "names storages.name"
        text tier
        int capacity_bytes
        blob s3_secret "sealed"
    }
    storage_generations {
        text storage PK
        int generation
    }
    storage_totals {
        text storage PK
        int bytes
    }
    storage_owners {
        text storage PK
        int generation
        int written
    }
    receivers {
        text name PK
        text type "dimse, stow, import, job"
        int enabled
        text storage "names storages.name"
        int port
        text config
        text naming
    }
    senders {
        text name PK
        text type
        int enabled
        int allow_identified
        blob credential "sealed"
    }
    route_rule_sets {
        text receiver PK "names receivers.name"
        int version PK
        text body "never changed"
        text created_by
    }
    route_matches {
        text reception_id PK
        text phase PK
        text rule PK
        text receiver "with version, names route_rule_sets"
        int version
    }
    receptions
    storages ||..o| storages : "replica_of"
    storages ||..o{ storage_generations : "storage"
    storages ||..o| storage_totals : "storage"
    storages ||..o| storage_owners : "storage"
    storages ||..o{ receivers : "storage"
    receivers ||..o{ route_rule_sets : "receiver"
    route_rule_sets ||..o{ route_matches : "receiver, version"
    receptions ||..o{ route_matches : "reception_id"
```

A receiver is of type `dimse` on a port of its own, or of type `stow`, `import` or `job` on
no port; a check holds that. `replica_of` makes a storage a replica of another
([replicas and repair](#replicas-and-repair)). `route_rule_sets` holds each version of a
receiver's routing rules, and two triggers (SQLite) or one trigger (Postgres) refuse any
update or delete of a row, so a version never changes and still reads after its receiver is
removed. `route_matches` records which rules a reception was routed by. `storage_owners` holds
the generation of the owner record this database last wrote on each storage, so a server finds
that another installation has taken the storage over; `storage_totals` holds the bytes the
catalog holds on a storage, and `storage_generations` how many switches changed the place a
name names.

### Receptions and the quarantine

```mermaid
erDiagram
    receptions {
        text id PK
        text receiver "names receivers.name"
        text study_uid
        int arrival "arrival number within the study"
        text state "receiving, then the later steps"
        text storage
        text failure
        text retry_of "names receptions.id"
        text route
    }
    reception_steps {
        text state PK "SQLite only"
        int receptions "receptions in that step"
    }
    reception_associations {
        text reception_id PK
        text association PK
    }
    received_files {
        text reception_id PK
        text sop_instance_uid PK
        blob sha256 PK
        text series_uid
        int size
        text incoming "path in the incoming area"
        int released
        int deidentified
        int must_deidentify "the source required it de-identified"
    }
    instance_claims {
        text sop_instance_uid PK
        text reception_id
        blob sha256
        int claimed_at
    }
    quarantine {
        text id PK
        text reason
        text state
        text receiver
        text reception_id
        blob sha256
        text sop_instance_uid
        int content_kept
        text release_mode
    }
    quarantine_totals {
        int files
        int bytes
    }
    receptions ||..o{ reception_associations : "reception_id"
    receptions ||..o{ received_files : "reception_id"
    receptions ||..o{ instance_claims : "reception_id"
    receptions ||..o{ quarantine : "reception_id"
    receptions }o..|| reception_steps : "state"
    receptions ||..o| receptions : "retry_of"
```

A unique index keeps one arrival number per study (`study_uid`, `arrival`), and another
keeps at most one reception of a study in the state `receiving` at one receiver, so two
opens at once make one, in either database. On SQLite, three triggers on `receptions` keep
`reception_steps` (the receptions in each step) in the transaction that writes the state,
so a status reads one row per step instead of counting. The state of a reception and the
reason of a quarantine record have no CHECK, because a later version adds to each.
`received_files.deidentified` says the file was recorded at a receiver that de-identifies, and
`must_deidentify` that its source (a transfer) required it de-identified: the archive step holds
a file with the second flag whose receiver no longer de-identifies, and its quarantine record is
marked de-identified, so it is released only through a receiver that does.
`instance_claims` holds an instance a reception claims before it writes the file, in flight only:
a SOP Instance UID is claimed or held, never both. `quarantine_totals` is a single-row running
total of the files held and their bytes.

### The catalog

```mermaid
erDiagram
    subjects {
        int id PK
        text label
        text folded UK "unique"
        int session_count
        int instance_count
        int bytes
    }
    sessions {
        int id PK
        text study_uid UK
        int subject_id "names subjects.id"
        text label
        text folded
        text storage
        text study_date
        text receiver
        int scan_count
        int as_received "a scan holds an instance archived as received"
    }
    scans {
        int id PK
        int session_id "names sessions.id"
        text series_uid UK
        text label
        text folded
        int instances
        blob manifest_sha256
        text modality
        text series_description
        int as_received "an instance was archived as received"
    }
    session_modalities {
        int session_id PK
        text modality PK
    }
    instances {
        int scan_id PK
        text sop_instance_uid PK
        text reception_id
        blob sha256
        blob archived_sha256
        text storage_key
        int byte_offset
        int byte_length
        int held_at
        int profile_version_id
    }
    reception_scans {
        text reception_id PK
        int scan_id PK
        int instances
        int bytes
    }
    instance_damage {
        text sop_instance_uid PK
        text finding_id
        int marked_at
    }
    catalog_totals {
        int subjects
        int sessions
        int scans
        int instances
    }
    receptions
    profile_versions
    subjects ||..o{ sessions : "subject_id"
    sessions ||..o{ scans : "session_id"
    sessions ||..o{ session_modalities : "session_id"
    scans ||..o{ instances : "scan_id"
    scans ||..o{ reception_scans : "scan_id"
    receptions ||..o{ reception_scans : "reception_id"
    receptions ||..o{ instances : "reception_id"
    profile_versions ||..o{ instances : "profile_version_id"
    instances ||..o| instance_damage : "sop_instance_uid"
```

Subject, session and scan are the three levels of the catalog, and `instances` is where a
file's bytes are found (`storage_key`, `byte_offset`, `byte_length`). A label is unique at
each level, compared in its folded form: `subjects.folded`, `(subject_id, folded)` on
sessions, and unique indexes on `(subject_folded, folded)` and
`(subject_folded, session_folded, folded)` so a session or scan is found by its labels. A
SOP Instance UID is unique across the whole tenant (the unique index `instances_by_uid`).
The counts on a subject, session and scan are columns, and `catalog_totals` is a single-row
total. `as_received` on a scan is set when an instance it holds records no profile version
(it was archived as received), and on a session when one of its scans is: the commit that adds
such an instance sets it, and every write that removes, replaces or changes the profile of an
instance, and a rebuild, works it out again from the rows beneath, in the same transaction.
The catalog's lists for a key that may not learn original values filter and mark by this column
and read no instance. A delete moves these rows to [the hidden tables](#rows-a-delete-hides).

### Work and events

```mermaid
erDiagram
    work {
        text id PK
        text kind
        text subject
        text reception_id
        text unique_key "unique while queued or leased"
        text state "queued, leased, done, failed"
        int due
        int attempt
        int max_attempts
        text owner
        text last_error
    }
    events {
        int id PK
        text type
        int at
        text payload
        text key "unique when set"
    }
    event_marks {
        int pruned_through
    }
    event_checkpoints {
        int at PK
        int through
    }
    event_history {
        text id "this feed's identity"
        int started
    }
    receptions
    receptions ||..o{ work : "reception_id"
```

`work` is the leased queue: a unit is queued, leased to an owner until a time, then done or
failed, and an attempt count and a due time give the retry. A unique index on `unique_key`
over the queued and leased units means a key queues its work once, and a finished unit
frees it. `events` is the feed delivered to subscribers, with its own increasing id; a
unique index on `key` (where set) keeps an event written twice once. `event_marks` holds
how far the feed was pruned, so a reader whose cursor is below it is told its cursor expired.
`event_checkpoints` records, with the server's own clock, the newest event committed at a time,
so an event's age for retention is never the time the event itself carries. `event_history` is
a single row made with the database.

### De-identification

```mermaid
erDiagram
    profiles {
        text name PK
        int latest_version
        int latest_created
    }
    profile_versions {
        int id PK
        text name "names profiles.name"
        int version
        text checksum
        blob source
        blob expanded
        text created_by_key
        text note
    }
    profile_trials {
        text receiver PK
        text profile_name PK
        int profile_version PK
        int files
        int changed
        int held
    }
    lookup_counters {
        text table_name PK
        int last_code
    }
    lookup_codes {
        text table_name PK
        text lookup_key PK
        text code
        int assigned
    }
    pixel_templates {
        text manufacturer_key PK
        text model_key PK
        int image_rows PK
        int image_columns PK
        text sop_class PK
        text rectangles
    }
    deid_coverage_jobs {
        text job_id PK
        int after_scan_id
        int instances
        int elements
        int reported "one report per tenant"
    }
    deid_coverage_elements {
        text job_id PK
        int tag PK
        text creator PK
        int withheld PK
        text vr PK
        int instances
        int occurrences
    }
    instances
    profiles ||..o{ profile_versions : "name"
    profile_versions ||..o{ profile_trials : "profile_name, profile_version"
    profile_versions ||..o{ instances : "profile_version_id"
    lookup_counters ||..o{ lookup_codes : "table_name"
    deid_coverage_jobs ||..o{ deid_coverage_elements : "job_id"
```

A profile version is never changed once written: its document and its expanded form are kept
as the exact bytes their checksum covers, and `id` is what an instance records
(`instances.profile_version_id`) while a person names a version by `(name, version)`, which is
unique. `profile_trials` records what trying a profile version over a receiver's latest receptions found, as counts only. `lookup_codes` holds the
codes assigned for a lookup table, with a unique index so one code is given once in a table,
and `lookup_counters` the last one given. `pixel_templates` hold the rectangles of burned-in
text by manufacturer, model, size and SOP class. A coverage job is a kind of job
([send jobs](#send-jobs)); the unique index on `reported` (where it is 1) keeps one job as the
tenant's report.

### Send jobs

```mermaid
erDiagram
    senders
    jobs {
        text id PK
        text kind "includes deid"
        text sender "names senders.name"
        text subject_label
        text session_label
        text scan_label
        text reception_id
        text unique_key UK "unique"
        text state
        int instances
        int sent
        int warned
        int refused
        text deid_mode
        text profile_name
        int profile_version
    }
    job_scans {
        text job_id PK
        int scan_id PK
        text work_id UK "unique"
        int instances
        int sent
        int refused
        text cursor
    }
    job_resends {
        text job_id PK
        int scan_id PK
        text sop_instance_uid PK
    }
    job_owed {
        text job_id PK
        int scan_id PK
        text sop_instance_uid PK
    }
    job_failures {
        text job_id PK
        text subject_label PK
        text session_label PK
        text scan_label PK
        text sop_instance_uid PK
        text code
        int warning
        text detail
    }
    job_tallies {
        text job_id PK
        text tally PK
        int count
    }
    scans
    work
    senders ||..o{ jobs : "sender"
    jobs ||..o{ job_scans : "job_id"
    jobs ||..o{ job_resends : "job_id"
    jobs ||..o{ job_owed : "job_id"
    jobs ||..o{ job_failures : "job_id"
    jobs ||..o{ job_tallies : "job_id"
    scans ||..o{ job_scans : "scan_id"
    work ||..o| job_scans : "work_id"
```

`jobs` is the common part of every job: a send, an import, a scrub, a repair and the others
all have a row here, and the kind-specific tables in the other areas hold "the job's own
part" keyed by the same `job_id`. A unique index on `unique_key` means a forward is made
once per reception and sender. Checks hold that a job is of kind `deid` exactly when it has a
`deid_mode`, and that its sent, warned and refused counts, and their bytes, never add up to
more than its instances and bytes. `job_scans` is one scan of a job and the unit of work that
carries it (`work_id` is unique). `job_failures` lists the instances that failed or warned, and
`job_tallies` holds a de-identification job's counts per key.

### Imports and transfers

```mermaid
erDiagram
    jobs
    receivers
    receptions
    corrections
    import_jobs {
        text job_id PK
        text receiver "names receivers.name"
        text path
        int found
        int skipped
        int handed_in
        int archived
        int quarantined
        int processing
    }
    import_units {
        text job_id PK
        text unit_id PK
        text work_id UK "unique"
        text folder_key
        text state
        int found
        int handed_in
    }
    import_arrivals {
        text job_id PK
        blob path_key PK
        text unit_id
        text reception_id
        text sop_instance_uid
        text outcome
    }
    import_files {
        text job_id PK
        blob path_key PK
        text kind PK
        int seq "unique within the job"
        text unit_id
        text skip_code
        text quarantine_id
    }
    import_tallies {
        text job_id PK
        text unit_id PK
        text kind PK
        text code PK
        int count
    }
    transfer_jobs {
        text job_id PK
        text target_tenant
        text target_tenant_id
        text receiver "of the target tenant"
        int move
        text step
        text delete_correction "names corrections.id"
    }
    transfer_items {
        text job_id PK
        text subject_label PK
        text session_label PK
        text scan_label PK
        text sop_instance_uid PK
        int revision
        text state
        text target_uid
    }
    transfer_provenance {
        text sop_instance_uid PK
        text source_tenant_id
        text job_id
        int revision
    }
    jobs ||..o| import_jobs : "job_id"
    jobs ||..o| transfer_jobs : "job_id"
    receivers ||..o{ import_jobs : "receiver"
    import_jobs ||..o{ import_units : "job_id"
    import_jobs ||..o{ import_arrivals : "job_id"
    import_jobs ||..o{ import_files : "job_id"
    import_jobs ||..o{ import_tallies : "job_id"
    import_units ||..o{ import_arrivals : "job_id, unit_id"
    receptions ||..o{ import_arrivals : "reception_id"
    transfer_jobs ||..o{ transfer_items : "job_id"
    transfer_jobs ||..o{ transfer_provenance : "job_id"
    corrections ||..o| transfer_jobs : "delete_correction"
```

An import job walks a folder tree in units, one folder each (a unique index on `folder_key`
within the job, and one on `work_id`). Its counts are held by checks: what was found is what
was skipped plus what was handed in, and what was handed in is what was archived, already held,
quarantined, lost, failed, discarded or still processing, on the job and on each unit. A
path an index holds is at most 2048 bytes in both drivers. A transfer job copies a scan list
into another tenant: `target_tenant` and the receiver it names there are named, not enforced,
and the runner reads them again at each step. `transfer_provenance` records, on the target,
where a copied instance came from, and holds no UID of the source.

### Scrub and rebuild

```mermaid
erDiagram
    jobs
    storages
    scans
    scrub_jobs {
        text job_id PK
        text storage "empty for every storage"
        int scheduled
    }
    scrub_findings {
        text id PK
        text kind
        text storage
        text object_key
        text sop_instance_uid
        blob expected_sha256
        blob found_sha256
        text job_id
        int found_at
        int cleared_at
        text replica_of
    }
    scrub_verified {
        text storage PK
        text series_uid PK
        int verified_at
        int presence_at
    }
    rebuild_jobs {
        text job_id PK
        text mode
        text scratch
        int activated
        int dropped
        text from_storage "only for compare"
    }
    rebuild_differences {
        text job_id PK
        int seq PK
        text table_name
        text kind
        text row_key
    }
    jobs ||..o| scrub_jobs : "job_id"
    jobs ||..o| rebuild_jobs : "job_id"
    storages ||..o{ scrub_jobs : "storage"
    scrub_jobs ||..o{ scrub_findings : "job_id"
    storages ||..o{ scrub_findings : "storage"
    storages ||..o{ scrub_verified : "storage"
    scans ||..o{ scrub_verified : "series_uid"
    rebuild_jobs ||..o{ rebuild_differences : "job_id"
```

A scrub job reads every stored object of one storage (or of every storage) again and compares
it with the catalog and the scan's manifest; a finding is one disagreement. A unique index on
`(storage, object_key, kind)` over findings not yet cleared keeps one open finding per object
and kind. A check holds that a finding with a clearing job is cleared. `scrub_verified` is the
coverage: when each scan was last verified on each storage. A rebuild job makes the catalog again from what the storages hold
into a shadow catalog, never into the one in use: in compare mode the shadow is compared with the
catalog and dropped, and `rebuild_differences` keeps the first 1000 differences; in replace mode one
transaction makes the shadow the catalog, and sets `activated`, which never changes after.

### Replicas and repair

```mermaid
erDiagram
    storages
    scans
    scrub_findings
    replica_totals {
        text replica PK
        int pending
        int copying
        int damaged
        int backfill
        text last_fault
    }
    replica_scans {
        text replica PK
        text series_uid PK
        text state "pending, copying, in_step, damaged"
        int queued_at
        int due
        text owner
        int lease_until
        int verified_at
    }
    replica_damage {
        text replica PK
        text series_uid PK
        text sop_instance_uid PK
        int revision
        text finding_id
    }
    scan_sidecars {
        text series_uid PK
        blob manifest_sha256
        blob sha256
    }
    recalls {
        text storage PK
        text object_key PK
        text state
        int online_until
    }
    repairs {
        text id PK
        text finding_id
        text storage
        text object_key
        text from_replica
        text job_id
        int repaired_at
        text outcome
    }
    repair_jobs {
        text job_id PK
        text storage
        text from_replica
        text findings "JSON array of finding ids"
        int automatic
    }
    replicate_jobs {
        text job_id PK
        text replica
        text primary_name
        int scheduled
    }
    repair_outcomes {
        text job_id PK
        text finding_id PK
        text outcome
        int bytes
    }
    storage_switches {
        text id PK
        text primary_name
        text replica_name
        text state
        text step
        text confirm_token UK "unique"
        int generation
        int ended_at
    }
    storages ||..o| replica_totals : "replica"
    storages ||..o{ replica_scans : "replica"
    scans ||..o{ replica_scans : "series_uid"
    storages ||..o{ replica_damage : "replica"
    scrub_findings ||..o{ replica_damage : "finding_id"
    scans ||..o| scan_sidecars : "series_uid"
    storages ||..o{ recalls : "storage"
    scrub_findings ||..o{ repairs : "finding_id"
    repair_jobs ||..o{ repairs : "job_id"
    repair_jobs ||..o{ repair_outcomes : "job_id"
    storages ||..o{ replicate_jobs : "replica"
    storages ||..o{ storage_switches : "primary_name, replica_name"
```

A replica is a storage with `replica_of` set, and `replica_scans` is the queue of scans it
still has to copy: checks hold that a row is `in_step` exactly when it has no `queued_at`, and
`copying` exactly when it has a `lease_until`. `repairs` holds one mend of one damaged object,
with checks that tie its outcome to its `repaired_at` and keep its open work, attempt and hold
time all set or all null; a finished repair holds no open work. `repair_jobs` and
`replicate_jobs` are the own parts of those job kinds. `scan_sidecars` holds the digest of each scan's metadata file beside the manifest digest it was
written with. `recalls` holds one recall per object of an archive-tier storage, shared by every
request waiting for it. `storage_switches` is the move of a primary to a replica; its confirm token is
unique and checks tie its ended and swapped times to its state.

### Corrections

```mermaid
erDiagram
    corrections {
        text id PK
        text kind
        text level
        text state "prepared, committed, purging, purged, undone, abandoned"
        text origin "person or retention"
        text subject_label
        text session_label
        text scan_label
        text instance_uid
        text to_label
        text confirm_token UK "unique when set"
        int committed_at
        int purge_after
        int ended_at
        text retention_rule
    }
    correction_scans {
        text correction_id PK
        text series_uid PK
        text storage
        text new_subject
        int instances
        int bytes
    }
    correction_keys {
        text correction_id PK
        int ord PK
        text storage PK
        text sop_instance_uid PK
        text storage_key
        text outcome
    }
    correction_objects {
        text correction_id PK
        text storage PK
        int generation PK
        text object_key PK
        text version PK
        text disposition
        int retain_until
        int purged_at
    }
    correction_records_owed {
        text correction_id PK
        text storage PK
        int owed_at
    }
    correction_copies_abandoned {
        text correction_id PK
        text storage PK
        text reason
    }
    deleted_targets {
        text kind PK
        text value PK
        text correction_id PK
    }
    instance_revisions {
        text sop_instance_uid PK
        int revision PK
        blob sha256
        text storage_key
        text correction_id
        text fate
        int purged
    }
    correction_edits {
        text correction_id PK
        text sop_instance_uid PK
        int old_revision
        int new_revision
        blob old_sha256
        blob new_sha256
    }
    correction_edit_scans {
        text correction_id PK
        text series_uid PK
        text modality
        text series_description
    }
    relabels {
        text correction_id PK
        text level
        text new_label
        text old_subject
    }
    purge_totals {
        int instances
        int bytes
    }
    correction_clock {
        int one PK
        int trusted "last time the clock was trusted"
    }
    corrections ||..o{ correction_scans : "correction_id"
    corrections ||..o{ correction_keys : "correction_id"
    corrections ||..o{ correction_objects : "correction_id"
    corrections ||..o{ correction_records_owed : "correction_id"
    corrections ||..o{ correction_copies_abandoned : "correction_id"
    corrections ||..o{ deleted_targets : "correction_id"
    corrections ||..o{ instance_revisions : "correction_id"
    corrections ||..o{ correction_edits : "correction_id"
    corrections ||..o{ correction_edit_scans : "correction_id"
    corrections ||..o| relabels : "correction_id"
```

A correction is an edit, a relabel, a move or a delete, and it goes forward from `prepared` to
`committed`, then to `purging` and `purged`, or back to `undone` or `abandoned`. Checks hold
that a committed correction has a purge time and one not committed does not, that a correction
is ended exactly when it is purged, undone or abandoned, that it came from retention exactly
when it names a retention rule, and that an override of retention carries its reason.
`instance_revisions` keeps every revision of an instance so an edit can be undone until its
purge; `correction_objects` lists the stored objects a correction replaced and when each may be
purged. `purge_totals` is a single-row total and `correction_clock` is the last time the
server's clock was trusted for an automatic purge.

### Rows a delete hides

```mermaid
erDiagram
    corrections
    hidden_subjects {
        text correction_id PK
        int id PK
        text label
        int session_count
        int instance_count
    }
    hidden_sessions {
        text correction_id PK
        int id PK
        text study_uid
        int subject_id "names hidden_subjects.id"
        text label
        int scan_count
    }
    hidden_scans {
        text correction_id PK
        int id PK
        int session_id "names hidden_sessions.id"
        text series_uid
        text label
        int instances
    }
    hidden_instances {
        text correction_id PK
        text sop_instance_uid PK
        int scan_id "names hidden_scans.id"
        blob sha256
        text storage_key
        int byte_offset
    }
    hidden_session_modalities {
        text correction_id PK
        int session_id PK
        text modality PK
    }
    hidden_reception_scans {
        text correction_id PK
        text reception_id PK
        int scan_id PK
        int instances
    }
    corrections ||..o{ hidden_subjects : "correction_id"
    corrections ||..o{ hidden_sessions : "correction_id"
    corrections ||..o{ hidden_scans : "correction_id"
    corrections ||..o{ hidden_instances : "correction_id"
    corrections ||..o{ hidden_session_modalities : "correction_id"
    corrections ||..o{ hidden_reception_scans : "correction_id"
    hidden_subjects ||..o{ hidden_sessions : "subject_id"
    hidden_sessions ||..o{ hidden_scans : "session_id"
    hidden_scans ||..o{ hidden_instances : "scan_id"
    hidden_sessions ||..o{ hidden_session_modalities : "session_id"
    hidden_scans ||..o{ hidden_reception_scans : "scan_id"
```

When a delete commits, its transaction moves the catalog rows it hides into these tables, each
with the same columns as its catalog table plus the correction's id. No read of the catalog
sees them, an undo moves them back as they were, and a purge removes them. They take no CHECK
of their own, because they hold only rows the catalog held. The search rows a delete hides are
in [search](#search).

### Retention, holds and batch deletes

```mermaid
erDiagram
    sessions
    storages
    corrections
    retention_rules {
        text name PK
        text receiver
        text route
        text storage
        int keep_years
        int keep_days
        int dispose_years
        int dispose_days
        text action
    }
    retention_sessions {
        text study_uid PK
        int start_at
        int keep_until
        int due_at
        text rule "names retention_rules.name"
        text action
    }
    retention_state {
        int trusted
        int paused
        text pause_reason
        int last_pass
    }
    holds {
        text id PK
        text level
        text subject_label
        text session_label
        text scan_label
        text state
        text placed_by
        int released_at
    }
    hold_records {
        text hold_id PK
        text storage PK
        int written_seq
    }
    dispose_jobs {
        text job_id PK
        text rule
        int scheduled
        text confirm_token UK "unique when set"
    }
    dispose_sessions {
        text job_id PK
        int ord PK
        text study_uid
        int due_at
        text outcome
        text correction_id
    }
    delete_batch_jobs {
        text job_id PK
        text reason
        text confirm_token UK "unique"
        int paused
    }
    delete_batch_targets {
        text job_id PK
        int ord PK
        text subject_label
        text session_label
        text scan_label
        text outcome
        text correction_id
    }
    sessions ||..o| retention_sessions : "study_uid"
    retention_rules ||..o{ retention_sessions : "rule"
    retention_rules ||..o{ dispose_jobs : "rule"
    dispose_jobs ||..o{ dispose_sessions : "job_id"
    corrections ||..o| dispose_sessions : "correction_id"
    holds ||..o{ hold_records : "hold_id"
    storages ||..o{ hold_records : "storage"
    delete_batch_jobs ||..o{ delete_batch_targets : "job_id"
    corrections ||..o| delete_batch_targets : "correction_id"
```

A retention rule says how long to keep a session and when to dispose of it; a rule must set at
least one of its four durations. `retention_sessions` holds the clock of each session, and a
check holds that a session has a due time exactly when it has a rule. A dispose job and a batch
delete job each list their sessions or targets and record the correction made for each in
`correction_id`. A hold names a subject, session or scan by its labels, as the catalog has them now; checks hold that a hold is released exactly when it has a release time, and that only
an active hold can be releasing. `hold_records` says on which storages the hold's record goes and the count it was last written at.
`retention_state` is a single-row state of the daily pass, and a check ties `paused` to its
time. A dispose job made by a person carries the confirm token of its preview, used once, and
the daily pass has none.

### Search

```mermaid
erDiagram
    scans
    corrections
    jobs
    scan_search {
        int scan_id PK
        text source_uid
        text modality
        text series_description
        text protocol_name
        text manufacturer
        text patient_sex
        real magnetic_field_strength
        real echo_time
        text acquisition_date
    }
    scan_search_image_type {
        int scan_id
        text image_type
        text image_type_folded
    }
    scan_search_scanning_sequence {
        int scan_id
        text scanning_sequence
    }
    scan_search_operators_name {
        int scan_id
        text operators_name
    }
    hidden_scan_search {
        text correction_id
        int scan_id
        text modality
        text series_description
    }
    hidden_scan_search_image_type {
        text correction_id
        int scan_id
        text image_type
    }
    hidden_scan_search_scanning_sequence {
        text correction_id
        int scan_id
        text scanning_sequence
    }
    hidden_scan_search_operators_name {
        text correction_id
        int scan_id
        text operators_name
    }
    correction_search {
        text correction_id PK
        text series_uid PK
        text prior
    }
    search_attributes {
        text field PK
        text state
    }
    search_backfill_jobs {
        text job_id PK
        text fields
        int after_scan_id
        int skipped
    }
    scans ||..o| scan_search : "scan_id"
    scan_search ||..o{ scan_search_image_type : "scan_id"
    scan_search ||..o{ scan_search_scanning_sequence : "scan_id"
    scan_search ||..o{ scan_search_operators_name : "scan_id"
    corrections ||..o{ hidden_scan_search : "correction_id"
    hidden_scan_search ||..o{ hidden_scan_search_image_type : "correction_id, scan_id"
    hidden_scan_search ||..o{ hidden_scan_search_scanning_sequence : "correction_id, scan_id"
    hidden_scan_search ||..o{ hidden_scan_search_operators_name : "correction_id, scan_id"
    corrections ||..o{ correction_search : "correction_id"
    jobs ||..o| search_backfill_jobs : "job_id"
```

`scan_search` holds one row per scan with the header values a search reads, each text value
with a folded copy for case-insensitive compare; the three child tables hold the attributes
that have several values (image type, scanning sequence, operators' names). The hidden tables
are what a delete moves out, as the hidden catalog tables are. `correction_search` keeps the edits a correction made to a scan's row, as the row was before, so an
undo gives the values back. The tables with a `scan_id` and no primary key are indexed by it and by value. Three text columns are also searched by
substring: SQLite keeps three trigram full-text tables beside `scan_search`, with three
triggers that keep them in step with every write, and Postgres uses trigram indexes on the
same columns. `search_attributes` records which attributes are ready to search, and the
backfill job fills them for scans archived before.

## Where SQLite and Postgres differ

The two drivers have the same tables and columns, with the exceptions below. Both reject a
wrong type, and the contract suite runs unchanged against both.

| | SQLite | Postgres |
|---|---|---|
| Tables | `STRICT`, so a value of the wrong type is refused; 61 tenant tables are also `WITHOUT ROWID` | plain tables |
| Times | `INTEGER` microseconds since 1970 in UTC, null for "never" | `timestamptz`, read back in UTC |
| Checksums | `BLOB` with a `CHECK` on its length | `bytea` with a `CHECK` on its length |
| Flags | `INTEGER` of 0 or 1 with a `CHECK` | `boolean` |
| Counts and sizes | `INTEGER` | `bigint` (or `integer`) |
| Row ids | `INTEGER PRIMARY KEY` | `bigint GENERATED ALWAYS AS IDENTITY` |
| Text order and uniqueness | byte by byte by default | every compared name, id or UID is `COLLATE "C"`, to match |
| Audit `details` | `TEXT` | `jsonb` |
| Constraints | unnamed | named, so the driver tells a violation by its name |
| Single-row tables | `one INTEGER PRIMARY KEY CHECK (one = 1)` | `only_row boolean` in `quarantine_totals`, `catalog_totals`, `event_marks`, `event_history` and `purge_totals`; `one bigint` in `correction_clock` and `retention_state` |
| `reception_steps` | a table kept by three triggers on `receptions` | not made |
| `tenant_identity` | not made | a one-row table naming the tenant that owns the schema |
| Rule set versions never change | two triggers (before update, before delete) | one trigger function (before update or delete) |
| Substring search of scans | three `fts5` trigram virtual tables, with three triggers keeping them in step | the `pg_trgm` extension and three GIN indexes |
| Foreign keys | the master's two, with `foreign_keys` turned on for each connection | the master's two |

So the tenant has 109 tables in each driver and 110 across both: `reception_steps` on SQLite and
`tenant_identity` on Postgres each have no counterpart. The master's seven tables are the same
in both.

## How the schema is checked

`make check-tenant-sqlite` creates a tenant on SQLite and reads its tables, constraints and
rows back with the `sqlite3` tool ([scripts/check/tenant-sqlite.sh](../../scripts/check/tenant-sqlite.sh)),
and `make check-tenant-postgres` does the same with `psql` and `pg_dump` against a Postgres
you name ([scripts/check/tenant-postgres.sh](../../scripts/check/tenant-postgres.sh)); the master's
are `make check-master-sqlite` and `make check-master-postgres`
([gates and outside checks](gates.md)).
