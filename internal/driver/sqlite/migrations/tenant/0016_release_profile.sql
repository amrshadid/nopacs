-- A release may now name the profile version the reception its file joins
-- is de-identified with, in place of the route's or the receiver's. The
-- version is kept with the released record, as name@version, so a file
-- that joins no reception keeps it when it lands again; with the
-- reception it joins it is kept in the reception's route column. Empty
-- for none, as every record released before.
ALTER TABLE quarantine ADD COLUMN release_profile TEXT NOT NULL DEFAULT '';
