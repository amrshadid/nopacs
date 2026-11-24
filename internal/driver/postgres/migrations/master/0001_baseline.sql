-- The master database's schema on Postgres, whole: every table once, in the
-- order the tables refer to each other.
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
-- Constraints are named, so the driver tells a violation by the
-- constraint's name, never by the text of a message.
--
-- Text a caller did not give is '' rather than NULL, so a comparison never
-- meets NULL's rules.
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
-- work again), NULL while it is not.
CREATE TABLE api_keys (
    id           text        COLLATE "C" NOT NULL,
    hash         bytea       NOT NULL,
    holder       text        NOT NULL,
    created      timestamptz NOT NULL,
    expires      timestamptz,
    last_used    timestamptz,
    revoked      timestamptz,
    networks     text        NOT NULL DEFAULT '',
    client_cert  text        NOT NULL DEFAULT '',
    last_address text        NOT NULL DEFAULT '',
    successor    text        COLLATE "C" NOT NULL DEFAULT '',
    suspended    timestamptz,
    CONSTRAINT api_keys_pkey PRIMARY KEY (id),
    CONSTRAINT api_keys_hash_check CHECK (octet_length(hash) = 32),
    CONSTRAINT api_keys_client_cert_check CHECK (client_cert = '' OR octet_length(client_cert) = 64)
);

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
    name  text COLLATE "C" NOT NULL,
    value text NOT NULL,
    CONSTRAINT settings_pkey PRIMARY KEY (name)
);

-- The tenants. The unique constraint on the name makes its uniqueness the
-- database's to keep, so two creations racing in two processes cannot both
-- succeed, and the driver tells a taken name from a taken id by the
-- constraint's name; its index also serves the lookup by name and the list
-- in order of name. address is the tenant database's address, sealed with
-- the installation key. A tenant moved (an export handed it to another
-- installation) or fenced (the owner record on one of its storages names
-- another installation or a later generation) is read only, its reads
-- served.
CREATE TABLE tenants (
    id           text        COLLATE "C" NOT NULL,
    name         text        COLLATE "C" NOT NULL,
    driver       text        NOT NULL,
    address      bytea       NOT NULL,
    state        text        NOT NULL,
    state_reason text        NOT NULL,
    created      timestamptz NOT NULL,
    CONSTRAINT tenants_pkey PRIMARY KEY (id),
    CONSTRAINT tenants_name_key UNIQUE (name),
    CONSTRAINT tenants_state_check CHECK (state IN ('ready', 'unreachable', 'upgrading', 'failed', 'moved', 'fenced'))
);

-- The grants: a key's role on the installation or on one tenant. scope is ''
-- for the installation and the tenant's id otherwise, so the primary key
-- (one grant per key and scope, so setting a grant again replaces its role
-- instead of adding a second) holds for the installation too: a NULL would
-- be distinct from every other NULL. tenant_id repeats the scope as NULL or
-- the id, so the foreign key can check it.
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

-- The audit log. It has no foreign key: an entry outlives the tenant and the
-- key it names, and records requests that named ones that never existed.
-- details is a JSON object of strings, read back whole. It is jsonb, not
-- text, so that an operator with psql can read one field of it
-- (details ->> 'old_role') without a cast; jsonb keeps no key order and no
-- spacing, which the driver, reading it into a map, does not need.
--
-- seq is the entry's place in the log's hash chain. It is given when an
-- entry is sealed, not when it is written, since entries written at once
-- commit in no fixed order (a transaction begun first may commit last); it
-- is NULL until then. Once given it never changes (the store's SealAudit
-- sets it only where it is NULL).
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
    seq         bigint,
    CONSTRAINT audit_pkey PRIMARY KEY (id),
    CONSTRAINT audit_outcome_check CHECK (outcome IN ('done', 'refused', 'failed')),
    CONSTRAINT audit_details_check CHECK (jsonb_typeof(details) = 'object'),
    CONSTRAINT audit_seq_check CHECK (seq >= 1)
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

-- One entry per place in the chain. NULLs are distinct in a unique index,
-- so every unsealed entry fits.
CREATE UNIQUE INDEX audit_by_seq ON audit (seq);

-- The unsealed entries, in the order a pass seals them, without reading the
-- sealed ones: the index holds only the rows whose seq is NULL, so it stays
-- as small as one interval's entries however long the log grows.
CREATE INDEX audit_unsealed ON audit (at, id) WHERE seq IS NULL;

-- The signed checkpoints of the audit chain: one row per checkpoint, kept
-- also once retention has removed the entries it covers (pruned), so the
-- chain is still checked across them. The signed text is rebuilt from the
-- columns; digest is its SHA-256, kept so an anchor or a digest known from
-- outside is found by the index of its unique constraint. from_seq is
-- unique too, so two seals racing for the same place cannot both be kept.
CREATE TABLE audit_checkpoints (
    to_seq       bigint      NOT NULL,
    from_seq     bigint      NOT NULL,
    installation text        COLLATE "C" NOT NULL,
    log          text        COLLATE "C" NOT NULL,
    head         bytea       NOT NULL,
    previous     bytea       NOT NULL,
    digest       bytea       NOT NULL,
    at           timestamptz NOT NULL,
    key_version  integer     NOT NULL,
    signature    bytea       NOT NULL,
    pruned       boolean     NOT NULL DEFAULT false,
    CONSTRAINT audit_checkpoints_pkey PRIMARY KEY (to_seq),
    CONSTRAINT audit_checkpoints_from_key UNIQUE (from_seq),
    CONSTRAINT audit_checkpoints_digest_key UNIQUE (digest),
    CONSTRAINT audit_checkpoints_range_check CHECK (from_seq >= 1 AND to_seq >= from_seq),
    CONSTRAINT audit_checkpoints_sizes_check CHECK (octet_length(head) = 32 AND octet_length(previous) = 32
        AND octet_length(digest) = 32 AND octet_length(signature) = 64),
    CONSTRAINT audit_checkpoints_key_version_check CHECK (key_version >= 1)
);

-- The checkpoints retention has still to reach, oldest first.
CREATE INDEX audit_checkpoints_unpruned ON audit_checkpoints (to_seq) WHERE NOT pruned;

-- The latest checkpoint of another log this one keeps: in the master, one
-- row per tenant (log is the tenant's id); in a tenant, one for the master.
CREATE TABLE audit_anchors (
    log    text   COLLATE "C" NOT NULL,
    to_seq bigint NOT NULL,
    digest bytea  NOT NULL,
    CONSTRAINT audit_anchors_pkey PRIMARY KEY (log),
    CONSTRAINT audit_anchors_check CHECK (to_seq >= 1 AND octet_length(digest) = 32)
);
