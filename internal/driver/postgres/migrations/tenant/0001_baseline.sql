-- A tenant database's schema on Postgres, whole: every table once, grouped
-- by what uses it. No table refers to another by a foreign key, so the
-- groups follow the order a reader meets them in: the tenant's identity,
-- settings and secrets, the audit log, the storages and receivers,
-- receptions and the quarantine, the catalog, the leased units of work, the
-- event feed, de-identification, jobs and their kinds, corrections, and
-- replicas with their recalls, repairs and switches.
--
-- It holds what the SQLite schema holds, in Postgres types: times are
-- timestamptz to the microsecond, read back in UTC; the audit entry's
-- details are jsonb; a checksum is a bytea of exactly 32 bytes, compared
-- byte by byte; a flag is boolean. JSON that is stored and returned whole
-- and never searched (a receiver's configuration, a reception's calling AE
-- titles, a quarantine record's details, an event's payload) is text. Text a
-- caller did not give is '' rather than NULL. Every identifier, name and UID
-- that is compared, ordered or unique is COLLATE "C", so order and
-- uniqueness are byte by byte, as SQLite's are, whatever collation the
-- database was made with. Every table is made in the schema the
-- connection's search_path starts with.
--
-- There are no foreign keys: a reception and a quarantine record keep the
-- name of a receiver removed, and a job the name of a sender removed, by the
-- API contract's rule, and every other reference is checked by the change
-- that makes it, inside its transaction, under the row locks the driver's
-- documentation lists. Counts, sizes, states the code relies on and
-- checksum lengths are held by CHECK constraints, named so the driver tells
-- a violation by its name; the state of a reception, the reason of a
-- quarantine record and the type of an event are the core's and the
-- contract's words and have none, since a later version adds to each.
--
-- No table holds who may enter the tenant: access lives only in the master,
-- so this database can be copied or handed over without carrying it.
--
-- Each list, claim and lookup has the index named for it below; the plan
-- tests (pipeline_plan_test.go) show the planner uses it at millions of rows.
--
-- The runner records this file's SHA-256 when it makes a database, and
-- refuses to open one whose recorded SHA-256 differs: a database made from
-- another version of this file was made by another build, and is never
-- half-migrated to this one. A later change to the schema is a new file
-- after this one, applied once and never edited.

-- Which tenant this schema belongs to. The driver writes the row the first
-- time it migrates the schema and compares it every time it opens it, so two
-- tenants pointed at one database and schema, or an address pasted for the
-- wrong tenant, are refused instead of sharing (and mixing) one set of
-- settings and one audit log. At most one row: only_row is always true.
CREATE TABLE tenant_identity (
    only_row boolean NOT NULL DEFAULT true,
    id       text    COLLATE "C" NOT NULL,
    CONSTRAINT tenant_identity_pkey PRIMARY KEY (only_row),
    CONSTRAINT tenant_identity_one_row CHECK (only_row)
);

-- A setting here overrides the installation's value of the same name; a
-- setting absent here takes the installation's.
CREATE TABLE settings (
    name  text COLLATE "C" NOT NULL,
    value text NOT NULL,
    CONSTRAINT settings_pkey PRIMARY KEY (name)
);

-- The tenant's own secrets, such as the key of the keyed hash its naming
-- rules use, each sealed with the installation key by the server before it
-- is stored: this database never holds a plaintext secret, and a copy of it
-- opens nothing without the installation's key file. A secret is written
-- once and never replaced, since what was made with it depends on it.
CREATE TABLE tenant_secrets (
    name    text        NOT NULL,
    sealed  bytea       NOT NULL,
    created timestamptz NOT NULL,
    CONSTRAINT tenant_secrets_pkey PRIMARY KEY (name),
    CONSTRAINT tenant_secrets_sealed_check CHECK (length(sealed) BETWEEN 1 AND 1024)
);

-- The tenant's audit log, with the master's columns, so one entry type and
-- one listing serve both logs. No foreign key: an entry names keys that live
-- in the master. details is a JSON object of strings. seq is the entry's
-- place in the log's hash chain, given when the entry is sealed, not when it
-- is written, since entries written at once commit in no fixed order (a
-- transaction begun first may commit last); it is NULL until then and never
-- changes once given (the store's SealAudit sets it only where it is NULL).
CREATE TABLE audit (
    id          text        COLLATE "C" NOT NULL,
    at          timestamptz NOT NULL,
    key_id      text        COLLATE "C" NOT NULL,
    local_user  text        NOT NULL,
    remote_addr text        NOT NULL,
    method      text        NOT NULL,
    tenant_name text        COLLATE "C" NOT NULL,
    object      text        NOT NULL,
    outcome     text        NOT NULL,
    reason      text        NOT NULL,
    details     jsonb       NOT NULL,
    request_id  text        NOT NULL,
    seq         bigint,
    CONSTRAINT audit_pkey PRIMARY KEY (id),
    CONSTRAINT audit_outcome_check CHECK (outcome IN ('done', 'refused', 'failed')),
    CONSTRAINT audit_details_check CHECK (jsonb_typeof(details) = 'object'),
    CONSTRAINT audit_seq_check CHECK (seq >= 1)
);

-- The audit listings, newest first, with the master's indexes (the same
-- listing code serves both):
--   no filter, or a time range only          audit_by_time
--   a tenant name (with or without a range)   audit_by_tenant
--   a key (with or without a range)           audit_by_key
--   an outcome (with or without a range), so a rare outcome such as
--   "failed" is found without reading every entry   audit_by_outcome
-- A start and an end time narrow the range read from whichever index is
-- used. Two equality filters together (a tenant and a key, or either with an
-- outcome) use the index of one of them, chosen by the planner, and test the
-- other on the rows that index yields. In a tenant's log the tenant name is
-- usually the tenant's own, so its index matters less than in the master's;
-- it is kept so one query plan serves both.
CREATE INDEX audit_by_time    ON audit (at, id);
CREATE INDEX audit_by_tenant  ON audit (tenant_name, at, id);
CREATE INDEX audit_by_key     ON audit (key_id, at, id);
CREATE INDEX audit_by_outcome ON audit (outcome, at, id);

-- One entry per place in the chain. NULLs are distinct in a unique index,
-- so every unsealed entry fits.
CREATE UNIQUE INDEX audit_by_seq ON audit (seq);

-- The unsealed entries, in the order a pass seals them, without reading the
-- sealed ones: the index holds only the rows whose seq is NULL, so it stays
-- as small as one interval's entries however long the log grows.
CREATE INDEX audit_unsealed ON audit (at, id) WHERE seq IS NULL;

-- The signed checkpoints of the tenant's audit chain, the same as the
-- master's: one row per checkpoint, kept also once retention has removed
-- the entries it covers (pruned), so the chain is still checked across
-- them. The signed text is rebuilt from the columns; digest is its SHA-256,
-- kept so an anchor or a digest known from outside is found by the index of
-- its unique constraint. from_seq is unique too, so two seals racing for the
-- same place cannot both be kept.
CREATE TABLE audit_checkpoints (
    to_seq       bigint      NOT NULL,
    from_seq     bigint      NOT NULL,
    installation text        COLLATE "C" NOT NULL,
    log          text        COLLATE "C" NOT NULL,
    head         bytea       NOT NULL,
    previous     bytea       NOT NULL,
    digest       bytea       NOT NULL,
    at           timestamptz NOT NULL,
    key_version  integer     NOT NULL,
    signature    bytea       NOT NULL,
    pruned       boolean     NOT NULL DEFAULT false,
    CONSTRAINT audit_checkpoints_pkey PRIMARY KEY (to_seq),
    CONSTRAINT audit_checkpoints_from_key UNIQUE (from_seq),
    CONSTRAINT audit_checkpoints_digest_key UNIQUE (digest),
    CONSTRAINT audit_checkpoints_range_check CHECK (from_seq >= 1 AND to_seq >= from_seq),
    CONSTRAINT audit_checkpoints_sizes_check CHECK (octet_length(head) = 32 AND octet_length(previous) = 32
        AND octet_length(digest) = 32 AND octet_length(signature) = 64),
    CONSTRAINT audit_checkpoints_key_version_check CHECK (key_version >= 1)
);

-- The checkpoints retention has still to reach, oldest first.
CREATE INDEX audit_checkpoints_unpruned ON audit_checkpoints (to_seq) WHERE NOT pruned;

-- The latest checkpoint of another log this one keeps: the master's (log
-- is "master").
CREATE TABLE audit_anchors (
    log    text   COLLATE "C" NOT NULL,
    to_seq bigint NOT NULL,
    digest bytea  NOT NULL,
    CONSTRAINT audit_anchors_pkey PRIMARY KEY (log),
    CONSTRAINT audit_anchors_check CHECK (to_seq >= 1 AND octet_length(digest) = 32)
);

-- A storage: a directory under a root the installation names, or a bucket of
-- an S3 store. The columns of the other type are ''/false. The secret access
-- key is sealed by the caller and never read back by a read operation:
-- s3_secret is selected only as whether it is set.
CREATE TABLE storages (
    name             text        COLLATE "C" NOT NULL,
    type             text        COLLATE "C" NOT NULL,
    dir_root         text        COLLATE "C" NOT NULL,
    dir_path         text        COLLATE "C" NOT NULL,
    s3_endpoint      text        COLLATE "C" NOT NULL,
    s3_region        text        NOT NULL,
    s3_bucket        text        COLLATE "C" NOT NULL,
    s3_prefix        text        COLLATE "C" NOT NULL,
    s3_path_style    boolean     NOT NULL,
    s3_access_key_id text        NOT NULL,
    s3_secret        bytea,
    -- true when a purge leaves the older versions of an object on a
    -- versioned bucket (S3Storage.purge_versions off); false, the default,
    -- deletes them.
    s3_keep_versions boolean     NOT NULL DEFAULT false,
    capacity_bytes   bigint      NOT NULL,
    created          timestamptz NOT NULL,
    updated          timestamptz NOT NULL,
    -- The primary this storage is a replica of, '' for none; its tier; for
    -- the archive tier, the days and speed of a recall (0 and '' online);
    -- and a directory's statement, made by an admin, that its volume
    -- encrypts at rest (an S3 store is asked instead).
    replica_of       text        COLLATE "C" NOT NULL DEFAULT '',
    tier             text        COLLATE "C" NOT NULL DEFAULT 'online',
    s3_recall_days   integer     NOT NULL DEFAULT 0,
    s3_recall_tier   text        COLLATE "C" NOT NULL DEFAULT '',
    dir_encrypted    boolean     NOT NULL DEFAULT false,
    CONSTRAINT storages_pkey PRIMARY KEY (name),
    CONSTRAINT storages_type_check CHECK (type IN ('directory', 's3')),
    CONSTRAINT storages_capacity_check CHECK (capacity_bytes >= 0),
    CONSTRAINT storages_tier_check CHECK (tier IN ('online', 'archive')),
    CONSTRAINT storages_recall_check CHECK (s3_recall_days BETWEEN 0 AND 365
        AND s3_recall_tier IN ('', 'standard', 'bulk', 'expedited'))
);

-- The overlap checks of a change read the storages of one root, or of one
-- endpoint and bucket, and no other.
CREATE INDEX storages_by_root   ON storages (dir_root) WHERE type = 'directory';
CREATE INDEX storages_by_bucket ON storages (s3_endpoint, s3_bucket) WHERE type = 's3';

-- A primary's replicas, which every commit to it reads to queue its copies:
-- the index holds the replicas alone, so a tenant without any reads nothing.
CREATE INDEX storages_by_primary ON storages (replica_of) WHERE replica_of <> '';

-- How many switches changed the place each name names. A row outlives its
-- storage, so a name removed and added again goes on from where it was and
-- a generation never names two places. A name with no row is at 0.
CREATE TABLE storage_generations (
    storage    text   COLLATE "C" NOT NULL,
    generation bigint NOT NULL,
    CONSTRAINT storage_generations_pkey PRIMARY KEY (storage),
    CONSTRAINT storage_generations_check CHECK (generation >= 0)
);

-- The bytes the catalog holds on each storage, as running totals updated in
-- the transaction of each commit, so a status reads them in its snapshot and
-- never sums the instances per call. A storage with none has no row and
-- holds nothing.
CREATE TABLE storage_totals (
    storage text   COLLATE "C" NOT NULL,
    bytes   bigint NOT NULL,
    CONSTRAINT storage_totals_pkey PRIMARY KEY (storage),
    CONSTRAINT storage_totals_bytes_check CHECK (bytes >= 0)
);

-- The owner generation the tenant holds for each of its storages: the
-- generation of the owner record (meta/.owner.json) this database last
-- wrote there. An import or a restore takes a storage over by writing a
-- record one generation higher, so a server whose database holds a lower
-- generation than the record finds another installation writes there now.
-- written is when this database's record was written. A storage with no row
-- has no record of this database's yet. The upper bound is the largest
-- integer a JSON number holds exactly, as the record writes it.
CREATE TABLE storage_owners (
    storage    text        COLLATE "C" NOT NULL,
    generation bigint      NOT NULL,
    written    timestamptz NOT NULL,
    CONSTRAINT storage_owners_pkey PRIMARY KEY (storage),
    CONSTRAINT storage_owners_generation_check CHECK (generation >= 1 AND generation <= 9007199254740991)
);

-- A receiver. Its protocol's configuration (a DIMSE receiver's callers, TLS,
-- SOP classes and transfer syntaxes; a stow receiver's keys and largest
-- request; an import receiver's root, layout and map) and its naming rule
-- are JSON, stored and returned whole; the port and bind address are
-- columns as well, since a change checks them against the other receivers.
-- A stow receiver takes instances stored over HTTP into the tenant's
-- DICOMweb service (PS3.18 10.5) and an import receiver is handed files by
-- its import jobs: neither listens on a port of its own, so each has port 0
-- and an empty bind address.
CREATE TABLE receivers (
    name           text        COLLATE "C" NOT NULL,
    type           text        COLLATE "C" NOT NULL,
    enabled        boolean     NOT NULL,
    storage        text        COLLATE "C" NOT NULL,
    port           integer     NOT NULL,
    bind_address   text        COLLATE "C" NOT NULL,
    config         text        NOT NULL,
    naming         text        NOT NULL,
    quiet_us       bigint      NOT NULL,
    max_open_us    bigint      NOT NULL,
    max_file_bytes bigint      NOT NULL,
    created        timestamptz NOT NULL,
    updated        timestamptz NOT NULL,
    CONSTRAINT receivers_pkey PRIMARY KEY (name),
    CONSTRAINT receivers_type_check CHECK (type IN ('dimse', 'stow', 'import')),
    CONSTRAINT receivers_port_check CHECK ((type = 'dimse' AND port BETWEEN 1 AND 65535)
        OR (type IN ('stow', 'import') AND port = 0 AND bind_address = ''))
);

-- Removing a storage asks whether a receiver names it; adding or changing a
-- receiver asks which receivers listen on its port. The stow receivers
-- share port 0, so a change of one reads the others by it, to refuse a key
-- two of them list.
CREATE INDEX receivers_by_storage ON receivers (storage);
CREATE INDEX receivers_by_port    ON receivers (port);

-- A reception: the files of one study arriving at one receiver. state is the
-- core's text, kept as it is; ended is whether that state is an end, as the
-- core said when it was written, so "in progress" stays right when a later
-- version adds a state. calling_ae_titles is a JSON array of at most 16.
-- route is what the receiver's routing chose for the study (the route, and
-- the profile version and senders it names in place of the receiver's),
-- kept when the reception is identified, so a step run again, and the steps
-- after, go by the same choice whatever the receiver's routes say by then;
-- a storage or profile a release named is kept there too; '' for nothing of
-- its own.
CREATE TABLE receptions (
    id                text        COLLATE "C" NOT NULL,
    receiver          text        COLLATE "C" NOT NULL,
    study_uid         text        COLLATE "C" NOT NULL,
    arrival           integer     NOT NULL,
    state             text        COLLATE "C" NOT NULL,
    ended             boolean     NOT NULL,
    files_received    bigint      NOT NULL,
    bytes_received    bigint      NOT NULL,
    files_archived    bigint      NOT NULL,
    bytes_archived    bigint      NOT NULL,
    files_duplicate   bigint      NOT NULL,
    bytes_duplicate   bigint      NOT NULL,
    files_quarantined bigint      NOT NULL,
    bytes_quarantined bigint      NOT NULL,
    files_released    bigint      NOT NULL,
    subject_label     text        COLLATE "C" NOT NULL,
    session_label     text        COLLATE "C" NOT NULL,
    storage           text        COLLATE "C" NOT NULL,
    failure           text        NOT NULL,
    started           timestamptz NOT NULL,
    last_file         timestamptz NOT NULL,
    step_at           timestamptz NOT NULL,
    ended_at          timestamptz,
    associations      bigint      NOT NULL,
    calling_ae_titles text        NOT NULL,
    calling_truncated boolean     NOT NULL,
    retry_of          text        COLLATE "C" NOT NULL,
    retry_reception   text        COLLATE "C" NOT NULL,
    discard_reason    text        NOT NULL,
    discard_at        timestamptz,
    route             text        NOT NULL DEFAULT '',
    CONSTRAINT receptions_pkey PRIMARY KEY (id),
    CONSTRAINT receptions_arrival_check CHECK (arrival >= 1),
    CONSTRAINT receptions_counts_check CHECK (files_received >= 0 AND bytes_received >= 0 AND files_archived >= 0 AND
        bytes_archived >= 0 AND files_duplicate >= 0 AND bytes_duplicate >= 0 AND files_quarantined >= 0 AND
        bytes_quarantined >= 0 AND files_released >= 0 AND associations >= 0),
    CONSTRAINT receptions_ended_check CHECK (ended = (ended_at IS NOT NULL))
);

-- One arrival number per study in the tenant; the next one is read from here.
CREATE UNIQUE INDEX receptions_arrival ON receptions (study_uid, arrival);
-- At most one reception of a study is receiving at one receiver: two opens at
-- once make one.
CREATE UNIQUE INDEX receptions_open ON receptions (receiver, study_uid) WHERE state = 'receiving';
-- The listings, newest first by start time, then id:
--   no filter, or a time range only   receptions_by_start
--   a receiver                        receptions_by_receiver
--   a study                           receptions_by_study
--   steps                             receptions_by_state
--   in progress                       receptions_in_progress (only those)
-- and the combinations a tenant with many receptions would otherwise answer
-- by testing each row of one index: a receiver in progress, a receiver with
-- steps, a study with steps.
CREATE INDEX receptions_by_start             ON receptions (started, id);
CREATE INDEX receptions_by_receiver          ON receptions (receiver, started, id);
CREATE INDEX receptions_by_study             ON receptions (study_uid, started, id);
CREATE INDEX receptions_by_state             ON receptions (state, started, id);
CREATE INDEX receptions_in_progress          ON receptions (started, id) WHERE NOT ended;
CREATE INDEX receptions_receiver_in_progress ON receptions (receiver, started, id) WHERE NOT ended;
CREATE INDEX receptions_by_receiver_state    ON receptions (receiver, state, started, id);
CREATE INDEX receptions_by_study_state       ON receptions (study_uid, state, started, id);
-- Removing or moving a storage asks whether a reception in progress names it.
CREATE INDEX receptions_by_storage ON receptions (storage) WHERE NOT ended;

-- The associations that brought a reception's files, each once, so a batch
-- written again counts none twice.
CREATE TABLE reception_associations (
    reception_id text COLLATE "C" NOT NULL,
    association  text COLLATE "C" NOT NULL,
    CONSTRAINT reception_associations_pkey PRIMARY KEY (reception_id, association)
);

-- A file that landed in the incoming area. The key is what makes a batch
-- written again change nothing: the same reception, instance and checksum is
-- the same file. deidentified is true for a file received at a receiver
-- that de-identifies, whose row holds the keyed remapping of the file's
-- placing UIDs and a keyed checksum, false for one recorded as received.
-- The received files of a reception that ended done or quarantined are
-- removed by the step that ends it: each is then an instance held, a
-- duplicate of one, or a quarantine record that keeps its own copy of the
-- row. A failed reception keeps its files, for its retry or its discard.
--
-- Its rows, like the claims', are in flight: written, then removed, in their
-- thousands a minute. Postgres's default autovacuum waits for a fifth of a
-- table to change, so a table that was empty when last analyzed is planned
-- as empty while it fills: a batch's lookup of 1000 claims among 60,000
-- then read every claim (4 ms) where the key reads none (0.2 ms). Each such
-- table is vacuumed and analyzed after a fixed number of rows changed
-- instead.
CREATE TABLE received_files (
    reception_id        text        COLLATE "C" NOT NULL,
    sop_instance_uid    text        COLLATE "C" NOT NULL,
    sha256              bytea       NOT NULL,
    series_uid          text        COLLATE "C" NOT NULL,
    study_uid           text        COLLATE "C" NOT NULL,
    sop_class_uid       text        COLLATE "C" NOT NULL,
    transfer_syntax_uid text        COLLATE "C" NOT NULL,
    size                bigint      NOT NULL,
    incoming            text        COLLATE "C" NOT NULL,
    calling_ae          text        NOT NULL,
    called_ae           text        NOT NULL,
    remote_addr         text        NOT NULL,
    association         text        COLLATE "C" NOT NULL,
    released            boolean     NOT NULL,
    received            timestamptz NOT NULL,
    deidentified        boolean     NOT NULL DEFAULT false,
    CONSTRAINT received_files_pkey PRIMARY KEY (reception_id, sop_instance_uid, sha256),
    CONSTRAINT received_files_sha256_check CHECK (octet_length(sha256) = 32),
    CONSTRAINT received_files_size_check CHECK (size >= 0)
) WITH (autovacuum_vacuum_scale_factor = 0, autovacuum_vacuum_threshold = 10000,
    autovacuum_analyze_scale_factor = 0, autovacuum_analyze_threshold = 10000);

-- A quarantined file's record. details is a JSON object of the reason's keys.
-- A record stays after its file is released or rejected, and is decided
-- exactly when it is no longer held; the reason a person gave for a release
-- is kept with it as a rejection's is (release_reason). deidentified is as
-- received_files' column. A release may name the storage (release_storage)
-- and the profile version, as name@version (release_profile), of the
-- reception its file joins; they are kept here when the file joins none and
-- lands again, '' for none. import_job and import_path name the import job
-- that handed the file in, and its path, '' for a file received otherwise.
CREATE TABLE quarantine (
    id                  text        COLLATE "C" NOT NULL,
    reason              text        COLLATE "C" NOT NULL,
    details             text        NOT NULL,
    state               text        COLLATE "C" NOT NULL,
    receiver            text        COLLATE "C" NOT NULL,
    reception_id        text        COLLATE "C" NOT NULL,
    calling_ae          text        NOT NULL,
    called_ae           text        NOT NULL,
    remote_addr         text        NOT NULL,
    size                bigint      NOT NULL,
    sha256              bytea,
    kept_sha256         bytea,
    sop_class_uid       text        COLLATE "C" NOT NULL,
    sop_instance_uid    text        COLLATE "C" NOT NULL,
    study_uid           text        COLLATE "C" NOT NULL,
    series_uid          text        COLLATE "C" NOT NULL,
    transfer_syntax_uid text        COLLATE "C" NOT NULL,
    incoming            text        COLLATE "C" NOT NULL,
    content_kept        boolean     NOT NULL,
    at                  timestamptz NOT NULL,
    decided_at          timestamptz,
    release_mode        text        COLLATE "C" NOT NULL,
    release_reception   text        COLLATE "C" NOT NULL,
    release_receiver    text        COLLATE "C" NOT NULL,
    reject_reason       text        NOT NULL,
    released_from       text        COLLATE "C" NOT NULL,
    release_reason      text        NOT NULL DEFAULT '',
    deidentified        boolean     NOT NULL DEFAULT false,
    release_storage     text        NOT NULL DEFAULT '',
    release_profile     text        NOT NULL DEFAULT '',
    import_job          text        COLLATE "C" NOT NULL DEFAULT '',
    import_path         text        NOT NULL DEFAULT '',
    CONSTRAINT quarantine_pkey PRIMARY KEY (id),
    CONSTRAINT quarantine_state_check CHECK (state IN ('held', 'released', 'rejected')),
    CONSTRAINT quarantine_size_check CHECK (size >= 0),
    CONSTRAINT quarantine_sha256_check CHECK (sha256 IS NULL OR octet_length(sha256) = 32),
    CONSTRAINT quarantine_kept_sha256_check CHECK (kept_sha256 IS NULL OR octet_length(kept_sha256) = 32),
    CONSTRAINT quarantine_release_mode_check CHECK (release_mode IN ('', 'as_is', 'overwrite')),
    CONSTRAINT quarantine_decided_check CHECK ((state = 'held') = (decided_at IS NULL))
);

-- The listings, newest first by quarantine time, then id, one index per
-- equality filter and for the combinations of the three filters that each
-- keep many records; a time range narrows whichever is used.
CREATE INDEX quarantine_by_time                  ON quarantine (at, id);
CREATE INDEX quarantine_by_state                 ON quarantine (state, at, id);
CREATE INDEX quarantine_by_reason                ON quarantine (reason, at, id);
CREATE INDEX quarantine_by_receiver              ON quarantine (receiver, at, id);
CREATE INDEX quarantine_by_reception             ON quarantine (reception_id, at, id);
CREATE INDEX quarantine_by_study                 ON quarantine (study_uid, at, id);
CREATE INDEX quarantine_by_instance              ON quarantine (sop_instance_uid, at, id);
CREATE INDEX quarantine_by_state_reason          ON quarantine (state, reason, at, id);
CREATE INDEX quarantine_by_state_receiver        ON quarantine (state, receiver, at, id);
CREATE INDEX quarantine_by_receiver_reason       ON quarantine (receiver, reason, at, id);
CREATE INDEX quarantine_by_state_receiver_reason ON quarantine (state, receiver, reason, at, id);
-- A discard leaves out the files a record keeps, found by their place.
CREATE INDEX quarantine_kept ON quarantine (incoming) WHERE content_kept;

-- The places in the incoming area that released quarantine records gave to
-- other receptions. A record released into a reception gives that reception
-- a received file at the record's own place, which may be a place a failed
-- reception received the file at. When that failed reception's files are
-- discarded, the list of what to remove must leave out a place a reception
-- still running holds, or a restart would delete bytes it has yet to
-- archive; this index answers that question for each file the list reads.
-- Released records are few, and a record is released once, so the index
-- costs nothing on the path a file is received by.
CREATE INDEX quarantine_released_places ON quarantine (incoming) WHERE state = 'released';

-- The quarantine's running totals: files held now and the bytes kept for
-- them, changed in the transaction of every change of a record, by an UPDATE
-- that adds, so they equal a count over the records after every commit. One
-- row, made here: only_row is always true.
CREATE TABLE quarantine_totals (
    only_row boolean NOT NULL DEFAULT true,
    files    bigint  NOT NULL,
    bytes    bigint  NOT NULL,
    CONSTRAINT quarantine_totals_pkey PRIMARY KEY (only_row),
    CONSTRAINT quarantine_totals_one_row CHECK (only_row),
    CONSTRAINT quarantine_totals_check CHECK (files >= 0 AND bytes >= 0)
);
INSERT INTO quarantine_totals (only_row, files, bytes) VALUES (true, 0, 0);

-- The catalog. A subject is found by its label, a session by its Study
-- Instance UID, a scan by its Series Instance UID; each keeps the label it was
-- first given. folded is the label as label.Fold makes it, unique at its
-- level, so two labels that a file system ignoring case would make one
-- directory are never both held. Ids come from an identity, whose sequence
-- never gives a value twice, so a send job, a retry's failures or a
-- correction's hidden rows that name a row by its id never come to name
-- another.
--
-- A list reads one table, never a join (sqlkit's catalog reads say why): so
-- a session keeps its subject's label and folded label, and a scan those of
-- its subject and session, its session's storage and Study Instance UID.
-- None of them ever changes once given. Running totals (counts, bytes,
-- updated, when a file was last archived into the row) are changed in the
-- transaction of every commit, so they equal a count of the instances after
-- each. Values a scan's files say of it and of its session (study date,
-- modality, series description, series number) are '' when not known, the
-- series number NULL. receiver is that of the reception that first archived
-- into the row. Every page is a range of one index read in its order: no
-- Sort and no join, whatever the size of the tables.
CREATE TABLE subjects (
    id             bigint      GENERATED ALWAYS AS IDENTITY,
    label          text        COLLATE "C" NOT NULL,
    folded         text        COLLATE "C" NOT NULL,
    created        timestamptz NOT NULL,
    session_count  bigint      NOT NULL DEFAULT 0,
    instance_count bigint      NOT NULL DEFAULT 0,
    bytes          bigint      NOT NULL DEFAULT 0,
    updated        timestamptz NOT NULL DEFAULT 'epoch',
    CONSTRAINT subjects_pkey PRIMARY KEY (id),
    CONSTRAINT subjects_folded_key UNIQUE (folded),
    CONSTRAINT subjects_totals_check CHECK (session_count >= 0 AND instance_count >= 0 AND bytes >= 0)
);

-- The lists and the index each reads, in the list's order (sqlkit's catalog
-- reads build the statements):
--   subjects newest first, with a time range or none   subjects_by_created
--   subjects by label                                  the unique folded
CREATE INDEX subjects_by_created ON subjects (created, id);

-- A session. route is what the routing chose for it when its first scan was
-- committed, so every later arrival of the session goes by the same choice;
-- '' for nothing of its own.
CREATE TABLE sessions (
    id             bigint      GENERATED ALWAYS AS IDENTITY,
    study_uid      text        COLLATE "C" NOT NULL,
    subject_id     bigint      NOT NULL,
    label          text        COLLATE "C" NOT NULL,
    folded         text        COLLATE "C" NOT NULL,
    storage        text        COLLATE "C" NOT NULL,
    created        timestamptz NOT NULL,
    subject_label  text        COLLATE "C" NOT NULL DEFAULT '',
    subject_folded text        COLLATE "C" NOT NULL DEFAULT '',
    study_date     text        COLLATE "C" NOT NULL DEFAULT '',
    receiver       text        COLLATE "C" NOT NULL DEFAULT '',
    scan_count     bigint      NOT NULL DEFAULT 0,
    instance_count bigint      NOT NULL DEFAULT 0,
    bytes          bigint      NOT NULL DEFAULT 0,
    updated        timestamptz NOT NULL DEFAULT 'epoch',
    route          text        NOT NULL DEFAULT '',
    CONSTRAINT sessions_pkey PRIMARY KEY (id),
    CONSTRAINT sessions_study_uid_key UNIQUE (study_uid),
    CONSTRAINT sessions_folded_key UNIQUE (subject_id, folded),
    CONSTRAINT sessions_totals_check CHECK (scan_count >= 0 AND instance_count >= 0 AND bytes >= 0)
);

-- The sessions on a storage, which make it in use; and the lists:
--   sessions newest first                              sessions_by_created
--     of a subject                                     sessions_by_subject_created
--     of a receiver                                    sessions_by_receiver
--     of a modality                                    session_modalities_by_modality
--   sessions by label                                  sessions_by_label
--     of a subject                                     the unique (subject_id, folded)
--   sessions by study date                             sessions_by_study_date
--     of a subject                                     sessions_by_subject_study_date
-- A session got by its labels is found by sessions_by_label, unique as the
-- labels are at each level.
CREATE INDEX sessions_by_storage ON sessions (storage);
CREATE INDEX sessions_by_created ON sessions (created, id);
CREATE INDEX sessions_by_subject_created ON sessions (subject_id, created, id);
CREATE INDEX sessions_by_receiver ON sessions (receiver, created, id);
CREATE INDEX sessions_by_study_date ON sessions (study_date, id);
CREATE INDEX sessions_by_subject_study_date ON sessions (subject_id, study_date, id);
CREATE UNIQUE INDEX sessions_by_label ON sessions (subject_folded, folded);

-- A scan. study_uid is its session's, so one row answers both what study a
-- held instance is in and what scan.
CREATE TABLE scans (
    id                 bigint      GENERATED ALWAYS AS IDENTITY,
    session_id         bigint      NOT NULL,
    series_uid         text        COLLATE "C" NOT NULL,
    label              text        COLLATE "C" NOT NULL,
    folded             text        COLLATE "C" NOT NULL,
    instances          bigint      NOT NULL,
    bytes              bigint      NOT NULL,
    manifest_sha256    bytea,
    created            timestamptz NOT NULL,
    updated            timestamptz NOT NULL,
    subject_label      text        COLLATE "C" NOT NULL DEFAULT '',
    subject_folded     text        COLLATE "C" NOT NULL DEFAULT '',
    session_label      text        COLLATE "C" NOT NULL DEFAULT '',
    session_folded     text        COLLATE "C" NOT NULL DEFAULT '',
    storage            text        COLLATE "C" NOT NULL DEFAULT '',
    modality           text        COLLATE "C" NOT NULL DEFAULT '',
    series_description text        NOT NULL DEFAULT '',
    description_folded text        COLLATE "C" NOT NULL DEFAULT '',
    series_number      integer,
    receiver           text        COLLATE "C" NOT NULL DEFAULT '',
    study_uid          text        COLLATE "C" NOT NULL DEFAULT '',
    CONSTRAINT scans_pkey PRIMARY KEY (id),
    CONSTRAINT scans_series_uid_key UNIQUE (series_uid),
    CONSTRAINT scans_folded_key UNIQUE (session_id, folded),
    CONSTRAINT scans_counts_check CHECK (instances >= 0 AND bytes >= 0),
    CONSTRAINT scans_manifest_check CHECK (manifest_sha256 IS NULL OR octet_length(manifest_sha256) = 32)
);

-- The lists:
--   scans newest first                                 scans_by_created
--     of a session                                     scans_by_session_created
--     of a modality                                    scans_by_modality
--     of a receiver                                    scans_by_receiver
--   scans by label, of all or of a subject             scans_by_label
--     of a session                                     the unique (session_id, folded)
--   scans by series description                        scans_by_description
-- The scans of a session are read in an order, each from its own index that
-- begins with the session; an index on the session alone would only tempt a
-- planner to read it and sort. A scan got by its labels is found by
-- scans_by_label.
CREATE INDEX scans_by_created ON scans (created, id);
CREATE INDEX scans_by_session_created ON scans (session_id, created, id);
CREATE INDEX scans_by_modality ON scans (modality, created, id);
CREATE INDEX scans_by_receiver ON scans (receiver, created, id);
CREATE INDEX scans_by_description ON scans (description_folded, id);
CREATE UNIQUE INDEX scans_by_label ON scans (subject_folded, session_folded, folded);

-- Whether a storage holds an instance is whether a scan on it holds one: on
-- the storage alone, the read keeps the test of instances > 0 as a filter
-- (the first scan of a storage nearly always passes it). An index that also
-- held instances would change on every commit that adds to a scan, and an
-- UPDATE of an indexed column is never heap-only, so every index of scans
-- would be written again for each commit.
CREATE INDEX scans_by_storage ON scans (storage);

-- A storage's scans in order of id, for a scrub of one storage: the scans'
-- index on the storage alone does not give that order in Postgres, whose
-- index entries are not ordered by the row's id.
CREATE INDEX scans_by_storage_scan ON scans (storage, id);

-- A study's instances are read scan by scan in order of id, then of SOP
-- Instance UID: this index gives the scans in that order and each scan's
-- instances are a range in that order, so a page is read without a sort
-- and stops at its limit. Read through sessions and the scans of each
-- session by creation, the first page of a large study sorted every
-- instance of it (p99 23 ms on Postgres, 71 ms on SQLite at ten million).
-- It serves a study's instances (DICOMweb's retrieval of a study and the
-- search for the instances of a study) and costs a row per scan, written
-- once when the scan is made: a scan's study never changes, so no commit
-- writes this index again.
CREATE INDEX scans_by_study ON scans (study_uid, id);

-- The modalities of each session's scans, each once, with the session's
-- creation time, so the sessions of a modality are listed newest first
-- from this index alone.
CREATE TABLE session_modalities (
    session_id bigint      NOT NULL,
    modality   text        COLLATE "C" NOT NULL,
    created    timestamptz NOT NULL,
    CONSTRAINT session_modalities_pkey PRIMARY KEY (session_id, modality)
);

CREATE INDEX session_modalities_by_modality ON session_modalities (modality, created, session_id);

-- An instance a reception claims before it writes the file to a storage, in
-- flight only: written by the claim, removed by the commit that confirms it
-- or the step that ends its reception. A SOP Instance UID is claimed or
-- held, never both: a claim is made only when neither this table nor
-- instances holds the UID, and checked against instances again after its
-- insert (a commit of another reception's claim may have moved it
-- meanwhile); a commit moves a claim of its own reception into instances in
-- one statement. The claim keeps the study and series it was made in, which
-- the commit checks. Kept apart from the held instances, an instance is
-- inserted once and never updated: an UPDATE that changes indexed columns
-- writes the row again with an entry in every index, and leaves a dead row
-- for VACUUM. Its rows are in flight, so it is vacuumed and analyzed after a
-- fixed number of rows changed (as received_files is, which says why).
CREATE TABLE instance_claims (
    sop_instance_uid text        COLLATE "C" NOT NULL,
    reception_id     text        COLLATE "C" NOT NULL,
    study_uid        text        COLLATE "C" NOT NULL,
    series_uid       text        COLLATE "C" NOT NULL,
    sha256           bytea       NOT NULL,
    claimed_at       timestamptz NOT NULL,
    CONSTRAINT instance_claims_pkey PRIMARY KEY (sop_instance_uid),
    CONSTRAINT instance_claims_reception_check CHECK (reception_id <> ''),
    CONSTRAINT instance_claims_sha256_check CHECK (octet_length(sha256) = 32)
) WITH (autovacuum_vacuum_scale_factor = 0, autovacuum_vacuum_threshold = 10000,
    autovacuum_analyze_scale_factor = 0, autovacuum_analyze_threshold = 10000);

-- A reception's claims, to release them when it ends.
CREATE INDEX instance_claims_by_reception ON instance_claims (reception_id);

-- A held instance, inserted once by its commit and never updated or
-- replaced by reception; its storage is its scan's. sha256 is the checksum
-- the file was received with; archived_sha256 is NULL when it equals
-- sha256, as it does for a file archived as it was received. storage_key
-- is '' for the layout's key of the instance's scan and UID, the only key a
-- commit is given, so a read makes it again from the scan's labels.
-- profile_version_id is the profile version that de-identified it (an id of
-- profile_versions), NULL for one archived as received. pixel_risk_accepted
-- is true when a pixel rule of that version accepted an image at risk of
-- burned-in text as it is, which a sender refuses unless marked; it takes
-- the alignment padding after the bigint before it. These keep the row
-- small: at ten million instances a wider row took 6.6 GB and its indexes
-- 2.3 GB.
CREATE TABLE instances (
    sop_instance_uid   text        COLLATE "C" NOT NULL,
    scan_id            bigint      NOT NULL,
    reception_id       text        COLLATE "C" NOT NULL,
    sha256             bytea       NOT NULL,
    archived_sha256    bytea,
    storage_key        text        COLLATE "C" NOT NULL,
    byte_offset        bigint      NOT NULL,
    byte_length        bigint      NOT NULL,
    size               bigint      NOT NULL,
    held_at            timestamptz NOT NULL,
    profile_version_id bigint,
    pixel_risk_accepted boolean    NOT NULL DEFAULT false,
    CONSTRAINT instances_pkey PRIMARY KEY (sop_instance_uid),
    CONSTRAINT instances_sha256_check CHECK (octet_length(sha256) = 32),
    CONSTRAINT instances_archived_check CHECK (archived_sha256 IS NULL OR octet_length(archived_sha256) = 32),
    CONSTRAINT instances_reception_check CHECK (reception_id <> ''),
    CONSTRAINT instances_place_check CHECK (byte_offset >= 0 AND byte_length >= 0 AND size >= 0)
);

-- A scan's instances in order of SOP Instance UID.
CREATE INDEX instances_by_scan ON instances (scan_id, sop_instance_uid);

-- The scans a reception confirmed instances in, with running totals, for
-- its list of scans: a row per scan in place of an index of every instance
-- by its reception.
CREATE TABLE reception_scans (
    reception_id text   COLLATE "C" NOT NULL,
    scan_id      bigint NOT NULL,
    instances    bigint NOT NULL,
    bytes        bigint NOT NULL,
    CONSTRAINT reception_scans_pkey PRIMARY KEY (reception_id, scan_id),
    CONSTRAINT reception_scans_counts_check CHECK (instances >= 0 AND bytes >= 0)
);

-- The catalog's running totals, changed in the transaction of every commit
-- by an UPDATE that adds, so they equal a count over the catalog after each.
-- One row, made here: only_row is always true.
CREATE TABLE catalog_totals (
    only_row  boolean NOT NULL DEFAULT true,
    subjects  bigint  NOT NULL,
    sessions  bigint  NOT NULL,
    scans     bigint  NOT NULL,
    instances bigint  NOT NULL,
    CONSTRAINT catalog_totals_pkey PRIMARY KEY (only_row),
    CONSTRAINT catalog_totals_one_row CHECK (only_row),
    CONSTRAINT catalog_totals_check CHECK (subjects >= 0 AND sessions >= 0 AND scans >= 0 AND instances >= 0)
);
INSERT INTO catalog_totals (only_row, subjects, sessions, scans, instances) VALUES (true, 0, 0, 0, 0);

-- An instance a scrub found missing or damaged: every read of it answers
-- data loss before its first byte. One per instance: the finding that marked
-- it. A sparse table rather than a column of instances, so no instance row
-- is rewritten and every read of an instance adds one lookup of a table
-- that is empty while the archive is whole.
CREATE TABLE instance_damage (
    sop_instance_uid text        COLLATE "C" NOT NULL,
    finding_id       text        COLLATE "C" NOT NULL,
    marked_at        timestamptz NOT NULL,
    CONSTRAINT instance_damage_pkey PRIMARY KEY (sop_instance_uid)
);

-- A unit of work. due is when a queued unit may be claimed, and when a leased
-- unit's lease ends; attempt counts claims and fences every write of the
-- worker holding the lease. unique_key is NULL for none. reception_id names
-- the reception the unit carries, '' for none: a unit held as failed fails its
-- reception in the same transaction.
CREATE TABLE work (
    id           text        COLLATE "C" NOT NULL,
    kind         text        COLLATE "C" NOT NULL,
    subject      text        NOT NULL,
    reception_id text        COLLATE "C" NOT NULL,
    unique_key   text        COLLATE "C",
    state        text        COLLATE "C" NOT NULL,
    due          timestamptz NOT NULL,
    attempt      integer     NOT NULL,
    max_attempts integer     NOT NULL,
    owner        text        COLLATE "C" NOT NULL,
    last_error   text        NOT NULL,
    created      timestamptz NOT NULL,
    updated      timestamptz NOT NULL,
    CONSTRAINT work_pkey PRIMARY KEY (id),
    CONSTRAINT work_state_check CHECK (state IN ('queued', 'leased', 'done', 'failed')),
    CONSTRAINT work_attempt_check CHECK (attempt >= 0 AND max_attempts >= 1)
);

-- A key queues its work once while the work is queued or leased; a finished
-- unit frees it.
CREATE UNIQUE INDEX work_by_key ON work (unique_key) WHERE unique_key IS NOT NULL AND state IN ('queued', 'leased');
-- A claim reads the earliest due of a kind among the units not finished, and
-- only those: finished units, however many, are not in this index.
CREATE INDEX work_claimable ON work (kind, due, id) WHERE state IN ('queued', 'leased');
-- The receptions with a live unit, for the search at start of those left
-- without one.
CREATE INDEX work_by_reception ON work (reception_id) WHERE state IN ('queued', 'leased');

-- The event feed: one row per thing that happened, written in the
-- transaction of the change it reports. id orders the feed. Several
-- transactions write at once on Postgres, so a change takes the row of
-- event_marks FOR UPDATE just before it writes its first event and holds it
-- to its commit: the changes that write events commit one after another,
-- each takes its ids after the one before committed, and the order of ids is
-- the order of commits. A reader's snapshot then holds every event up to
-- some id and none after it, which is what a cursor needs. Ids only grow: a
-- sequence never gives a value twice, even after every row was pruned or a
-- transaction that took one was undone. payload is JSON, stored and
-- returned whole and never searched inside. key is the caller's key of an
-- event the server appended, NULL for one the store wrote with its change.
CREATE TABLE events (
    id      bigint      GENERATED ALWAYS AS IDENTITY,
    type    text        COLLATE "C" NOT NULL,
    at      timestamptz NOT NULL,
    payload text        NOT NULL,
    key     text        COLLATE "C",
    CONSTRAINT events_pkey PRIMARY KEY (id)
);

-- A filtered read walks the events of each type it names after the cursor;
-- an unfiltered read and the pruning walk the ids themselves.
CREATE INDEX events_by_type ON events (type, id);
-- An appended event is written once per key.
CREATE UNIQUE INDEX events_by_key ON events (key) WHERE key IS NOT NULL;

-- What pruning removed: every event whose id is at or below pruned_through
-- is gone, and every event above it is kept. It moves in the pruning's
-- transaction, so a reader whose cursor is below it is told its cursor
-- expired, never answered from past a gap. Its one row, made here, is also
-- the feed's lock (above).
CREATE TABLE event_marks (
    only_row       boolean NOT NULL DEFAULT true,
    pruned_through bigint  NOT NULL,
    CONSTRAINT event_marks_pkey PRIMARY KEY (only_row),
    CONSTRAINT event_marks_one_row CHECK (only_row),
    CONSTRAINT event_marks_pruned_through_check CHECK (pruned_through >= 0)
);
INSERT INTO event_marks (only_row, pruned_through) VALUES (true, 0);

-- Checkpoints of the feed, recorded by the server with its own clock: at the
-- time at, the newest event committed was through. Retention removes every
-- event up to the newest checkpoint older than its period, so an event's age
-- is measured by the server's clock from when it was written, never by the
-- time the event itself carries, which a caller gave.
CREATE TABLE event_checkpoints (
    at      timestamptz NOT NULL,
    through bigint      NOT NULL,
    CONSTRAINT event_checkpoints_pkey PRIMARY KEY (at),
    CONSTRAINT event_checkpoints_through_check CHECK (through >= 0)
);

-- The event feed's history: one row, its id 32 lowercase hexadecimal digits
-- of a random value, made with the database and made again whenever a
-- rebuild replaces the catalog, and since when it holds; so a consumer that
-- keys what it read by (history, cursor) knows the feed it follows began
-- again from a new catalog.
CREATE TABLE event_history (
    only_row boolean     NOT NULL DEFAULT true,
    id       text        COLLATE "C" NOT NULL,
    started  timestamptz NOT NULL,
    CONSTRAINT event_history_pkey PRIMARY KEY (only_row),
    CONSTRAINT event_history_one_row CHECK (only_row),
    CONSTRAINT event_history_id_check CHECK (length(id) = 32)
);
INSERT INTO event_history (only_row, id, started) VALUES (true, replace(gen_random_uuid()::text, '-', ''), clock_timestamp());

-- A de-identification profile version, never changed once written: its
-- document and its expanded form as the exact bytes their checksum covers
-- (bytea, so nothing is normalized), the checksum of the expanded form, who
-- made it, when and why. id is what an instance records; name and version
-- are what a person names.
CREATE TABLE profile_versions (
    id             bigint      GENERATED ALWAYS AS IDENTITY,
    name           text        COLLATE "C" NOT NULL,
    version        integer     NOT NULL,
    checksum       text        COLLATE "C" NOT NULL,
    note           text        NOT NULL,
    created_by_key text        COLLATE "C" NOT NULL,
    created        timestamptz NOT NULL,
    source         bytea       NOT NULL,
    expanded       bytea       NOT NULL,
    CONSTRAINT profile_versions_pkey PRIMARY KEY (id),
    CONSTRAINT profile_versions_by_name UNIQUE (name, version),
    CONSTRAINT profile_versions_check CHECK (version BETWEEN 1 AND 10000 AND length(checksum) = 64
        AND length(source) > 0 AND length(expanded) > 0)
);

-- A profile and its latest version, written with each version, so a list
-- of profiles reads one row each.
CREATE TABLE profiles (
    name           text        COLLATE "C" NOT NULL,
    latest_version integer     NOT NULL,
    latest_created timestamptz NOT NULL,
    CONSTRAINT profiles_pkey PRIMARY KEY (name),
    CONSTRAINT profiles_check CHECK (latest_version BETWEEN 1 AND 10000)
);

-- A lookup table's last code assigned in order; its row is the lock under
-- which the table's codes are assigned.
CREATE TABLE lookup_counters (
    table_name text   COLLATE "C" NOT NULL,
    last_code  bigint NOT NULL,
    CONSTRAINT lookup_counters_pkey PRIMARY KEY (table_name),
    CONSTRAINT lookup_counters_check CHECK (last_code >= 0)
);

-- A lookup table's codes, by the keyed hash of the value each replaces,
-- never the value. A code is given to one value of a table.
CREATE TABLE lookup_codes (
    table_name text        COLLATE "C" NOT NULL,
    lookup_key text        COLLATE "C" NOT NULL,
    code       text        COLLATE "C" NOT NULL,
    assigned   timestamptz NOT NULL,
    CONSTRAINT lookup_codes_pkey PRIMARY KEY (table_name, lookup_key),
    CONSTRAINT lookup_codes_by_code UNIQUE (table_name, code)
);

-- A sender. Its protocol's configuration (a DIMSE peer's host, port, AE
-- titles and TLS files; a stow peer's URL, authentication and request
-- bounds) is JSON, stored and returned whole. The credential is sealed by
-- the caller and never read back by a read operation: it is selected only
-- as whether it is set. allow_identified is true when the installation's
-- admin let it send instances archived as received, without full profile
-- provenance, or with an accepted pixel risk.
CREATE TABLE senders (
    name                 text        COLLATE "C" NOT NULL,
    type                 text        COLLATE "C" NOT NULL,
    enabled              boolean     NOT NULL,
    allow_identified     boolean     NOT NULL DEFAULT false,
    config               text        NOT NULL,
    credential           bytea,
    max_associations     integer     NOT NULL,
    max_bytes_per_second bigint      NOT NULL,
    connect_us           bigint      NOT NULL,
    response_us          bigint      NOT NULL,
    created              timestamptz NOT NULL,
    updated              timestamptz NOT NULL,
    CONSTRAINT senders_pkey PRIMARY KEY (name),
    CONSTRAINT senders_type_check CHECK (type IN ('dimse', 'stow')),
    CONSTRAINT senders_bounds_check CHECK (max_associations BETWEEN 1 AND 16 AND max_bytes_per_second >= 0)
);

-- A job: what it does, where it is, and its running counts, kept in the
-- transaction of every write of its units, whose work is units of the work
-- table. A send job sends a target by labels, or a reception's instances,
-- to a sender; a deid job judges archived instances against a
-- de-identification profile version, changing nothing, and names no sender
-- (''), a mode (dry_run or check) and, for a dry run, the profile version
-- it runs ('' and 0 for each instance's own), where every other kind keeps
-- '' and 0; an import, scrub or rebuild job keeps what it reads in its own
-- table below. bound_at is the moment whose catalog it reads (its
-- creation, or the creation of the job a retry sends again). Its listing
-- has two parts: a retry's first lists the ended job's scans (retry_after,
-- retry_listed), then every job lists what its target holds that is not
-- listed yet (list_*, target_listed); listed is both. progress_at and
-- progress_mark are the time and the instances answered at its last
-- progress event, which is written at most once in a period. A forward is
-- made once per reception and sender (jobs_key).
CREATE TABLE jobs (
    id                  text        COLLATE "C" NOT NULL,
    kind                text        COLLATE "C" NOT NULL,
    sender              text        COLLATE "C" NOT NULL,
    subject_label       text        COLLATE "C" NOT NULL,
    session_label       text        COLLATE "C" NOT NULL,
    scan_label          text        COLLATE "C" NOT NULL,
    reception_id        text        COLLATE "C" NOT NULL,
    receiver            text        COLLATE "C" NOT NULL,
    unique_key          text        COLLATE "C",
    state               text        COLLATE "C" NOT NULL,
    ended               boolean     NOT NULL,
    reason              text        NOT NULL,
    created_by_key      text        COLLATE "C" NOT NULL,
    created             timestamptz NOT NULL,
    started             timestamptz,
    state_at            timestamptz NOT NULL,
    ended_at            timestamptz,
    bound_at            timestamptz NOT NULL,
    retry_of            text        COLLATE "C" NOT NULL,
    retry_job           text        COLLATE "C" NOT NULL,
    list_work           text        COLLATE "C" NOT NULL,
    listed              boolean     NOT NULL,
    retry_listed        boolean     NOT NULL,
    target_listed       boolean     NOT NULL,
    retry_after         bigint      NOT NULL,
    list_session        text        COLLATE "C" NOT NULL,
    list_scan           text        COLLATE "C" NOT NULL,
    list_id             bigint      NOT NULL,
    scans               bigint      NOT NULL,
    scans_done          bigint      NOT NULL,
    instances           bigint      NOT NULL,
    bytes               bigint      NOT NULL,
    sent                bigint      NOT NULL,
    sent_bytes          bigint      NOT NULL,
    warned              bigint      NOT NULL,
    warned_bytes        bigint      NOT NULL,
    refused             bigint      NOT NULL,
    refused_bytes       bigint      NOT NULL,
    failures_listed     bigint      NOT NULL,
    failures_not_listed bigint      NOT NULL,
    progress_at         timestamptz,
    progress_mark       bigint      NOT NULL,
    deid_mode           text        COLLATE "C" NOT NULL DEFAULT '',
    profile_name        text        COLLATE "C" NOT NULL DEFAULT '',
    profile_version     integer     NOT NULL DEFAULT 0,
    CONSTRAINT jobs_pkey PRIMARY KEY (id),
    CONSTRAINT jobs_key UNIQUE (unique_key),
    CONSTRAINT jobs_kind_check CHECK (kind IN ('send', 'deid', 'import', 'scrub', 'rebuild', 'replicate', 'repair', 'dispose')),
    CONSTRAINT jobs_state_check CHECK (state IN ('queued', 'running', 'done', 'partial', 'failed', 'canceled')),
    CONSTRAINT jobs_scans_check CHECK (scans >= 0 AND scans_done >= 0 AND scans_done <= scans),
    CONSTRAINT jobs_counts_check CHECK (sent >= 0 AND warned >= 0 AND refused >= 0 AND sent + warned + refused <= instances),
    CONSTRAINT jobs_bytes_check CHECK (sent_bytes >= 0 AND warned_bytes >= 0 AND refused_bytes >= 0
        AND sent_bytes + warned_bytes + refused_bytes <= bytes),
    CONSTRAINT jobs_deid_mode_check CHECK (deid_mode IN ('', 'dry_run', 'check')),
    CONSTRAINT jobs_deid_kind_check CHECK ((kind = 'deid') = (deid_mode <> '')),
    CONSTRAINT jobs_profile_version_check CHECK (profile_version >= 0)
);

-- The lists, newest first: every job, by state, by sender, and those not
-- ended (also what a sender's removal and a status read).
CREATE INDEX jobs_by_created            ON jobs (created, id);
CREATE INDEX jobs_by_state              ON jobs (state, created, id);
CREATE INDEX jobs_by_sender             ON jobs (sender, created, id);
CREATE INDEX jobs_in_progress           ON jobs (created, id) WHERE NOT ended;
CREATE INDEX jobs_of_sender_in_progress ON jobs (sender, created, id) WHERE NOT ended;

-- A job's scan, one unit of work each: the instances its stream holds after
-- start_after (and those of job_resends), the last one the peer answered
-- (cursor), and its counts.
CREATE TABLE job_scans (
    job_id             text    COLLATE "C" NOT NULL,
    scan_id            bigint  NOT NULL,
    work_id            text    COLLATE "C" NOT NULL,
    start_after        text    COLLATE "C" NOT NULL,
    cursor             text    COLLATE "C" NOT NULL,
    instances          bigint  NOT NULL,
    bytes              bigint  NOT NULL,
    sent               bigint  NOT NULL,
    sent_bytes         bigint  NOT NULL,
    warned             bigint  NOT NULL,
    warned_bytes       bigint  NOT NULL,
    refused            bigint  NOT NULL,
    refused_bytes      bigint  NOT NULL,
    refused_not_listed bigint  NOT NULL,
    ended              boolean NOT NULL,
    CONSTRAINT job_scans_pkey PRIMARY KEY (job_id, scan_id),
    CONSTRAINT job_scans_counts_check CHECK (sent >= 0 AND warned >= 0 AND refused >= 0 AND sent + warned + refused <= instances)
);

-- A unit's writes find its scan by the unit.
CREATE UNIQUE INDEX job_scans_by_work ON job_scans (work_id);

-- The instances a retry's scan sends again that lie at or before its
-- start_after: the ended job's refused instances.
CREATE TABLE job_resends (
    job_id           text   COLLATE "C" NOT NULL,
    scan_id          bigint NOT NULL,
    sop_instance_uid text   COLLATE "C" NOT NULL,
    CONSTRAINT job_resends_pkey PRIMARY KEY (job_id, scan_id, sop_instance_uid)
);

-- An instance a scan's unit owes: the peer left it unaccounted, and the
-- unit's cursor moved past it, so a crash sends at most one batch again; it
-- is tried again by name, and a retry sends it while it is still owed.
CREATE TABLE job_owed (
    job_id           text   COLLATE "C" NOT NULL,
    scan_id          bigint NOT NULL,
    sop_instance_uid text   COLLATE "C" NOT NULL,
    CONSTRAINT job_owed_pkey PRIMARY KEY (job_id, scan_id, sop_instance_uid)
);

-- An instance a job's peer refused, or kept with a warning, listed by its
-- scan's labels; at most the setting job.failures-kept a job.
CREATE TABLE job_failures (
    job_id              text        COLLATE "C" NOT NULL,
    subject_label       text        COLLATE "C" NOT NULL,
    session_label       text        COLLATE "C" NOT NULL,
    scan_label          text        COLLATE "C" NOT NULL,
    sop_instance_uid    text        COLLATE "C" NOT NULL,
    scan_id             bigint      NOT NULL,
    warning             boolean     NOT NULL,
    code                text        COLLATE "C" NOT NULL,
    peer_status         integer     NOT NULL,
    sop_class_uid       text        NOT NULL,
    transfer_syntax_uid text        NOT NULL,
    detail              text        NOT NULL,
    at                  timestamptz NOT NULL,
    CONSTRAINT job_failures_pkey PRIMARY KEY (job_id, subject_label, session_label, scan_label, sop_instance_uid)
);

-- The refused alone, in the same order.
CREATE INDEX job_failures_by_kind ON job_failures (job_id, warning, subject_label, session_label, scan_label, sop_instance_uid);
-- A scan's failures by the scan's key, for a retry.
CREATE INDEX job_failures_by_scan ON job_failures (job_id, scan_id, sop_instance_uid);

-- A deid job's counts per key (the contract's JobTally), added in the
-- transaction of each progress write: at most 1000 keys a job, and the key
-- 'other' for those past it.
CREATE TABLE job_tallies (
    job_id text   COLLATE "C" NOT NULL,
    tally  text   COLLATE "C" NOT NULL,
    count  bigint NOT NULL,
    CONSTRAINT job_tallies_pkey PRIMARY KEY (job_id, tally),
    CONSTRAINT job_tallies_count_check CHECK (count >= 0)
);

-- What an import job reads: the files of a directory under an import
-- receiver's root, handed to that receiver one at a time; its receiver,
-- path, map and map configuration, the map's lists as JSON arrays; and its
-- counts, kept in the transaction of every file's record and every outcome.
-- next_unit and next_file number its units and its listed files.
-- fail_reason is the first failed unit's reason; settling_since when its
-- last walk ended with files processing; retention_start the start time an
-- admin gave the sessions it archives first (NULL for their archive time).
-- The CHECKs make the counts reconcile: every file found is skipped or
-- handed in, and every file handed in has one outcome.
--
-- A path can identify a person, so it is kept in the import's rows only.
-- Where an index orders paths (a unit's folder and cursor, a file's path) it
-- is kept as bytea with each "/" as the byte 0: no segment holds NUL, so
-- bytea's order (memcmp, then length) is the order an import walks a tree
-- in, a directory's whole subtree before the next name beside it. A path an
-- index holds is at most 2048 bytes, so an index row with the job id, a
-- unit id and a kind beside it stays below the 2704 bytes a B-tree takes in
-- one row; an entry whose path is longer is skipped and listed under its
-- deepest ancestor that fits. A unit's cursor, which no index holds, may be
-- such an entry's path: at most 2048 + 1 + 255 bytes.
CREATE TABLE import_jobs (
    job_id           text        COLLATE "C" NOT NULL,
    receiver         text        COLLATE "C" NOT NULL,
    path             text        COLLATE "C" NOT NULL,
    map_file         text        COLLATE "C" NOT NULL,
    map_csv          bytea,
    map_key_columns  text        NOT NULL,
    map_value_column text        NOT NULL,
    map_key_parts    text        NOT NULL,
    map_gives        text        COLLATE "C" NOT NULL,
    failures_kept    bigint      NOT NULL,
    files_listed     bigint      NOT NULL,
    files_not_listed bigint      NOT NULL,
    listed           boolean     NOT NULL,
    units            bigint      NOT NULL,
    units_walked     bigint      NOT NULL,
    next_unit        bigint      NOT NULL,
    next_file        bigint      NOT NULL,
    fail_reason      text        NOT NULL,
    settling_since   timestamptz,
    found            bigint      NOT NULL,
    skipped          bigint      NOT NULL,
    handed_in        bigint      NOT NULL,
    archived         bigint      NOT NULL,
    already_held     bigint      NOT NULL,
    quarantined      bigint      NOT NULL,
    lost             bigint      NOT NULL,
    failed           bigint      NOT NULL,
    discarded        bigint      NOT NULL,
    processing       bigint      NOT NULL,
    bytes_handed_in  bigint      NOT NULL,
    label_differs    bigint      NOT NULL,
    retention_start  timestamptz,
    CONSTRAINT import_jobs_pkey PRIMARY KEY (job_id),
    CONSTRAINT import_jobs_bounds_check CHECK (failures_kept > 0 AND files_listed >= 0 AND files_not_listed >= 0
        AND units >= 0 AND units_walked >= 0 AND units_walked <= units),
    CONSTRAINT import_jobs_found_check CHECK (found = skipped + handed_in),
    CONSTRAINT import_jobs_handed_check CHECK (handed_in = archived + already_held + quarantined + lost + failed + discarded + processing),
    CONSTRAINT import_jobs_counts_check CHECK (skipped >= 0 AND archived >= 0 AND already_held >= 0 AND quarantined >= 0
        AND lost >= 0 AND failed >= 0 AND discarded >= 0 AND processing >= 0)
);

-- A receiver's import jobs, for the rule that two of them in progress never
-- read the same files.
CREATE INDEX import_jobs_by_receiver ON import_jobs (receiver);

-- An import job's unit, one row of the work table of the kind import each:
-- its folder, where it is, the last path it judged (NULL before the first),
-- and its counts, which reconcile as the job's.
CREATE TABLE import_units (
    job_id          text        COLLATE "C" NOT NULL,
    unit_id         text        COLLATE "C" NOT NULL,
    work_id         text        COLLATE "C" NOT NULL,
    seq             bigint      NOT NULL,
    folder_key      bytea       NOT NULL,
    listing         boolean     NOT NULL,
    state           text        COLLATE "C" NOT NULL,
    reason          text        NOT NULL,
    cursor_key      bytea,
    walk_ended      timestamptz,
    found           bigint      NOT NULL,
    skipped         bigint      NOT NULL,
    handed_in       bigint      NOT NULL,
    archived        bigint      NOT NULL,
    already_held    bigint      NOT NULL,
    quarantined     bigint      NOT NULL,
    lost            bigint      NOT NULL,
    failed          bigint      NOT NULL,
    discarded       bigint      NOT NULL,
    processing      bigint      NOT NULL,
    bytes_handed_in bigint      NOT NULL,
    label_differs   bigint      NOT NULL,
    CONSTRAINT import_units_pkey PRIMARY KEY (job_id, unit_id),
    CONSTRAINT import_units_state_check CHECK (state IN ('queued', 'walking', 'walked', 'done', 'failed', 'canceled')),
    CONSTRAINT import_units_path_check CHECK (octet_length(folder_key) <= 2048 AND octet_length(cursor_key) <= 2304),
    CONSTRAINT import_units_found_check CHECK (found = skipped + handed_in),
    CONSTRAINT import_units_handed_check CHECK (handed_in = archived + already_held + quarantined + lost + failed + discarded + processing),
    CONSTRAINT import_units_counts_check CHECK (skipped >= 0 AND archived >= 0 AND already_held >= 0 AND quarantined >= 0
        AND lost >= 0 AND failed >= 0 AND discarded >= 0 AND processing >= 0)
);

-- A unit's writes find it by its unit of work; a folder is queued once per
-- job; the units are listed by folder in the walk's order.
CREATE UNIQUE INDEX import_units_by_work   ON import_units (work_id);
CREATE UNIQUE INDEX import_units_by_folder ON import_units (job_id, folder_key);

-- A file an import handed in that has no outcome yet, or whose reception
-- failed (outcome 'failed', listed_seq its row of import_files, 0 when past
-- the job's bound), linked to its received file by reception, SOP Instance
-- UID and received checksum. A decided outcome removes it: what an outcome
-- leaves to list is a row of import_files. parts are the parts its path
-- gave the naming rule (a JSON object, '' for none), so the pipeline names
-- the file's session and scan by them when its reception is named.
CREATE TABLE import_arrivals (
    job_id           text        COLLATE "C" NOT NULL,
    path_key         bytea       NOT NULL,
    unit_id          text        COLLATE "C" NOT NULL,
    reception_id     text        COLLATE "C" NOT NULL,
    sop_instance_uid text        COLLATE "C" NOT NULL,
    sha256           bytea       NOT NULL,
    size             bigint      NOT NULL,
    outcome          text        COLLATE "C" NOT NULL,
    listed_seq       bigint      NOT NULL,
    at               timestamptz NOT NULL,
    parts            text        COLLATE "C" NOT NULL DEFAULT '',
    CONSTRAINT import_arrivals_pkey PRIMARY KEY (job_id, path_key),
    CONSTRAINT import_arrivals_check CHECK (length(sha256) = 32 AND size >= 0 AND outcome IN ('', 'failed')
        AND octet_length(path_key) <= 2048)
);

-- The deciding transactions find a reception's arrivals by its received
-- file's key.
CREATE INDEX import_arrivals_by_file ON import_arrivals (reception_id, sop_instance_uid, sha256);

-- A file of an import job that did not reach the archive, listed by its
-- path in the walk's order, then kind: skipped, quarantined, lost, failed
-- or discarded. seq numbers the job's rows, so a page's cursor holds a
-- number, never a path. A failed file retried is hidden, not removed, so a
-- cursor's row stays.
CREATE TABLE import_files (
    job_id            text        COLLATE "C" NOT NULL,
    path_key          bytea       NOT NULL,
    kind              text        COLLATE "C" NOT NULL,
    seq               bigint      NOT NULL,
    unit_id           text        COLLATE "C" NOT NULL,
    skip_code         text        COLLATE "C" NOT NULL,
    quarantine_id     text        COLLATE "C" NOT NULL,
    quarantine_reason text        COLLATE "C" NOT NULL,
    reception_id      text        COLLATE "C" NOT NULL,
    size              bigint      NOT NULL,
    detail            text        NOT NULL,
    hidden            boolean     NOT NULL,
    at                timestamptz NOT NULL,
    CONSTRAINT import_files_pkey PRIMARY KEY (job_id, path_key, kind),
    CONSTRAINT import_files_check CHECK (kind IN ('skipped', 'quarantined', 'lost', 'failed', 'discarded') AND size >= 0
        AND octet_length(path_key) <= 2048)
);

-- A cursor finds its row by number; a unit's files are listed by path.
CREATE UNIQUE INDEX import_files_by_seq  ON import_files (job_id, seq);
CREATE INDEX import_files_by_unit        ON import_files (job_id, unit_id, path_key, kind);

-- An import job's, and each unit's, counts by skip code (kind 'skip') and
-- by quarantine reason (kind 'quarantined'); the job's under unit_id ''.
CREATE TABLE import_tallies (
    job_id  text   COLLATE "C" NOT NULL,
    unit_id text   COLLATE "C" NOT NULL,
    kind    text   COLLATE "C" NOT NULL,
    code    text   COLLATE "C" NOT NULL,
    count   bigint NOT NULL,
    CONSTRAINT import_tallies_pkey PRIMARY KEY (job_id, unit_id, kind, code),
    CONSTRAINT import_tallies_check CHECK (kind IN ('skip', 'quarantined') AND count >= 0)
);

-- What a scrub job reads: every stored object of one storage, or of every
-- storage of the tenant (''), read again and compared with what the catalog
-- and the scan's manifest say it holds; scheduled when the server made it
-- by itself. Its work is the units of a job like any other's, one per scan.
CREATE TABLE scrub_jobs (
    job_id    text    COLLATE "C" NOT NULL,
    storage   text    COLLATE "C" NOT NULL,
    scheduled boolean NOT NULL,
    CONSTRAINT scrub_jobs_pkey PRIMARY KEY (job_id)
);

-- A storage's scrubs, for the last one, which a scheduled scrub follows.
CREATE INDEX scrub_jobs_by_storage ON scrub_jobs (storage);

-- One difference a scrub found and believed, after reading it twice: at
-- most one that holds per object and kind, so a finding found again by a
-- later scrub is the same row, seen again, and the findings that hold are
-- counted once. The labels are the scan's, '' for an orphan that names no
-- scan; sop_instance_uid is the instance's, '' for a manifest, a sidecar or
-- an orphan. The expected size and checksum are the catalog's or the
-- manifest's (NULL for an orphan), the found ones what was read (NULL for
-- an object missing). job_id is the job that found it; last_job and
-- last_seen the latest job that found it again. cleared_at and clear_job
-- are set when a later scrub read the object good, and then never change;
-- a replica's damage is cleared with its replica_damage row, by a scrub
-- (clear_job its job) or by the replicator's verified copy (clear_job '').
-- replica_of names the primary of storage when storage was a replica when
-- the finding was made, '' otherwise: such a finding never marks its
-- instance, and is the only kind cleared with no job.
CREATE TABLE scrub_findings (
    id               text        COLLATE "C" NOT NULL,
    kind             text        COLLATE "C" NOT NULL,
    storage          text        COLLATE "C" NOT NULL,
    object_key       text        COLLATE "C" NOT NULL,
    subject_label    text        COLLATE "C" NOT NULL,
    session_label    text        COLLATE "C" NOT NULL,
    scan_label       text        COLLATE "C" NOT NULL,
    sop_instance_uid text        COLLATE "C" NOT NULL,
    revision         integer     NOT NULL,
    expected_size    bigint,
    found_size       bigint,
    expected_sha256  bytea,
    found_sha256     bytea,
    job_id           text        COLLATE "C" NOT NULL,
    found_at         timestamptz NOT NULL,
    last_job         text        COLLATE "C" NOT NULL,
    last_seen        timestamptz NOT NULL,
    cleared_at       timestamptz,
    clear_job        text        COLLATE "C" NOT NULL,
    replica_of       text        COLLATE "C" NOT NULL DEFAULT '',
    CONSTRAINT scrub_findings_pkey PRIMARY KEY (id),
    CONSTRAINT scrub_findings_kind_check CHECK (kind IN ('missing', 'orphan', 'damaged', 'disagreeing', 'manifest_damaged')),
    CONSTRAINT scrub_findings_values_check CHECK (object_key <> '' AND revision >= 0
        AND (expected_size IS NULL OR expected_size >= 0) AND (found_size IS NULL OR found_size >= 0)
        AND (expected_sha256 IS NULL OR length(expected_sha256) = 32) AND (found_sha256 IS NULL OR length(found_sha256) = 32)),
    CONSTRAINT scrub_findings_cleared_check CHECK (cleared_at IS NOT NULL OR clear_job = ''),
    CONSTRAINT scrub_findings_clear_job_check CHECK (cleared_at IS NULL OR clear_job <> '' OR replica_of <> '')
);

-- At most one finding that holds per object and kind; and the list's
-- order, by storage, then key, then kind, then id.
CREATE UNIQUE INDEX scrub_findings_open ON scrub_findings (storage, object_key, kind) WHERE cleared_at IS NULL;
CREATE INDEX scrub_findings_by_key ON scrub_findings (storage, object_key, kind, id);

-- The findings that hold of one scan, which its unit reads page by page in
-- order of key and kind, to clear those it reads good again.
CREATE INDEX scrub_findings_of_scan ON scrub_findings (storage, subject_label, session_label, scan_label, object_key, kind)
    WHERE cleared_at IS NULL;

-- The findings that hold of an instance and mark it, which the clear of one
-- of them reads to mark it again with another.
CREATE INDEX scrub_findings_marking ON scrub_findings (sop_instance_uid)
    WHERE cleared_at IS NULL AND kind IN ('missing', 'damaged') AND replica_of = '';

-- When each scan was last read whole (verified_at) and last checked by
-- presence only (presence_at) on each storage, its own or a replica, NULL
-- for never: the ground of a storage's coverage. Keyed by the storage's name
-- and the scan's Series Instance UID, never an integer id, so it survives a
-- rebuild; a row whose scan is gone is never read (the coverage reads the
-- scans, then this), and is dropped when the scan leaves the catalog for
-- good or its storage is removed. A commit that gives a scan more
-- instances drops its rows: no read recorded covered them.
CREATE TABLE scrub_verified (
    storage     text        COLLATE "C" NOT NULL,
    series_uid  text        COLLATE "C" NOT NULL,
    verified_at timestamptz,
    presence_at timestamptz,
    CONSTRAINT scrub_verified_pkey PRIMARY KEY (storage, series_uid)
);

-- What a rebuild job does: the catalog made again from what the tenant's
-- storages hold (their correction records, manifests and metadata) into a
-- shadow catalog, tables of the same shape in a scratch schema of this
-- database the job names (scratch), never into the catalog in use. In
-- compare mode the shadow is compared with the catalog in use and dropped;
-- in replace mode one transaction makes it the catalog in use. How far the
-- build got is kept in the shadow itself, written with each scan's rows
-- there, so a crash or a lost lease goes on from the last scan written.
-- activated is set by the transaction that made its shadow the catalog in
-- use, and never changes after. dropped is set once its shadow is removed,
-- so a shadow a job left when it ended without its worker (its attempts ran
-- out, or it was canceled while queued) is found and removed. from_storage,
-- when not '', is the one storage a compare reads every scan from (a
-- replica a move is about to switch to).
CREATE TABLE rebuild_jobs (
    job_id       text    COLLATE "C" NOT NULL,
    mode         text    COLLATE "C" NOT NULL,
    scratch      text    COLLATE "C" NOT NULL,
    activated    boolean NOT NULL,
    dropped      boolean NOT NULL,
    from_storage text    COLLATE "C" NOT NULL DEFAULT '',
    CONSTRAINT rebuild_jobs_pkey PRIMARY KEY (job_id),
    CONSTRAINT rebuild_jobs_mode_check CHECK (mode IN ('compare', 'replace')),
    CONSTRAINT rebuild_jobs_from_check CHECK (from_storage = '' OR mode = 'compare')
);

-- The differences a compare job found, by name, the first 1000 in the
-- order the comparison found them, kept with the job so an admin reads
-- which rows differ after its shadow is gone; the job's tallies count
-- them all. row_key names the row by its label or UIDs, joined by "/".
CREATE TABLE rebuild_differences (
    job_id     text    COLLATE "C" NOT NULL,
    seq        integer NOT NULL,
    table_name text    COLLATE "C" NOT NULL,
    kind       text    COLLATE "C" NOT NULL,
    row_key    text    COLLATE "C" NOT NULL,
    CONSTRAINT rebuild_differences_pkey PRIMARY KEY (job_id, seq),
    CONSTRAINT rebuild_differences_seq_check CHECK (seq >= 0),
    CONSTRAINT rebuild_differences_kind_check CHECK (kind IN ('added', 'removed', 'changed'))
);

-- A correction: a recorded change to archived data, a delete, a relabel or
-- a replace, committed first in the state prepared with its operation id
-- (a ULID in lowercase, so 26 characters), its target, its reason and its
-- audit entry, before anything is written to a storage. It moves only
-- forward: prepared to committed or abandoned, committed to purging or (a
-- delete) undone, purging to purged; the CHECKs hold that a committed one
-- has its purge time and an ended one its end. records_state is the state
-- its correction records on the storages were last written with ('' before
-- the first), so a stop between writing the row and the records is found at
-- start. swept says an abandoned relabel's copies were removed.
-- retention_rule and retention_due are, for a delete a disposal made
-- (origin retention), the rule and the due time its session was listed
-- under; override_retention and override_reason say a person's delete went
-- over a rule's keep-at-least, and why. copies_in_flight_until is the last
-- moment a put of a copy to a replica that the purge's start or the
-- delete's publish ended can still land: the purge removes nothing before it.
CREATE TABLE corrections (
    id                 text        COLLATE "C" NOT NULL,
    kind               text        COLLATE "C" NOT NULL,
    level              text        COLLATE "C" NOT NULL,
    state              text        COLLATE "C" NOT NULL,
    records_state      text        COLLATE "C" NOT NULL,
    origin             text        COLLATE "C" NOT NULL,
    subject_label      text        COLLATE "C" NOT NULL,
    session_label      text        COLLATE "C" NOT NULL,
    scan_label         text        COLLATE "C" NOT NULL,
    instance_uid       text        COLLATE "C" NOT NULL,
    to_label           text        COLLATE "C" NOT NULL,
    quarantine_id      text        COLLATE "C" NOT NULL,
    reason             text        NOT NULL,
    key_id             text        COLLATE "C" NOT NULL,
    confirm_token      text        COLLATE "C",
    created            timestamptz NOT NULL,
    committed_at       timestamptz,
    purge_after        timestamptz,
    ended_at           timestamptz,
    state_reason       text        NOT NULL,
    old_revision       bigint      NOT NULL,
    new_revision       bigint      NOT NULL,
    old_sha256         bytea,
    new_sha256         bytea,
    received_sha256    bytea,
    new_size           bigint      NOT NULL,
    instance_key       text        COLLATE "C" NOT NULL,
    instance_size      bigint      NOT NULL,
    restore_line       bytea,
    arrivals_allowed   boolean     NOT NULL,
    undo_reason        text        NOT NULL,
    unresolved         boolean     NOT NULL,
    subjects           bigint      NOT NULL,
    sessions           bigint      NOT NULL,
    scans              bigint      NOT NULL,
    instances          bigint      NOT NULL,
    bytes              bigint      NOT NULL,
    objects_copied     bigint      NOT NULL,
    objects_superseded bigint      NOT NULL,
    swept              boolean     NOT NULL DEFAULT false,
    retention_rule     text        COLLATE "C" NOT NULL DEFAULT '',
    retention_due      timestamptz,
    override_retention boolean     NOT NULL DEFAULT false,
    override_reason    text        NOT NULL DEFAULT '',
    copies_in_flight_until timestamptz,
    CONSTRAINT corrections_pkey PRIMARY KEY (id),
    CONSTRAINT corrections_retention_check CHECK ((origin = 'retention') = (retention_rule <> '')),
    CONSTRAINT corrections_retention_due_check CHECK ((retention_rule = '') = (retention_due IS NULL)),
    CONSTRAINT corrections_override_check CHECK (override_retention = (override_reason <> '')),
    CONSTRAINT corrections_id_check CHECK (length(id) = 26),
    CONSTRAINT corrections_kind_check CHECK (kind IN ('delete', 'relabel', 'replace', 'edit')),
    CONSTRAINT corrections_level_check CHECK (level IN ('subject', 'session', 'scan', 'instance')),
    CONSTRAINT corrections_state_check CHECK (state IN ('prepared', 'committed', 'purging', 'purged', 'undone', 'abandoned')),
    CONSTRAINT corrections_origin_check CHECK (origin IN ('person', 'retention')),
    CONSTRAINT corrections_counts_check CHECK (old_revision >= 0 AND new_revision >= 0 AND new_size >= 0 AND instance_size >= 0
        AND subjects >= 0 AND sessions >= 0 AND scans >= 0 AND instances >= 0 AND bytes >= 0
        AND objects_copied >= 0 AND objects_superseded >= 0),
    CONSTRAINT corrections_sha256_check CHECK ((old_sha256 IS NULL OR octet_length(old_sha256) = 32)
        AND (new_sha256 IS NULL OR octet_length(new_sha256) = 32)
        AND (received_sha256 IS NULL OR octet_length(received_sha256) = 32)),
    CONSTRAINT corrections_commit_check CHECK ((committed_at IS NULL) = (purge_after IS NULL)),
    CONSTRAINT corrections_ended_check CHECK ((state IN ('purged', 'undone', 'abandoned')) = (ended_at IS NOT NULL))
);

-- A confirm token is used once; a Delete that carries it again answers the
-- correction it made. A replace of one quarantined file, and the list,
-- newest first, by kind and by state.
CREATE UNIQUE INDEX corrections_by_confirm ON corrections (confirm_token) WHERE confirm_token IS NOT NULL;
CREATE INDEX corrections_by_quarantine ON corrections (quarantine_id) WHERE quarantine_id <> '';
CREATE INDEX corrections_by_state ON corrections (state, id);
CREATE INDEX corrections_due ON corrections (purge_after, id) WHERE state = 'committed' AND NOT unresolved;

-- The scans a correction touches, by the labels they had when it was
-- made, and for a relabel the labels it gives them; the storage each is on.
CREATE TABLE correction_scans (
    correction_id text   COLLATE "C" NOT NULL,
    series_uid    text   COLLATE "C" NOT NULL,
    study_uid     text   COLLATE "C" NOT NULL,
    storage       text   COLLATE "C" NOT NULL,
    subject_label text   COLLATE "C" NOT NULL,
    session_label text   COLLATE "C" NOT NULL,
    scan_label    text   COLLATE "C" NOT NULL,
    new_subject   text   COLLATE "C" NOT NULL,
    new_session   text   COLLATE "C" NOT NULL,
    new_scan      text   COLLATE "C" NOT NULL,
    instances     bigint NOT NULL,
    bytes         bigint NOT NULL,
    CONSTRAINT correction_scans_pkey PRIMARY KEY (correction_id, series_uid),
    CONSTRAINT correction_scans_counts_check CHECK (instances >= 0 AND bytes >= 0)
);

-- The scans of the corrections under a subject's label, old or new, in the
-- case folded form a label is unique in (a label is ASCII, so lower() is
-- label.Fold): a correction that would overlap one not ended is refused,
-- and an arrival into a path one not ended holds is held.
CREATE INDEX correction_scans_by_subject     ON correction_scans (lower(subject_label));
CREATE INDEX correction_scans_by_new_subject ON correction_scans (lower(new_subject));

-- The objects a correction owns on its storages, fixed in its publishing
-- transaction from the rows it hides, relabels or supersedes: each
-- instance's object (ord 1) by its scan's labels then, its SOP Instance UID
-- and the key the catalog held whole ('' for the layout's), and each
-- scan's metadata (ord 2) and manifest (ord 3) with the SHA-256 read before
-- the publish. A purge removes these and nothing else: an object a live
-- row resolves to is kept (kept_shared), a manifest or metadata whose bytes
-- are no longer the ones read is kept (kept_changed). storage is the
-- primary; the purge removes the object from it and from each of its
-- replicas at the same key. copies_done names, separated by spaces, the
-- copies whose removal of an object not yet settled is recorded already
-- (another copy did not answer), each as name@generation, its switch
-- generation then, so the purge run again asks only the rest; once one
-- copy's generation moved, it asks every copy again.
CREATE TABLE correction_keys (
    correction_id    text   COLLATE "C" NOT NULL,
    ord              bigint NOT NULL,
    storage          text   COLLATE "C" NOT NULL,
    subject_label    text   COLLATE "C" NOT NULL,
    session_label    text   COLLATE "C" NOT NULL,
    scan_label       text   COLLATE "C" NOT NULL,
    sop_instance_uid text   COLLATE "C" NOT NULL,
    storage_key      text   COLLATE "C" NOT NULL,
    size             bigint NOT NULL,
    sha256           bytea,
    outcome          text   COLLATE "C" NOT NULL,
    copies_done      text   COLLATE "C" NOT NULL DEFAULT '',
    CONSTRAINT correction_keys_pkey PRIMARY KEY (correction_id, ord, storage, subject_label, session_label, scan_label, sop_instance_uid),
    CONSTRAINT correction_keys_ord_check CHECK (ord IN (1, 2, 3)),
    CONSTRAINT correction_keys_size_check CHECK (size >= 0),
    CONSTRAINT correction_keys_sha256_check CHECK (sha256 IS NULL OR octet_length(sha256) = 32),
    CONSTRAINT correction_keys_outcome_check CHECK (outcome IN ('', 'purged', 'kept_shared', 'kept_changed'))
);

-- What a purge has left to do, in order.
CREATE INDEX correction_keys_pending ON correction_keys (correction_id, ord) WHERE outcome = '';

-- The last time the server's clock was trusted for a purge: an automatic
-- purge is refused when the clock reads earlier than a time the
-- corrections recorded, and paused when it jumped forward too far since.
-- One row, made here.
CREATE TABLE correction_clock (
    one     bigint      NOT NULL,
    trusted timestamptz,
    CONSTRAINT correction_clock_pkey PRIMARY KEY (one),
    CONSTRAINT correction_clock_one_check CHECK (one = 1)
);
INSERT INTO correction_clock (one, trusted) VALUES (1, NULL);

-- Each object a purge removed, by storage, the storage's switch generation
-- when it acted (the place it acted at), and key, and its version on a
-- versioned bucket ('' for none): its size and SHA-256 as the manifest or
-- the catalog held them, what the storage did (removed, hidden, retained,
-- unknown), and the primary the storage was a replica of then ('' for a
-- primary). A replica detached with abandon_purges has its objects not
-- yet removed recorded unknown.
CREATE TABLE correction_objects (
    correction_id text        COLLATE "C" NOT NULL,
    storage       text        COLLATE "C" NOT NULL,
    generation    bigint      NOT NULL DEFAULT 0,
    replica_of    text        COLLATE "C" NOT NULL DEFAULT '',
    object_key    text        COLLATE "C" NOT NULL,
    version       text        COLLATE "C" NOT NULL,
    size          bigint      NOT NULL,
    sha256        bytea,
    disposition   text        COLLATE "C" NOT NULL,
    retain_until  timestamptz,
    purged_at     timestamptz NOT NULL,
    CONSTRAINT correction_objects_pkey PRIMARY KEY (correction_id, storage, generation, object_key, version),
    CONSTRAINT correction_objects_generation_check CHECK (generation >= 0),
    CONSTRAINT correction_objects_key_check CHECK (object_key <> ''),
    CONSTRAINT correction_objects_size_check CHECK (size >= 0),
    CONSTRAINT correction_objects_sha256_check CHECK (sha256 IS NULL OR octet_length(sha256) = 32),
    CONSTRAINT correction_objects_disposition_check CHECK (disposition IN ('removed', 'hidden', 'retained', 'unknown'))
);

-- A correction's record owed to a replica of its target's storages: one
-- row per replica made in the transactions that publish the correction
-- and that start and end its purge, and in the one that attaches a
-- replica, and one made again when a replica could not take a record
-- rewritten later; removed once the replica holds the record at the correction's
-- state. While a row stands, the correction's purge leaves every copy as
-- it is, and a switch to that replica does not swap: a rebuild or a switch
-- reading the replica alone would not know the correction.
CREATE TABLE correction_records_owed (
    correction_id text        COLLATE "C" NOT NULL,
    storage       text        COLLATE "C" NOT NULL,
    owed_at       timestamptz NOT NULL,
    CONSTRAINT correction_records_owed_pkey PRIMARY KEY (correction_id, storage)
);

-- The records owed to one replica, which a switch to it and its detach
-- read.
CREATE INDEX correction_records_owed_by_storage ON correction_records_owed (storage, correction_id);

-- A replica detached with abandon_purges while a correction counted it
-- among its copies: the person's reason and when. Its objects not yet
-- removed are recorded unknown (correction_objects).
CREATE TABLE correction_copies_abandoned (
    correction_id text        COLLATE "C" NOT NULL,
    storage       text        COLLATE "C" NOT NULL,
    reason        text        COLLATE "C" NOT NULL,
    abandoned_at  timestamptz NOT NULL,
    CONSTRAINT correction_copies_abandoned_pkey PRIMARY KEY (correction_id, storage)
);

-- What names a delete's data (a subject's label, a session's Study Instance
-- UID, a scan's Series Instance UID, an instance's SOP Instance UID), from
-- the delete's prepare on, so an arrival that matches is held as
-- was_deleted, until an admin allows it again.
CREATE TABLE deleted_targets (
    kind          text COLLATE "C" NOT NULL,
    value         text COLLATE "C" NOT NULL,
    correction_id text COLLATE "C" NOT NULL,
    CONSTRAINT deleted_targets_pkey PRIMARY KEY (kind, value, correction_id),
    CONSTRAINT deleted_targets_kind_check CHECK (kind IN ('subject', 'study', 'series', 'instance')),
    CONSTRAINT deleted_targets_value_check CHECK (value <> '')
);

CREATE INDEX deleted_targets_by_correction ON deleted_targets (correction_id);

-- A revision of an instance that is no longer current: superseded by a
-- replace, or deleted by a delete whose purge removed it, kept for as long
-- as the tenant is. A replace never writes a key twice: its new revision has
-- a key of its own. purged says its bytes were removed.
CREATE TABLE instance_revisions (
    sop_instance_uid text    COLLATE "C" NOT NULL,
    revision         bigint  NOT NULL,
    sha256           bytea   NOT NULL,
    size             bigint  NOT NULL,
    storage          text    COLLATE "C" NOT NULL,
    storage_key      text    COLLATE "C" NOT NULL,
    correction_id    text    COLLATE "C" NOT NULL,
    fate             text    COLLATE "C" NOT NULL,
    purged           boolean NOT NULL,
    CONSTRAINT instance_revisions_pkey PRIMARY KEY (sop_instance_uid, revision),
    CONSTRAINT instance_revisions_check CHECK (revision >= 1 AND size >= 0 AND octet_length(sha256) = 32),
    CONSTRAINT instance_revisions_fate_check CHECK (fate IN ('superseded', 'deleted'))
);

CREATE INDEX instance_revisions_by_correction ON instance_revisions (correction_id);

-- An edit's instances: each one's revision and archived checksum when the
-- edit read it, its line of the scan's metadata then (written back by a
-- stop before the publish and by an undo, and cleared once the edit ends),
-- and the new revision it wrote (new_sha256 set by the publish). The new
-- revision is above every revision the instance was ever given (an undone
-- edit's among them), so no two corrections name one key. new_line is the
-- new revision's line from the publish until the edit ends, which an undo
-- that may not be published writes back.
CREATE TABLE correction_edits (
    correction_id    text   COLLATE "C" NOT NULL,
    sop_instance_uid text   COLLATE "C" NOT NULL,
    series_uid       text   COLLATE "C" NOT NULL,
    old_revision     bigint NOT NULL,
    new_revision     bigint NOT NULL,
    old_sha256       bytea  NOT NULL,
    new_sha256       bytea,
    old_key          text   COLLATE "C" NOT NULL,
    old_size         bigint NOT NULL,
    new_size         bigint NOT NULL,
    restore_line     bytea,
    new_line         bytea,
    CONSTRAINT correction_edits_pkey PRIMARY KEY (correction_id, sop_instance_uid),
    CONSTRAINT correction_edits_check CHECK (old_revision >= 1 AND new_revision > old_revision AND old_size >= 0
        AND new_size >= 0 AND octet_length(old_sha256) = 32 AND (new_sha256 IS NULL OR octet_length(new_sha256) = 32))
);

CREATE INDEX correction_edits_by_instance ON correction_edits (sop_instance_uid);

-- The catalog values of each scan an edit touches as they were before it,
-- which its undo gives back.
CREATE TABLE correction_edit_scans (
    correction_id      text   COLLATE "C" NOT NULL,
    series_uid         text   COLLATE "C" NOT NULL,
    modality           text   COLLATE "C" NOT NULL,
    series_description text   NOT NULL,
    series_number      integer,
    CONSTRAINT correction_edit_scans_pkey PRIMARY KEY (correction_id, series_uid)
);

-- A relabel's labels, before and after, at its level.
CREATE TABLE relabels (
    correction_id text COLLATE "C" NOT NULL,
    level         text COLLATE "C" NOT NULL,
    old_subject   text COLLATE "C" NOT NULL,
    old_session   text COLLATE "C" NOT NULL,
    old_scan      text COLLATE "C" NOT NULL,
    new_label     text COLLATE "C" NOT NULL,
    CONSTRAINT relabels_pkey PRIMARY KEY (correction_id),
    CONSTRAINT relabels_level_check CHECK (level IN ('subject', 'session', 'scan'))
);

-- A lookup by labels that finds nothing asks which relabel gave them up
-- (the old subject's relabels after a given one, in order): served by this
-- index, so a not found stays as cheap as the read it follows.
CREATE INDEX relabels_by_old_subject ON relabels (old_subject, correction_id);

-- The catalog's rows a committed delete hides, as they were: a delete's
-- publishing transaction moves them here, each with the same columns as its
-- catalog table and the correction's id, so no read of the catalog sees
-- them and an undo moves them back as they were; its purge removes them.
-- They take no CHECK of their own: they hold only rows the catalog held.
CREATE TABLE hidden_subjects (
    correction_id  text        COLLATE "C" NOT NULL,
    id             bigint      NOT NULL,
    label          text        COLLATE "C" NOT NULL,
    folded         text        COLLATE "C" NOT NULL,
    created        timestamptz NOT NULL,
    session_count  bigint      NOT NULL,
    instance_count bigint      NOT NULL,
    bytes          bigint      NOT NULL,
    updated        timestamptz NOT NULL,
    PRIMARY KEY (correction_id, id)
);

CREATE TABLE hidden_sessions (
    correction_id  text        COLLATE "C" NOT NULL,
    id             bigint      NOT NULL,
    study_uid      text        COLLATE "C" NOT NULL,
    subject_id     bigint      NOT NULL,
    label          text        COLLATE "C" NOT NULL,
    folded         text        COLLATE "C" NOT NULL,
    storage        text        COLLATE "C" NOT NULL,
    created        timestamptz NOT NULL,
    subject_label  text        COLLATE "C" NOT NULL,
    subject_folded text        COLLATE "C" NOT NULL,
    study_date     text        COLLATE "C" NOT NULL,
    receiver       text        COLLATE "C" NOT NULL,
    scan_count     bigint      NOT NULL,
    instance_count bigint      NOT NULL,
    bytes          bigint      NOT NULL,
    updated        timestamptz NOT NULL,
    route          text        NOT NULL,
    PRIMARY KEY (correction_id, id)
);

CREATE TABLE hidden_scans (
    correction_id      text        COLLATE "C" NOT NULL,
    id                 bigint      NOT NULL,
    session_id         bigint      NOT NULL,
    series_uid         text        COLLATE "C" NOT NULL,
    label              text        COLLATE "C" NOT NULL,
    folded             text        COLLATE "C" NOT NULL,
    instances          bigint      NOT NULL,
    bytes              bigint      NOT NULL,
    manifest_sha256    bytea,
    created            timestamptz NOT NULL,
    updated            timestamptz NOT NULL,
    subject_label      text        COLLATE "C" NOT NULL,
    subject_folded     text        COLLATE "C" NOT NULL,
    session_label      text        COLLATE "C" NOT NULL,
    session_folded     text        COLLATE "C" NOT NULL,
    storage            text        COLLATE "C" NOT NULL,
    modality           text        COLLATE "C" NOT NULL,
    series_description text        NOT NULL,
    description_folded text        COLLATE "C" NOT NULL,
    series_number      integer,
    receiver           text        COLLATE "C" NOT NULL,
    study_uid          text        COLLATE "C" NOT NULL,
    PRIMARY KEY (correction_id, id)
);

CREATE TABLE hidden_instances (
    correction_id      text        COLLATE "C" NOT NULL,
    sop_instance_uid   text        COLLATE "C" NOT NULL,
    scan_id            bigint      NOT NULL,
    reception_id       text        COLLATE "C" NOT NULL,
    sha256             bytea       NOT NULL,
    archived_sha256    bytea,
    storage_key        text        COLLATE "C" NOT NULL,
    byte_offset        bigint      NOT NULL,
    byte_length        bigint      NOT NULL,
    size               bigint      NOT NULL,
    held_at            timestamptz NOT NULL,
    profile_version_id bigint,
    pixel_risk_accepted boolean    NOT NULL DEFAULT false,
    PRIMARY KEY (correction_id, sop_instance_uid)
);

-- A hidden instance is found by its UID (ResolveInstances).
CREATE INDEX hidden_instances_by_uid ON hidden_instances (sop_instance_uid);

CREATE TABLE hidden_session_modalities (
    correction_id text        COLLATE "C" NOT NULL,
    session_id    bigint      NOT NULL,
    modality      text        COLLATE "C" NOT NULL,
    created       timestamptz NOT NULL,
    PRIMARY KEY (correction_id, session_id, modality)
);

CREATE TABLE hidden_reception_scans (
    correction_id text   COLLATE "C" NOT NULL,
    reception_id  text   COLLATE "C" NOT NULL,
    scan_id       bigint NOT NULL,
    instances     bigint NOT NULL,
    bytes         bigint NOT NULL,
    PRIMARY KEY (correction_id, reception_id, scan_id)
);

-- A hidden row's labels and UIDs stay taken for arrivals while its delete
-- is not ended: the archive step looks each scan's path, study and series
-- up here before it puts anything.
CREATE INDEX hidden_subjects_by_label  ON hidden_subjects (folded);
CREATE INDEX hidden_sessions_by_study  ON hidden_sessions (study_uid);
CREATE INDEX hidden_sessions_by_label  ON hidden_sessions (subject_folded, folded);
CREATE INDEX hidden_scans_by_series    ON hidden_scans (series_uid);
CREATE INDEX hidden_scans_by_label     ON hidden_scans (subject_folded, session_folded, folded);

-- A replica's counts, kept in the transaction of every change of its rows
-- below, so a status reads them without counting: its scans by state, its
-- damaged objects, the scans its backfill still owes; when it became a
-- replica and when its last backfill was owed nothing more (NULL while one
-- is); and the fault of its last failed copy ('' after one that did not).
-- One row per replica, made by its attach and removed by its detach.
CREATE TABLE replica_totals (
    replica          text        COLLATE "C" NOT NULL,
    pending          bigint      NOT NULL,
    copying          bigint      NOT NULL,
    in_step          bigint      NOT NULL,
    damaged          bigint      NOT NULL,
    objects_damaged  bigint      NOT NULL,
    backfill         bigint      NOT NULL,
    attached_at      timestamptz NOT NULL,
    backfill_done_at timestamptz,
    last_fault       text        COLLATE "C" NOT NULL,
    last_fault_at    timestamptz,
    CONSTRAINT replica_totals_pkey PRIMARY KEY (replica),
    CONSTRAINT replica_totals_counts_check CHECK (pending >= 0 AND copying >= 0 AND in_step >= 0 AND damaged >= 0
        AND objects_damaged >= 0 AND backfill >= 0)
);

-- A replica's state of one of its primary's scans, keyed by the scan's
-- Series Instance UID, never an integer id, so a rebuild that makes the
-- catalog's ids again leaves it standing. It is also the queue of copies:
-- a row pending or damaged is due at due, and a claim leases it (copying,
-- owner, lease_until) under attempt, which fences the copier's writes. seq
-- counts the changes of the scan published since the row was made, and
-- claimed_seq the one a claim saw, so a copy claimed before a change never
-- ends in step. manifest_sha256 is the manifest digest the replica holds
-- verified (NULL before the first); queued_at when the row last became
-- pending (NULL in step), the start of its lag; backfill that the
-- replica's backfill owes it.
CREATE TABLE replica_scans (
    replica         text        COLLATE "C" NOT NULL,
    series_uid      text        COLLATE "C" NOT NULL,
    state           text        COLLATE "C" NOT NULL,
    manifest_sha256 bytea,
    queued_at       timestamptz,
    due             timestamptz NOT NULL,
    seq             bigint      NOT NULL,
    claimed_seq     bigint      NOT NULL,
    attempt         integer     NOT NULL,
    owner           text        NOT NULL,
    lease_until     timestamptz,
    verified_at     timestamptz,
    backfill        boolean     NOT NULL,
    last_error      text        NOT NULL,
    CONSTRAINT replica_scans_pkey PRIMARY KEY (replica, series_uid),
    CONSTRAINT replica_scans_state_check CHECK (state IN ('pending', 'copying', 'in_step', 'damaged')),
    CONSTRAINT replica_scans_values_check CHECK (seq >= 1 AND claimed_seq >= 0 AND attempt >= 0
        AND (manifest_sha256 IS NULL OR length(manifest_sha256) = 32)),
    CONSTRAINT replica_scans_queued_check CHECK ((state = 'in_step') = (queued_at IS NULL)),
    CONSTRAINT replica_scans_lease_check CHECK ((state = 'copying') = (lease_until IS NOT NULL))
);

-- A claim reads the rows due, earliest first, and the leases ended, and
-- only those: rows in step, however many, are in neither index. Each holds
-- the claim's whole order, so a claim reads its few rows in order and
-- sorts none however long the backlog. A read that turns to a replica
-- finds a scan's rows by its series; a status finds a replica's oldest row
-- not in step.
CREATE INDEX replica_scans_due     ON replica_scans (due, replica, series_uid) WHERE state IN ('pending', 'damaged');
CREATE INDEX replica_scans_leased  ON replica_scans (lease_until, replica, series_uid) WHERE state = 'copying';
CREATE INDEX replica_scans_by_uid  ON replica_scans (series_uid);
CREATE INDEX replica_scans_lagging ON replica_scans (replica, queued_at) WHERE queued_at IS NOT NULL;

-- An object of a replica a scrub or a read found missing or wrong, to copy
-- again from the primary: sparse, so a replica that is whole holds none.
-- sop_instance_uid is '' for the scan's manifest or metadata; the found
-- size and checksum are NULL for an object missing. finding_id is the scrub
-- finding recorded with it ('' for damage a read or a copy found), which is
-- cleared when the damage is; on the scan's row ('') it is the manifest's,
-- and sidecar_finding_id the metadata file's, so the one row keeps both
-- and its end clears both.
CREATE TABLE replica_damage (
    replica            text        COLLATE "C" NOT NULL,
    series_uid         text        COLLATE "C" NOT NULL,
    sop_instance_uid   text        COLLATE "C" NOT NULL,
    revision           integer     NOT NULL,
    found_size         bigint,
    found_sha256       bytea,
    found_at           timestamptz NOT NULL,
    finding_id         text        COLLATE "C" NOT NULL DEFAULT '',
    sidecar_finding_id text        COLLATE "C" NOT NULL DEFAULT '',
    CONSTRAINT replica_damage_pkey PRIMARY KEY (replica, series_uid, sop_instance_uid),
    CONSTRAINT replica_damage_values_check CHECK (revision >= 0 AND (found_size IS NULL OR found_size >= 0)
        AND (found_sha256 IS NULL OR length(found_sha256) = 32)),
    CONSTRAINT replica_damage_sidecar_check CHECK (sidecar_finding_id = '' OR sop_instance_uid = '')
);

-- The SHA-256 of each scan's metadata file (its sidecar of headers) as last
-- written, beside the digest of the manifest written with it: it holds only
-- while the scan's manifest_sha256 is that manifest's, so a change of the
-- scan that wrote a new manifest without telling the sidecar's digest (an
-- older writer, a purge) leaves the sidecar unchecked, as a scan archived
-- before digests were kept is, never checked against a digest that no
-- longer describes it; a writer that puts a new file before it can record
-- its digest (a pass that rewrites it, an instance delete's purge) drops
-- the row first. Kept apart from scans so a rebuild, which reads the
-- storages' files and does not hash them, does not compare it (it drops
-- only the rows of scans it did not find again), and keyed by the Series
-- Instance UID, never an integer id.
CREATE TABLE scan_sidecars (
    series_uid      text  COLLATE "C" NOT NULL,
    manifest_sha256 bytea NOT NULL,
    sha256          bytea NOT NULL,
    CONSTRAINT scan_sidecars_pkey PRIMARY KEY (series_uid),
    CONSTRAINT scan_sidecars_values_check CHECK (length(manifest_sha256) = 32 AND length(sha256) = 32)
);

-- A recall of an object of an archive-tier storage: one per object, shared
-- by every request and job waiting for it, so one recall is asked of the
-- store however many wait. object_key holds a SOP Instance UID as received
-- and is kept in this table only. due is when the worker next asks or
-- polls; online_until when the store said the object goes back to its cold
-- class (NULL when it did not say).
CREATE TABLE recalls (
    storage      text        COLLATE "C" NOT NULL,
    object_key   text        COLLATE "C" NOT NULL,
    state        text        COLLATE "C" NOT NULL,
    days         integer     NOT NULL,
    speed        text        COLLATE "C" NOT NULL,
    requested_at timestamptz NOT NULL,
    updated      timestamptz NOT NULL,
    due          timestamptz NOT NULL,
    online_until timestamptz,
    reason       text        NOT NULL,
    CONSTRAINT recalls_pkey PRIMARY KEY (storage, object_key),
    CONSTRAINT recalls_state_check CHECK (state IN ('requested', 'in_progress', 'online', 'failed')),
    CONSTRAINT recalls_values_check CHECK (object_key <> '' AND days BETWEEN 1 AND 365
        AND speed IN ('', 'standard', 'bulk', 'expedited'))
);

-- The worker reads the recalls due, earliest first, and only those; a
-- status counts them by state, and a replica's in progress.
CREATE INDEX recalls_due      ON recalls (due) WHERE state IN ('requested', 'in_progress');
CREATE INDEX recalls_by_state ON recalls (state, storage);

-- A primary's object put back from a replica: what the damaged object was
-- (its found size and checksum, NULL for one missing) recorded before the
-- good bytes are put over it, the finding it mends, the replica and the
-- job, and when it ended (NULL until then), with its outcome ('changed'
-- when the catalog no longer named the bytes it expected). series_uid is
-- its object's scan. While it runs it holds that scan still: open_work and
-- open_attempt name the unit of work and attempt that recorded it, and the
-- hold stands until hold_until, that unit's due at the record plus a grace
-- longer than a put cut at its deadline takes to land (all three NULL once
-- it ended or was let go, or for a record made under no unit).
CREATE TABLE repairs (
    id               text        COLLATE "C" NOT NULL,
    finding_id       text        COLLATE "C" NOT NULL,
    storage          text        COLLATE "C" NOT NULL,
    object_key       text        COLLATE "C" NOT NULL,
    sop_instance_uid text        COLLATE "C" NOT NULL,
    revision         integer     NOT NULL,
    from_replica     text        COLLATE "C" NOT NULL,
    job_id           text        COLLATE "C" NOT NULL,
    expected_size    bigint      NOT NULL,
    expected_sha256  bytea       NOT NULL,
    found_size       bigint,
    found_sha256     bytea,
    recorded_at      timestamptz NOT NULL,
    repaired_at      timestamptz,
    series_uid       text        COLLATE "C" NOT NULL,
    outcome          text        COLLATE "C" NOT NULL,
    hold_until       timestamptz,
    open_work        text        COLLATE "C",
    open_attempt     integer,
    CONSTRAINT repairs_pkey PRIMARY KEY (id),
    CONSTRAINT repairs_values_check CHECK (object_key <> '' AND revision >= 0 AND expected_size >= 0
        AND length(expected_sha256) = 32 AND (found_size IS NULL OR found_size >= 0)
        AND (found_sha256 IS NULL OR length(found_sha256) = 32)),
    CONSTRAINT repairs_outcome_check CHECK (outcome IN ('', 'repaired', 'changed')
        AND (repaired_at IS NULL) = (outcome = '')),
    CONSTRAINT repairs_hold_check CHECK ((open_work IS NULL) = (open_attempt IS NULL)
        AND (open_work IS NULL) = (hold_until IS NULL) AND (repaired_at IS NULL OR open_work IS NULL))
);

CREATE INDEX repairs_by_finding ON repairs (finding_id, recorded_at, id);
-- A change of a scan looks for a repair holding it, among those that hold
-- alone.
CREATE INDEX repairs_holding ON repairs (series_uid) WHERE open_work IS NOT NULL;

-- What a repair job mends: its primary, the replica it reads from ('' for
-- any in step), the ids of the findings it names as a JSON array ('[]'
-- with all or for an automatic job), and how it was made. A job's
-- unfinished repair of a storage is found by it.
CREATE TABLE repair_jobs (
    job_id       text    COLLATE "C" NOT NULL,
    storage      text    COLLATE "C" NOT NULL,
    from_replica text    COLLATE "C" NOT NULL,
    findings     text    NOT NULL,
    all_findings boolean NOT NULL,
    automatic    boolean NOT NULL,
    CONSTRAINT repair_jobs_pkey PRIMARY KEY (job_id),
    CONSTRAINT repair_jobs_mode_check CHECK (NOT (all_findings AND automatic))
);

CREATE INDEX repair_jobs_by_storage ON repair_jobs (storage);

-- Each finding a repair job counted, with the outcome counted, so an
-- attempt that walks the findings again counts none twice; one counted
-- no_good_copy and repaired later moves from refused to sent.
CREATE TABLE repair_outcomes (
    job_id     text   COLLATE "C" NOT NULL,
    finding_id text   COLLATE "C" NOT NULL,
    outcome    text   COLLATE "C" NOT NULL,
    bytes      bigint NOT NULL,
    CONSTRAINT repair_outcomes_pkey PRIMARY KEY (job_id, finding_id),
    CONSTRAINT repair_outcomes_check CHECK (outcome IN ('repaired', 'already_good', 'no_good_copy') AND bytes >= 0)
);

-- A switch of a primary to its replica, recorded when it is accepted, with
-- each step it reaches: accepted, then swapped once the one transaction
-- that swaps the two places committed, then done; or refused before the
-- swap. confirm_token is the preview's token, used once. generation is the
-- primary's switch generation when it was accepted.
CREATE TABLE storage_switches (
    id                   text        COLLATE "C" NOT NULL,
    primary_name         text        COLLATE "C" NOT NULL,
    replica_name         text        COLLATE "C" NOT NULL,
    state                text        COLLATE "C" NOT NULL,
    step                 text        COLLATE "C" NOT NULL,
    confirm_token        text        COLLATE "C" NOT NULL,
    generation           bigint      NOT NULL,
    skip_verification    boolean     NOT NULL,
    skip_reason          text        NOT NULL,
    key_id               text        COLLATE "C" NOT NULL,
    accepted_at          timestamptz NOT NULL,
    swapped_at           timestamptz,
    ended_at             timestamptz,
    refusal              text        NOT NULL,
    scans_drained        bigint      NOT NULL,
    associations_refused bigint      NOT NULL,
    pause_us             bigint      NOT NULL,
    CONSTRAINT storage_switches_pkey PRIMARY KEY (id),
    CONSTRAINT storage_switches_token_key UNIQUE (confirm_token),
    CONSTRAINT storage_switches_state_check CHECK (state IN ('accepted', 'swapped', 'done', 'refused')),
    CONSTRAINT storage_switches_counts_check CHECK (generation >= 0 AND scans_drained >= 0 AND associations_refused >= 0
        AND pause_us >= 0),
    CONSTRAINT storage_switches_ended_check CHECK ((state IN ('done', 'refused')) = (ended_at IS NOT NULL)),
    CONSTRAINT storage_switches_swapped_check CHECK ((state IN ('swapped', 'done')) = (swapped_at IS NOT NULL))
);

-- The switches running (a start refuses or finishes them, and a new one
-- waits for them), and a storage's last switch, as primary or as replica.
CREATE INDEX storage_switches_running    ON storage_switches (accepted_at, id) WHERE state IN ('accepted', 'swapped');
CREATE INDEX storage_switches_by_primary ON storage_switches (primary_name, accepted_at, id);
CREATE INDEX storage_switches_by_replica ON storage_switches (replica_name, accepted_at, id);

-- A retention rule: its selector (receiver and route, or storage, or none
-- for the whole tenant), its periods in years then days (0 and 0 for
-- none), its action, and until when the clamp of its last change that made
-- anything due sooner or kept it less holds (NULL for none).
CREATE TABLE retention_rules (
    name          text        COLLATE "C" NOT NULL,
    receiver      text        COLLATE "C" NOT NULL,
    route         text        COLLATE "C" NOT NULL,
    storage       text        COLLATE "C" NOT NULL,
    keep_years    bigint      NOT NULL,
    keep_days     bigint      NOT NULL,
    dispose_years bigint      NOT NULL,
    dispose_days  bigint      NOT NULL,
    action        text        COLLATE "C" NOT NULL,
    created       timestamptz NOT NULL,
    updated       timestamptz NOT NULL,
    notice_until  timestamptz,
    CONSTRAINT retention_rules_pkey PRIMARY KEY (name),
    CONSTRAINT retention_rules_periods_check CHECK (keep_years BETWEEN 0 AND 100 AND keep_days BETWEEN 0 AND 365
        AND dispose_years BETWEEN 0 AND 100 AND dispose_days BETWEEN 0 AND 365
        AND keep_years + keep_days + dispose_years + dispose_days > 0),
    CONSTRAINT retention_rules_action_check CHECK (action IN ('review', 'delete'))
);

-- The retention times of a session, keyed by its Study Instance UID,
-- never an integer id, so a rebuild that makes the catalog's ids again
-- leaves it standing: start_at is when retention counts from (when the
-- session was first archived, by the server's clock, or an import's
-- retention start), archived_at when it was first archived, floor_at the
-- time it is never due before (an import's start: archived plus the
-- notice; NULL for none), keep_until the latest time a rule keeps it until,
-- due_at when it is due under rule (NULL for never), with that rule's
-- action. A session a delete hides keeps its row, which every listing
-- passes over (it reads only sessions the catalog shows), so an undo finds
-- its times as they were; the delete's purge removes the row. exempt_rule
-- names the rule a person's undo of a retention delete exempts it from in
-- the daily pass, '' for none.
CREATE TABLE retention_sessions (
    study_uid   text        COLLATE "C" NOT NULL,
    start_at    timestamptz NOT NULL,
    archived_at timestamptz NOT NULL,
    floor_at    timestamptz,
    keep_until  timestamptz,
    due_at      timestamptz,
    rule        text        COLLATE "C" NOT NULL,
    action      text        COLLATE "C" NOT NULL,
    exempt_rule text        COLLATE "C" NOT NULL,
    CONSTRAINT retention_sessions_pkey PRIMARY KEY (study_uid),
    CONSTRAINT retention_sessions_action_check CHECK (action IN ('', 'review', 'delete')),
    CONSTRAINT retention_sessions_due_check CHECK ((due_at IS NULL) = (rule = ''))
);

-- The sessions due now under a rule, oldest due first, read as a range of
-- one index (a held or exempt session is passed over as it is read); and
-- those due under every rule, and the next to become due.
CREATE INDEX retention_sessions_due  ON retention_sessions (rule, due_at, study_uid) WHERE due_at IS NOT NULL;
CREATE INDEX retention_sessions_next ON retention_sessions (due_at, study_uid) WHERE due_at IS NOT NULL;

-- Where retention's clock stands: the last time the server's clock was
-- trusted, the daily pass's last time, whether retention is paused (and
-- why, by how far the clock moved, at which reading), the step and notice
-- the last check gave (microseconds), and the last confirmation. One row,
-- made here.
CREATE TABLE retention_state (
    one          bigint      NOT NULL,
    trusted      timestamptz,
    last_pass    timestamptz,
    paused       boolean     NOT NULL,
    pause_reason text        NOT NULL,
    pause_step   bigint      NOT NULL,
    paused_at    timestamptz,
    max_step     bigint      NOT NULL,
    notice       bigint      NOT NULL,
    confirmed_at timestamptz,
    observed_at  timestamptz,
    CONSTRAINT retention_state_pkey PRIMARY KEY (one),
    CONSTRAINT retention_state_one_check CHECK (one = 1),
    CONSTRAINT retention_state_bounds_check CHECK (max_step > 0 AND notice > 0),
    CONSTRAINT retention_state_paused_check CHECK (paused = (paused_at IS NOT NULL))
);
INSERT INTO retention_state (one, trusted, last_pass, paused, pause_reason, pause_step, paused_at, max_step, notice, confirmed_at,
    observed_at) VALUES (1, NULL, NULL, false, '', 0, NULL, 172800000000, 2592000000000, NULL, NULL);

-- A hold: its target by labels as the catalog has them now (a relabel
-- rewrites them) and case folded, its reason, who placed and released it,
-- whether it was made from a record found on a storage with no row, and
-- seq, which counts its changes (placed, relabeled, released) so a record
-- written at an older count is owed again. releasing_at is set while a
-- release is under way: the hold still holds, its count already moved to
-- the release's, before any record says released.
CREATE TABLE holds (
    id               text        COLLATE "C" NOT NULL,
    level            text        COLLATE "C" NOT NULL,
    subject_label    text        COLLATE "C" NOT NULL,
    session_label    text        COLLATE "C" NOT NULL,
    scan_label       text        COLLATE "C" NOT NULL,
    subject_folded   text        COLLATE "C" NOT NULL,
    session_folded   text        COLLATE "C" NOT NULL,
    scan_folded      text        COLLATE "C" NOT NULL,
    reason           text        NOT NULL,
    state            text        COLLATE "C" NOT NULL,
    placed_by        text        COLLATE "C" NOT NULL,
    placed_at        timestamptz NOT NULL,
    release_reason   text        NOT NULL,
    released_by      text        COLLATE "C" NOT NULL,
    released_at      timestamptz,
    found_on_storage boolean     NOT NULL,
    seq              bigint      NOT NULL,
    releasing_at     timestamptz,
    CONSTRAINT holds_pkey PRIMARY KEY (id),
    CONSTRAINT holds_id_check CHECK (length(id) = 26),
    CONSTRAINT holds_level_check CHECK (level IN ('tenant', 'subject', 'session', 'scan')),
    CONSTRAINT holds_state_check CHECK (state IN ('active', 'released')),
    CONSTRAINT holds_seq_check CHECK (seq >= 1),
    CONSTRAINT holds_released_check CHECK ((state = 'released') = (released_at IS NOT NULL)),
    CONSTRAINT holds_releasing_check CHECK (releasing_at IS NULL OR state = 'active')
);

-- The active holds of a subject, which a delete, a purge and a disposal
-- look up; and a subject's holds for a listing.
CREATE INDEX holds_active     ON holds (subject_folded, session_folded, scan_folded) WHERE state = 'active';
CREATE INDEX holds_by_subject ON holds (subject_folded, id);

-- The storages a hold's record goes on, the hold's count its record was
-- last written at there (0 before the first): owed while below the hold's
-- seq; and the version of the installation key it was sealed under (0
-- when not known), so a record under a retired version is written again.
CREATE TABLE hold_records (
    hold_id      text   COLLATE "C" NOT NULL,
    storage      text   COLLATE "C" NOT NULL,
    written_seq  bigint NOT NULL,
    seal_version bigint NOT NULL DEFAULT 0,
    CONSTRAINT hold_records_pkey PRIMARY KEY (hold_id, storage),
    CONSTRAINT hold_records_seq_check CHECK (written_seq >= 0),
    CONSTRAINT hold_records_seal_check CHECK (seal_version >= 0)
);

-- A dispose job's own part: its rule, the admin's reason ('' for the
-- daily pass), whether the pass made it, and the confirm token of the
-- preview it was made from, used once (NULL for the pass).
CREATE TABLE dispose_jobs (
    job_id        text    COLLATE "C" NOT NULL,
    rule          text    COLLATE "C" NOT NULL,
    reason        text    NOT NULL,
    scheduled     boolean NOT NULL,
    confirm_token text    COLLATE "C",
    CONSTRAINT dispose_jobs_pkey PRIMARY KEY (job_id),
    CONSTRAINT dispose_jobs_token_check CHECK (scheduled = (confirm_token IS NULL))
);

CREATE UNIQUE INDEX dispose_jobs_by_confirm ON dispose_jobs (confirm_token) WHERE confirm_token IS NOT NULL;
CREATE INDEX dispose_jobs_by_rule ON dispose_jobs (rule);

-- The sessions a dispose job lists, in order, as they were listed, and
-- what became of each ('' before it is reached) with the delete that took
-- it ('' for none).
CREATE TABLE dispose_sessions (
    job_id        text        COLLATE "C" NOT NULL,
    ord           bigint      NOT NULL,
    study_uid     text        COLLATE "C" NOT NULL,
    subject_label text        COLLATE "C" NOT NULL,
    session_label text        COLLATE "C" NOT NULL,
    due_at        timestamptz NOT NULL,
    instances     bigint      NOT NULL,
    bytes         bigint      NOT NULL,
    outcome       text        COLLATE "C" NOT NULL,
    correction_id text        COLLATE "C" NOT NULL,
    CONSTRAINT dispose_sessions_pkey PRIMARY KEY (job_id, ord),
    CONSTRAINT dispose_sessions_counts_check CHECK (ord >= 1 AND instances >= 0 AND bytes >= 0),
    CONSTRAINT dispose_sessions_outcome_check CHECK (outcome IN ('', 'deleted', 'skipped|held', 'skipped|not_due',
        'skipped|already_deleted', 'skipped|undone',
        'skipped|overlap', 'skipped|refused'))
);

CREATE UNIQUE INDEX dispose_sessions_by_study ON dispose_sessions (job_id, study_uid);
