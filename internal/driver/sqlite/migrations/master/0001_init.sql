-- The master database's first schema.
--
-- Every table is STRICT, so a value of the wrong type is refused as Postgres
-- would refuse it, instead of being kept as SQLite otherwise does. Times are
-- INTEGER microseconds since 1970 in UTC, NULL for "never", so they sort,
-- page and compare the same way in every driver. Text a caller did not give
-- is '' rather than NULL, so a comparison never meets NULL's rules.
--
-- An applied migration is never changed (the runner refuses a database whose
-- recorded checksum differs): a change to this schema is a new file.

CREATE TABLE tenants (
    id           TEXT    NOT NULL PRIMARY KEY,
    -- UNIQUE makes the name's uniqueness the database's to keep, so two
    -- creations racing in two processes cannot both succeed. Its index also
    -- serves the lookup by name and the list in order of name.
    name         TEXT    NOT NULL UNIQUE,
    driver       TEXT    NOT NULL,
    address      BLOB    NOT NULL,
    state        TEXT    NOT NULL CHECK (state IN ('ready', 'unreachable', 'upgrading', 'failed')),
    state_reason TEXT    NOT NULL,
    created      INTEGER NOT NULL
) STRICT;

CREATE TABLE api_keys (
    id        TEXT    NOT NULL PRIMARY KEY,
    hash      BLOB    NOT NULL CHECK (length(hash) = 32),
    holder    TEXT    NOT NULL,
    created   INTEGER NOT NULL,
    expires   INTEGER,
    last_used INTEGER,
    revoked   INTEGER
) STRICT;

-- The list of keys in order of creation, then id.
CREATE INDEX api_keys_by_created ON api_keys (created, id);

-- scope is '' for the installation and the tenant's id otherwise, so the
-- primary key (one grant per key and scope, so setting a grant again replaces
-- its role instead of adding a second) holds for the installation too: a NULL
-- would be distinct from every other NULL. tenant_id repeats the scope as
-- NULL or the id, so the foreign key can check it.
CREATE TABLE grants (
    key_id    TEXT NOT NULL REFERENCES api_keys (id),
    scope     TEXT NOT NULL,
    tenant_id TEXT REFERENCES tenants (id),
    role      TEXT NOT NULL CHECK (role IN ('reader', 'writer', 'operator', 'admin')),
    PRIMARY KEY (key_id, scope),
    CHECK ((scope = '' AND tenant_id IS NULL) OR (scope <> '' AND tenant_id = scope))
) STRICT;

-- The list of grants on one scope, in order of key id. The primary key
-- serves the list by key.
CREATE INDEX grants_by_scope ON grants (scope, key_id);

-- The foreign key's own column. Without an index SQLite checks the foreign
-- key on every removal of a tenant by reading every grant.
CREATE INDEX grants_by_tenant ON grants (tenant_id);

CREATE TABLE settings (
    name  TEXT NOT NULL PRIMARY KEY,
    value TEXT NOT NULL
) STRICT;

-- The audit log. It has no foreign key: an entry outlives the tenant and the
-- key it names, and records requests that named ones that never existed.
-- details is a JSON object of strings, stored and returned whole.
CREATE TABLE audit (
    id          TEXT    NOT NULL PRIMARY KEY,
    at          INTEGER NOT NULL,
    key_id      TEXT    NOT NULL,
    local_user  TEXT    NOT NULL,
    remote_addr TEXT    NOT NULL,
    method      TEXT    NOT NULL,
    tenant_name TEXT    NOT NULL,
    object      TEXT    NOT NULL,
    outcome     TEXT    NOT NULL CHECK (outcome IN ('done', 'refused', 'failed')),
    reason      TEXT    NOT NULL,
    details     TEXT    NOT NULL,
    request_id  TEXT    NOT NULL
) STRICT;

-- The listings that are indexed, newest first: everything, by time; one
-- tenant's entries, by time; one key's entries, by time; one outcome's
-- entries, by time, so that a rare outcome ("failed") is found without
-- reading every entry. A start and an end time narrow the range read from
-- whichever index is used. Two equality filters together (a tenant and a
-- key, or either with an outcome) use the index of one of them, chosen by
-- SQLite's planner, and test the other on the rows that index yields.
CREATE INDEX audit_by_time    ON audit (at, id);
CREATE INDEX audit_by_tenant  ON audit (tenant_name, at, id);
CREATE INDEX audit_by_key     ON audit (key_id, at, id);
CREATE INDEX audit_by_outcome ON audit (outcome, at, id);
