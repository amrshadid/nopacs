-- The bytes the catalog holds on each storage, as running totals updated in
-- the transaction of each commit, so a status reads them in its snapshot and
-- never sums the instances per call. Filled here from the instances already
-- held; a storage with none has no row and holds nothing.
CREATE TABLE storage_totals (
    storage TEXT    NOT NULL PRIMARY KEY,
    bytes   INTEGER NOT NULL CHECK (bytes >= 0)
) STRICT;
INSERT INTO storage_totals (storage, bytes)
    SELECT storage, sum(size) FROM instances WHERE state = 'held' GROUP BY storage;
