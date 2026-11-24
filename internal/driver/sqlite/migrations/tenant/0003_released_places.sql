-- The places in the incoming area that released quarantine records gave to
-- other receptions. A record released into a reception gives that reception
-- a received file at the record's own place, which may be a place a failed
-- reception received the file at. When that failed reception's files are
-- discarded, the list of what to remove must leave out a place a reception
-- still running holds, or a restart would delete bytes it has yet to
-- archive; this index answers that question for each file the list reads.
-- Released records are few, and a record is released once, so the index
-- costs nothing on the path a file is received by.
CREATE INDEX quarantine_released_places ON quarantine (incoming) WHERE state = 'released';
