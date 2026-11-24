-- The audit seal of the tenant's log, the same as the master's (master
-- 0004_audit_seal.sql): each entry's place in the log's hash chain, the
-- signed checkpoints of the chain, and the master's latest checkpoint this
-- log keeps. It only adds: no existing column or row changes.
--
-- seq is given when an entry is sealed, not when it is written, since
-- entries written at once commit in no fixed order; it is NULL until then.
-- Once given it never changes (the store's SealAudit sets it only where it
-- is NULL). Every entry written before this migration starts unsealed, and
-- the first sealing pass seals them.
ALTER TABLE audit ADD COLUMN seq INTEGER CHECK (seq >= 1);

-- One entry per place in the chain. NULLs are distinct in a UNIQUE index
-- (sqlite.org/lang_createindex.html), so every unsealed entry fits.
CREATE UNIQUE INDEX audit_by_seq ON audit (seq);

-- The unsealed entries, in the order a pass seals them, without reading the
-- sealed ones: the index holds only the rows whose seq is NULL, so it stays
-- as small as one interval's entries however long the log grows.
CREATE INDEX audit_unsealed ON audit (at, id) WHERE seq IS NULL;

-- One row per checkpoint, kept also once retention has removed the entries
-- it covers (pruned), so the chain is still checked across them. The
-- signed text is rebuilt from the columns; digest is its SHA-256, kept so
-- an anchor or a digest known from outside is found by the index of its
-- UNIQUE constraint. from_seq is UNIQUE too, so two seals racing for the
-- same place cannot both be kept.
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

-- The latest checkpoint of another log this one keeps: the master's (log
-- is "master").
CREATE TABLE audit_anchors (
    log    TEXT    NOT NULL PRIMARY KEY,
    to_seq INTEGER NOT NULL CHECK (to_seq >= 1),
    digest BLOB    NOT NULL CHECK (length(digest) = 32)
) STRICT;
