-- Checkpoints of the event feed, recorded by the server with its own clock:
-- at the time at, the newest event committed was through. Retention removes
-- every event up to the newest checkpoint older than its period, so an
-- event's age is measured by the server's clock from when it was written,
-- never by the time the event itself carries, which a caller gave.
CREATE TABLE event_checkpoints (
    at      INTEGER NOT NULL PRIMARY KEY,
    through INTEGER NOT NULL CHECK (through >= 0)
) STRICT;
