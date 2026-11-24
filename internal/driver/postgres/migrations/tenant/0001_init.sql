-- A tenant database's first schema, on Postgres: the tenant's overrides of
-- the installation's settings, and the tenant's audit log. The catalog,
-- receptions, events and jobs are added when the code that uses them is, each
-- as a new file.
--
-- The rules are the master's (migrations/master/0001_init.sql) and the SQLite
-- tenant schema's: times are timestamptz (microseconds, read back in UTC),
-- the audit entry's details are jsonb, text a caller did not give is ''
-- rather than NULL, and text that is compared or ordered is COLLATE "C", so
-- it orders byte by byte whatever collation the database was made with.
-- Every table is made in the schema the connection's search_path starts
-- with. An applied migration is never changed: a change to this schema is a
-- new file.
--
-- No table holds who may enter the tenant: access lives only in the master,
-- so this database can be copied or handed over without carrying it.

-- Which tenant this schema belongs to. The driver writes the row the first
-- time it migrates the schema and compares it every time it opens it, so two
-- tenants pointed at one database and schema, or an address pasted for the
-- wrong tenant, are refused instead of sharing (and mixing) one set of
-- settings and one audit log. At most one row: only_row is always true.
CREATE TABLE tenant_identity (
    only_row boolean NOT NULL DEFAULT true,
    id       text    COLLATE "C" NOT NULL,
    CONSTRAINT tenant_identity_pkey PRIMARY KEY (only_row),
    CONSTRAINT tenant_identity_one_row CHECK (only_row)
);

-- A setting here overrides the installation's value of the same name; a
-- setting absent here takes the installation's.
CREATE TABLE settings (
    name  text COLLATE "C" NOT NULL,
    value text NOT NULL,
    CONSTRAINT settings_pkey PRIMARY KEY (name)
);

-- The tenant's audit log, with the master's columns, so one entry type and
-- one listing serve both logs. No foreign key: an entry names keys that live
-- in the master. details is a JSON object of strings.
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

-- The audit listings, newest first, with the master's indexes (the same
-- listing code serves both):
--   no filter, or a time range only          audit_by_time
--   a tenant name (with or without a range)   audit_by_tenant
--   a key (with or without a range)           audit_by_key
--   an outcome (with or without a range), so a rare outcome such as
--   "failed" is found without reading every entry   audit_by_outcome
-- A start and an end time narrow the range read from whichever index is
-- used. Two equality filters together (a tenant and a key, or either with an
-- outcome) use the index of one of them, chosen by the planner, and test the
-- other on the rows that index yields. In a tenant's log the tenant name is
-- usually the tenant's own, so its index matters less than in the master's;
-- it is kept so one query plan serves both.
CREATE INDEX audit_by_time    ON audit (at, id);
CREATE INDEX audit_by_tenant  ON audit (tenant_name, at, id);
CREATE INDEX audit_by_key     ON audit (key_id, at, id);
CREATE INDEX audit_by_outcome ON audit (outcome, at, id);
