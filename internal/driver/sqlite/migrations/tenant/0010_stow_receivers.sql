-- A receiver of a second type, stow: it takes instances stored over HTTP
-- into the tenant's DICOMweb service (PS3.18 10.5) and listens on no port
-- of its own, so its port is 0 and its bind address empty. The keys it
-- takes requests from and its largest request are in its config JSON, as a
-- DIMSE receiver's callers are. SQLite cannot change a CHECK constraint in
-- place, so the table is made again with the wider rule and its rows and
-- indexes copied over as they were.
--
-- The rules of 0002_pipeline.sql hold.

CREATE TABLE receivers_with_stow (
    name           TEXT    NOT NULL PRIMARY KEY,
    type           TEXT    NOT NULL CHECK (type IN ('dimse', 'stow')),
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
    CHECK ((type = 'dimse' AND port BETWEEN 1 AND 65535) OR (type = 'stow' AND port = 0 AND bind_address = ''))
) STRICT;

INSERT INTO receivers_with_stow (name, type, enabled, storage, port, bind_address, config, naming, quiet_us,
        max_open_us, max_file_bytes, created, updated)
    SELECT name, type, enabled, storage, port, bind_address, config, naming, quiet_us,
        max_open_us, max_file_bytes, created, updated
    FROM receivers;

DROP TABLE receivers;
ALTER TABLE receivers_with_stow RENAME TO receivers;

-- As in 0002_pipeline.sql. The stow receivers share port 0, so a change of
-- one reads the others by it, to refuse a key two of them list.
CREATE INDEX receivers_by_storage ON receivers (storage);
CREATE INDEX receivers_by_port    ON receivers (port);
