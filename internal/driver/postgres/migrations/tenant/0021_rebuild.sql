-- A job may now be of the kind rebuild: the catalog made again from what
-- the tenant's storages hold (their correction records, manifests and
-- metadata), into a shadow catalog, tables of the same shape in a scratch
-- schema of this database the job names, never into the catalog in use. In
-- compare mode the shadow is compared with the catalog in use and dropped;
-- in replace mode one transaction makes it the catalog in use. What a
-- rebuild job does is a row of rebuild_jobs; how far its build got is kept
-- in the shadow itself, written in the transaction that writes each scan's
-- rows there, so a crash or a lost lease goes on from the last scan
-- written.
--
-- The tenant's event feed now has a history id: a random id, made with the
-- database and made again whenever a rebuild replaces the catalog, so a
-- consumer that keys what it read by (history, cursor) knows the feed it
-- follows began again from a new catalog.
--
-- Only a check is widened, and tables added, so the migration is safe on a
-- database that holds data.
ALTER TABLE jobs DROP CONSTRAINT jobs_kind_check;
ALTER TABLE jobs ADD CONSTRAINT jobs_kind_check CHECK (kind IN ('send', 'deid', 'import', 'scrub', 'rebuild'));

-- What a rebuild job does: its mode, and the scratch schema its shadow is
-- built in. activated is set by the transaction that made its shadow the
-- catalog in use, and never changes after. dropped is set once its shadow
-- is removed, so a shadow a job left when it ended without its worker (its
-- attempts ran out, or it was canceled while queued) is found and removed.
CREATE TABLE rebuild_jobs (
    job_id    text    COLLATE "C" NOT NULL,
    mode      text    COLLATE "C" NOT NULL CHECK (mode IN ('compare', 'replace')),
    scratch   text    COLLATE "C" NOT NULL,
    activated boolean NOT NULL,
    dropped   boolean NOT NULL,
    CONSTRAINT rebuild_jobs_pkey PRIMARY KEY (job_id)
);

-- The differences a compare job found, by name, the first 1000 in the
-- order the comparison found them, kept with the job so an admin reads
-- which rows differ after its shadow is gone; the job's tallies count
-- them all. row_key names the row by its label or UIDs, joined by "/".
CREATE TABLE rebuild_differences (
    job_id     text    COLLATE "C" NOT NULL,
    seq        integer NOT NULL CHECK (seq >= 0),
    table_name text    COLLATE "C" NOT NULL,
    kind       text    COLLATE "C" NOT NULL CHECK (kind IN ('added', 'removed', 'changed')),
    row_key    text    COLLATE "C" NOT NULL,
    CONSTRAINT rebuild_differences_pkey PRIMARY KEY (job_id, seq)
);

-- The event feed's history: one row, its id 32 lowercase hexadecimal
-- digits of a random value, and since when it holds.
CREATE TABLE event_history (
    only_row boolean     NOT NULL DEFAULT true,
    id       text        COLLATE "C" NOT NULL CHECK (length(id) = 32),
    started  timestamptz NOT NULL,
    CONSTRAINT event_history_pkey PRIMARY KEY (only_row),
    CONSTRAINT event_history_one_row CHECK (only_row)
);
INSERT INTO event_history (only_row, id, started) VALUES (true, replace(gen_random_uuid()::text, '-', ''), clock_timestamp());
