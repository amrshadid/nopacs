-- A job may now be of the kind deid: archived instances judged against a
-- de-identification profile version, changing nothing. Such a job names no
-- sender (''), a mode (dry_run or check) and, for a dry run, the profile
-- version it runs ('' and 0 for each instance's own); a send job keeps
-- '' and 0 in each. What a deid job found is counted per key in
-- job_tallies, added in the transaction of each progress write.
--
-- SQLite cannot change a table's CHECK in place, so jobs is copied into a
-- table with the wider check, every row as it is with '' and 0 in the new
-- columns, and its indexes made again. No row changes meaning, so the
-- migration is safe on a database that holds data. The rules of
-- 0002_pipeline.sql hold.
CREATE TABLE jobs_new (
    id                  TEXT    NOT NULL PRIMARY KEY,
    kind                TEXT    NOT NULL CHECK (kind IN ('send', 'deid')),
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

INSERT INTO jobs_new (id, kind, sender, subject_label, session_label, scan_label, reception_id, receiver, unique_key, state,
        ended, reason, created_by_key, created, started, state_at, ended_at, bound_at, retry_of, retry_job, list_work, listed,
        retry_listed, target_listed, retry_after, list_session, list_scan, list_id, scans, scans_done, instances, bytes, sent,
        sent_bytes, warned, warned_bytes, refused, refused_bytes, failures_listed, failures_not_listed, progress_at,
        progress_mark, deid_mode, profile_name, profile_version)
    SELECT id, kind, sender, subject_label, session_label, scan_label, reception_id, receiver, unique_key, state,
        ended, reason, created_by_key, created, started, state_at, ended_at, bound_at, retry_of, retry_job, list_work, listed,
        retry_listed, target_listed, retry_after, list_session, list_scan, list_id, scans, scans_done, instances, bytes, sent,
        sent_bytes, warned, warned_bytes, refused, refused_bytes, failures_listed, failures_not_listed, progress_at,
        progress_mark, '', '', 0
    FROM jobs;

DROP TABLE jobs;
ALTER TABLE jobs_new RENAME TO jobs;

CREATE UNIQUE INDEX jobs_by_key ON jobs (unique_key);
CREATE INDEX jobs_by_created            ON jobs (created, id);
CREATE INDEX jobs_by_state              ON jobs (state, created, id);
CREATE INDEX jobs_by_sender             ON jobs (sender, created, id);
CREATE INDEX jobs_in_progress           ON jobs (created, id) WHERE ended = 0;
CREATE INDEX jobs_of_sender_in_progress ON jobs (sender, created, id) WHERE ended = 0;

-- A deid job's counts per key (the contract's JobTally): at most 1000 keys
-- a job, and the key 'other' for those past it.
CREATE TABLE job_tallies (
    job_id TEXT    NOT NULL,
    tally  TEXT    NOT NULL,
    count  INTEGER NOT NULL CHECK (count >= 0),
    PRIMARY KEY (job_id, tally)
) STRICT, WITHOUT ROWID;
