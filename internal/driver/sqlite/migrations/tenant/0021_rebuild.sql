-- A job may now be of the kind rebuild: the catalog made again from what
-- the tenant's storages hold (their correction records, manifests and
-- metadata), into a shadow catalog, a scratch database of the same shape
-- beside the tenant's, never into the catalog in use. In compare mode the
-- shadow is compared with the catalog in use and dropped; in replace mode
-- one transaction makes it the catalog in use. What a rebuild job does is a
-- row of rebuild_jobs; how far its build got is kept in the shadow itself,
-- written in the transaction that writes each scan's rows there, so a
-- crash or a lost lease goes on from the last scan written.
--
-- The tenant's event feed now has a history id: a random id, made with the
-- database and made again whenever a rebuild replaces the catalog, so a
-- consumer that keys what it read by (history, cursor) knows the feed it
-- follows began again from a new catalog.
--
-- SQLite cannot change a table's CHECK in place, so jobs is copied into a
-- table whose kind may be rebuild, every row as it is, and its indexes made
-- again, as 0019_scrub.sql did; subjects, sessions and scans are copied so
-- too, into tables whose ids only grow (below); the other changes add
-- tables. No row changes meaning, so the migration is safe on a database
-- that holds data.
-- The rules of 0002_pipeline.sql hold.
CREATE TABLE jobs_new (
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

INSERT INTO jobs_new SELECT * FROM jobs;

DROP TABLE jobs;
ALTER TABLE jobs_new RENAME TO jobs;

CREATE UNIQUE INDEX jobs_by_key ON jobs (unique_key);
CREATE INDEX jobs_by_created            ON jobs (created, id);
CREATE INDEX jobs_by_state              ON jobs (state, created, id);
CREATE INDEX jobs_by_sender             ON jobs (sender, created, id);
CREATE INDEX jobs_in_progress           ON jobs (created, id) WHERE ended = 0;
CREATE INDEX jobs_of_sender_in_progress ON jobs (sender, created, id) WHERE ended = 0;

-- What a rebuild job does: its mode, and the scratch schema its shadow is
-- built in, '' on SQLite, where the shadow is a file beside the tenant's
-- database. activated is set by the transaction that made its shadow the
-- catalog in use, and never changes after. dropped is set once its shadow
-- is removed, so a shadow a job left when it ended without its worker (its
-- attempts ran out, or it was canceled while queued) is found and removed.
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

-- The ids of subjects, sessions and scans now only grow: the tables are
-- made again with AUTOINCREMENT (sqlite.org/autoinc.html), every column
-- in the same order, every constraint and index as it was. Without it
-- SQLite gives a new row one more than the largest id held, so removing
-- the row of the largest id (a rebuild that replaces the catalog, a
-- correction's delete) would give its id to the next row made, while a
-- send job, a retry's failures or a correction's hidden rows still name
-- the row by that id: a send would send another scan's files. Copying the
-- rows with their ids sets each table's sequence to its largest id.
CREATE TABLE subjects_new (
    id             INTEGER NOT NULL PRIMARY KEY AUTOINCREMENT,
    label          TEXT    NOT NULL,
    folded         TEXT    NOT NULL UNIQUE,
    created        INTEGER NOT NULL,
    session_count  INTEGER NOT NULL DEFAULT 0 CHECK (session_count >= 0),
    instance_count INTEGER NOT NULL DEFAULT 0 CHECK (instance_count >= 0),
    bytes          INTEGER NOT NULL DEFAULT 0 CHECK (bytes >= 0),
    updated        INTEGER NOT NULL DEFAULT 0
) STRICT;
INSERT INTO subjects_new SELECT * FROM subjects;
DROP TABLE subjects;
ALTER TABLE subjects_new RENAME TO subjects;
CREATE INDEX subjects_by_created ON subjects (created, id);

CREATE TABLE sessions_new (
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
INSERT INTO sessions_new SELECT * FROM sessions;
DROP TABLE sessions;
ALTER TABLE sessions_new RENAME TO sessions;
CREATE INDEX sessions_by_storage ON sessions (storage);
CREATE INDEX sessions_by_created ON sessions (created, id);
CREATE INDEX sessions_by_subject_created ON sessions (subject_id, created, id);
CREATE INDEX sessions_by_receiver ON sessions (receiver, created, id);
CREATE INDEX sessions_by_study_date ON sessions (study_date, id);
CREATE INDEX sessions_by_subject_study_date ON sessions (subject_id, study_date, id);
CREATE UNIQUE INDEX sessions_by_label ON sessions (subject_folded, folded);

CREATE TABLE scans_new (
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
INSERT INTO scans_new SELECT * FROM scans;
DROP TABLE scans;
ALTER TABLE scans_new RENAME TO scans;
CREATE INDEX scans_by_created ON scans (created, id);
CREATE INDEX scans_by_session_created ON scans (session_id, created, id);
CREATE INDEX scans_by_modality ON scans (modality, created, id);
CREATE INDEX scans_by_receiver ON scans (receiver, created, id);
CREATE INDEX scans_by_description ON scans (description_folded, id);
CREATE UNIQUE INDEX scans_by_label ON scans (subject_folded, session_folded, folded);
CREATE INDEX scans_by_storage ON scans (storage);
CREATE INDEX scans_by_study ON scans (study_uid, id);

-- The event feed's history: one row, its id 32 lowercase hexadecimal
-- digits of a random value, and since when it holds.
CREATE TABLE event_history (
    one     INTEGER NOT NULL PRIMARY KEY CHECK (one = 1),
    id      TEXT    NOT NULL CHECK (length(id) = 32),
    started INTEGER NOT NULL
) STRICT;
INSERT INTO event_history (one, id, started) VALUES (1, lower(hex(randomblob(16))),
    CAST((julianday('now') - 2440587.5) * 86400000000 AS INTEGER));
