-- What the catalog's reads need: the values a scan's files say of it and of
-- its session (study date, modality, series description, series number),
-- the receiver that first archived into each, running totals on subjects
-- and sessions, and an index for each combination of filter and order a
-- list answers, so every page is a range of one index read in its order.
--
-- A list reads one table, never a join (sqlkit's catalog reads say why):
-- so a session keeps its subject's label and folded label, and a scan
-- those of its subject and session and its session's storage. None of them
-- ever changes once given.
--
-- Running totals, like the catalog's own (catalog_totals), are changed in
-- the transaction of every commit, so they equal a count of the instances
-- after each. updated is when a file was last archived into the row.
--
-- The rules of 0002_pipeline.sql hold. Text not known is ''; a series
-- number not known is NULL.

ALTER TABLE subjects ADD COLUMN session_count  INTEGER NOT NULL DEFAULT 0 CHECK (session_count >= 0);
ALTER TABLE subjects ADD COLUMN instance_count INTEGER NOT NULL DEFAULT 0 CHECK (instance_count >= 0);
ALTER TABLE subjects ADD COLUMN bytes          INTEGER NOT NULL DEFAULT 0 CHECK (bytes >= 0);
ALTER TABLE subjects ADD COLUMN updated        INTEGER NOT NULL DEFAULT 0;

ALTER TABLE sessions ADD COLUMN subject_label  TEXT    NOT NULL DEFAULT '';
ALTER TABLE sessions ADD COLUMN subject_folded TEXT    NOT NULL DEFAULT '';
ALTER TABLE sessions ADD COLUMN study_date     TEXT    NOT NULL DEFAULT '';
ALTER TABLE sessions ADD COLUMN receiver       TEXT    NOT NULL DEFAULT '';
ALTER TABLE sessions ADD COLUMN scan_count     INTEGER NOT NULL DEFAULT 0 CHECK (scan_count >= 0);
ALTER TABLE sessions ADD COLUMN instance_count INTEGER NOT NULL DEFAULT 0 CHECK (instance_count >= 0);
ALTER TABLE sessions ADD COLUMN bytes          INTEGER NOT NULL DEFAULT 0 CHECK (bytes >= 0);
ALTER TABLE sessions ADD COLUMN updated        INTEGER NOT NULL DEFAULT 0;

ALTER TABLE scans ADD COLUMN subject_label      TEXT NOT NULL DEFAULT '';
ALTER TABLE scans ADD COLUMN subject_folded     TEXT NOT NULL DEFAULT '';
ALTER TABLE scans ADD COLUMN session_label      TEXT NOT NULL DEFAULT '';
ALTER TABLE scans ADD COLUMN session_folded     TEXT NOT NULL DEFAULT '';
ALTER TABLE scans ADD COLUMN storage            TEXT NOT NULL DEFAULT '';
ALTER TABLE scans ADD COLUMN modality           TEXT NOT NULL DEFAULT '';
ALTER TABLE scans ADD COLUMN series_description TEXT NOT NULL DEFAULT '';
ALTER TABLE scans ADD COLUMN description_folded TEXT NOT NULL DEFAULT '';
ALTER TABLE scans ADD COLUMN series_number      INTEGER;
ALTER TABLE scans ADD COLUMN receiver           TEXT NOT NULL DEFAULT '';

-- The modalities of each session's scans, each once, with the session's
-- creation time, so the sessions of a modality are listed newest first
-- from this index alone.
CREATE TABLE session_modalities (
    session_id INTEGER NOT NULL,
    modality   TEXT    NOT NULL,
    created    INTEGER NOT NULL,
    PRIMARY KEY (session_id, modality)
) STRICT, WITHOUT ROWID;

-- What the catalog holds already, filled in. The values of the files'
-- headers are not in the database, so they stay unknown for rows made
-- before this version, until a later commit into the row gives them. The
-- receiver is that of the reception that confirmed the row's first held
-- instance.
UPDATE scans SET
    subject_label  = (SELECT sub.label FROM sessions AS se JOIN subjects AS sub ON sub.id = se.subject_id WHERE se.id = scans.session_id),
    subject_folded = (SELECT sub.folded FROM sessions AS se JOIN subjects AS sub ON sub.id = se.subject_id WHERE se.id = scans.session_id),
    session_label  = (SELECT se.label FROM sessions AS se WHERE se.id = scans.session_id),
    session_folded = (SELECT se.folded FROM sessions AS se WHERE se.id = scans.session_id),
    storage        = (SELECT se.storage FROM sessions AS se WHERE se.id = scans.session_id),
    receiver       = coalesce((SELECT p.receiver FROM instances AS i JOIN receptions AS p ON p.id = i.reception_id
                               WHERE i.scan_id = scans.id AND i.state = 'held' ORDER BY i.held_at, i.sop_instance_uid LIMIT 1), '');

UPDATE sessions SET
    subject_label  = (SELECT label FROM subjects WHERE id = sessions.subject_id),
    subject_folded = (SELECT folded FROM subjects WHERE id = sessions.subject_id),
    receiver       = coalesce((SELECT receiver FROM scans WHERE session_id = sessions.id AND receiver <> '' ORDER BY created, id LIMIT 1), ''),
    scan_count     = (SELECT count(*) FROM scans WHERE session_id = sessions.id),
    instance_count = (SELECT coalesce(sum(instances), 0) FROM scans WHERE session_id = sessions.id),
    bytes          = (SELECT coalesce(sum(bytes), 0) FROM scans WHERE session_id = sessions.id),
    updated        = max(created, (SELECT coalesce(max(updated), 0) FROM scans WHERE session_id = sessions.id));

UPDATE subjects SET
    session_count  = (SELECT count(*) FROM sessions WHERE subject_id = subjects.id),
    instance_count = (SELECT coalesce(sum(instance_count), 0) FROM sessions WHERE subject_id = subjects.id),
    bytes          = (SELECT coalesce(sum(bytes), 0) FROM sessions WHERE subject_id = subjects.id),
    updated        = max(created, (SELECT coalesce(max(updated), 0) FROM sessions WHERE subject_id = subjects.id));

-- The lists and the index each reads, in the list's order (sqlkit's
-- catalog reads build the statements):
--   subjects newest first, with a time range or none   subjects_by_created
--   subjects by label                                  the unique folded
--   sessions newest first                              sessions_by_created
--     of a subject                                     sessions_by_subject_created
--     of a receiver                                    sessions_by_receiver
--     of a modality                                    session_modalities_by_modality
--   sessions by label                                  sessions_by_label
--     of a subject                                     the unique (subject_id, folded)
--   sessions by study date                             sessions_by_study_date
--     of a subject                                     sessions_by_subject_study_date
--   scans newest first                                 scans_by_created
--     of a session                                     scans_by_session_created
--     of a modality                                    scans_by_modality
--     of a receiver                                    scans_by_receiver
--   scans by label, of all or of a subject             scans_by_label
--     of a session                                     the unique (session_id, folded)
--   scans by series description                        scans_by_description
-- A session and a scan got by their labels are found by sessions_by_label
-- and scans_by_label, unique as the labels are at each level; a scan's
-- instances by instances_by_scan (0004).
CREATE INDEX subjects_by_created ON subjects (created, id);
CREATE INDEX sessions_by_created ON sessions (created, id);
CREATE INDEX sessions_by_subject_created ON sessions (subject_id, created, id);
CREATE INDEX sessions_by_receiver ON sessions (receiver, created, id);
CREATE INDEX sessions_by_study_date ON sessions (study_date, id);
CREATE INDEX sessions_by_subject_study_date ON sessions (subject_id, study_date, id);
CREATE UNIQUE INDEX sessions_by_label ON sessions (subject_folded, folded);
CREATE INDEX session_modalities_by_modality ON session_modalities (modality, created, session_id);
CREATE INDEX scans_by_created ON scans (created, id);
CREATE INDEX scans_by_session_created ON scans (session_id, created, id);
CREATE INDEX scans_by_modality ON scans (modality, created, id);
CREATE INDEX scans_by_receiver ON scans (receiver, created, id);
CREATE INDEX scans_by_description ON scans (description_folded, id);
CREATE UNIQUE INDEX scans_by_label ON scans (subject_folded, session_folded, folded);

-- The scans of a session are read in an order (newest first, or by label),
-- each from its own index that begins with the session; an index on the
-- session alone would only tempt a planner to read it and sort.
DROP INDEX scans_by_session;
