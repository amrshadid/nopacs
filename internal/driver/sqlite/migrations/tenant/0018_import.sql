-- A job may now be of the kind import: the files of a directory under an
-- import receiver's root handed to that receiver, one at a time. What it
-- reads is a row of import_jobs; its work is units of the work table of the
-- kind import, one row of import_units each, with the unit's cursor and
-- counts; a file it handed in and not yet decided is a row of
-- import_arrivals, linked to its received file by reception, SOP Instance
-- UID and received checksum; a file that did not reach the archive is a row
-- of import_files; the counts by skip code and by quarantine reason are
-- rows of import_tallies. A quarantine record names the import job and the
-- path of a file an import handed in.
--
-- A path can identify a person, so it is kept in these rows only. Where an
-- index orders paths (a unit's folder and cursor, a file's path) it is kept
-- as a BLOB with each "/" as the byte 0: no segment holds NUL, so the
-- BLOB's byte order (memcmp, then length) is the order an import walks a
-- tree in, a directory's whole subtree before the next name beside it. A
-- path an index holds is at most 2048 bytes, the bound the Postgres driver
-- needs for its B-tree rows, held here too so the two drivers take the same
-- rows; an entry whose path is longer is skipped and listed under its
-- deepest ancestor that fits. A unit's cursor, which no index holds, may be
-- such an entry's path: at most 2048 + 1 + 255 bytes.
--
-- SQLite cannot change a table's CHECK in place, so jobs is copied into a
-- table whose kind may be import, every row as it is, and its indexes made
-- again; the other changes add tables and columns with their defaults. No
-- row changes meaning, so the migration is safe on a database that holds
-- data. The rules of 0002_pipeline.sql hold.
CREATE TABLE jobs_new (
    id                  TEXT    NOT NULL PRIMARY KEY,
    kind                TEXT    NOT NULL CHECK (kind IN ('send', 'deid', 'import')),
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

-- What an import job reads (its receiver, path, map and map
-- configuration, the map's lists as JSON arrays), and its counts, kept in
-- the transaction of every file's record and every outcome; next_unit and
-- next_file number its units and its listed files. fail_reason is the
-- first failed unit's reason; settling_since when its last walk ended with
-- files processing.
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

-- An import job's unit: its folder, where it is, the last path it judged
-- (NULL before the first), and its counts, which reconcile as the job's.
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
-- the job's bound). A decided outcome removes it: what an outcome leaves to
-- list is a row of import_files.
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

-- The import job that handed a quarantined file in, and its path.
ALTER TABLE quarantine ADD COLUMN import_job TEXT NOT NULL DEFAULT '';
ALTER TABLE quarantine ADD COLUMN import_path TEXT NOT NULL DEFAULT '';
