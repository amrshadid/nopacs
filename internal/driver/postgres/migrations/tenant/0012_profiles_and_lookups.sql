-- What a tenant keeps to de-identify what it receives: the versions of its
-- de-identification profiles, its lookup tables, and, for each instance
-- held, the profile version that de-identified it. Tables are added empty
-- and the instances' column is added NULL without a default, which an
-- instance archived as received keeps, so the migration is safe on a
-- database that holds data and rewrites no row.
--
-- The rules of 0002_pipeline.sql hold: times are timestamptz to the
-- microsecond, text a caller did not give is '' rather than NULL, compared
-- text is COLLATE "C", and there are no foreign keys: every reference is
-- checked by the change that makes it, under the row locks the driver's
-- documentation lists.

-- A profile version, never changed once written: its document and its
-- expanded form as the exact bytes their checksum covers (bytea, never
-- jsonb, which would normalize them), the checksum of the expanded form,
-- who made it, when and why. id is what an instance records; name and
-- version are what a person names.
CREATE TABLE profile_versions (
    id             bigint      GENERATED ALWAYS AS IDENTITY,
    name           text        COLLATE "C" NOT NULL,
    version        integer     NOT NULL,
    checksum       text        COLLATE "C" NOT NULL,
    note           text        NOT NULL,
    created_by_key text        COLLATE "C" NOT NULL,
    created        timestamptz NOT NULL,
    source         bytea       NOT NULL,
    expanded       bytea       NOT NULL,
    CONSTRAINT profile_versions_pkey PRIMARY KEY (id),
    CONSTRAINT profile_versions_by_name UNIQUE (name, version),
    CONSTRAINT profile_versions_check CHECK (version BETWEEN 1 AND 10000 AND length(checksum) = 64
        AND length(source) > 0 AND length(expanded) > 0)
);

-- A profile and its latest version, written with each version, so a list
-- of profiles reads one row each.
CREATE TABLE profiles (
    name           text        COLLATE "C" NOT NULL,
    latest_version integer     NOT NULL,
    latest_created timestamptz NOT NULL,
    CONSTRAINT profiles_pkey PRIMARY KEY (name),
    CONSTRAINT profiles_check CHECK (latest_version BETWEEN 1 AND 10000)
);

-- A lookup table's last code assigned in order; its row is the lock under
-- which the table's codes are assigned.
CREATE TABLE lookup_counters (
    table_name text   COLLATE "C" NOT NULL,
    last_code  bigint NOT NULL,
    CONSTRAINT lookup_counters_pkey PRIMARY KEY (table_name),
    CONSTRAINT lookup_counters_check CHECK (last_code >= 0)
);

-- A lookup table's codes, by the keyed hash of the value each replaces,
-- never the value. A code is given to one value of a table.
CREATE TABLE lookup_codes (
    table_name text        COLLATE "C" NOT NULL,
    lookup_key text        COLLATE "C" NOT NULL,
    code       text        COLLATE "C" NOT NULL,
    assigned   timestamptz NOT NULL,
    CONSTRAINT lookup_codes_pkey PRIMARY KEY (table_name, lookup_key),
    CONSTRAINT lookup_codes_by_code UNIQUE (table_name, code)
);

-- The profile version that de-identified a held instance (an id of
-- profile_versions), NULL for one archived as received. A column added
-- NULL without a default changes only the catalog of the table.
ALTER TABLE instances ADD COLUMN profile_version_id bigint;
