-- What a tenant keeps to de-identify what it receives: the versions of its
-- de-identification profiles, its lookup tables, and, for each instance
-- held, the profile version that de-identified it. Tables are added empty
-- and the instances' column is added NULL, which an instance archived as
-- received keeps, so the migration is safe on a database that holds data
-- and rewrites no row.
--
-- The rules of 0002_pipeline.sql hold: times are microseconds since 1970,
-- text a caller did not give is '' rather than NULL, and there are no
-- foreign keys: every reference is checked by the change that makes it.

-- A profile version, never changed once written: its document and its
-- expanded form as the exact bytes their checksum covers (BLOB, so nothing
-- is normalized), the checksum of the expanded form, who made it, when and
-- why. id is what an instance records; name and version are what a person
-- names.
CREATE TABLE profile_versions (
    id             INTEGER PRIMARY KEY,
    name           TEXT    NOT NULL,
    version        INTEGER NOT NULL CHECK (version BETWEEN 1 AND 10000),
    checksum       TEXT    NOT NULL CHECK (length(checksum) = 64),
    note           TEXT    NOT NULL,
    created_by_key TEXT    NOT NULL,
    created        INTEGER NOT NULL,
    source         BLOB    NOT NULL CHECK (length(source) > 0),
    expanded       BLOB    NOT NULL CHECK (length(expanded) > 0),
    UNIQUE (name, version)
) STRICT;

-- A profile and its latest version, written with each version, so a list
-- of profiles reads one row each.
CREATE TABLE profiles (
    name           TEXT    NOT NULL PRIMARY KEY,
    latest_version INTEGER NOT NULL CHECK (latest_version BETWEEN 1 AND 10000),
    latest_created INTEGER NOT NULL
) STRICT;

-- A lookup table's last code assigned in order; its row is the lock under
-- which the table's codes are assigned.
CREATE TABLE lookup_counters (
    table_name TEXT    NOT NULL PRIMARY KEY,
    last_code  INTEGER NOT NULL CHECK (last_code >= 0)
) STRICT;

-- A lookup table's codes, by the keyed hash of the value each replaces,
-- never the value. A code is given to one value of a table.
CREATE TABLE lookup_codes (
    table_name TEXT    NOT NULL,
    lookup_key TEXT    NOT NULL,
    code       TEXT    NOT NULL,
    assigned   INTEGER NOT NULL,
    PRIMARY KEY (table_name, lookup_key)
) STRICT, WITHOUT ROWID;

CREATE UNIQUE INDEX lookup_codes_by_code ON lookup_codes (table_name, code);

-- The profile version that de-identified a held instance (an id of
-- profile_versions), NULL for one archived as received.
ALTER TABLE instances ADD COLUMN profile_version_id INTEGER;
