-- The key a rotation replaced with a given key: a list of keys (a stow
-- receiver's) that names a key takes the keys rotated from it, so the line
-- of rotations is followed backward from the key a request comes with, one
-- read of this index for each step. Only rotated keys have a successor, so
-- the index holds those rows alone; a read gives the same condition so the
-- index is used.

CREATE INDEX api_keys_by_successor ON api_keys (successor) WHERE successor <> '';
