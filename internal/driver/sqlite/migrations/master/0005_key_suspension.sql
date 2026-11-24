-- A key may now be suspended: refused as a key that is no good, until the
-- local operator reinstates it. A restore suspends every key it brings
-- back, since a key revoked, or a grant removed, after the backup was made
-- would otherwise work again. suspended is when it was suspended, in
-- microseconds since 1970, NULL while it is not.
--
-- Only a column with no default is added, so the migration is safe on a
-- database that holds data, and every key it holds reads as not suspended.
ALTER TABLE api_keys ADD COLUMN suspended INTEGER;
