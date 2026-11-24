-- What a tenant keeps to send archived data out: its senders, its jobs, each
-- job's scans with how far each scan's unit got, the instances a retry sends
-- again below its scan's starting point, the instances a scan's unit owes
-- a second try, and the instances a job's peer refused or kept with a
-- warning. The work itself is units of the work
-- table, of the kind send-<sender>. Only tables are added, empty, so the
-- migration is safe on a database that holds data.
--
-- The rules of 0002_pipeline.sql hold: times are microseconds since 1970,
-- text a caller did not give is '' rather than NULL, and there are no
-- foreign keys: a job keeps the name of a sender removed, and every other
-- reference is checked by the change that makes it.

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

-- A job: what it sends (a target by labels, or a reception's instances),
-- where it is, and its running counts, kept in the transaction of every
-- write of its units. bound_at is the moment whose catalog it sends (its
-- creation, or the creation of the job a retry sends again). Its listing
-- has two parts: a retry's first lists the ended job's scans (retry_after,
-- retry_listed), then every job lists what its target holds that is not
-- listed yet (list_*, target_listed); listed is both. progress_at and progress_mark are the time and the
-- instances answered at its last progress event, which is written at most
-- once in a period.
CREATE TABLE jobs (
    id                  TEXT    NOT NULL PRIMARY KEY,
    kind                TEXT    NOT NULL CHECK (kind = 'send'),
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
