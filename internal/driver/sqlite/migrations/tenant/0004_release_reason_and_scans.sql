-- The reason a person gave for a release, empty when none was given: a
-- release is a decision, and its reason is kept with the record as a
-- rejection's is.
ALTER TABLE quarantine ADD COLUMN release_reason TEXT NOT NULL DEFAULT '';

-- The instances a scan holds, in order of SOP Instance UID: the catalog's
-- own list of a scan, which a reception reads to write the scan's manifest
-- and to check that no instance of another reception is missing from it.
CREATE INDEX instances_by_scan ON instances (scan_id, sop_instance_uid) WHERE state = 'held';
