-- A tenant database's first schema: the tenant's overrides of the
-- installation's settings, and the tenant's audit log. The catalog,
-- receptions, events and jobs are added when the code that uses them is, each
-- as a new file.
--
-- The rules are the master's (migrations/master/0001_init.sql): every table
-- is STRICT; times are INTEGER microseconds since 1970 in UTC; text a caller
-- did not give is '' rather than NULL. An applied migration is never changed:
-- a change to this schema is a new file.
--
-- No table holds who may enter the tenant: access lives only in the master,
-- so this database can be copied or handed over without carrying it.

-- A setting here overrides the installation's value of the same name; a
-- setting absent here takes the installation's.
CREATE TABLE settings (
    name  TEXT NOT NULL PRIMARY KEY,
    value TEXT NOT NULL
) STRICT;

-- The tenant's audit log, with the master's columns, so one entry type and
-- one listing serve both logs. No foreign key: an entry names keys that live
-- in the master.
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

-- The audit listings, newest first, with the master's indexes (the same
-- listing code serves both):
--   no filter, or a time range only          audit_by_time
--   a tenant name (with or without a range)   audit_by_tenant
--   a key (with or without a range)           audit_by_key
--   an outcome (with or without a range), so a rare outcome such as
--   "failed" is found without reading every entry   audit_by_outcome
-- A start and an end time narrow the range read from whichever index is
-- used. Two equality filters together (a tenant and a key, or either with an
-- outcome) use the index of one of them, chosen by SQLite's planner, and
-- test the other on the rows that index yields. In a tenant's log the tenant
-- name is usually the tenant's own, so its index matters less than in the
-- master's; it is kept so one query plan serves both.
CREATE INDEX audit_by_time    ON audit (at, id);
CREATE INDEX audit_by_tenant  ON audit (tenant_name, at, id);
CREATE INDEX audit_by_key     ON audit (key_id, at, id);
CREATE INDEX audit_by_outcome ON audit (outcome, at, id);
