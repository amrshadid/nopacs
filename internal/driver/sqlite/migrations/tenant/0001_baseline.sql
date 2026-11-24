-- A tenant database's schema, whole: every table once, grouped by what uses
-- it. No table refers to another by a foreign key, so the groups follow the
-- order a reader meets them in: settings and secrets, the audit log, the
-- storages and receivers, receptions and the quarantine, the catalog, the
-- leased units of work, the event feed, de-identification, jobs and their
-- kinds, and corrections.
--
-- The rules are the master's: every table is STRICT, so a value of the
-- wrong type is refused as Postgres would refuse it; times are INTEGER
-- microseconds since 1970 in UTC, NULL for "never"; text a caller did not
-- give is '' rather than NULL; a checksum is a 32-byte BLOB. Counts, sizes
-- and checksum lengths are held by CHECK constraints. Everything a restart
-- must not lose is a row here.
--
-- There are no foreign keys: a reception and a quarantine record keep the
-- name of a receiver removed, and a job the name of a sender removed, by the
-- API contract's rule, and every other reference is checked by the change
-- that makes it, inside its transaction. The state of a reception, the
-- reason of a quarantine record and the type of an event are the core's and
-- the contract's words and have no CHECK: a later version adds to each, and
-- SQLite cannot change a table's CHECK without rebuilding the table.
--
-- No table holds who may enter the tenant: access lives only in the master,
-- so this database can be copied or handed over without carrying it.
--
-- Each list, claim and lookup has the index named for it below; the plan
-- tests (pipeline_plan_test.go) show SQLite's planner uses it.
--
-- The runner records this file's SHA-256 when it makes a database, and
-- refuses to open one whose recorded SHA-256 differs: a database made from
-- another version of this file was made by another build, and is never
-- half-migrated to this one. A later change to the schema is a new file
-- after this one, applied once and never edited.

-- A setting here overrides the installation's value of the same name; a
-- setting absent here takes the installation's.
CREATE TABLE settings (
    name  TEXT NOT NULL PRIMARY KEY,
    value TEXT NOT NULL
) STRICT;

-- The tenant's own secrets, such as the key of the keyed hash its naming
-- rules use, each sealed with the installation key by the server before it
-- is stored: this database never holds a plaintext secret, and a copy of it
-- opens nothing without the installation's key file. A secret is written
-- once and never replaced, since what was made with it depends on it.
CREATE TABLE tenant_secrets (
    name    TEXT    NOT NULL PRIMARY KEY,
    sealed  BLOB    NOT NULL CHECK (length(sealed) BETWEEN 1 AND 1024),
    created INTEGER NOT NULL
) STRICT;

-- The tenant's audit log, with the master's columns, so one entry type and
-- one listing serve both logs. No foreign key: an entry names keys that live
-- in the master. seq is the entry's place in the log's hash chain, given
-- when the entry is sealed, not when it is written, since entries written at
-- once commit in no fixed order; it is NULL until then and never changes
-- once given (the store's SealAudit sets it only where it is NULL).
CREATE TABLE audit (
    id          TEXT    NOT NULL PRIMARY KEY,
    at          INTEGER NOT NULL,
    key_id      TEXT    NOT NULL,
    local_user  TEXT    NOT NULL,
    remote_addr TEXT    NOT NULL,
    method      TEXT    NOT NULL,
    tenant_name TEXT    NOT NULL,
    object      TEXT    NOT NULL,
    outcome     TEXT    NOT NULL CHECK (outcome IN ('done', 'refused', 'failed')),
    reason      TEXT    NOT NULL,
    details     TEXT    NOT NULL,
    request_id  TEXT    NOT NULL,
    seq INTEGER CHECK (seq >= 1)
) STRICT;

-- The audit listings, newest first, with the master's indexes (the same
-- listing code serves both):
--   no filter, or a time range only          audit_by_time
--   a tenant name (with or without a range)   audit_by_tenant
--   a key (with or without a range)           audit_by_key
--   an outcome (with or without a range), so a rare outcome such as
--   "failed" is found without reading every entry   audit_by_outcome
-- A start and an end time narrow the range read from whichever index is
-- used. Two equality filters together (a tenant and a key, or either with an
-- outcome) use the index of one of them, chosen by SQLite's planner, and
-- test the other on the rows that index yields. In a tenant's log the tenant
-- name is usually the tenant's own, so its index matters less than in the
-- master's; it is kept so one query plan serves both.
CREATE INDEX audit_by_time    ON audit (at, id);
CREATE INDEX audit_by_tenant  ON audit (tenant_name, at, id);
CREATE INDEX audit_by_key     ON audit (key_id, at, id);
CREATE INDEX audit_by_outcome ON audit (outcome, at, id);

-- One entry per place in the chain. NULLs are distinct in a UNIQUE index
-- (sqlite.org/lang_createindex.html), so every unsealed entry fits.
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
-- its UNIQUE constraint. from_seq is UNIQUE too, so two seals racing for the
-- same place cannot both be kept.
CREATE TABLE audit_checkpoints (
    to_seq       INTEGER NOT NULL PRIMARY KEY,
    from_seq     INTEGER NOT NULL UNIQUE CHECK (from_seq >= 1),
    installation TEXT    NOT NULL,
    log          TEXT    NOT NULL,
    head         BLOB    NOT NULL CHECK (length(head) = 32),
    previous     BLOB    NOT NULL CHECK (length(previous) = 32),
    digest       BLOB    NOT NULL UNIQUE CHECK (length(digest) = 32),
    at           INTEGER NOT NULL,
    key_version  INTEGER NOT NULL CHECK (key_version BETWEEN 1 AND 2147483647),
    signature    BLOB    NOT NULL CHECK (length(signature) = 64),
    pruned       INTEGER NOT NULL DEFAULT 0 CHECK (pruned IN (0, 1)),
    CHECK (to_seq >= from_seq)
) STRICT;

-- The checkpoints retention has still to reach, oldest first.
CREATE INDEX audit_checkpoints_unpruned ON audit_checkpoints (to_seq) WHERE pruned = 0;

-- The latest checkpoint of another log this one keeps: the master's (log
-- is "master").
CREATE TABLE audit_anchors (
    log    TEXT    NOT NULL PRIMARY KEY,
    to_seq INTEGER NOT NULL CHECK (to_seq >= 1),
    digest BLOB    NOT NULL CHECK (length(digest) = 32)
) STRICT;

-- A storage: a directory under a root the installation names, or a bucket
-- of an S3 store. The columns of the other type are ''/0. The secret access
-- key is sealed by the caller, bound to the storage, and never read back by
-- a read operation: s3_secret is selected only as whether it is set.
CREATE TABLE storages (
    name             TEXT    NOT NULL PRIMARY KEY,
    type             TEXT    NOT NULL CHECK (type IN ('directory', 's3')),
    dir_root         TEXT    NOT NULL,
    dir_path         TEXT    NOT NULL,
    s3_endpoint      TEXT    NOT NULL,
    s3_region        TEXT    NOT NULL,
    s3_bucket        TEXT    NOT NULL,
    s3_prefix        TEXT    NOT NULL,
    s3_path_style    INTEGER NOT NULL CHECK (s3_path_style IN (0, 1)),
    s3_access_key_id TEXT    NOT NULL,
    s3_secret        BLOB,
    -- 1 when a purge leaves the older versions of an object on a versioned
    -- bucket (S3Storage.purge_versions off); 0, the default, deletes them.
    s3_keep_versions INTEGER NOT NULL DEFAULT 0 CHECK (s3_keep_versions IN (0, 1)),
    capacity_bytes   INTEGER NOT NULL CHECK (capacity_bytes >= 0),
    created          INTEGER NOT NULL,
    updated          INTEGER NOT NULL
) STRICT;

-- The overlap checks of a change read the storages of one root, or of one
-- endpoint and bucket, and no other.
CREATE INDEX storages_by_root   ON storages (dir_root) WHERE type = 'directory';
CREATE INDEX storages_by_bucket ON storages (s3_endpoint, s3_bucket) WHERE type = 's3';

-- The bytes the catalog holds on each storage, as running totals updated in
-- the transaction of each commit, so a status reads them in its snapshot and
-- never sums the instances per call. A storage with none has no row and
-- holds nothing.
CREATE TABLE storage_totals (
    storage TEXT    NOT NULL PRIMARY KEY,
    bytes   INTEGER NOT NULL CHECK (bytes >= 0)
) STRICT;

-- The owner generation the tenant holds for each of its storages: the
-- generation of the owner record (meta/.owner.json) this database last
-- wrote there. An import or a restore takes a storage over by writing a
-- record one generation higher, so a server whose database holds a lower
-- generation than the record finds another installation writes there now.
-- written is when this database's record was written. A storage with no row
-- has no record of this database's yet. The upper bound is the largest
-- integer a JSON number holds exactly, as the record writes it.
CREATE TABLE storage_owners (
    storage    TEXT    NOT NULL PRIMARY KEY,
    generation INTEGER NOT NULL CHECK (generation >= 1 AND generation <= 9007199254740991),
    written    INTEGER NOT NULL
) STRICT, WITHOUT ROWID;

-- A receiver. Its protocol's configuration (a DIMSE receiver's callers, TLS,
-- SOP classes and transfer syntaxes; a stow receiver's keys and largest
-- request; an import receiver's root, layout and map) and its naming rule
-- are JSON, stored and returned whole and never searched inside; the port
-- and bind address are columns as well, since a change checks them against
-- the other receivers. A stow receiver takes instances stored over HTTP into
-- the tenant's DICOMweb service (PS3.18 10.5) and an import receiver is
-- handed files by its import jobs: neither listens on a port of its own, so
-- each has port 0 and an empty bind address.
CREATE TABLE receivers (
    name           TEXT    NOT NULL PRIMARY KEY,
    type           TEXT    NOT NULL CHECK (type IN ('dimse', 'stow', 'import')),
    enabled        INTEGER NOT NULL CHECK (enabled IN (0, 1)),
    storage        TEXT    NOT NULL,
    port           INTEGER NOT NULL,
    bind_address   TEXT    NOT NULL,
    config         TEXT    NOT NULL,
    naming         TEXT    NOT NULL,
    quiet_us       INTEGER NOT NULL,
    max_open_us    INTEGER NOT NULL,
    max_file_bytes INTEGER NOT NULL,
    created        INTEGER NOT NULL,
    updated        INTEGER NOT NULL,
    CHECK ((type = 'dimse' AND port BETWEEN 1 AND 65535) OR (type IN ('stow', 'import') AND port = 0 AND bind_address = ''))
) STRICT;

-- Removing a storage asks whether a receiver names it; adding or changing a
-- receiver asks which receivers listen on its port. The stow receivers
-- share port 0, so a change of one reads the others by it, to refuse a key
-- two of them list.
CREATE INDEX receivers_by_storage ON receivers (storage);
CREATE INDEX receivers_by_port    ON receivers (port);

-- A reception: the files of one study arriving at one receiver. state is the
-- core's text (internal/core/reception), kept as it is; ended is whether that
-- state is an end, as the core said when it was written, so "in progress"
-- stays right when a later version adds a state. calling_ae_titles is a JSON
-- array of at most 16. route is what the receiver's routing chose for the
-- study (the route, and the profile version and senders it names in place
-- of the receiver's), kept when the reception is identified, so a step run
-- again, and the steps after, go by the same choice whatever the receiver's
-- routes say by then; a storage or profile a release named is kept there
-- too; '' for nothing of its own.
CREATE TABLE receptions (
    id                TEXT    NOT NULL PRIMARY KEY,
    receiver          TEXT    NOT NULL,
    study_uid         TEXT    NOT NULL,
    arrival           INTEGER NOT NULL CHECK (arrival >= 1),
    state             TEXT    NOT NULL,
    ended             INTEGER NOT NULL CHECK (ended IN (0, 1)),
    files_received    INTEGER NOT NULL CHECK (files_received >= 0),
    bytes_received    INTEGER NOT NULL CHECK (bytes_received >= 0),
    files_archived    INTEGER NOT NULL CHECK (files_archived >= 0),
    bytes_archived    INTEGER NOT NULL CHECK (bytes_archived >= 0),
    files_duplicate   INTEGER NOT NULL CHECK (files_duplicate >= 0),
    bytes_duplicate   INTEGER NOT NULL CHECK (bytes_duplicate >= 0),
    files_quarantined INTEGER NOT NULL CHECK (files_quarantined >= 0),
    bytes_quarantined INTEGER NOT NULL CHECK (bytes_quarantined >= 0),
    files_released    INTEGER NOT NULL CHECK (files_released >= 0),
    subject_label     TEXT    NOT NULL,
    session_label     TEXT    NOT NULL,
    storage           TEXT    NOT NULL,
    failure           TEXT    NOT NULL,
    started           INTEGER NOT NULL,
    last_file         INTEGER NOT NULL,
    step_at           INTEGER NOT NULL,
    ended_at          INTEGER,
    associations      INTEGER NOT NULL CHECK (associations >= 0),
    calling_ae_titles TEXT    NOT NULL,
    calling_truncated INTEGER NOT NULL CHECK (calling_truncated IN (0, 1)),
    retry_of          TEXT    NOT NULL,
    retry_reception   TEXT    NOT NULL,
    discard_reason    TEXT    NOT NULL,
    discard_at        INTEGER,
    route TEXT NOT NULL DEFAULT ''
) STRICT;

-- One arrival number per study in the tenant; the next one is read from here.
CREATE UNIQUE INDEX receptions_arrival ON receptions (study_uid, arrival);
-- At most one reception of a study is receiving at one receiver: two opens
-- at once make one, in any database.
CREATE UNIQUE INDEX receptions_open ON receptions (receiver, study_uid) WHERE state = 'receiving';
-- The listings, newest first by start time, then id:
--   no filter, or a time range only   receptions_by_start
--   a receiver                        receptions_by_receiver
--   a study                           receptions_by_study
--   steps                             receptions_by_state
--   in progress                       receptions_in_progress (only those)
CREATE INDEX receptions_by_start       ON receptions (started, id);
CREATE INDEX receptions_by_receiver    ON receptions (receiver, started, id);
CREATE INDEX receptions_by_study       ON receptions (study_uid, started, id);
CREATE INDEX receptions_by_state       ON receptions (state, started, id);
CREATE INDEX receptions_in_progress    ON receptions (started, id) WHERE ended = 0;
-- The combinations of the list's filters that a tenant with many receptions
-- would otherwise answer by testing each row of one index: a receiver with
-- in progress, a receiver with steps, a study with steps. A filter with in
-- progress and steps keeps only the steps that have not ended, and needs no
-- index of its own.
CREATE INDEX receptions_receiver_in_progress ON receptions (receiver, started, id) WHERE ended = 0;
CREATE INDEX receptions_by_receiver_state    ON receptions (receiver, state, started, id);
CREATE INDEX receptions_by_study_state       ON receptions (study_uid, state, started, id);
-- Removing or moving a storage asks whether a reception in progress names
-- it.
CREATE INDEX receptions_by_storage ON receptions (storage, ended);

-- The receptions whose files are in the incoming area: those that have not
-- ended, and those that failed and were neither retried nor discarded. The
-- status sums their bytes from this index alone; its rows are only those
-- receptions, however many have ended. The condition is written the same in
-- incomingBytesSQL, which is what lets SQLite's planner use it.
CREATE INDEX receptions_holding ON receptions (bytes_received, bytes_quarantined)
    WHERE ended = 0 OR (state = 'failed' AND retry_reception = '' AND discard_reason = '');

-- The receptions in each step, a running total kept by triggers, so a
-- status reads one row per step rather than counting receptions, and no
-- change that moves a reception (a step, a unit of work giving up, a retry,
-- an open) can forget it: the trigger runs in the transaction of the
-- statement that wrote the reception's state.
CREATE TABLE reception_steps (
    state      TEXT    NOT NULL PRIMARY KEY,
    receptions INTEGER NOT NULL CHECK (receptions >= 0)
) STRICT, WITHOUT ROWID;

CREATE TRIGGER reception_steps_insert AFTER INSERT ON receptions BEGIN
    INSERT INTO reception_steps (state, receptions) VALUES (NEW.state, 1)
        ON CONFLICT (state) DO UPDATE SET receptions = receptions + 1;
END;

CREATE TRIGGER reception_steps_update AFTER UPDATE OF state ON receptions WHEN OLD.state <> NEW.state BEGIN
    UPDATE reception_steps SET receptions = receptions - 1 WHERE state = OLD.state;
    INSERT INTO reception_steps (state, receptions) VALUES (NEW.state, 1)
        ON CONFLICT (state) DO UPDATE SET receptions = receptions + 1;
END;

CREATE TRIGGER reception_steps_delete AFTER DELETE ON receptions BEGIN
    UPDATE reception_steps SET receptions = receptions - 1 WHERE state = OLD.state;
END;

-- The associations that brought a reception's files, each once, so a batch
-- written again counts none twice.
CREATE TABLE reception_associations (
    reception_id TEXT NOT NULL,
    association  TEXT NOT NULL,
    PRIMARY KEY (reception_id, association)
) STRICT, WITHOUT ROWID;

-- A file that landed in the incoming area. The key is what makes a batch
-- written again change nothing: the same reception, instance and checksum is
-- the same file. deidentified is 1 for a file received at a receiver that
-- de-identifies, whose row holds the keyed remapping of the file's placing
-- UIDs and a keyed checksum, 0 for one recorded as received. The received
-- files of a reception that ended done or quarantined are removed by the
-- step that ends it: each is then an instance held, a duplicate of one, or
-- a quarantine record that keeps its own copy of the row. A failed
-- reception keeps its files, for its retry or its discard.
CREATE TABLE received_files (
    reception_id        TEXT    NOT NULL,
    sop_instance_uid    TEXT    NOT NULL,
    sha256              BLOB    NOT NULL CHECK (length(sha256) = 32),
    series_uid          TEXT    NOT NULL,
    study_uid           TEXT    NOT NULL,
    sop_class_uid       TEXT    NOT NULL,
    transfer_syntax_uid TEXT    NOT NULL,
    size                INTEGER NOT NULL CHECK (size >= 0),
    incoming            TEXT    NOT NULL,
    calling_ae          TEXT    NOT NULL,
    called_ae           TEXT    NOT NULL,
    remote_addr         TEXT    NOT NULL,
    association         TEXT    NOT NULL,
    released            INTEGER NOT NULL CHECK (released IN (0, 1)),
    received            INTEGER NOT NULL,
    deidentified INTEGER NOT NULL DEFAULT 0 CHECK (deidentified IN (0, 1)),
    PRIMARY KEY (reception_id, sop_instance_uid, sha256)
) STRICT, WITHOUT ROWID;

-- A quarantined file's record. details is a JSON object of the reason's
-- keys. A record stays after its file is released or rejected; the reason a
-- person gave for a release is kept with it as a rejection's is
-- (release_reason). deidentified is as received_files' column. A release
-- may name the storage (release_storage) and the profile version, as
-- name@version (release_profile), of the reception its file joins; they are
-- kept here when the file joins none and lands again, '' for none.
-- import_job and import_path name the import job that handed the file in,
-- and its path, '' for a file received otherwise.
CREATE TABLE quarantine (
    id                  TEXT    NOT NULL PRIMARY KEY,
    reason              TEXT    NOT NULL,
    details             TEXT    NOT NULL,
    state               TEXT    NOT NULL CHECK (state IN ('held', 'released', 'rejected')),
    receiver            TEXT    NOT NULL,
    reception_id        TEXT    NOT NULL,
    calling_ae          TEXT    NOT NULL,
    called_ae           TEXT    NOT NULL,
    remote_addr         TEXT    NOT NULL,
    size                INTEGER NOT NULL CHECK (size >= 0),
    sha256              BLOB CHECK (sha256 IS NULL OR length(sha256) = 32),
    kept_sha256         BLOB CHECK (kept_sha256 IS NULL OR length(kept_sha256) = 32),
    sop_class_uid       TEXT    NOT NULL,
    sop_instance_uid    TEXT    NOT NULL,
    study_uid           TEXT    NOT NULL,
    series_uid          TEXT    NOT NULL,
    transfer_syntax_uid TEXT    NOT NULL,
    incoming            TEXT    NOT NULL,
    content_kept        INTEGER NOT NULL CHECK (content_kept IN (0, 1)),
    at                  INTEGER NOT NULL,
    decided_at          INTEGER,
    release_mode        TEXT    NOT NULL CHECK (release_mode IN ('', 'as_is', 'overwrite')),
    release_reception   TEXT    NOT NULL,
    release_receiver    TEXT    NOT NULL,
    reject_reason       TEXT    NOT NULL,
    released_from       TEXT    NOT NULL,
    release_reason TEXT NOT NULL DEFAULT '',
    deidentified INTEGER NOT NULL DEFAULT 0 CHECK (deidentified IN (0, 1)),
    release_storage TEXT NOT NULL DEFAULT '',
    release_profile TEXT NOT NULL DEFAULT '',
    import_job TEXT NOT NULL DEFAULT '',
    import_path TEXT NOT NULL DEFAULT ''
) STRICT;

-- The listings, newest first by quarantine time, then id, one index per
-- equality filter; a time range narrows whichever is used. The instance
-- index also serves the duplicate check, which leaves out files a reception
-- quarantined.
CREATE INDEX quarantine_by_time      ON quarantine (at, id);
CREATE INDEX quarantine_by_state     ON quarantine (state, at, id);
CREATE INDEX quarantine_by_reason    ON quarantine (reason, at, id);
CREATE INDEX quarantine_by_receiver  ON quarantine (receiver, at, id);
CREATE INDEX quarantine_by_reception ON quarantine (reception_id, at, id);
CREATE INDEX quarantine_by_study     ON quarantine (study_uid, at, id);
CREATE INDEX quarantine_by_instance  ON quarantine (sop_instance_uid, at, id);
-- The combinations of the three filters that each keep many records, so a
-- look through the held files of one receiver or one reason reads only those.
CREATE INDEX quarantine_by_state_reason          ON quarantine (state, reason, at, id);
CREATE INDEX quarantine_by_state_receiver        ON quarantine (state, receiver, at, id);
CREATE INDEX quarantine_by_receiver_reason       ON quarantine (receiver, reason, at, id);
CREATE INDEX quarantine_by_state_receiver_reason ON quarantine (state, receiver, reason, at, id);
-- A discard leaves out the files a record keeps, found by their place.
CREATE INDEX quarantine_kept ON quarantine (incoming) WHERE content_kept = 1;

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
-- them, changed in the transaction of every change of a record, so they
-- equal a count over the records after every commit. One row, made here.
CREATE TABLE quarantine_totals (
    one   INTEGER NOT NULL PRIMARY KEY CHECK (one = 1),
    files INTEGER NOT NULL CHECK (files >= 0),
    bytes INTEGER NOT NULL CHECK (bytes >= 0)
) STRICT;
INSERT INTO quarantine_totals (one, files, bytes) VALUES (1, 0, 0);

-- The catalog. A subject is found by its label, a session by its Study
-- Instance UID, a scan by its Series Instance UID; each keeps the label it
-- was first given. folded is the label as label.Fold makes it, unique at its
-- level, so two labels that a file system ignoring case would make one
-- directory are never both held.
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
-- into the row.
--
-- The ids only grow: AUTOINCREMENT (sqlite.org/autoinc.html) never gives
-- the id of a removed row again. Without it SQLite gives a new row one more
-- than the largest id held, so removing the row of the largest id (a
-- rebuild that replaces the catalog, a correction's delete) would give its
-- id to the next row made, while a send job, a retry's failures or a
-- correction's hidden rows still name the row by that id: a send would send
-- another scan's files.
CREATE TABLE subjects (
    id             INTEGER NOT NULL PRIMARY KEY AUTOINCREMENT,
    label          TEXT    NOT NULL,
    folded         TEXT    NOT NULL UNIQUE,
    created        INTEGER NOT NULL,
    session_count  INTEGER NOT NULL DEFAULT 0 CHECK (session_count >= 0),
    instance_count INTEGER NOT NULL DEFAULT 0 CHECK (instance_count >= 0),
    bytes          INTEGER NOT NULL DEFAULT 0 CHECK (bytes >= 0),
    updated        INTEGER NOT NULL DEFAULT 0
) STRICT;

-- The lists and the index each reads, in the list's order (sqlkit's catalog
-- reads build the statements):
--   subjects newest first, with a time range or none   subjects_by_created
--   subjects by label                                  the unique folded
CREATE INDEX subjects_by_created ON subjects (created, id);

-- A session. route is what the routing chose for it when its first scan was
-- committed, so every later arrival of the session goes by the same choice;
-- '' for nothing of its own.
CREATE TABLE sessions (
    id             INTEGER NOT NULL PRIMARY KEY AUTOINCREMENT,
    study_uid      TEXT    NOT NULL UNIQUE,
    subject_id     INTEGER NOT NULL,
    label          TEXT    NOT NULL,
    folded         TEXT    NOT NULL,
    storage        TEXT    NOT NULL,
    created        INTEGER NOT NULL,
    subject_label  TEXT    NOT NULL DEFAULT '',
    subject_folded TEXT    NOT NULL DEFAULT '',
    study_date     TEXT    NOT NULL DEFAULT '',
    receiver       TEXT    NOT NULL DEFAULT '',
    scan_count     INTEGER NOT NULL DEFAULT 0 CHECK (scan_count >= 0),
    instance_count INTEGER NOT NULL DEFAULT 0 CHECK (instance_count >= 0),
    bytes          INTEGER NOT NULL DEFAULT 0 CHECK (bytes >= 0),
    updated        INTEGER NOT NULL DEFAULT 0,
    route          TEXT    NOT NULL DEFAULT '',
    UNIQUE (subject_id, folded)
) STRICT;

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
    id                 INTEGER NOT NULL PRIMARY KEY AUTOINCREMENT,
    session_id         INTEGER NOT NULL,
    series_uid         TEXT    NOT NULL UNIQUE,
    label              TEXT    NOT NULL,
    folded             TEXT    NOT NULL,
    instances          INTEGER NOT NULL CHECK (instances >= 0),
    bytes              INTEGER NOT NULL CHECK (bytes >= 0),
    manifest_sha256    BLOB CHECK (manifest_sha256 IS NULL OR length(manifest_sha256) = 32),
    created            INTEGER NOT NULL,
    updated            INTEGER NOT NULL,
    subject_label      TEXT    NOT NULL DEFAULT '',
    subject_folded     TEXT    NOT NULL DEFAULT '',
    session_label      TEXT    NOT NULL DEFAULT '',
    session_folded     TEXT    NOT NULL DEFAULT '',
    storage            TEXT    NOT NULL DEFAULT '',
    modality           TEXT    NOT NULL DEFAULT '',
    series_description TEXT    NOT NULL DEFAULT '',
    description_folded TEXT    NOT NULL DEFAULT '',
    series_number      INTEGER,
    receiver           TEXT    NOT NULL DEFAULT '',
    study_uid          TEXT    NOT NULL DEFAULT '',
    UNIQUE (session_id, folded)
) STRICT;

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
-- (the first scan of a storage nearly always passes it), and a commit that
-- adds to a scan rewrites no entry of this index.
CREATE INDEX scans_by_storage ON scans (storage);

-- A study's instances are read scan by scan in order of id, then of SOP
-- Instance UID: this index gives the scans in that order and each scan's
-- instances are a range in that order, so a page is read without a sort
-- and stops at its limit. Read through sessions and the scans of each
-- session by creation, the first page of a large study sorted every
-- instance of it (p99 23 ms on Postgres, 71 ms on SQLite at ten million).
-- It serves a study's instances (DICOMweb's retrieval of a study and the
-- search for the instances of a study) and costs an entry per scan, written
-- once when the scan is made: a scan's study never changes.
CREATE INDEX scans_by_study ON scans (study_uid, id);

-- The modalities of each session's scans, each once, with the session's
-- creation time, so the sessions of a modality are listed newest first
-- from this index alone.
CREATE TABLE session_modalities (
    session_id INTEGER NOT NULL,
    modality   TEXT    NOT NULL,
    created    INTEGER NOT NULL,
    PRIMARY KEY (session_id, modality)
) STRICT, WITHOUT ROWID;

CREATE INDEX session_modalities_by_modality ON session_modalities (modality, created, session_id);

-- An instance a reception claims before it writes the file to a storage, in
-- flight only: written by the claim, removed by the commit that confirms it
-- or the step that ends its reception. A SOP Instance UID is claimed or
-- held, never both: a claim is made only when no row of this table or of
-- instances holds the UID, and a commit moves a claim of its own reception
-- into instances in one transaction. The claim keeps the study and series it
-- was made in, which a commit checks. Kept apart from the held instances, a
-- commit writes one row per instance instead of two.
CREATE TABLE instance_claims (
    sop_instance_uid TEXT    NOT NULL PRIMARY KEY,
    reception_id     TEXT    NOT NULL CHECK (reception_id <> ''),
    study_uid        TEXT    NOT NULL,
    series_uid       TEXT    NOT NULL,
    sha256           BLOB    NOT NULL CHECK (length(sha256) = 32),
    claimed_at       INTEGER NOT NULL
) STRICT, WITHOUT ROWID;

-- A reception's claims, to release them when it ends.
CREATE INDEX instance_claims_by_reception ON instance_claims (reception_id);

-- A held instance, written once by its commit and never replaced by
-- reception. It is keyed by (scan_id, sop_instance_uid), so a scan's rows
-- are one range of the table, written at its end as scans are made and read
-- in order without a lookup per row; its storage is its scan's. sha256 is
-- the checksum the file was received with; archived_sha256 is NULL when it
-- equals sha256, as it does for a file archived as it was received.
-- storage_key is '' for the layout's key of the instance's scan and UID, the
-- only key a commit is given, so a read makes it again from the scan's
-- labels. profile_version_id is the profile version that de-identified it
-- (an id of profile_versions), NULL for one archived as received. These
-- keep the row small: at ten million instances a wider row and its indexes
-- took about 7 GB.
CREATE TABLE instances (
    scan_id          INTEGER NOT NULL,
    sop_instance_uid TEXT    NOT NULL,
    reception_id     TEXT    NOT NULL CHECK (reception_id <> ''),
    sha256           BLOB    NOT NULL CHECK (length(sha256) = 32),
    archived_sha256  BLOB CHECK (archived_sha256 IS NULL OR length(archived_sha256) = 32),
    storage_key      TEXT    NOT NULL,
    byte_offset      INTEGER NOT NULL CHECK (byte_offset >= 0),
    byte_length      INTEGER NOT NULL CHECK (byte_length >= 0),
    size             INTEGER NOT NULL CHECK (size >= 0),
    held_at          INTEGER NOT NULL,
    profile_version_id INTEGER,
    PRIMARY KEY (scan_id, sop_instance_uid)
) STRICT, WITHOUT ROWID;

-- The uniqueness of a SOP Instance UID in the tenant, and a row found by it.
CREATE UNIQUE INDEX instances_by_uid ON instances (sop_instance_uid);

-- The scans a reception confirmed instances in, with running totals, for
-- its list of scans: a row per scan in place of an index of every instance
-- by its reception (about 1 GB at ten million).
CREATE TABLE reception_scans (
    reception_id TEXT    NOT NULL,
    scan_id      INTEGER NOT NULL,
    instances    INTEGER NOT NULL CHECK (instances >= 0),
    bytes        INTEGER NOT NULL CHECK (bytes >= 0),
    PRIMARY KEY (reception_id, scan_id)
) STRICT, WITHOUT ROWID;

-- The catalog's running totals, changed in the transaction of every
-- commit, so they equal a count over the catalog after each. One row, made
-- here.
CREATE TABLE catalog_totals (
    one       INTEGER NOT NULL PRIMARY KEY CHECK (one = 1),
    subjects  INTEGER NOT NULL CHECK (subjects >= 0),
    sessions  INTEGER NOT NULL CHECK (sessions >= 0),
    scans     INTEGER NOT NULL CHECK (scans >= 0),
    instances INTEGER NOT NULL CHECK (instances >= 0)
) STRICT;
INSERT INTO catalog_totals (one, subjects, sessions, scans, instances) VALUES (1, 0, 0, 0, 0);

-- An instance a scrub found missing or damaged: every read of it answers
-- data loss before its first byte. One per instance: the finding that marked
-- it. A sparse table rather than a column of instances, so no instance row
-- is rewritten and every read of an instance adds one lookup of a table
-- that is empty while the archive is whole.
CREATE TABLE instance_damage (
    sop_instance_uid TEXT    NOT NULL PRIMARY KEY,
    finding_id       TEXT    NOT NULL,
    marked_at        INTEGER NOT NULL
) STRICT, WITHOUT ROWID;

-- A unit of work. due is when a queued unit may be claimed, and when a
-- leased unit's lease ends; attempt counts claims and fences every write of
-- the worker holding the lease. unique_key is NULL for none. reception_id
-- names the reception the unit carries, '' for none: a unit held as failed
-- fails its reception in the same transaction.
CREATE TABLE work (
    id           TEXT    NOT NULL PRIMARY KEY,
    kind         TEXT    NOT NULL,
    subject      TEXT    NOT NULL,
    reception_id TEXT    NOT NULL,
    unique_key   TEXT,
    state        TEXT    NOT NULL CHECK (state IN ('queued', 'leased', 'done', 'failed')),
    due          INTEGER NOT NULL,
    attempt      INTEGER NOT NULL CHECK (attempt >= 0),
    max_attempts INTEGER NOT NULL CHECK (max_attempts >= 1),
    owner        TEXT    NOT NULL,
    last_error   TEXT    NOT NULL,
    created      INTEGER NOT NULL,
    updated      INTEGER NOT NULL
) STRICT;

-- A key queues its work once while the work is queued or leased; a finished
-- unit frees it.
CREATE UNIQUE INDEX work_by_key ON work (unique_key) WHERE unique_key IS NOT NULL AND state IN ('queued', 'leased');
-- A claim reads the earliest due of a kind among the units not finished,
-- and only those: finished units, however many, are not in this index.
CREATE INDEX work_claimable ON work (kind, due, id) WHERE state IN ('queued', 'leased');
-- The receptions with a live unit, for the search at start of those left
-- without one.
CREATE INDEX work_by_reception ON work (reception_id) WHERE state IN ('queued', 'leased');

-- The event feed: one row per thing that happened, written in the
-- transaction of the change it reports. id orders the feed: the one writer
-- runs one transaction at a time, so the order of ids is the order of
-- commits. AUTOINCREMENT makes ids only grow, even after every row has been
-- pruned, so a cursor never comes to name a newer event
-- (sqlite.org/autoinc.html). payload is JSON, stored and returned whole and
-- never searched inside. key is the caller's key of an event the server
-- appended, NULL for one the store wrote with its change.
CREATE TABLE events (
    id      INTEGER NOT NULL PRIMARY KEY AUTOINCREMENT,
    type    TEXT    NOT NULL,
    at      INTEGER NOT NULL,
    payload TEXT    NOT NULL,
    key     TEXT
) STRICT;

-- A filtered read walks the events of each type it names after the cursor;
-- an unfiltered read and the pruning walk the ids themselves.
CREATE INDEX events_by_type ON events (type, id);
-- An appended event is written once per key.
CREATE UNIQUE INDEX events_by_key ON events (key) WHERE key IS NOT NULL;

-- What pruning removed: every event whose id is at or below pruned_through
-- is gone, and every event above it is kept. It moves in the pruning's
-- transaction, so a reader whose cursor is below it is told its cursor
-- expired, never answered from past a gap. One row, made here.
CREATE TABLE event_marks (
    one            INTEGER NOT NULL PRIMARY KEY CHECK (one = 1),
    pruned_through INTEGER NOT NULL CHECK (pruned_through >= 0)
) STRICT;
INSERT INTO event_marks (one, pruned_through) VALUES (1, 0);

-- Checkpoints of the event feed, recorded by the server with its own clock:
-- at the time at, the newest event committed was through. Retention removes
-- every event up to the newest checkpoint older than its period, so an
-- event's age is measured by the server's clock from when it was written,
-- never by the time the event itself carries, which a caller gave.
CREATE TABLE event_checkpoints (
    at      INTEGER NOT NULL PRIMARY KEY,
    through INTEGER NOT NULL CHECK (through >= 0)
) STRICT;

-- The event feed's history: one row, its id 32 lowercase hexadecimal digits
-- of a random value, made with the database and made again whenever a
-- rebuild replaces the catalog, and since when it holds; so a consumer that
-- keys what it read by (history, cursor) knows the feed it follows began
-- again from a new catalog.
CREATE TABLE event_history (
    one     INTEGER NOT NULL PRIMARY KEY CHECK (one = 1),
    id      TEXT    NOT NULL CHECK (length(id) = 32),
    started INTEGER NOT NULL
) STRICT;
INSERT INTO event_history (one, id, started) VALUES (1, lower(hex(randomblob(16))),
    CAST((julianday('now') - 2440587.5) * 86400000000 AS INTEGER));

-- A de-identification profile version, never changed once written: its
-- document and its expanded form as the exact bytes their checksum covers
-- (BLOB, so nothing is normalized), the checksum of the expanded form, who
-- made it, when and why. id is what an instance records; name and version
-- are what a person names.
CREATE TABLE profile_versions (
    id             INTEGER PRIMARY KEY,
    name           TEXT    NOT NULL,
    version        INTEGER NOT NULL CHECK (version BETWEEN 1 AND 10000),
    checksum       TEXT    NOT NULL CHECK (length(checksum) = 64),
    note           TEXT    NOT NULL,
    created_by_key TEXT    NOT NULL,
    created        INTEGER NOT NULL,
    source         BLOB    NOT NULL CHECK (length(source) > 0),
    expanded       BLOB    NOT NULL CHECK (length(expanded) > 0),
    UNIQUE (name, version)
) STRICT;

-- A profile and its latest version, written with each version, so a list
-- of profiles reads one row each.
CREATE TABLE profiles (
    name           TEXT    NOT NULL PRIMARY KEY,
    latest_version INTEGER NOT NULL CHECK (latest_version BETWEEN 1 AND 10000),
    latest_created INTEGER NOT NULL
) STRICT;

-- A lookup table's last code assigned in order; its row is the lock under
-- which the table's codes are assigned.
CREATE TABLE lookup_counters (
    table_name TEXT    NOT NULL PRIMARY KEY,
    last_code  INTEGER NOT NULL CHECK (last_code >= 0)
) STRICT;

-- A lookup table's codes, by the keyed hash of the value each replaces,
-- never the value. A code is given to one value of a table.
CREATE TABLE lookup_codes (
    table_name TEXT    NOT NULL,
    lookup_key TEXT    NOT NULL,
    code       TEXT    NOT NULL,
    assigned   INTEGER NOT NULL,
    PRIMARY KEY (table_name, lookup_key)
) STRICT, WITHOUT ROWID;

CREATE UNIQUE INDEX lookup_codes_by_code ON lookup_codes (table_name, code);

-- A sender. Its protocol's configuration (a DIMSE peer's host, port, AE
-- titles and TLS files; a stow peer's URL, authentication and request
-- bounds) is JSON, stored and returned whole. The credential is sealed by
-- the caller and never read back by a read operation: it is selected only
-- as whether it is set.
CREATE TABLE senders (
    name                 TEXT    NOT NULL PRIMARY KEY,
    type                 TEXT    NOT NULL CHECK (type IN ('dimse', 'stow')),
    enabled              INTEGER NOT NULL CHECK (enabled IN (0, 1)),
    config               TEXT    NOT NULL,
    credential           BLOB,
    max_associations     INTEGER NOT NULL CHECK (max_associations BETWEEN 1 AND 16),
    max_bytes_per_second INTEGER NOT NULL CHECK (max_bytes_per_second >= 0),
    connect_us           INTEGER NOT NULL,
    response_us          INTEGER NOT NULL,
    created              INTEGER NOT NULL,
    updated              INTEGER NOT NULL
) STRICT;

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
-- progress event, which is written at most once in a period.
CREATE TABLE jobs (
    id                  TEXT    NOT NULL PRIMARY KEY,
    kind                TEXT    NOT NULL CHECK (kind IN ('send', 'deid', 'import', 'scrub', 'rebuild')),
    sender              TEXT    NOT NULL,
    subject_label       TEXT    NOT NULL,
    session_label       TEXT    NOT NULL,
    scan_label          TEXT    NOT NULL,
    reception_id        TEXT    NOT NULL,
    receiver            TEXT    NOT NULL,
    unique_key          TEXT,
    state               TEXT    NOT NULL CHECK (state IN ('queued', 'running', 'done', 'partial', 'failed', 'canceled')),
    ended               INTEGER NOT NULL CHECK (ended IN (0, 1)),
    reason              TEXT    NOT NULL,
    created_by_key      TEXT    NOT NULL,
    created             INTEGER NOT NULL,
    started             INTEGER,
    state_at            INTEGER NOT NULL,
    ended_at            INTEGER,
    bound_at            INTEGER NOT NULL,
    retry_of            TEXT    NOT NULL,
    retry_job           TEXT    NOT NULL,
    list_work           TEXT    NOT NULL,
    listed              INTEGER NOT NULL CHECK (listed IN (0, 1)),
    retry_listed        INTEGER NOT NULL CHECK (retry_listed IN (0, 1)),
    target_listed       INTEGER NOT NULL CHECK (target_listed IN (0, 1)),
    retry_after         INTEGER NOT NULL,
    list_session        TEXT    NOT NULL,
    list_scan           TEXT    NOT NULL,
    list_id             INTEGER NOT NULL,
    scans               INTEGER NOT NULL CHECK (scans >= 0),
    scans_done          INTEGER NOT NULL CHECK (scans_done >= 0 AND scans_done <= scans),
    instances           INTEGER NOT NULL,
    bytes               INTEGER NOT NULL,
    sent                INTEGER NOT NULL,
    sent_bytes          INTEGER NOT NULL,
    warned              INTEGER NOT NULL,
    warned_bytes        INTEGER NOT NULL,
    refused             INTEGER NOT NULL,
    refused_bytes       INTEGER NOT NULL,
    failures_listed     INTEGER NOT NULL,
    failures_not_listed INTEGER NOT NULL,
    progress_at         INTEGER,
    progress_mark       INTEGER NOT NULL,
    deid_mode           TEXT    NOT NULL CHECK (deid_mode IN ('', 'dry_run', 'check')),
    profile_name        TEXT    NOT NULL,
    profile_version     INTEGER NOT NULL CHECK (profile_version >= 0),
    CHECK ((kind = 'deid') = (deid_mode <> '')),
    CHECK (sent >= 0 AND warned >= 0 AND refused >= 0 AND sent + warned + refused <= instances),
    CHECK (sent_bytes >= 0 AND warned_bytes >= 0 AND refused_bytes >= 0 AND sent_bytes + warned_bytes + refused_bytes <= bytes)
) STRICT;

-- A forward is made once per reception and sender.
CREATE UNIQUE INDEX jobs_by_key ON jobs (unique_key);
-- The lists, newest first: every job, by state, by sender, and those not
-- ended (also what a sender's removal and a status read).
CREATE INDEX jobs_by_created            ON jobs (created, id);
CREATE INDEX jobs_by_state              ON jobs (state, created, id);
CREATE INDEX jobs_by_sender             ON jobs (sender, created, id);
CREATE INDEX jobs_in_progress           ON jobs (created, id) WHERE ended = 0;
CREATE INDEX jobs_of_sender_in_progress ON jobs (sender, created, id) WHERE ended = 0;

-- A job's scan, one unit of work each: the instances its stream holds after
-- start_after (and those of job_resends), the last one the peer answered
-- (cursor), and its counts.
CREATE TABLE job_scans (
    job_id             TEXT    NOT NULL,
    scan_id            INTEGER NOT NULL,
    work_id            TEXT    NOT NULL,
    start_after        TEXT    NOT NULL,
    cursor             TEXT    NOT NULL,
    instances          INTEGER NOT NULL,
    bytes              INTEGER NOT NULL,
    sent               INTEGER NOT NULL,
    sent_bytes         INTEGER NOT NULL,
    warned             INTEGER NOT NULL,
    warned_bytes       INTEGER NOT NULL,
    refused            INTEGER NOT NULL,
    refused_bytes      INTEGER NOT NULL,
    refused_not_listed INTEGER NOT NULL,
    ended              INTEGER NOT NULL CHECK (ended IN (0, 1)),
    PRIMARY KEY (job_id, scan_id),
    CHECK (sent >= 0 AND warned >= 0 AND refused >= 0 AND sent + warned + refused <= instances)
) STRICT, WITHOUT ROWID;

-- A unit's writes find its scan by the unit.
CREATE UNIQUE INDEX job_scans_by_work ON job_scans (work_id);

-- The instances a retry's scan sends again that lie at or before its
-- start_after: the ended job's refused instances.
CREATE TABLE job_resends (
    job_id           TEXT    NOT NULL,
    scan_id          INTEGER NOT NULL,
    sop_instance_uid TEXT    NOT NULL,
    PRIMARY KEY (job_id, scan_id, sop_instance_uid)
) STRICT, WITHOUT ROWID;

-- An instance a scan's unit owes: the peer left it unaccounted, and the
-- unit's cursor moved past it, so a crash sends at most one batch again; it
-- is tried again by name, and a retry sends it while it is still owed.
CREATE TABLE job_owed (
    job_id           TEXT    NOT NULL,
    scan_id          INTEGER NOT NULL,
    sop_instance_uid TEXT    NOT NULL,
    PRIMARY KEY (job_id, scan_id, sop_instance_uid)
) STRICT, WITHOUT ROWID;

-- An instance a job's peer refused, or kept with a warning, listed by its
-- scan's labels; at most the setting job.failures-kept a job.
CREATE TABLE job_failures (
    job_id              TEXT    NOT NULL,
    subject_label       TEXT    NOT NULL,
    session_label       TEXT    NOT NULL,
    scan_label          TEXT    NOT NULL,
    sop_instance_uid    TEXT    NOT NULL,
    scan_id             INTEGER NOT NULL,
    warning             INTEGER NOT NULL CHECK (warning IN (0, 1)),
    code                TEXT    NOT NULL,
    peer_status         INTEGER NOT NULL,
    sop_class_uid       TEXT    NOT NULL,
    transfer_syntax_uid TEXT    NOT NULL,
    detail              TEXT    NOT NULL,
    at                  INTEGER NOT NULL,
    PRIMARY KEY (job_id, subject_label, session_label, scan_label, sop_instance_uid)
) STRICT, WITHOUT ROWID;

-- The refused alone, in the same order.
CREATE INDEX job_failures_by_kind ON job_failures (job_id, warning, subject_label, session_label, scan_label, sop_instance_uid);
-- A scan's failures by the scan's key, for a retry.
CREATE INDEX job_failures_by_scan ON job_failures (job_id, scan_id, sop_instance_uid);

-- A deid job's counts per key (the contract's JobTally), added in the
-- transaction of each progress write: at most 1000 keys a job, and the key
-- 'other' for those past it.
CREATE TABLE job_tallies (
    job_id TEXT    NOT NULL,
    tally  TEXT    NOT NULL,
    count  INTEGER NOT NULL CHECK (count >= 0),
    PRIMARY KEY (job_id, tally)
) STRICT, WITHOUT ROWID;

-- What an import job reads: the files of a directory under an import
-- receiver's root, handed to that receiver one at a time; its receiver,
-- path, map and map configuration, the map's lists as JSON arrays; and its
-- counts, kept in the transaction of every file's record and every outcome.
-- next_unit and next_file number its units and its listed files.
-- fail_reason is the first failed unit's reason; settling_since when its
-- last walk ended with files processing. The CHECKs make the counts
-- reconcile: every file found is skipped or handed in, and every file
-- handed in has one outcome.
--
-- A path can identify a person, so it is kept in the import's rows only.
-- Where an index orders paths (a unit's folder and cursor, a file's path) it
-- is kept as a BLOB with each "/" as the byte 0: no segment holds NUL, so
-- the BLOB's byte order (memcmp, then length) is the order an import walks a
-- tree in, a directory's whole subtree before the next name beside it. A
-- path an index holds is at most 2048 bytes, the bound the Postgres driver
-- needs for its B-tree rows, held here too so the two drivers take the same
-- rows; an entry whose path is longer is skipped and listed under its
-- deepest ancestor that fits. A unit's cursor, which no index holds, may be
-- such an entry's path: at most 2048 + 1 + 255 bytes.
CREATE TABLE import_jobs (
    job_id           TEXT    NOT NULL PRIMARY KEY,
    receiver         TEXT    NOT NULL,
    path             TEXT    NOT NULL,
    map_file         TEXT    NOT NULL,
    map_csv          BLOB,
    map_key_columns  TEXT    NOT NULL,
    map_value_column TEXT    NOT NULL,
    map_key_parts    TEXT    NOT NULL,
    map_gives        TEXT    NOT NULL,
    failures_kept    INTEGER NOT NULL CHECK (failures_kept > 0),
    files_listed     INTEGER NOT NULL CHECK (files_listed >= 0),
    files_not_listed INTEGER NOT NULL CHECK (files_not_listed >= 0),
    listed           INTEGER NOT NULL CHECK (listed IN (0, 1)),
    units            INTEGER NOT NULL CHECK (units >= 0),
    units_walked     INTEGER NOT NULL CHECK (units_walked >= 0 AND units_walked <= units),
    next_unit        INTEGER NOT NULL,
    next_file        INTEGER NOT NULL,
    fail_reason      TEXT    NOT NULL,
    settling_since   INTEGER,
    found            INTEGER NOT NULL,
    skipped          INTEGER NOT NULL,
    handed_in        INTEGER NOT NULL,
    archived         INTEGER NOT NULL,
    already_held     INTEGER NOT NULL,
    quarantined      INTEGER NOT NULL,
    lost             INTEGER NOT NULL,
    failed           INTEGER NOT NULL,
    discarded        INTEGER NOT NULL,
    processing       INTEGER NOT NULL,
    bytes_handed_in  INTEGER NOT NULL,
    label_differs    INTEGER NOT NULL,
    CHECK (found = skipped + handed_in),
    CHECK (handed_in = archived + already_held + quarantined + lost + failed + discarded + processing),
    CHECK (skipped >= 0 AND archived >= 0 AND already_held >= 0 AND quarantined >= 0 AND lost >= 0 AND failed >= 0
        AND discarded >= 0 AND processing >= 0)
) STRICT;

-- A receiver's import jobs, for the rule that two of them in progress never
-- read the same files.
CREATE INDEX import_jobs_by_receiver ON import_jobs (receiver);

-- An import job's unit, one row of the work table of the kind import each:
-- its folder, where it is, the last path it judged (NULL before the first),
-- and its counts, which reconcile as the job's.
CREATE TABLE import_units (
    job_id          TEXT    NOT NULL,
    unit_id         TEXT    NOT NULL,
    work_id         TEXT    NOT NULL,
    seq             INTEGER NOT NULL,
    folder_key      BLOB    NOT NULL CHECK (length(folder_key) <= 2048),
    listing         INTEGER NOT NULL CHECK (listing IN (0, 1)),
    state           TEXT    NOT NULL CHECK (state IN ('queued', 'walking', 'walked', 'done', 'failed', 'canceled')),
    reason          TEXT    NOT NULL,
    cursor_key      BLOB    CHECK (length(cursor_key) <= 2304),
    walk_ended      INTEGER,
    found           INTEGER NOT NULL,
    skipped         INTEGER NOT NULL,
    handed_in       INTEGER NOT NULL,
    archived        INTEGER NOT NULL,
    already_held    INTEGER NOT NULL,
    quarantined     INTEGER NOT NULL,
    lost            INTEGER NOT NULL,
    failed          INTEGER NOT NULL,
    discarded       INTEGER NOT NULL,
    processing      INTEGER NOT NULL,
    bytes_handed_in INTEGER NOT NULL,
    label_differs   INTEGER NOT NULL,
    PRIMARY KEY (job_id, unit_id),
    CHECK (found = skipped + handed_in),
    CHECK (handed_in = archived + already_held + quarantined + lost + failed + discarded + processing),
    CHECK (skipped >= 0 AND archived >= 0 AND already_held >= 0 AND quarantined >= 0 AND lost >= 0 AND failed >= 0
        AND discarded >= 0 AND processing >= 0)
) STRICT, WITHOUT ROWID;

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
    job_id           TEXT    NOT NULL,
    path_key         BLOB    NOT NULL CHECK (length(path_key) <= 2048),
    unit_id          TEXT    NOT NULL,
    reception_id     TEXT    NOT NULL,
    sop_instance_uid TEXT    NOT NULL,
    sha256           BLOB    NOT NULL CHECK (length(sha256) = 32),
    size             INTEGER NOT NULL CHECK (size >= 0),
    outcome          TEXT    NOT NULL CHECK (outcome IN ('', 'failed')),
    listed_seq       INTEGER NOT NULL,
    at               INTEGER NOT NULL,
    parts TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (job_id, path_key)
) STRICT, WITHOUT ROWID;

-- The deciding transactions find a reception's arrivals by its received
-- file's key.
CREATE INDEX import_arrivals_by_file ON import_arrivals (reception_id, sop_instance_uid, sha256);

-- A file of an import job that did not reach the archive, listed by its
-- path in the walk's order, then kind: skipped, quarantined, lost, failed
-- or discarded. seq numbers the job's rows, so a page's cursor holds a
-- number, never a path. A failed file retried is hidden, not removed, so a
-- cursor's row stays.
CREATE TABLE import_files (
    job_id            TEXT    NOT NULL,
    path_key          BLOB    NOT NULL CHECK (length(path_key) <= 2048),
    kind              TEXT    NOT NULL CHECK (kind IN ('skipped', 'quarantined', 'lost', 'failed', 'discarded')),
    seq               INTEGER NOT NULL,
    unit_id           TEXT    NOT NULL,
    skip_code         TEXT    NOT NULL,
    quarantine_id     TEXT    NOT NULL,
    quarantine_reason TEXT    NOT NULL,
    reception_id      TEXT    NOT NULL,
    size              INTEGER NOT NULL CHECK (size >= 0),
    detail            TEXT    NOT NULL,
    hidden            INTEGER NOT NULL CHECK (hidden IN (0, 1)),
    at                INTEGER NOT NULL,
    PRIMARY KEY (job_id, path_key, kind)
) STRICT, WITHOUT ROWID;

-- A cursor finds its row by number; a unit's files are listed by path.
CREATE UNIQUE INDEX import_files_by_seq  ON import_files (job_id, seq);
CREATE INDEX import_files_by_unit        ON import_files (job_id, unit_id, path_key, kind);

-- An import job's, and each unit's, counts by skip code (kind 'skip') and
-- by quarantine reason (kind 'quarantined'); the job's under unit_id ''.
CREATE TABLE import_tallies (
    job_id  TEXT    NOT NULL,
    unit_id TEXT    NOT NULL,
    kind    TEXT    NOT NULL CHECK (kind IN ('skip', 'quarantined')),
    code    TEXT    NOT NULL,
    count   INTEGER NOT NULL CHECK (count >= 0),
    PRIMARY KEY (job_id, unit_id, kind, code)
) STRICT, WITHOUT ROWID;

-- What a scrub job reads: every stored object of one storage, or of every
-- storage of the tenant (''), read again and compared with what the catalog
-- and the scan's manifest say it holds; scheduled when the server made it
-- by itself. Its work is the units of a job like any other's, one per scan.
CREATE TABLE scrub_jobs (
    job_id    TEXT    NOT NULL PRIMARY KEY,
    storage   TEXT    NOT NULL,
    scheduled INTEGER NOT NULL CHECK (scheduled IN (0, 1))
) STRICT, WITHOUT ROWID;

-- One difference a scrub found and believed, after reading it twice: at
-- most one that holds per object and kind, so a finding found again by a
-- later scrub is the same row, seen again, and the findings that hold are
-- counted once. The labels are the scan's, '' for an orphan that names no
-- scan; sop_instance_uid is the instance's, '' for a manifest, a sidecar or
-- an orphan. The expected size and checksum are the catalog's or the
-- manifest's (NULL for an orphan), the found ones what was read (NULL for
-- an object missing). job_id is the job that found it; last_job and
-- last_seen the latest job that found it again. cleared_at and clear_job
-- are set when a later scrub read the object good, and then never change.
CREATE TABLE scrub_findings (
    id              TEXT    NOT NULL PRIMARY KEY,
    kind            TEXT    NOT NULL CHECK (kind IN ('missing', 'orphan', 'damaged', 'disagreeing', 'manifest_damaged')),
    storage         TEXT    NOT NULL,
    object_key      TEXT    NOT NULL CHECK (object_key <> ''),
    subject_label   TEXT    NOT NULL,
    session_label   TEXT    NOT NULL,
    scan_label      TEXT    NOT NULL,
    sop_instance_uid TEXT   NOT NULL,
    revision        INTEGER NOT NULL CHECK (revision >= 0),
    expected_size   INTEGER CHECK (expected_size IS NULL OR expected_size >= 0),
    found_size      INTEGER CHECK (found_size IS NULL OR found_size >= 0),
    expected_sha256 BLOB CHECK (expected_sha256 IS NULL OR length(expected_sha256) = 32),
    found_sha256    BLOB CHECK (found_sha256 IS NULL OR length(found_sha256) = 32),
    job_id          TEXT    NOT NULL,
    found_at        INTEGER NOT NULL,
    last_job        TEXT    NOT NULL,
    last_seen       INTEGER NOT NULL,
    cleared_at      INTEGER,
    clear_job       TEXT    NOT NULL,
    CHECK ((cleared_at IS NULL) = (clear_job = ''))
) STRICT;

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
    WHERE cleared_at IS NULL AND kind IN ('missing', 'damaged');

-- A storage's scrubs, for the last one, which a scheduled scrub follows.
CREATE INDEX scrub_jobs_by_storage ON scrub_jobs (storage);

-- What a rebuild job does: the catalog made again from what the tenant's
-- storages hold (their correction records, manifests and metadata) into a
-- shadow catalog, a scratch database of the same shape beside the tenant's,
-- never into the catalog in use. In compare mode the shadow is compared
-- with the catalog in use and dropped; in replace mode one transaction
-- makes it the catalog in use. How far the build got is kept in the shadow
-- itself, written with each scan's rows there, so a crash or a lost lease
-- goes on from the last scan written. scratch is the scratch schema the
-- shadow is built in, '' on SQLite, where the shadow is a file beside the
-- tenant's database. activated is set by the transaction that made its
-- shadow the catalog in use, and never changes after. dropped is set once
-- its shadow is removed, so a shadow a job left when it ended without its
-- worker (its attempts ran out, or it was canceled while queued) is found
-- and removed.
CREATE TABLE rebuild_jobs (
    job_id    TEXT    NOT NULL PRIMARY KEY,
    mode      TEXT    NOT NULL CHECK (mode IN ('compare', 'replace')),
    scratch   TEXT    NOT NULL,
    activated INTEGER NOT NULL CHECK (activated IN (0, 1)),
    dropped   INTEGER NOT NULL CHECK (dropped IN (0, 1))
) STRICT, WITHOUT ROWID;

-- The differences a compare job found, by name, the first 1000 in the
-- order the comparison found them, kept with the job so an admin reads
-- which rows differ after its shadow is gone; the job's tallies count
-- them all. row_key names the row by its label or UIDs, joined by "/".
CREATE TABLE rebuild_differences (
    job_id     TEXT    NOT NULL,
    seq        INTEGER NOT NULL CHECK (seq >= 0),
    table_name TEXT    NOT NULL,
    kind       TEXT    NOT NULL CHECK (kind IN ('added', 'removed', 'changed')),
    row_key    TEXT    NOT NULL,
    PRIMARY KEY (job_id, seq)
) STRICT, WITHOUT ROWID;

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
CREATE TABLE corrections (
    id                 TEXT    NOT NULL PRIMARY KEY CHECK (length(id) = 26),
    kind               TEXT    NOT NULL CHECK (kind IN ('delete', 'relabel', 'replace')),
    level              TEXT    NOT NULL CHECK (level IN ('subject', 'session', 'scan', 'instance')),
    state              TEXT    NOT NULL CHECK (state IN ('prepared', 'committed', 'purging', 'purged', 'undone', 'abandoned')),
    records_state      TEXT    NOT NULL,
    origin             TEXT    NOT NULL CHECK (origin IN ('person', 'retention')),
    subject_label      TEXT    NOT NULL,
    session_label      TEXT    NOT NULL,
    scan_label         TEXT    NOT NULL,
    instance_uid       TEXT    NOT NULL,
    to_label           TEXT    NOT NULL,
    quarantine_id      TEXT    NOT NULL,
    reason             TEXT    NOT NULL,
    key_id             TEXT    NOT NULL,
    confirm_token      TEXT,
    created            INTEGER NOT NULL,
    committed_at       INTEGER,
    purge_after        INTEGER,
    ended_at           INTEGER,
    state_reason       TEXT    NOT NULL,
    old_revision       INTEGER NOT NULL CHECK (old_revision >= 0),
    new_revision       INTEGER NOT NULL CHECK (new_revision >= 0),
    old_sha256         BLOB CHECK (old_sha256 IS NULL OR length(old_sha256) = 32),
    new_sha256         BLOB CHECK (new_sha256 IS NULL OR length(new_sha256) = 32),
    received_sha256    BLOB CHECK (received_sha256 IS NULL OR length(received_sha256) = 32),
    new_size           INTEGER NOT NULL CHECK (new_size >= 0),
    instance_key       TEXT    NOT NULL,
    instance_size      INTEGER NOT NULL CHECK (instance_size >= 0),
    restore_line       BLOB,
    arrivals_allowed   INTEGER NOT NULL CHECK (arrivals_allowed IN (0, 1)),
    undo_reason        TEXT    NOT NULL,
    unresolved         INTEGER NOT NULL CHECK (unresolved IN (0, 1)),
    subjects           INTEGER NOT NULL CHECK (subjects >= 0),
    sessions           INTEGER NOT NULL CHECK (sessions >= 0),
    scans              INTEGER NOT NULL CHECK (scans >= 0),
    instances          INTEGER NOT NULL CHECK (instances >= 0),
    bytes              INTEGER NOT NULL CHECK (bytes >= 0),
    objects_copied     INTEGER NOT NULL CHECK (objects_copied >= 0),
    objects_superseded INTEGER NOT NULL CHECK (objects_superseded >= 0),
    swept              INTEGER NOT NULL DEFAULT 0 CHECK (swept IN (0, 1)),
    CHECK ((committed_at IS NULL) = (purge_after IS NULL)),
    CHECK ((state IN ('purged', 'undone', 'abandoned')) = (ended_at IS NOT NULL))
) STRICT;

-- A confirm token is used once; a Delete that carries it again answers the
-- correction it made. A replace of one quarantined file, and the list,
-- newest first, by kind and by state.
CREATE UNIQUE INDEX corrections_by_confirm ON corrections (confirm_token) WHERE confirm_token IS NOT NULL;
CREATE INDEX corrections_by_quarantine ON corrections (quarantine_id) WHERE quarantine_id <> '';
CREATE INDEX corrections_by_state ON corrections (state, id);
CREATE INDEX corrections_due ON corrections (purge_after, id) WHERE state = 'committed' AND unresolved = 0;

-- The scans a correction touches, by the labels they had when it was
-- made, and for a relabel the labels it gives them; the storage each is on.
CREATE TABLE correction_scans (
    correction_id TEXT    NOT NULL,
    series_uid    TEXT    NOT NULL,
    study_uid     TEXT    NOT NULL,
    storage       TEXT    NOT NULL,
    subject_label TEXT    NOT NULL,
    session_label TEXT    NOT NULL,
    scan_label    TEXT    NOT NULL,
    new_subject   TEXT    NOT NULL,
    new_session   TEXT    NOT NULL,
    new_scan      TEXT    NOT NULL,
    instances     INTEGER NOT NULL CHECK (instances >= 0),
    bytes         INTEGER NOT NULL CHECK (bytes >= 0),
    PRIMARY KEY (correction_id, series_uid)
) STRICT, WITHOUT ROWID;

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
-- are no longer the ones read is kept (kept_changed).
CREATE TABLE correction_keys (
    correction_id    TEXT    NOT NULL,
    ord              INTEGER NOT NULL CHECK (ord IN (1, 2, 3)),
    storage          TEXT    NOT NULL,
    subject_label    TEXT    NOT NULL,
    session_label    TEXT    NOT NULL,
    scan_label       TEXT    NOT NULL,
    sop_instance_uid TEXT    NOT NULL,
    storage_key      TEXT    NOT NULL,
    size             INTEGER NOT NULL CHECK (size >= 0),
    sha256           BLOB CHECK (sha256 IS NULL OR length(sha256) = 32),
    outcome          TEXT    NOT NULL CHECK (outcome IN ('', 'purged', 'kept_shared', 'kept_changed')),
    PRIMARY KEY (correction_id, ord, storage, subject_label, session_label, scan_label, sop_instance_uid)
) STRICT, WITHOUT ROWID;

-- What a purge has left to do, in order.
CREATE INDEX correction_keys_pending ON correction_keys (correction_id, ord) WHERE outcome = '';

-- The last time the server's clock was trusted for a purge: an automatic
-- purge is refused when the clock reads earlier than a time the
-- corrections recorded, and paused when it jumped forward too far since.
-- One row, made here.
CREATE TABLE correction_clock (
    one     INTEGER NOT NULL PRIMARY KEY CHECK (one = 1),
    trusted INTEGER
) STRICT;
INSERT INTO correction_clock (one, trusted) VALUES (1, NULL);

-- Each object a purge removed, by storage and key, and its version on a
-- versioned bucket ('' for none): its size and SHA-256 as the manifest or
-- the catalog held them, and what the storage did (removed, hidden,
-- retained, unknown).
CREATE TABLE correction_objects (
    correction_id TEXT    NOT NULL,
    storage       TEXT    NOT NULL,
    object_key    TEXT    NOT NULL CHECK (object_key <> ''),
    version       TEXT    NOT NULL,
    size          INTEGER NOT NULL CHECK (size >= 0),
    sha256        BLOB CHECK (sha256 IS NULL OR length(sha256) = 32),
    disposition   TEXT    NOT NULL CHECK (disposition IN ('removed', 'hidden', 'retained', 'unknown')),
    retain_until  INTEGER,
    purged_at     INTEGER NOT NULL,
    PRIMARY KEY (correction_id, storage, object_key, version)
) STRICT, WITHOUT ROWID;

-- What names a delete's data (a subject's label, a session's Study Instance
-- UID, a scan's Series Instance UID, an instance's SOP Instance UID), from
-- the delete's prepare on, so an arrival that matches is held as
-- was_deleted, until an admin allows it again.
CREATE TABLE deleted_targets (
    kind          TEXT NOT NULL CHECK (kind IN ('subject', 'study', 'series', 'instance')),
    value         TEXT NOT NULL CHECK (value <> ''),
    correction_id TEXT NOT NULL,
    PRIMARY KEY (kind, value, correction_id)
) STRICT, WITHOUT ROWID;

CREATE INDEX deleted_targets_by_correction ON deleted_targets (correction_id);

-- A revision of an instance that is no longer current: superseded by a
-- replace, or deleted by a delete whose purge removed it, kept for as long
-- as the tenant is. A replace never writes a key twice: its new revision has
-- a key of its own. purged says its bytes were removed.
CREATE TABLE instance_revisions (
    sop_instance_uid TEXT    NOT NULL,
    revision         INTEGER NOT NULL CHECK (revision >= 1),
    sha256           BLOB    NOT NULL CHECK (length(sha256) = 32),
    size             INTEGER NOT NULL CHECK (size >= 0),
    storage          TEXT    NOT NULL,
    storage_key      TEXT    NOT NULL,
    correction_id    TEXT    NOT NULL,
    fate             TEXT    NOT NULL CHECK (fate IN ('superseded', 'deleted')),
    purged           INTEGER NOT NULL CHECK (purged IN (0, 1)),
    PRIMARY KEY (sop_instance_uid, revision)
) STRICT, WITHOUT ROWID;

CREATE INDEX instance_revisions_by_correction ON instance_revisions (correction_id);

-- A relabel's labels, before and after, at its level.
CREATE TABLE relabels (
    correction_id TEXT NOT NULL PRIMARY KEY,
    level         TEXT NOT NULL CHECK (level IN ('subject', 'session', 'scan')),
    old_subject   TEXT NOT NULL,
    old_session   TEXT NOT NULL,
    old_scan      TEXT NOT NULL,
    new_label     TEXT NOT NULL
) STRICT, WITHOUT ROWID;

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
    correction_id  TEXT    NOT NULL,
    id             INTEGER NOT NULL,
    label          TEXT    NOT NULL,
    folded         TEXT    NOT NULL,
    created        INTEGER NOT NULL,
    session_count  INTEGER NOT NULL,
    instance_count INTEGER NOT NULL,
    bytes          INTEGER NOT NULL,
    updated        INTEGER NOT NULL,
    PRIMARY KEY (correction_id, id)
) STRICT, WITHOUT ROWID;

CREATE TABLE hidden_sessions (
    correction_id  TEXT    NOT NULL,
    id             INTEGER NOT NULL,
    study_uid      TEXT    NOT NULL,
    subject_id     INTEGER NOT NULL,
    label          TEXT    NOT NULL,
    folded         TEXT    NOT NULL,
    storage        TEXT    NOT NULL,
    created        INTEGER NOT NULL,
    subject_label  TEXT    NOT NULL,
    subject_folded TEXT    NOT NULL,
    study_date     TEXT    NOT NULL,
    receiver       TEXT    NOT NULL,
    scan_count     INTEGER NOT NULL,
    instance_count INTEGER NOT NULL,
    bytes          INTEGER NOT NULL,
    updated        INTEGER NOT NULL,
    route          TEXT    NOT NULL,
    PRIMARY KEY (correction_id, id)
) STRICT, WITHOUT ROWID;

CREATE TABLE hidden_scans (
    correction_id      TEXT    NOT NULL,
    id                 INTEGER NOT NULL,
    session_id         INTEGER NOT NULL,
    series_uid         TEXT    NOT NULL,
    label              TEXT    NOT NULL,
    folded             TEXT    NOT NULL,
    instances          INTEGER NOT NULL,
    bytes              INTEGER NOT NULL,
    manifest_sha256    BLOB,
    created            INTEGER NOT NULL,
    updated            INTEGER NOT NULL,
    subject_label      TEXT    NOT NULL,
    subject_folded     TEXT    NOT NULL,
    session_label      TEXT    NOT NULL,
    session_folded     TEXT    NOT NULL,
    storage            TEXT    NOT NULL,
    modality           TEXT    NOT NULL,
    series_description TEXT    NOT NULL,
    description_folded TEXT    NOT NULL,
    series_number      INTEGER,
    receiver           TEXT    NOT NULL,
    study_uid          TEXT    NOT NULL,
    PRIMARY KEY (correction_id, id)
) STRICT, WITHOUT ROWID;

CREATE TABLE hidden_instances (
    correction_id      TEXT    NOT NULL,
    scan_id            INTEGER NOT NULL,
    sop_instance_uid   TEXT    NOT NULL,
    reception_id       TEXT    NOT NULL,
    sha256             BLOB    NOT NULL,
    archived_sha256    BLOB,
    storage_key        TEXT    NOT NULL,
    byte_offset        INTEGER NOT NULL,
    byte_length        INTEGER NOT NULL,
    size               INTEGER NOT NULL,
    held_at            INTEGER NOT NULL,
    profile_version_id INTEGER,
    PRIMARY KEY (correction_id, sop_instance_uid)
) STRICT, WITHOUT ROWID;

-- A hidden instance is found by its UID (ResolveInstances).
CREATE INDEX hidden_instances_by_uid ON hidden_instances (sop_instance_uid);

CREATE TABLE hidden_session_modalities (
    correction_id TEXT    NOT NULL,
    session_id    INTEGER NOT NULL,
    modality      TEXT    NOT NULL,
    created       INTEGER NOT NULL,
    PRIMARY KEY (correction_id, session_id, modality)
) STRICT, WITHOUT ROWID;

CREATE TABLE hidden_reception_scans (
    correction_id TEXT    NOT NULL,
    reception_id  TEXT    NOT NULL,
    scan_id       INTEGER NOT NULL,
    instances     INTEGER NOT NULL,
    bytes         INTEGER NOT NULL,
    PRIMARY KEY (correction_id, reception_id, scan_id)
) STRICT, WITHOUT ROWID;

-- A hidden row's labels and UIDs stay taken for arrivals while its delete
-- is not ended: the archive step looks each scan's path, study and series
-- up here before it puts anything.
CREATE INDEX hidden_subjects_by_label  ON hidden_subjects (folded);
CREATE INDEX hidden_sessions_by_study  ON hidden_sessions (study_uid);
CREATE INDEX hidden_sessions_by_label  ON hidden_sessions (subject_folded, folded);
CREATE INDEX hidden_scans_by_series    ON hidden_scans (series_uid);
CREATE INDEX hidden_scans_by_label     ON hidden_scans (subject_folded, session_folded, folded);
