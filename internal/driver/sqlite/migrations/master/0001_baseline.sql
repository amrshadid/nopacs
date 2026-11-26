-- The master database's schema, whole: every table once, in the order the
-- tables refer to each other.
--
-- Every table is STRICT, so a value of the wrong type is refused as Postgres
-- would refuse it, instead of being kept as SQLite otherwise does. Times are
-- INTEGER microseconds since 1970 in UTC, NULL for "never", so they sort,
-- page and compare the same way in every driver. Text a caller did not give
-- is '' rather than NULL, so a comparison never meets NULL's rules.
--
-- The runner records this file's SHA-256 when it makes a database, and
-- refuses to open one whose recorded SHA-256 differs: a database made from
-- another version of this file was made by another build, and is never
-- half-migrated to this one. A later change to the schema is a new file
-- after this one, applied once and never edited.

-- The API keys. Only the SHA-256 of a key's secret is kept (hash), so a copy
-- of the database gives no key. networks is the key's allowed networks as
-- one text, in the one written form the settings of networks take
-- (comma-separated, each an address or a CIDR network), '' for every
-- network. client_cert is the SHA-256 of the TLS client certificate the key
-- must come with, 64 lowercase hexadecimal digits, '' for none. last_address
-- is the caller's address written with last_used, '' before the first use
-- off the local socket. successor is the id of the key that replaced this
-- one by a rotation, '' when it was never rotated; it is not a foreign key,
-- since the successor is written in the same transaction and a key is never
-- deleted. suspended is when the key was suspended (a restore suspends every
-- key it brings back, since a key revoked after the backup would otherwise
-- work again), NULL while it is not. masked is 1 for a key that is never
-- answered a value not in its archived, de-identified form (one an outside
-- service holds); it is written when the key is made and never changed, and
-- a rotation's successor takes it.
CREATE TABLE api_keys (
    id        TEXT    NOT NULL PRIMARY KEY,
    hash      BLOB    NOT NULL CHECK (length(hash) = 32),
    holder    TEXT    NOT NULL,
    created   INTEGER NOT NULL,
    expires   INTEGER,
    last_used INTEGER,
    revoked   INTEGER,
    networks     TEXT NOT NULL DEFAULT '',
    client_cert  TEXT NOT NULL DEFAULT '' CHECK (client_cert = '' OR length(client_cert) = 64),
    last_address TEXT NOT NULL DEFAULT '',
    successor    TEXT NOT NULL DEFAULT '',
    suspended INTEGER,
    masked    INTEGER NOT NULL DEFAULT 0 CHECK (masked IN (0, 1))
) STRICT;

-- The list of keys in order of creation, then id.
CREATE INDEX api_keys_by_created ON api_keys (created, id);

-- The key a rotation replaced with a given key: a list of keys (a stow
-- receiver's) that names a key takes the keys rotated from it, so the line
-- of rotations is followed backward from the key a request comes with, one
-- read of this index for each step. Only rotated keys have a successor, so
-- the index holds those rows alone; a read gives the same condition so the
-- index is used.
CREATE INDEX api_keys_by_successor ON api_keys (successor) WHERE successor <> '';

-- The installation's settings, by name; the server knows each name and its
-- rule, so the table holds text only.
CREATE TABLE settings (
    name  TEXT NOT NULL PRIMARY KEY,
    value TEXT NOT NULL
) STRICT;

-- The tenants. UNIQUE makes the name's uniqueness the database's to keep, so
-- two creations racing in two processes cannot both succeed; its index also
-- serves the lookup by name and the list in order of name. address is the
-- tenant database's address, sealed with the installation key. A tenant
-- moved (an export handed it to another installation) or fenced (the owner
-- record on one of its storages names another installation or a later
-- generation) is read only, its reads served.
CREATE TABLE tenants (
    id           TEXT    NOT NULL PRIMARY KEY,
    name         TEXT    NOT NULL UNIQUE,
    driver       TEXT    NOT NULL,
    address      BLOB    NOT NULL,
    state        TEXT    NOT NULL CHECK (state IN ('ready', 'unreachable', 'upgrading', 'failed', 'moved', 'fenced')),
    state_reason TEXT    NOT NULL,
    created      INTEGER NOT NULL
) STRICT;

-- The grants: a key's role on the installation or on one tenant. scope is ''
-- for the installation and the tenant's id otherwise, so the primary key
-- (one grant per key and scope, so setting a grant again replaces its role
-- instead of adding a second) holds for the installation too: a NULL would
-- be distinct from every other NULL. tenant_id repeats the scope as NULL or
-- the id, so the foreign key can check it.
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

-- The audit log. It has no foreign key: an entry outlives the tenant and the
-- key it names, and records requests that named ones that never existed.
-- details is a JSON object of strings, stored and returned whole.
--
-- seq is the entry's place in the log's hash chain. It is given when an
-- entry is sealed, not when it is written, since entries written at once
-- commit in no fixed order; it is NULL until then. Once given it never
-- changes (the store's SealAudit sets it only where it is NULL).
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
    request_id  TEXT    NOT NULL,
    seq INTEGER CHECK (seq >= 1)
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

-- One entry per place in the chain. NULLs are distinct in a UNIQUE index
-- (sqlite.org/lang_createindex.html), so every unsealed entry fits.
CREATE UNIQUE INDEX audit_by_seq ON audit (seq);

-- The unsealed entries, in the order a pass seals them, without reading the
-- sealed ones: the index holds only the rows whose seq is NULL, so it stays
-- as small as one interval's entries however long the log grows.
CREATE INDEX audit_unsealed ON audit (at, id) WHERE seq IS NULL;

-- The signed checkpoints of the audit chain: one row per checkpoint, kept
-- also once retention has removed the entries it covers (pruned), so the
-- chain is still checked across them. The signed text is rebuilt from the
-- columns; digest is its SHA-256, kept so an anchor or a digest known from
-- outside is found by the index of its UNIQUE constraint. from_seq is
-- UNIQUE too, so two seals racing for the same place cannot both be kept.
CREATE TABLE audit_checkpoints (
    to_seq       INTEGER NOT NULL PRIMARY KEY,
    from_seq     INTEGER NOT NULL UNIQUE CHECK (from_seq >= 1),
    installation TEXT    NOT NULL,
    log          TEXT    NOT NULL,
    head         BLOB    NOT NULL CHECK (length(head) = 32),
    previous     BLOB    NOT NULL CHECK (length(previous) = 32),
    digest       BLOB    NOT NULL UNIQUE CHECK (length(digest) = 32),
    at           INTEGER NOT NULL,
    key_version  INTEGER NOT NULL CHECK (key_version BETWEEN 1 AND 2147483647),
    signature    BLOB    NOT NULL CHECK (length(signature) = 64),
    pruned       INTEGER NOT NULL DEFAULT 0 CHECK (pruned IN (0, 1)),
    CHECK (to_seq >= from_seq)
) STRICT;

-- The checkpoints retention has still to reach, oldest first.
CREATE INDEX audit_checkpoints_unpruned ON audit_checkpoints (to_seq) WHERE pruned = 0;

-- The latest checkpoint of another log this one keeps: in the master, one
-- row per tenant (log is the tenant's id); in a tenant, one for the master.
CREATE TABLE audit_anchors (
    log    TEXT    NOT NULL PRIMARY KEY,
    to_seq INTEGER NOT NULL CHECK (to_seq >= 1),
    digest BLOB    NOT NULL CHECK (length(digest) = 32)
) STRICT;
