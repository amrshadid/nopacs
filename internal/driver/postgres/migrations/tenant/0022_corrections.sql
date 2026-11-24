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
    CONSTRAINT corrections_pkey PRIMARY KEY (id),
    CONSTRAINT corrections_id_check CHECK (length(id) = 26),
    CONSTRAINT corrections_kind_check CHECK (kind IN ('delete', 'relabel', 'replace')),
    CONSTRAINT corrections_level_check CHECK (level IN ('subject', 'session', 'scan', 'instance')),
    CONSTRAINT corrections_state_check CHECK (state IN ('prepared', 'committed', 'purging', 'purged', 'undone', 'abandoned')),
    CONSTRAINT corrections_origin_check CHECK (origin IN ('person', 'retention')),
    CONSTRAINT corrections_counts_check CHECK (old_revision >= 0 AND new_revision >= 0 AND new_size >= 0 AND instance_size >= 0
        AND subjects >= 0 AND sessions >= 0 AND scans >= 0 AND instances >= 0 AND bytes >= 0 AND objects_copied >= 0
        AND objects_superseded >= 0),
    CONSTRAINT corrections_sha256_check CHECK ((old_sha256 IS NULL OR octet_length(old_sha256) = 32)
        AND (new_sha256 IS NULL OR octet_length(new_sha256) = 32) AND (received_sha256 IS NULL OR octet_length(received_sha256) = 32)),
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
-- are no longer the ones read is kept (kept_changed).
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
CREATE TABLE correction_clock (
    one     bigint      NOT NULL,
    trusted timestamptz,
    CONSTRAINT correction_clock_pkey PRIMARY KEY (one),
    CONSTRAINT correction_clock_one_check CHECK (one = 1)
);
INSERT INTO correction_clock (one, trusted) VALUES (1, NULL);

-- Each object a purge removed, by storage and key, and its version on a
-- versioned bucket ('' for none): its size and SHA-256 as the manifest or
-- the catalog held them, and what the storage did (removed, hidden,
-- retained, unknown).
CREATE TABLE correction_objects (
    correction_id text        COLLATE "C" NOT NULL,
    storage       text        COLLATE "C" NOT NULL,
    object_key    text        COLLATE "C" NOT NULL,
    version       text        COLLATE "C" NOT NULL,
    size          bigint      NOT NULL,
    sha256        bytea,
    disposition   text        COLLATE "C" NOT NULL,
    retain_until  timestamptz,
    purged_at     timestamptz NOT NULL,
    CONSTRAINT correction_objects_pkey PRIMARY KEY (correction_id, storage, object_key, version),
    CONSTRAINT correction_objects_key_check CHECK (object_key <> ''),
    CONSTRAINT correction_objects_size_check CHECK (size >= 0),
    CONSTRAINT correction_objects_sha256_check CHECK (sha256 IS NULL OR octet_length(sha256) = 32),
    CONSTRAINT correction_objects_disposition_check CHECK (disposition IN ('removed', 'hidden', 'retained', 'unknown'))
);

-- What names a delete's data, so an arrival that matches is held: kind
-- subject (a label), study, series or instance (a UID).
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
-- replace, or deleted by a delete whose purge removed it. purged says its
-- bytes were removed.
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

-- The catalog's rows a committed delete hides, as they were.
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
    CONSTRAINT hidden_subjects_pkey PRIMARY KEY (correction_id, id)
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
    CONSTRAINT hidden_sessions_pkey PRIMARY KEY (correction_id, id)
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
    CONSTRAINT hidden_scans_pkey PRIMARY KEY (correction_id, id)
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
    CONSTRAINT hidden_instances_pkey PRIMARY KEY (correction_id, sop_instance_uid)
);

-- A hidden instance is found by its UID (ResolveInstances).
CREATE INDEX hidden_instances_by_uid ON hidden_instances (sop_instance_uid);

CREATE TABLE hidden_session_modalities (
    correction_id text        COLLATE "C" NOT NULL,
    session_id    bigint      NOT NULL,
    modality      text        COLLATE "C" NOT NULL,
    created       timestamptz NOT NULL,
    CONSTRAINT hidden_session_modalities_pkey PRIMARY KEY (correction_id, session_id, modality)
);

CREATE TABLE hidden_reception_scans (
    correction_id text   COLLATE "C" NOT NULL,
    reception_id  text   COLLATE "C" NOT NULL,
    scan_id       bigint NOT NULL,
    instances     bigint NOT NULL,
    bytes         bigint NOT NULL,
    CONSTRAINT hidden_reception_scans_pkey PRIMARY KEY (correction_id, reception_id, scan_id)
);

-- A hidden row's labels and UIDs stay taken for arrivals while its delete
-- is not ended: the archive step looks each scan's path, study and series
-- up here before it puts anything.
CREATE INDEX hidden_subjects_by_label  ON hidden_subjects (folded);
CREATE INDEX hidden_sessions_by_study  ON hidden_sessions (study_uid);
CREATE INDEX hidden_sessions_by_label  ON hidden_sessions (subject_folded, folded);
CREATE INDEX hidden_scans_by_series    ON hidden_scans (series_uid);
CREATE INDEX hidden_scans_by_label     ON hidden_scans (subject_folded, session_folded, folded);
