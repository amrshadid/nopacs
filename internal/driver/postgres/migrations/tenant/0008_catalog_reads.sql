-- What the catalog's reads need, on Postgres: the values a scan's files say
-- of it and of its session (study date, modality, series description,
-- series number), the receiver that first archived into each, running
-- totals on subjects and sessions, and an index for each combination of
-- filter and order a list answers, so every page is a range of one index
-- read in its order: no Sort and no join, whatever the size of the tables.
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
-- The rules of 0002_pipeline.sql hold: text that is compared, ordered or
-- unique is COLLATE "C", so order is byte by byte as SQLite's is. Text not
-- known is ''; a series number not known is NULL. Adding a column with a
-- constant default rewrites no row.

ALTER TABLE subjects
    ADD COLUMN session_count  bigint      NOT NULL DEFAULT 0,
    ADD COLUMN instance_count bigint      NOT NULL DEFAULT 0,
    ADD COLUMN bytes          bigint      NOT NULL DEFAULT 0,
    ADD COLUMN updated        timestamptz NOT NULL DEFAULT 'epoch',
    ADD CONSTRAINT subjects_totals_check CHECK (session_count >= 0 AND instance_count >= 0 AND bytes >= 0);

ALTER TABLE sessions
    ADD COLUMN subject_label  text        COLLATE "C" NOT NULL DEFAULT '',
    ADD COLUMN subject_folded text        COLLATE "C" NOT NULL DEFAULT '',
    ADD COLUMN study_date     text        COLLATE "C" NOT NULL DEFAULT '',
    ADD COLUMN receiver       text        COLLATE "C" NOT NULL DEFAULT '',
    ADD COLUMN scan_count     bigint      NOT NULL DEFAULT 0,
    ADD COLUMN instance_count bigint      NOT NULL DEFAULT 0,
    ADD COLUMN bytes          bigint      NOT NULL DEFAULT 0,
    ADD COLUMN updated        timestamptz NOT NULL DEFAULT 'epoch',
    ADD CONSTRAINT sessions_totals_check CHECK (scan_count >= 0 AND instance_count >= 0 AND bytes >= 0);

ALTER TABLE scans
    ADD COLUMN subject_label      text    COLLATE "C" NOT NULL DEFAULT '',
    ADD COLUMN subject_folded     text    COLLATE "C" NOT NULL DEFAULT '',
    ADD COLUMN session_label      text    COLLATE "C" NOT NULL DEFAULT '',
    ADD COLUMN session_folded     text    COLLATE "C" NOT NULL DEFAULT '',
    ADD COLUMN storage            text    COLLATE "C" NOT NULL DEFAULT '',
    ADD COLUMN modality           text    COLLATE "C" NOT NULL DEFAULT '',
    ADD COLUMN series_description text    NOT NULL DEFAULT '',
    ADD COLUMN description_folded text    COLLATE "C" NOT NULL DEFAULT '',
    ADD COLUMN series_number      integer,
    ADD COLUMN receiver           text    COLLATE "C" NOT NULL DEFAULT '';

-- The modalities of each session's scans, each once, with the session's
-- creation time, so the sessions of a modality are listed newest first
-- from this index alone.
CREATE TABLE session_modalities (
    session_id bigint      NOT NULL,
    modality   text        COLLATE "C" NOT NULL,
    created    timestamptz NOT NULL,
    CONSTRAINT session_modalities_pkey PRIMARY KEY (session_id, modality)
);

-- What the catalog holds already, filled in. The values of the files'
-- headers are not in the database, so they stay unknown for rows made
-- before this version, until a later commit into the row gives them. The
-- receiver is that of the reception that confirmed the row's first held
-- instance.
UPDATE scans AS c SET
    subject_label  = sub.label,
    subject_folded = sub.folded,
    session_label  = se.label,
    session_folded = se.folded,
    storage        = se.storage,
    receiver       = coalesce((SELECT p.receiver FROM instances AS i JOIN receptions AS p ON p.id = i.reception_id
                               WHERE i.scan_id = c.id AND i.state = 'held' ORDER BY i.held_at, i.sop_instance_uid LIMIT 1), '')
    FROM sessions AS se JOIN subjects AS sub ON sub.id = se.subject_id
    WHERE se.id = c.session_id;

UPDATE sessions AS s SET
    subject_label  = sub.label,
    subject_folded = sub.folded,
    receiver       = coalesce((SELECT c.receiver FROM scans AS c WHERE c.session_id = s.id AND c.receiver <> ''
                               ORDER BY c.created, c.id LIMIT 1), ''),
    scan_count     = (SELECT count(*) FROM scans AS c WHERE c.session_id = s.id),
    instance_count = (SELECT coalesce(sum(c.instances), 0) FROM scans AS c WHERE c.session_id = s.id),
    bytes          = (SELECT coalesce(sum(c.bytes), 0) FROM scans AS c WHERE c.session_id = s.id),
    updated        = greatest(s.created, (SELECT max(c.updated) FROM scans AS c WHERE c.session_id = s.id))
    FROM subjects AS sub
    WHERE sub.id = s.subject_id;

UPDATE subjects AS sub SET
    session_count  = (SELECT count(*) FROM sessions AS s WHERE s.subject_id = sub.id),
    instance_count = (SELECT coalesce(sum(s.instance_count), 0) FROM sessions AS s WHERE s.subject_id = sub.id),
    bytes          = (SELECT coalesce(sum(s.bytes), 0) FROM sessions AS s WHERE s.subject_id = sub.id),
    updated        = greatest(sub.created, (SELECT max(s.updated) FROM sessions AS s WHERE s.subject_id = sub.id));

-- The lists and the index each reads, in the list's order (sqlkit's
-- catalog reads build the statements):
--   subjects newest first, with a time range or none   subjects_by_created
--   subjects by label                                  subjects_folded_key
--   sessions newest first                              sessions_by_created
--     of a subject                                     sessions_by_subject_created
--     of a receiver                                    sessions_by_receiver
--     of a modality                                    session_modalities_by_modality
--   sessions by label                                  sessions_by_label
--     of a subject                                     sessions_folded_key
--   sessions by study date                             sessions_by_study_date
--     of a subject                                     sessions_by_subject_study_date
--   scans newest first                                 scans_by_created
--     of a session                                     scans_by_session_created
--     of a modality                                    scans_by_modality
--     of a receiver                                    scans_by_receiver
--   scans by label, of all or of a subject             scans_by_label
--     of a session                                     scans_folded_key
--   scans by series description                        scans_by_description
-- A session and a scan got by their labels are found by sessions_by_label
-- and scans_by_label, unique as the labels are at each level; a scan's
-- instances by instances_by_scan (0004). A newest-first list reads its
-- index backward.
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
