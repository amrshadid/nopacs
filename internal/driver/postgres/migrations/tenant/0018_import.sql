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
-- as bytea with each "/" as the byte 0: no segment holds NUL, so bytea's
-- order (memcmp, then length) is the order an import walks a tree in, a
-- directory's whole subtree before the next name beside it. A path an
-- index holds is at most 2048 bytes, so an index row with the job id, a
-- unit id and a kind beside it stays below the 2704 bytes a B-tree takes in
-- one row; an entry whose path is longer is skipped and listed under its
-- deepest ancestor that fits. A unit's cursor, which no index holds, may be
-- such an entry's path: at most 2048 + 1 + 255 bytes.
--
-- Only a check is widened, and tables and columns with their defaults
-- added, so the migration is safe on a database that holds data.
ALTER TABLE jobs DROP CONSTRAINT jobs_kind_check;
ALTER TABLE jobs ADD CONSTRAINT jobs_kind_check CHECK (kind IN ('send', 'deid', 'import'));

-- What an import job reads (its receiver, path, map and map
-- configuration, the map's lists as JSON arrays), and its counts, kept in
-- the transaction of every file's record and every outcome; next_unit and
-- next_file number its units and its listed files. fail_reason is the
-- first failed unit's reason; settling_since when its last walk ended with
-- files processing.
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

-- An import job's unit: its folder, where it is, the last path it judged
-- (NULL before the first), and its counts, which reconcile as the job's.
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
-- the job's bound). A decided outcome removes it: what an outcome leaves to
-- list is a row of import_files.
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
CREATE UNIQUE INDEX import_files_by_seq ON import_files (job_id, seq);
CREATE INDEX import_files_by_unit       ON import_files (job_id, unit_id, path_key, kind);

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

-- The import job that handed a quarantined file in, and its path.
ALTER TABLE quarantine ADD COLUMN import_job text COLLATE "C" NOT NULL DEFAULT '';
ALTER TABLE quarantine ADD COLUMN import_path text NOT NULL DEFAULT '';
