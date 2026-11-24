-- A tenant may now be moved (an export handed it to another installation)
-- or fenced (the owner record on one of its storages names another
-- installation or a later generation): read only, its reads served.
--
-- SQLite cannot change a CHECK constraint in place, so tenants is made again
-- with the wider one (sqlite.org/lang_altertable.html, "Making Other Kinds
-- Of Table Schema Changes"). The migration runs in one transaction with
-- foreign keys on, so grants, whose rows refer to tenants, is moved aside
-- first, without its foreign keys, and made again after: dropping tenants
-- with grants still referring to it would fail the foreign key, deferred
-- or not. Every row of both tables is copied as it is, so nothing a tenant
-- or a grant holds changes.
CREATE TABLE tenants_new (
    id           TEXT    NOT NULL PRIMARY KEY,
    name         TEXT    NOT NULL UNIQUE,
    driver       TEXT    NOT NULL,
    address      BLOB    NOT NULL,
    state        TEXT    NOT NULL CHECK (state IN ('ready', 'unreachable', 'upgrading', 'failed', 'moved', 'fenced')),
    state_reason TEXT    NOT NULL,
    created      INTEGER NOT NULL
) STRICT;
INSERT INTO tenants_new (id, name, driver, address, state, state_reason, created)
    SELECT id, name, driver, address, state, state_reason, created FROM tenants;

CREATE TABLE grants_aside (
    key_id    TEXT NOT NULL,
    scope     TEXT NOT NULL,
    tenant_id TEXT,
    role      TEXT NOT NULL
) STRICT;
INSERT INTO grants_aside (key_id, scope, tenant_id, role) SELECT key_id, scope, tenant_id, role FROM grants;

DROP TABLE grants;
DROP TABLE tenants;
ALTER TABLE tenants_new RENAME TO tenants;

CREATE TABLE grants (
    key_id    TEXT NOT NULL REFERENCES api_keys (id),
    scope     TEXT NOT NULL,
    tenant_id TEXT REFERENCES tenants (id),
    role      TEXT NOT NULL CHECK (role IN ('reader', 'writer', 'operator', 'admin')),
    PRIMARY KEY (key_id, scope),
    CHECK ((scope = '' AND tenant_id IS NULL) OR (scope <> '' AND tenant_id = scope))
) STRICT;
INSERT INTO grants (key_id, scope, tenant_id, role) SELECT key_id, scope, tenant_id, role FROM grants_aside;
DROP TABLE grants_aside;

CREATE INDEX grants_by_scope ON grants (scope, key_id);
CREATE INDEX grants_by_tenant ON grants (tenant_id);
