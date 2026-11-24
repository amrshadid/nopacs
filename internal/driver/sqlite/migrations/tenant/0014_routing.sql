-- A receiver may now route each study by its routes, kept in its config
-- column's text. What the routing chose for a session (the route, and the
-- profile version and senders it names in place of the receiver's) is
-- kept with the reception when it is identified, so a step run again, and
-- the steps after, go by the same choice whatever the receiver's routes
-- say by then; and with the session when its first scan is committed, so
-- every later arrival of the session goes by the same choice. A storage a
-- release named is kept with the reception it joins, or with the released
-- record when the file joins none and lands again. Empty for nothing of
-- its own, as every row before routing.
ALTER TABLE receptions ADD COLUMN route TEXT NOT NULL DEFAULT '';
ALTER TABLE sessions ADD COLUMN route TEXT NOT NULL DEFAULT '';
ALTER TABLE quarantine ADD COLUMN release_storage TEXT NOT NULL DEFAULT '';
