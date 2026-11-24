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
-- Only a check is widened, and tables and indexes added, so the migration
-- is safe on a database that holds data.
ALTER TABLE jobs DROP CONSTRAINT jobs_kind_check;
ALTER TABLE jobs ADD CONSTRAINT jobs_kind_check CHECK (kind IN ('send', 'deid', 'import', 'scrub'));

-- What a scrub job reads: one storage, or '' for every storage of the
-- tenant; scheduled when the server made it by itself.
CREATE TABLE scrub_jobs (
    job_id    text    COLLATE "C" NOT NULL,
    storage   text    COLLATE "C" NOT NULL,
    scheduled boolean NOT NULL,
    CONSTRAINT scrub_jobs_pkey PRIMARY KEY (job_id)
);

-- One difference a scrub found and believed. The labels are the scan's,
-- '' for an orphan that names no scan; sop_instance_uid is the instance's,
-- '' for a manifest, a sidecar or an orphan. The expected size and
-- checksum are the catalog's or the manifest's (NULL for an orphan), the
-- found ones what was read (NULL for an object missing). job_id is the job
-- that found it; last_job and last_seen the latest job that found it
-- again. cleared_at and clear_job are set when a later scrub read the
-- object good, and then never change.
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
    CONSTRAINT scrub_findings_pkey PRIMARY KEY (id),
    CONSTRAINT scrub_findings_kind_check CHECK (kind IN ('missing', 'orphan', 'damaged', 'disagreeing', 'manifest_damaged')),
    CONSTRAINT scrub_findings_values_check CHECK (object_key <> '' AND revision >= 0
        AND (expected_size IS NULL OR expected_size >= 0) AND (found_size IS NULL OR found_size >= 0)
        AND (expected_sha256 IS NULL OR length(expected_sha256) = 32) AND (found_sha256 IS NULL OR length(found_sha256) = 32)),
    CONSTRAINT scrub_findings_cleared_check CHECK ((cleared_at IS NULL) = (clear_job = ''))
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
    WHERE cleared_at IS NULL AND kind IN ('missing', 'damaged');

-- A storage's scrubs, for the last one, which a scheduled scrub follows.
CREATE INDEX scrub_jobs_by_storage ON scrub_jobs (storage);

-- An instance a scrub found missing or damaged: every read of it answers
-- data loss before its first byte. One per instance: the finding that marked
-- it.
CREATE TABLE instance_damage (
    sop_instance_uid text        COLLATE "C" NOT NULL,
    finding_id       text        COLLATE "C" NOT NULL,
    marked_at        timestamptz NOT NULL,
    CONSTRAINT instance_damage_pkey PRIMARY KEY (sop_instance_uid)
);

-- A storage's scans in order of id, for a scrub of one storage: the scans'
-- index on the storage alone does not give that order in Postgres, whose
-- index entries are not ordered by the row's id.
CREATE INDEX scans_by_storage_scan ON scans (storage, id);
