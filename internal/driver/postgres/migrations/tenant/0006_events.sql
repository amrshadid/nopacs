-- The event feed: one row per thing that happened, written in the
-- transaction of the change it reports. id orders the feed. Several
-- transactions write at once on Postgres, so a change takes the row of
-- event_marks FOR UPDATE just before it writes its first event and holds it
-- to its commit: the changes that write events commit one after another,
-- each takes its ids after the one before committed, and the order of ids is
-- the order of commits. A reader's snapshot then holds every event up to
-- some id and none after it, which is what a cursor needs. Ids only grow: a
-- sequence never gives a value twice, even after every row was pruned or a
-- transaction that took one was undone. type is the contract's word and has
-- no CHECK, as a later version adds types. payload is JSON, stored and
-- returned whole and never searched inside. key is the caller's key of an
-- event the server appended, NULL for one the store wrote with its change.
CREATE TABLE events (
    id      bigint      GENERATED ALWAYS AS IDENTITY,
    type    text        COLLATE "C" NOT NULL,
    at      timestamptz NOT NULL,
    payload text        NOT NULL,
    key     text        COLLATE "C",
    CONSTRAINT events_pkey PRIMARY KEY (id)
);

-- A filtered read walks the events of each type it names after the cursor;
-- an unfiltered read and the pruning walk the ids themselves.
CREATE INDEX events_by_type ON events (type, id);
-- An appended event is written once per key.
CREATE UNIQUE INDEX events_by_key ON events (key) WHERE key IS NOT NULL;

-- What pruning removed: every event whose id is at or below pruned_through
-- is gone, and every event above it is kept. It moves in the pruning's
-- transaction, so a reader whose cursor is below it is told its cursor
-- expired, never answered from past a gap. Its one row is also the feed's
-- lock (above).
CREATE TABLE event_marks (
    only_row       boolean NOT NULL DEFAULT true,
    pruned_through bigint  NOT NULL CHECK (pruned_through >= 0),
    CONSTRAINT event_marks_pkey PRIMARY KEY (only_row),
    CONSTRAINT event_marks_one_row CHECK (only_row)
);
INSERT INTO event_marks (only_row, pruned_through) VALUES (true, 0);

-- Checkpoints of the feed, recorded by the server with its own clock: at the
-- time at, the newest event committed was through. Retention removes every
-- event up to the newest checkpoint older than its period, so an event's age
-- is measured by the server's clock from when it was written, never by the
-- time the event itself carries, which a caller gave.
CREATE TABLE event_checkpoints (
    at      timestamptz NOT NULL,
    through bigint      NOT NULL CHECK (through >= 0),
    CONSTRAINT event_checkpoints_pkey PRIMARY KEY (at)
);
