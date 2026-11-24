-- Corrections: recorded changes to archived data, a delete, a relabel or a
-- replace. Each is a row of corrections, committed first in the state
-- prepared with its operation id (a ULID in lowercase), its target, its
-- reason and its audit entry, before anything is written to a storage. It
-- moves only forward: prepared to committed or abandoned, committed to
-- purging or (a delete) undone, purging to purged. records_state is the
-- state its correction records on the storages were last written with
-- ('' before the first), so a stop between writing the row and the records is found at start.
--
-- A delete's publishing transaction moves the catalog's rows it hides into
-- the hidden_ tables, each the same columns as its catalog table and the
-- correction's id, so no read of the catalog sees them and an undo moves
-- them back as they were; its purge removes them. What names the deleted
-- data (a subject's label, a session's Study Instance UID, a scan's Series
-- Instance UID, an instance's SOP Instance UID) is a row of
-- deleted_targets, from the delete's prepare on, so an arrival that matches
-- is held as was_deleted, until an admin allows it again.
--
-- A replace never writes a key twice: its new revision has a key of its
-- own, and the revision it superseded is a row of instance_revisions, kept
-- for as long as the tenant is, as is the revision of each instance a
-- purged delete removed. A relabel keeps its old and new labels in
-- relabels. Every object a purge removed is a row of correction_objects,
-- with its size, its SHA-256 and what its storage said it did with it; what
-- it may remove at all is fixed at the publish, in correction_keys. swept
-- says an abandoned relabel's copies were removed.
--
-- Only tables are added, so the migration is safe on a database that holds
-- data. The rules of 0002_pipeline.sql hold.
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

-- What names a delete's data, so an arrival that matches is held: kind
-- subject (a label), study, series or instance (a UID).
CREATE TABLE deleted_targets (
    kind          TEXT NOT NULL CHECK (kind IN ('subject', 'study', 'series', 'instance')),
    value         TEXT NOT NULL CHECK (value <> ''),
    correction_id TEXT NOT NULL,
    PRIMARY KEY (kind, value, correction_id)
) STRICT, WITHOUT ROWID;

CREATE INDEX deleted_targets_by_correction ON deleted_targets (correction_id);

-- A revision of an instance that is no longer current: superseded by a
-- replace, or deleted by a delete whose purge removed it. purged says its
-- bytes were removed.
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

-- The catalog's rows a committed delete hides, as they were.
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
