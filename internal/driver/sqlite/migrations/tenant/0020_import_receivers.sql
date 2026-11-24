-- A receiver of a third type, import: it listens on nothing, so its port
-- is 0 and its bind address empty, and its import jobs hand it the files
-- of a directory under one of the installation's import roots. Its root,
-- layout and map are in its config JSON, as a stow receiver's keys are.
-- SQLite cannot change a CHECK constraint in place, so the table is made
-- again with the wider rule and its rows and indexes copied over as they
-- were.
--
-- Each file an import hands in keeps, with its arrival, the parts its
-- path gave the naming rule (a JSON object, '' for none), so the pipeline
-- names the file's session and scan by them when its reception is named;
-- an arrival recorded before this migration has none.
--
-- No row changes meaning, so the migration is safe on a database that
-- holds data. The rules of 0002_pipeline.sql hold.

CREATE TABLE receivers_with_import (
    name           TEXT    NOT NULL PRIMARY KEY,
    type           TEXT    NOT NULL CHECK (type IN ('dimse', 'stow', 'import')),
    enabled        INTEGER NOT NULL CHECK (enabled IN (0, 1)),
    storage        TEXT    NOT NULL,
    port           INTEGER NOT NULL,
    bind_address   TEXT    NOT NULL,
    config         TEXT    NOT NULL,
    naming         TEXT    NOT NULL,
    quiet_us       INTEGER NOT NULL,
    max_open_us    INTEGER NOT NULL,
    max_file_bytes INTEGER NOT NULL,
    created        INTEGER NOT NULL,
    updated        INTEGER NOT NULL,
    CHECK ((type = 'dimse' AND port BETWEEN 1 AND 65535) OR (type IN ('stow', 'import') AND port = 0 AND bind_address = ''))
) STRICT;

INSERT INTO receivers_with_import (name, type, enabled, storage, port, bind_address, config, naming, quiet_us,
        max_open_us, max_file_bytes, created, updated)
    SELECT name, type, enabled, storage, port, bind_address, config, naming, quiet_us,
        max_open_us, max_file_bytes, created, updated
    FROM receivers;

DROP TABLE receivers;
ALTER TABLE receivers_with_import RENAME TO receivers;

-- As in 0010_stow_receivers.sql.
CREATE INDEX receivers_by_storage ON receivers (storage);
CREATE INDEX receivers_by_port    ON receivers (port);

ALTER TABLE import_arrivals ADD COLUMN parts TEXT NOT NULL DEFAULT '';
