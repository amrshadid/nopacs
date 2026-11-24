-- The master database's first schema, on Postgres.
--
-- It holds what the SQLite schema holds, in Postgres types: times are
-- timestamptz, which keeps microseconds exactly and is read back in UTC by
-- the driver; byte values are bytea; the audit entry's details are jsonb.
-- Every table is created in the schema the connection's search_path starts
-- with, so the driver works with a login that owns only that schema, and
-- never makes a database or a role.
--
-- Text that is compared or ordered (ids, names, scopes) is COLLATE "C", that
-- is, compared byte by byte, so that a name sorts the same on every
-- database. Without it a column takes the database's own default collation,
-- and a database created with en_US.UTF-8 would order "Zeta" after "alpha"
-- and ignore punctuation, so the same keyset cursor would page through a
-- different order than it does on SQLite or on a database made with C. The
-- collation is on the column, so the index on it uses it too.
--
-- Text a caller did not give is '' rather than NULL, so a comparison never
-- meets NULL's rules.
--
-- An applied migration is never changed (the runner refuses a database whose
-- recorded checksum differs): a change to this schema is a new file.

CREATE TABLE tenants (
    id           text        COLLATE "C" NOT NULL,
    name         text        COLLATE "C" NOT NULL,
    driver       text        NOT NULL,
    address      bytea       NOT NULL,
    state        text        NOT NULL,
    state_reason text        NOT NULL,
    created      timestamptz NOT NULL,
    CONSTRAINT tenants_pkey PRIMARY KEY (id),
    -- A named, unique constraint makes the name's uniqueness the database's
    -- to keep, so two creations racing in two processes cannot both
    -- succeed, and the driver can tell a taken name from a taken id by the
    -- constraint's name, never by the text of a message. Its index also
    -- serves the lookup by name and the list in order of name.
    CONSTRAINT tenants_name_key UNIQUE (name),
    CONSTRAINT tenants_state_check CHECK (state IN ('ready', 'unreachable', 'upgrading', 'failed'))
);

CREATE TABLE api_keys (
    id        text        COLLATE "C" NOT NULL,
    hash      bytea       NOT NULL,
    holder    text        NOT NULL,
    created   timestamptz NOT NULL,
    expires   timestamptz,
    last_used timestamptz,
    revoked   timestamptz,
    CONSTRAINT api_keys_pkey PRIMARY KEY (id),
    CONSTRAINT api_keys_hash_check CHECK (octet_length(hash) = 32)
);

-- The list of keys in order of creation, then id.
CREATE INDEX api_keys_by_created ON api_keys (created, id);

-- scope is '' for the installation and the tenant's id otherwise, so the
-- primary key (one grant per key and scope, so setting a grant again replaces
-- its role instead of adding a second) holds for the installation too: a NULL
-- would be distinct from every other NULL. tenant_id repeats the scope as
-- NULL or the id, so the foreign key can check it.
CREATE TABLE grants (
    key_id    text COLLATE "C" NOT NULL REFERENCES api_keys (id),
    scope     text COLLATE "C" NOT NULL,
    tenant_id text COLLATE "C" REFERENCES tenants (id),
    role      text NOT NULL,
    CONSTRAINT grants_pkey PRIMARY KEY (key_id, scope),
    CONSTRAINT grants_role_check CHECK (role IN ('reader', 'writer', 'operator', 'admin')),
    CONSTRAINT grants_scope_check CHECK ((scope = '' AND tenant_id IS NULL) OR (scope <> '' AND tenant_id = scope))
);

-- The list of grants on one scope, in order of key id. The primary key
-- serves the list by key.
CREATE INDEX grants_by_scope ON grants (scope, key_id);

-- The foreign key's own column. Postgres does not index the referencing side
-- of a foreign key, so without this every removal of a tenant would read
-- every grant to check it.
CREATE INDEX grants_by_tenant ON grants (tenant_id);

CREATE TABLE settings (
    name  text COLLATE "C" NOT NULL,
    value text NOT NULL,
    CONSTRAINT settings_pkey PRIMARY KEY (name)
);

-- The audit log. It has no foreign key: an entry outlives the tenant and the
-- key it names, and records requests that named ones that never existed.
-- details is a JSON object of strings, read back whole. It is jsonb, not
-- text, so that an operator with psql can read one field of it
-- (details ->> 'old_role') without a cast; jsonb keeps no key order and no
-- spacing, which the driver, reading it into a map, does not need.
CREATE TABLE audit (
    id          text        COLLATE "C" NOT NULL,
    at          timestamptz NOT NULL,
    key_id      text        COLLATE "C" NOT NULL,
    local_user  text        NOT NULL,
    remote_addr text        NOT NULL,
    method      text        NOT NULL,
    tenant_name text        COLLATE "C" NOT NULL,
    object      text        NOT NULL,
    outcome     text        NOT NULL,
    reason      text        NOT NULL,
    details     jsonb       NOT NULL,
    request_id  text        NOT NULL,
    CONSTRAINT audit_pkey PRIMARY KEY (id),
    CONSTRAINT audit_outcome_check CHECK (outcome IN ('done', 'refused', 'failed')),
    CONSTRAINT audit_details_check CHECK (jsonb_typeof(details) = 'object')
);

-- The listings that are indexed, newest first: everything, by time; one
-- tenant's entries, by time; one key's entries, by time; one outcome's
-- entries, by time, so that a rare outcome ("failed") is found without
-- reading every entry. A start and an end time narrow the range read from
-- whichever index is used. Two equality filters together (a tenant and a
-- key, or either with an outcome) use the index of one of them, chosen by
-- the planner, and test the other on the rows that index yields.
CREATE INDEX audit_by_time    ON audit (at, id);
CREATE INDEX audit_by_tenant  ON audit (tenant_name, at, id);
CREATE INDEX audit_by_key     ON audit (key_id, at, id);
CREATE INDEX audit_by_outcome ON audit (outcome, at, id);
