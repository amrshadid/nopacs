-- A job may now be of the kind deid: archived instances judged against a
-- de-identification profile version, changing nothing. Such a job names no
-- sender (''), a mode (dry_run or check) and, for a dry run, the profile
-- version it runs ('' and 0 for each instance's own); a send job keeps
-- '' and 0 in each, which every row before takes. What a deid job found is
-- counted per key in job_tallies, added in the transaction of each
-- progress write. Only a check is widened and columns added with their
-- defaults, so the migration is safe on a database that holds data.
ALTER TABLE jobs DROP CONSTRAINT jobs_kind_check;
ALTER TABLE jobs ADD CONSTRAINT jobs_kind_check CHECK (kind IN ('send', 'deid'));
ALTER TABLE jobs ADD COLUMN deid_mode text COLLATE "C" NOT NULL DEFAULT ''
    CONSTRAINT jobs_deid_mode_check CHECK (deid_mode IN ('', 'dry_run', 'check'));
ALTER TABLE jobs ADD COLUMN profile_name text COLLATE "C" NOT NULL DEFAULT '';
ALTER TABLE jobs ADD COLUMN profile_version integer NOT NULL DEFAULT 0
    CONSTRAINT jobs_profile_version_check CHECK (profile_version >= 0);
ALTER TABLE jobs ADD CONSTRAINT jobs_deid_kind_check CHECK ((kind = 'deid') = (deid_mode <> ''));

-- A deid job's counts per key (the contract's JobTally): at most 1000 keys
-- a job, and the key 'other' for those past it.
CREATE TABLE job_tallies (
    job_id text   COLLATE "C" NOT NULL,
    tally  text   COLLATE "C" NOT NULL,
    count  bigint NOT NULL,
    CONSTRAINT job_tallies_pkey PRIMARY KEY (job_id, tally),
    CONSTRAINT job_tallies_count_check CHECK (count >= 0)
);
