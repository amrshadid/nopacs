-- The catalog's instances made lean, and a reception's received files kept
-- only while something can still read them, on Postgres. At ten million
-- instances the instance rows took 6.6 GB and their indexes 2.3 GB, and the
-- received files 6.2 GB with their key. A claim was a row of instances and
-- its confirmation an UPDATE of it, which under MVCC writes the row again
-- with an entry in every index (the UPDATE changes indexed columns, so it is
-- never a heap-only update): each instance was written twice, and left a
-- dead row for VACUUM. What changes:
--
--   - A claim is a row of instance_claims, in flight only: written by the
--     claim, removed by the commit that confirms it or the step that ends
--     its reception. A held instance is a row of instances, inserted once by
--     its commit and never updated. A UID is claimed or held, never both: a
--     claim is made only when neither table holds the UID, and checked
--     against instances again after its insert (a commit of another
--     reception's claim may have moved it meanwhile); a commit moves a claim
--     of its own reception into instances in one statement. The claim keeps
--     the study and series it was made in, which the commit checks; a held
--     instance is in its scan, which holds them (scans gain study_uid).
--   - storage_key is '' for the layout's key of the instance's scan and UID,
--     the only key a commit is given (ScanCommit.Validate), so a read makes
--     it again from the scan's labels; a key kept from before is kept as it
--     was. archived_sha256 is NULL when it equals sha256, as it does for a
--     file archived as it was received. The storage is the scan's.
--   - The scans a reception confirmed instances in are rows of
--     reception_scans with running totals, in place of an index of every
--     instance by its reception (instances_held_by); whether a storage
--     holds an instance is whether a scan on it holds one
--     (scans_by_storage), in place of instances_on_storage.
--   - The received files of a reception that ended done or quarantined are
--     removed by the step that ends it: each file is then an instance held,
--     a duplicate of one, or a quarantine record that keeps its own copy of
--     the row, and nothing reads them again (a restart reads those of
--     receptions in progress and failed ones). A failed reception keeps its
--     files, for its retry or its discard.
--
-- The rules of 0002_pipeline.sql hold.

-- An instance's own storage is dropped and its scan's is read instead, which
-- is the same only if no held instance is on another storage than its
-- scan. Stop here, with nothing changed, if one is.
DO $guard$
BEGIN
    IF EXISTS (SELECT 1 FROM instances AS i JOIN scans AS s ON s.id = i.scan_id
            WHERE i.state = 'held' AND i.storage <> s.storage) THEN
        RAISE EXCEPTION 'migration 0009 cannot run: a held instance is on a different storage than its scan, and an instance no longer keeps its own';
    END IF;
END
$guard$;

CREATE TABLE instance_claims (
    sop_instance_uid text        COLLATE "C" NOT NULL,
    reception_id     text        COLLATE "C" NOT NULL,
    study_uid        text        COLLATE "C" NOT NULL,
    series_uid       text        COLLATE "C" NOT NULL,
    sha256           bytea       NOT NULL,
    claimed_at       timestamptz NOT NULL,
    CONSTRAINT instance_claims_pkey PRIMARY KEY (sop_instance_uid),
    CONSTRAINT instance_claims_reception_check CHECK (reception_id <> ''),
    CONSTRAINT instance_claims_sha256_check CHECK (octet_length(sha256) = 32)
);

-- A reception's claims, to release them when it ends.
CREATE INDEX instance_claims_by_reception ON instance_claims (reception_id);

-- The claims, and now the received files, are tables of rows in flight:
-- written, then removed, in their thousands a minute. Postgres's default
-- autovacuum waits for a fifth of a table to change, so a table that was
-- empty when last analyzed is planned as empty while it fills: a batch's
-- lookup of 1000 claims among 60,000 then read every claim (4 ms) where
-- the key reads none (0.2 ms). Each is vacuumed and analyzed after a fixed
-- number of rows changed instead.
ALTER TABLE instance_claims SET (autovacuum_vacuum_scale_factor = 0, autovacuum_vacuum_threshold = 10000,
    autovacuum_analyze_scale_factor = 0, autovacuum_analyze_threshold = 10000);

INSERT INTO instance_claims (sop_instance_uid, reception_id, study_uid, series_uid, sha256, claimed_at)
    SELECT sop_instance_uid, reception_id, study_uid, series_uid, sha256, claimed_at FROM instances WHERE state = 'claimed';

ALTER TABLE scans ADD COLUMN study_uid text COLLATE "C" NOT NULL DEFAULT '';
UPDATE scans AS s SET study_uid = se.study_uid FROM sessions AS se WHERE se.id = s.session_id;

CREATE TABLE held_instances (
    sop_instance_uid text        COLLATE "C" NOT NULL,
    scan_id          bigint      NOT NULL,
    reception_id     text        COLLATE "C" NOT NULL,
    sha256           bytea       NOT NULL,
    archived_sha256  bytea,
    storage_key      text        COLLATE "C" NOT NULL,
    byte_offset      bigint      NOT NULL,
    byte_length      bigint      NOT NULL,
    size             bigint      NOT NULL,
    held_at          timestamptz NOT NULL
);

INSERT INTO held_instances (sop_instance_uid, scan_id, reception_id, sha256, archived_sha256, storage_key, byte_offset,
        byte_length, size, held_at)
    SELECT i.sop_instance_uid, i.scan_id, i.reception_id, i.sha256,
        CASE WHEN i.archived_sha256 = i.sha256 THEN NULL ELSE i.archived_sha256 END,
        CASE WHEN i.storage_key = 'data/' || s.subject_label || '/' || s.session_label || '/' || s.label || '/'
            || i.sop_instance_uid || '.dcm' THEN '' ELSE i.storage_key END,
        i.byte_offset, i.byte_length, i.size, i.held_at
    FROM instances AS i JOIN scans AS s ON s.id = i.scan_id WHERE i.state = 'held'
    ORDER BY i.scan_id, i.sop_instance_uid;

CREATE TABLE reception_scans (
    reception_id text   COLLATE "C" NOT NULL,
    scan_id      bigint NOT NULL,
    instances    bigint NOT NULL,
    bytes        bigint NOT NULL,
    CONSTRAINT reception_scans_pkey PRIMARY KEY (reception_id, scan_id),
    CONSTRAINT reception_scans_counts_check CHECK (instances >= 0 AND bytes >= 0)
);

INSERT INTO reception_scans (reception_id, scan_id, instances, bytes)
    SELECT reception_id, scan_id, count(*), sum(size) FROM held_instances GROUP BY reception_id, scan_id;

DROP TABLE instances;
ALTER TABLE held_instances RENAME TO instances;
ALTER TABLE instances
    ADD CONSTRAINT instances_pkey PRIMARY KEY (sop_instance_uid),
    ADD CONSTRAINT instances_sha256_check CHECK (octet_length(sha256) = 32),
    ADD CONSTRAINT instances_archived_check CHECK (archived_sha256 IS NULL OR octet_length(archived_sha256) = 32),
    ADD CONSTRAINT instances_reception_check CHECK (reception_id <> ''),
    ADD CONSTRAINT instances_place_check CHECK (byte_offset >= 0 AND byte_length >= 0 AND size >= 0);

-- A scan's instances in order of SOP Instance UID.
CREATE INDEX instances_by_scan ON instances (scan_id, sop_instance_uid);

-- On the storage alone: the read keeps the test of instances > 0 as a filter
-- (the first scan of a storage nearly always passes it). An index that also
-- held instances would change on every commit that adds to a scan, and an
-- UPDATE of an indexed column is never heap-only, so every index of scans
-- (nine) would be written again for each commit.
CREATE INDEX scans_by_storage ON scans (storage);

-- A study's instances are read scan by scan in order of id, then of SOP
-- Instance UID: this index gives the scans in that order and each scan's
-- instances are a range in that order, so a page is read without a sort
-- and stops at its limit. Read through sessions and the scans of each
-- session by creation, the first page of a large study sorted every
-- instance of it (p99 23 ms on Postgres, 71 ms on SQLite at ten million).
-- It serves a study's instances (DICOMweb's retrieval of a study and the
-- search for the instances of a study) and costs a row per scan, written
-- once when the scan is made: a scan's study never changes, so no commit
-- writes this index again.
CREATE INDEX scans_by_study ON scans (study_uid, id);

DELETE FROM received_files WHERE reception_id IN (SELECT id FROM receptions WHERE state IN ('done', 'quarantined'));
ALTER TABLE received_files SET (autovacuum_vacuum_scale_factor = 0, autovacuum_vacuum_threshold = 10000,
    autovacuum_analyze_scale_factor = 0, autovacuum_analyze_threshold = 10000);
