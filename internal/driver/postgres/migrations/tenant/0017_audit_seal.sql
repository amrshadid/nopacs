-- The audit seal of the tenant's log, the same as the master's (master
-- 0004_audit_seal.sql): each entry's place in the log's hash chain, the
-- signed checkpoints of the chain, and the master's latest checkpoint this
-- log keeps. It only adds: no existing column or row changes. Adding a
-- column with no default to a table is a change of the catalog only, so a
-- tenant with a long log upgrades at once.
--
-- seq is given when an entry is sealed, not when it is written, since
-- entries written at once commit in no fixed order (a transaction begun
-- first may commit last); it is NULL until then. Once given it never
-- changes (the store's SealAudit sets it only where it is NULL). Every
-- entry written before this migration starts unsealed, and the first
-- sealing pass seals them.
ALTER TABLE audit ADD COLUMN seq bigint CONSTRAINT audit_seq_check CHECK (seq >= 1);

-- One entry per place in the chain. NULLs are distinct in a unique index,
-- so every unsealed entry fits.
CREATE UNIQUE INDEX audit_by_seq ON audit (seq);

-- The unsealed entries, in the order a pass seals them, without reading the
-- sealed ones: the index holds only the rows whose seq is NULL, so it stays
-- as small as one interval's entries however long the log grows.
CREATE INDEX audit_unsealed ON audit (at, id) WHERE seq IS NULL;

-- One row per checkpoint, kept also once retention has removed the entries
-- it covers (pruned), so the chain is still checked across them. The
-- signed text is rebuilt from the columns; digest is its SHA-256, kept so
-- an anchor or a digest known from outside is found by the index of its
-- unique constraint. from_seq is unique too, so two seals racing for the
-- same place cannot both be kept; the constraints are named so the driver
-- tells their violation by name.
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

-- The latest checkpoint of the master's log this one keeps (log is
-- "master").
CREATE TABLE audit_anchors (
    log    text   COLLATE "C" NOT NULL,
    to_seq bigint NOT NULL,
    digest bytea  NOT NULL,
    CONSTRAINT audit_anchors_pkey PRIMARY KEY (log),
    CONSTRAINT audit_anchors_check CHECK (to_seq >= 1 AND octet_length(digest) = 32)
);
