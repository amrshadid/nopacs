-- A job may now be of the kind scrub: every stored object of a storage, or
-- of every storage of the tenant, read again and compared with what the
-- catalog and the scan's manifest say it holds. What a scrub job reads is a
-- row of scrub_jobs; its work is the units of a job like any other's, one
-- per scan, with their fenced cursors and counts.
--
-- What a scrub believes, after reading it twice, is a row of
-- scrub_findings: at most one that holds per object and kind (its storage,
-- key and kind), so a finding found again by a later scrub is the same row,
-- seen again, and the findings that hold are counted once. A finding read
-- good later is cleared, and keeps its row with when and by which job. An
-- instance whose object is missing or damaged is a row of instance_damage,
-- a sparse table rather than a column of instances, so no instance row is
-- rewritten and every read of an instance adds one lookup of a table that
-- is empty while the archive is whole.
--
-- SQLite cannot change a table's CHECK in place, so jobs is copied into a
-- table whose kind may be scrub, every row as it is, and its indexes made
-- again, as 0018_import.sql did; the other changes add tables. No row
-- changes meaning, so the migration is safe on a database that holds data.
-- The rules of 0002_pipeline.sql hold.
CREATE TABLE jobs_new (
    id                  TEXT    NOT NULL PRIMARY KEY,
    kind                TEXT    NOT NULL CHECK (kind IN ('send', 'deid', 'import', 'scrub')),
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

INSERT INTO jobs_new SELECT * FROM jobs;

DROP TABLE jobs;
ALTER TABLE jobs_new RENAME TO jobs;

CREATE UNIQUE INDEX jobs_by_key ON jobs (unique_key);
CREATE INDEX jobs_by_created            ON jobs (created, id);
CREATE INDEX jobs_by_state              ON jobs (state, created, id);
CREATE INDEX jobs_by_sender             ON jobs (sender, created, id);
CREATE INDEX jobs_in_progress           ON jobs (created, id) WHERE ended = 0;
CREATE INDEX jobs_of_sender_in_progress ON jobs (sender, created, id) WHERE ended = 0;

-- What a scrub job reads: one storage, or '' for every storage of the
-- tenant; scheduled when the server made it by itself.
CREATE TABLE scrub_jobs (
    job_id    TEXT    NOT NULL PRIMARY KEY,
    storage   TEXT    NOT NULL,
    scheduled INTEGER NOT NULL CHECK (scheduled IN (0, 1))
) STRICT, WITHOUT ROWID;

-- One difference a scrub found and believed. The labels are the scan's,
-- '' for an orphan that names no scan; sop_instance_uid is the instance's,
-- '' for a manifest, a sidecar or an orphan. The expected size and
-- checksum are the catalog's or the manifest's (NULL for an orphan), the
-- found ones what was read (NULL for an object missing). job_id is the job
-- that found it; last_job and last_seen the latest job that found it
-- again. cleared_at and clear_job are set when a later scrub read the
-- object good, and then never change.
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

-- An instance a scrub found missing or damaged: every read of it answers
-- data loss before its first byte. One per instance: the finding that marked
-- it.
CREATE TABLE instance_damage (
    sop_instance_uid TEXT    NOT NULL PRIMARY KEY,
    finding_id       TEXT    NOT NULL,
    marked_at        INTEGER NOT NULL
) STRICT, WITHOUT ROWID;
