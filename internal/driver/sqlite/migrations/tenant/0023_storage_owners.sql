-- The owner generation the tenant holds for each of its storages: the
-- generation of the owner record (meta/.owner.json) this database last
-- wrote there. An import or a restore takes a storage over by writing a
-- record one generation higher, so a server whose database holds a lower
-- generation than the record finds another installation writes there now.
-- written is when this database's record was written, in microseconds since
-- 1970. A storage with no row has no record of this database's yet.
--
-- Only a table is added, so the migration is safe on a database that holds
-- data. The rules of 0002_pipeline.sql hold.
CREATE TABLE storage_owners (
    storage    TEXT    NOT NULL PRIMARY KEY,
    generation INTEGER NOT NULL CHECK (generation >= 1 AND generation <= 9007199254740991),
    written    INTEGER NOT NULL
) STRICT, WITHOUT ROWID;
