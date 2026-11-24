-- The catalog's instances made lean, and a reception's received files kept
-- only while something can still read them. At ten million instances the
-- instance rows and their indexes took about 7 GB and the received files
-- about 6 GB of a 13 GB file, about 1.3 KB an instance; and a commit of a
-- thousand instances dirtied a page of each table and index keyed by the
-- SOP Instance UID for each instance (random keys), twice over, as the
-- claim and the confirmation wrote one row each time. What changes:
--
--   - A claim is a row of instance_claims, in flight only: written by the
--     claim, removed by the commit that confirms it or the step that ends
--     its reception. A held instance is a row of instances, written once by
--     its commit. A UID is claimed or held, never both: a claim is made
--     only when no row of either table holds the UID, and a commit moves a
--     claim of its own reception into instances in one transaction. The
--     claim keeps the study and series it was made in, which a commit
--     checks; a held instance is in its scan, which holds them (scans gain
--     study_uid, so one row answers both).
--   - instances is keyed by (scan_id, sop_instance_uid), so a scan's rows
--     are one range of the table, written at its end as scans are made and
--     read in order without a lookup per row; instances_by_uid holds the
--     uniqueness of a SOP Instance UID in the tenant and finds a row by it.
--   - storage_key is '' for the layout's key of the instance's scan and UID,
--     the only key a commit is given (ScanCommit.Validate), so a read makes
--     it again from the scan's labels; a key kept from before is kept as it
--     was. archived_sha256 is NULL when it equals sha256, as it does for a
--     file archived as it was received. The storage is the scan's.
--   - The scans a reception confirmed instances in are rows of
--     reception_scans with running totals, in place of an index of every
--     instance by its reception (instances_held_by, about 1 GB at ten
--     million); whether a storage holds an instance is whether a scan on it
--     holds one (scans_by_storage), in place of instances_on_storage.
--   - received_by_instance went unused once the duplicate check read the
--     catalog; it is dropped. The received files of a reception that ended
--     done or quarantined are removed by the step that ends it: each file
--     is then an instance held, a duplicate of one, or a quarantine record
--     that keeps its own copy of the row, and nothing reads them again (a
--     restart reads those of receptions in progress and failed ones). A
--     failed reception keeps its files, for its retry or its discard.
--
-- The rules of 0002_pipeline.sql hold.

-- An instance's own storage is dropped and its scan's is read instead, which
-- is the same only if no held instance is on another storage than its
-- scan. SQLite has no statement that fails on a condition, so a temporary
-- trigger raises the error when a row is inserted into a temporary table:
-- the migration stops, and its transaction leaves nothing changed.
CREATE TEMP TABLE migration_guard (x INTEGER);
CREATE TEMP TRIGGER migration_guard_storage BEFORE INSERT ON migration_guard
WHEN EXISTS (SELECT 1 FROM main.instances AS i JOIN main.scans AS s ON s.id = i.scan_id
        WHERE i.state = 'held' AND i.storage <> s.storage)
BEGIN
    SELECT RAISE(ABORT, 'migration 0009 cannot run: a held instance is on a different storage than its scan, and an instance no longer keeps its own');
END;
INSERT INTO migration_guard VALUES (1);
DROP TABLE migration_guard;

CREATE TABLE instance_claims (
    sop_instance_uid TEXT    NOT NULL PRIMARY KEY,
    reception_id     TEXT    NOT NULL CHECK (reception_id <> ''),
    study_uid        TEXT    NOT NULL,
    series_uid       TEXT    NOT NULL,
    sha256           BLOB    NOT NULL CHECK (length(sha256) = 32),
    claimed_at       INTEGER NOT NULL
) STRICT, WITHOUT ROWID;

-- A reception's claims, to release them when it ends.
CREATE INDEX instance_claims_by_reception ON instance_claims (reception_id);

INSERT INTO instance_claims (sop_instance_uid, reception_id, study_uid, series_uid, sha256, claimed_at)
    SELECT sop_instance_uid, reception_id, study_uid, series_uid, sha256, claimed_at FROM instances WHERE state = 'claimed';

ALTER TABLE scans ADD COLUMN study_uid TEXT NOT NULL DEFAULT '';
UPDATE scans SET study_uid = (SELECT study_uid FROM sessions WHERE id = scans.session_id);

CREATE TABLE held_instances (
    scan_id          INTEGER NOT NULL,
    sop_instance_uid TEXT    NOT NULL,
    reception_id     TEXT    NOT NULL CHECK (reception_id <> ''),
    sha256           BLOB    NOT NULL CHECK (length(sha256) = 32),
    archived_sha256  BLOB CHECK (archived_sha256 IS NULL OR length(archived_sha256) = 32),
    storage_key      TEXT    NOT NULL,
    byte_offset      INTEGER NOT NULL CHECK (byte_offset >= 0),
    byte_length      INTEGER NOT NULL CHECK (byte_length >= 0),
    size             INTEGER NOT NULL CHECK (size >= 0),
    held_at          INTEGER NOT NULL,
    PRIMARY KEY (scan_id, sop_instance_uid)
) STRICT, WITHOUT ROWID;

INSERT INTO held_instances (scan_id, sop_instance_uid, reception_id, sha256, archived_sha256, storage_key, byte_offset,
        byte_length, size, held_at)
    SELECT i.scan_id, i.sop_instance_uid, i.reception_id, i.sha256,
        CASE WHEN i.archived_sha256 = i.sha256 THEN NULL ELSE i.archived_sha256 END,
        CASE WHEN i.storage_key = 'data/' || s.subject_label || '/' || s.session_label || '/' || s.label || '/'
            || i.sop_instance_uid || '.dcm' THEN '' ELSE i.storage_key END,
        i.byte_offset, i.byte_length, i.size, i.held_at
    FROM instances AS i JOIN scans AS s ON s.id = i.scan_id WHERE i.state = 'held';

CREATE TABLE reception_scans (
    reception_id TEXT    NOT NULL,
    scan_id      INTEGER NOT NULL,
    instances    INTEGER NOT NULL CHECK (instances >= 0),
    bytes        INTEGER NOT NULL CHECK (bytes >= 0),
    PRIMARY KEY (reception_id, scan_id)
) STRICT, WITHOUT ROWID;

INSERT INTO reception_scans (reception_id, scan_id, instances, bytes)
    SELECT reception_id, scan_id, count(*), sum(size) FROM held_instances GROUP BY reception_id, scan_id;

DROP TABLE instances;
ALTER TABLE held_instances RENAME TO instances;
CREATE UNIQUE INDEX instances_by_uid ON instances (sop_instance_uid);

-- On the storage alone: the read keeps the test of instances > 0 as a filter
-- (the first scan of a storage nearly always passes it), and every commit
-- that adds to a scan no longer rewrites an entry of this index.
CREATE INDEX scans_by_storage ON scans (storage);

-- A study's instances are read scan by scan in order of id, then of SOP
-- Instance UID: this index gives the scans in that order and each scan's
-- instances are a range in that order, so a page is read without a sort
-- and stops at its limit. Read through sessions and the scans of each
-- session by creation, the first page of a large study sorted every
-- instance of it (p99 23 ms on Postgres, 71 ms on SQLite at ten million).
-- It serves a study's instances (DICOMweb's retrieval of a study and the
-- search for the instances of a study) and costs an entry per scan, written
-- once when the scan is made: a scan's study never changes.
CREATE INDEX scans_by_study ON scans (study_uid, id);

DROP INDEX received_by_instance;
DELETE FROM received_files WHERE reception_id IN (SELECT id FROM receptions WHERE state IN ('done', 'quarantined'));
