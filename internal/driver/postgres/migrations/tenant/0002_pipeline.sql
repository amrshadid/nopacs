-- What the reception pipeline keeps in a tenant's database, on Postgres: the
-- tenant's storages and receivers, its receptions and the files they
-- received, its catalog (subjects, sessions, scans and the instances held or
-- claimed), its quarantine with running totals, and the leased units of work
-- that carry a reception through its steps. Everything the pipeline must not
-- lose in a restart is a row here.
--
-- The rules of 0001_init.sql hold: times are timestamptz to the microsecond,
-- read back in UTC; text a caller did not give is '' rather than NULL; every
-- identifier, name and UID that is compared, ordered or unique is COLLATE
-- "C", so order and uniqueness are byte by byte, as SQLite's are, whatever
-- collation the database was made with. A checksum is a bytea of exactly 32
-- bytes, compared byte by byte. JSON that is stored and returned whole and
-- never searched (a receiver's configuration, a reception's calling AE titles,
-- a quarantine record's details) is text. There are no foreign keys: a
-- reception and a quarantine record keep the name of a receiver removed, by
-- the API contract's rule, and every other reference is checked by the change
-- that makes it, inside its transaction, under the row locks the driver's
-- documentation lists. Counts, sizes, states the code relies on and checksum
-- lengths are held by CHECK constraints; the state of a reception and the
-- reason of a quarantine record are the core's and the contract's words and
-- have none, since a later version adds to both.
--
-- Each list, claim and lookup has the index named for it below; the plan
-- tests (pipeline_plan_test.go) show the planner uses it at millions of rows.

-- A storage: a directory under a root the installation names, or a bucket of
-- an S3 store. The columns of the other type are ''/false. The secret access
-- key is sealed by the caller and never read back by a read operation: s3_secret
-- is selected only as whether it is set.
CREATE TABLE storages (
    name             text        COLLATE "C" NOT NULL,
    type             text        COLLATE "C" NOT NULL,
    dir_root         text        COLLATE "C" NOT NULL,
    dir_path         text        COLLATE "C" NOT NULL,
    s3_endpoint      text        COLLATE "C" NOT NULL,
    s3_region        text        NOT NULL,
    s3_bucket        text        COLLATE "C" NOT NULL,
    s3_prefix        text        COLLATE "C" NOT NULL,
    s3_path_style    boolean     NOT NULL,
    s3_access_key_id text        NOT NULL,
    s3_secret        bytea,
    capacity_bytes   bigint      NOT NULL,
    created          timestamptz NOT NULL,
    updated          timestamptz NOT NULL,
    CONSTRAINT storages_pkey PRIMARY KEY (name),
    CONSTRAINT storages_type_check CHECK (type IN ('directory', 's3')),
    CONSTRAINT storages_capacity_check CHECK (capacity_bytes >= 0)
);

-- The overlap checks of a change read the storages of one root, or of one
-- endpoint and bucket, and no other.
CREATE INDEX storages_by_root   ON storages (dir_root) WHERE type = 'directory';
CREATE INDEX storages_by_bucket ON storages (s3_endpoint, s3_bucket) WHERE type = 's3';

-- A receiver. Its protocol's configuration (callers, TLS, SOP classes,
-- transfer syntaxes) and its naming rule are JSON, stored and returned whole;
-- the port and bind address are columns as well, since a change checks them
-- against the other receivers.
CREATE TABLE receivers (
    name           text        COLLATE "C" NOT NULL,
    type           text        COLLATE "C" NOT NULL,
    enabled        boolean     NOT NULL,
    storage        text        COLLATE "C" NOT NULL,
    port           integer     NOT NULL,
    bind_address   text        COLLATE "C" NOT NULL,
    config         text        NOT NULL,
    naming         text        NOT NULL,
    quiet_us       bigint      NOT NULL,
    max_open_us    bigint      NOT NULL,
    max_file_bytes bigint      NOT NULL,
    created        timestamptz NOT NULL,
    updated        timestamptz NOT NULL,
    CONSTRAINT receivers_pkey PRIMARY KEY (name),
    CONSTRAINT receivers_type_check CHECK (type IN ('dimse')),
    CONSTRAINT receivers_port_check CHECK (port BETWEEN 1 AND 65535)
);

-- Removing a storage asks whether a receiver names it; adding or changing a
-- receiver asks which receivers listen on its port.
CREATE INDEX receivers_by_storage ON receivers (storage);
CREATE INDEX receivers_by_port    ON receivers (port);

-- A reception: the files of one study arriving at one receiver. state is the
-- core's text, kept as it is; ended is whether that state is an end, as the
-- core said when it was written, so "in progress" stays right when a later
-- version adds a state. calling_ae_titles is a JSON array of at most 16.
CREATE TABLE receptions (
    id                text        COLLATE "C" NOT NULL,
    receiver          text        COLLATE "C" NOT NULL,
    study_uid         text        COLLATE "C" NOT NULL,
    arrival           integer     NOT NULL,
    state             text        COLLATE "C" NOT NULL,
    ended             boolean     NOT NULL,
    files_received    bigint      NOT NULL,
    bytes_received    bigint      NOT NULL,
    files_archived    bigint      NOT NULL,
    bytes_archived    bigint      NOT NULL,
    files_duplicate   bigint      NOT NULL,
    bytes_duplicate   bigint      NOT NULL,
    files_quarantined bigint      NOT NULL,
    bytes_quarantined bigint      NOT NULL,
    files_released    bigint      NOT NULL,
    subject_label     text        COLLATE "C" NOT NULL,
    session_label     text        COLLATE "C" NOT NULL,
    storage           text        COLLATE "C" NOT NULL,
    failure           text        NOT NULL,
    started           timestamptz NOT NULL,
    last_file         timestamptz NOT NULL,
    step_at           timestamptz NOT NULL,
    ended_at          timestamptz,
    associations      bigint      NOT NULL,
    calling_ae_titles text        NOT NULL,
    calling_truncated boolean     NOT NULL,
    retry_of          text        COLLATE "C" NOT NULL,
    retry_reception   text        COLLATE "C" NOT NULL,
    discard_reason    text        NOT NULL,
    discard_at        timestamptz,
    CONSTRAINT receptions_pkey PRIMARY KEY (id),
    CONSTRAINT receptions_arrival_check CHECK (arrival >= 1),
    CONSTRAINT receptions_counts_check CHECK (files_received >= 0 AND bytes_received >= 0 AND files_archived >= 0 AND
        bytes_archived >= 0 AND files_duplicate >= 0 AND bytes_duplicate >= 0 AND files_quarantined >= 0 AND
        bytes_quarantined >= 0 AND files_released >= 0 AND associations >= 0),
    CONSTRAINT receptions_ended_check CHECK (ended = (ended_at IS NOT NULL))
);

-- One arrival number per study in the tenant; the next one is read from here.
CREATE UNIQUE INDEX receptions_arrival ON receptions (study_uid, arrival);
-- At most one reception of a study is receiving at one receiver: two opens at
-- once make one.
CREATE UNIQUE INDEX receptions_open ON receptions (receiver, study_uid) WHERE state = 'receiving';
-- The listings, newest first by start time, then id:
--   no filter, or a time range only   receptions_by_start
--   a receiver                        receptions_by_receiver
--   a study                           receptions_by_study
--   steps                             receptions_by_state
--   in progress                       receptions_in_progress (only those)
-- and the combinations a tenant with many receptions would otherwise answer
-- by testing each row of one index: a receiver in progress, a receiver with
-- steps, a study with steps.
CREATE INDEX receptions_by_start             ON receptions (started, id);
CREATE INDEX receptions_by_receiver          ON receptions (receiver, started, id);
CREATE INDEX receptions_by_study             ON receptions (study_uid, started, id);
CREATE INDEX receptions_by_state             ON receptions (state, started, id);
CREATE INDEX receptions_in_progress          ON receptions (started, id) WHERE NOT ended;
CREATE INDEX receptions_receiver_in_progress ON receptions (receiver, started, id) WHERE NOT ended;
CREATE INDEX receptions_by_receiver_state    ON receptions (receiver, state, started, id);
CREATE INDEX receptions_by_study_state       ON receptions (study_uid, state, started, id);
-- Removing or moving a storage asks whether a reception in progress names it.
CREATE INDEX receptions_by_storage ON receptions (storage) WHERE NOT ended;

-- The associations that brought a reception's files, each once, so a batch
-- written again counts none twice.
CREATE TABLE reception_associations (
    reception_id text COLLATE "C" NOT NULL,
    association  text COLLATE "C" NOT NULL,
    CONSTRAINT reception_associations_pkey PRIMARY KEY (reception_id, association)
);

-- A file that landed in the incoming area. The key is what makes a batch
-- written again change nothing: the same reception, instance and checksum is
-- the same file.
CREATE TABLE received_files (
    reception_id        text        COLLATE "C" NOT NULL,
    sop_instance_uid    text        COLLATE "C" NOT NULL,
    sha256              bytea       NOT NULL,
    series_uid          text        COLLATE "C" NOT NULL,
    study_uid           text        COLLATE "C" NOT NULL,
    sop_class_uid       text        COLLATE "C" NOT NULL,
    transfer_syntax_uid text        COLLATE "C" NOT NULL,
    size                bigint      NOT NULL,
    incoming            text        COLLATE "C" NOT NULL,
    calling_ae          text        NOT NULL,
    called_ae           text        NOT NULL,
    remote_addr         text        NOT NULL,
    association         text        COLLATE "C" NOT NULL,
    released            boolean     NOT NULL,
    received            timestamptz NOT NULL,
    CONSTRAINT received_files_pkey PRIMARY KEY (reception_id, sop_instance_uid, sha256),
    CONSTRAINT received_files_sha256_check CHECK (octet_length(sha256) = 32),
    CONSTRAINT received_files_size_check CHECK (size >= 0)
);

-- A quarantined file's record. details is a JSON object of the reason's keys.
-- A record stays after its file is released or rejected.
CREATE TABLE quarantine (
    id                  text        COLLATE "C" NOT NULL,
    reason              text        COLLATE "C" NOT NULL,
    details             text        NOT NULL,
    state               text        COLLATE "C" NOT NULL,
    receiver            text        COLLATE "C" NOT NULL,
    reception_id        text        COLLATE "C" NOT NULL,
    calling_ae          text        NOT NULL,
    called_ae           text        NOT NULL,
    remote_addr         text        NOT NULL,
    size                bigint      NOT NULL,
    sha256              bytea,
    kept_sha256         bytea,
    sop_class_uid       text        COLLATE "C" NOT NULL,
    sop_instance_uid    text        COLLATE "C" NOT NULL,
    study_uid           text        COLLATE "C" NOT NULL,
    series_uid          text        COLLATE "C" NOT NULL,
    transfer_syntax_uid text        COLLATE "C" NOT NULL,
    incoming            text        COLLATE "C" NOT NULL,
    content_kept        boolean     NOT NULL,
    at                  timestamptz NOT NULL,
    decided_at          timestamptz,
    release_mode        text        COLLATE "C" NOT NULL,
    release_reception   text        COLLATE "C" NOT NULL,
    release_receiver    text        COLLATE "C" NOT NULL,
    reject_reason       text        NOT NULL,
    released_from       text        COLLATE "C" NOT NULL,
    CONSTRAINT quarantine_pkey PRIMARY KEY (id),
    CONSTRAINT quarantine_state_check CHECK (state IN ('held', 'released', 'rejected')),
    CONSTRAINT quarantine_size_check CHECK (size >= 0),
    CONSTRAINT quarantine_sha256_check CHECK (sha256 IS NULL OR octet_length(sha256) = 32),
    CONSTRAINT quarantine_kept_sha256_check CHECK (kept_sha256 IS NULL OR octet_length(kept_sha256) = 32),
    CONSTRAINT quarantine_release_mode_check CHECK (release_mode IN ('', 'as_is', 'overwrite')),
    CONSTRAINT quarantine_decided_check CHECK ((state = 'held') = (decided_at IS NULL))
);

-- The listings, newest first by quarantine time, then id, one index per
-- equality filter and for the combinations of the three filters that each
-- keep many records; a time range narrows whichever is used.
CREATE INDEX quarantine_by_time                  ON quarantine (at, id);
CREATE INDEX quarantine_by_state                 ON quarantine (state, at, id);
CREATE INDEX quarantine_by_reason                ON quarantine (reason, at, id);
CREATE INDEX quarantine_by_receiver              ON quarantine (receiver, at, id);
CREATE INDEX quarantine_by_reception             ON quarantine (reception_id, at, id);
CREATE INDEX quarantine_by_study                 ON quarantine (study_uid, at, id);
CREATE INDEX quarantine_by_instance              ON quarantine (sop_instance_uid, at, id);
CREATE INDEX quarantine_by_state_reason          ON quarantine (state, reason, at, id);
CREATE INDEX quarantine_by_state_receiver        ON quarantine (state, receiver, at, id);
CREATE INDEX quarantine_by_receiver_reason       ON quarantine (receiver, reason, at, id);
CREATE INDEX quarantine_by_state_receiver_reason ON quarantine (state, receiver, reason, at, id);
-- A discard leaves out the files a record keeps, found by their place.
CREATE INDEX quarantine_kept ON quarantine (incoming) WHERE content_kept;

-- The quarantine's running totals: files held now and the bytes kept for
-- them, changed in the transaction of every change of a record, by an UPDATE
-- that adds, so they equal a count over the records after every commit.
CREATE TABLE quarantine_totals (
    only_row boolean NOT NULL DEFAULT true,
    files    bigint  NOT NULL,
    bytes    bigint  NOT NULL,
    CONSTRAINT quarantine_totals_pkey PRIMARY KEY (only_row),
    CONSTRAINT quarantine_totals_one_row CHECK (only_row),
    CONSTRAINT quarantine_totals_check CHECK (files >= 0 AND bytes >= 0)
);
INSERT INTO quarantine_totals (only_row, files, bytes) VALUES (true, 0, 0);

-- The catalog. A subject is found by its label, a session by its Study
-- Instance UID, a scan by its Series Instance UID; each keeps the label it was
-- first given. folded is the label as label.Fold makes it, unique at its
-- level, so two labels that a file system ignoring case would make one
-- directory are never both held.
CREATE TABLE subjects (
    id      bigint      GENERATED ALWAYS AS IDENTITY,
    label   text        COLLATE "C" NOT NULL,
    folded  text        COLLATE "C" NOT NULL,
    created timestamptz NOT NULL,
    CONSTRAINT subjects_pkey PRIMARY KEY (id),
    CONSTRAINT subjects_folded_key UNIQUE (folded)
);

CREATE TABLE sessions (
    id         bigint      GENERATED ALWAYS AS IDENTITY,
    study_uid  text        COLLATE "C" NOT NULL,
    subject_id bigint      NOT NULL,
    label      text        COLLATE "C" NOT NULL,
    folded     text        COLLATE "C" NOT NULL,
    storage    text        COLLATE "C" NOT NULL,
    created    timestamptz NOT NULL,
    CONSTRAINT sessions_pkey PRIMARY KEY (id),
    CONSTRAINT sessions_study_uid_key UNIQUE (study_uid),
    CONSTRAINT sessions_folded_key UNIQUE (subject_id, folded)
);

CREATE TABLE scans (
    id              bigint      GENERATED ALWAYS AS IDENTITY,
    session_id      bigint      NOT NULL,
    series_uid      text        COLLATE "C" NOT NULL,
    label           text        COLLATE "C" NOT NULL,
    folded          text        COLLATE "C" NOT NULL,
    instances       bigint      NOT NULL,
    bytes           bigint      NOT NULL,
    manifest_sha256 bytea,
    created         timestamptz NOT NULL,
    updated         timestamptz NOT NULL,
    CONSTRAINT scans_pkey PRIMARY KEY (id),
    CONSTRAINT scans_series_uid_key UNIQUE (series_uid),
    CONSTRAINT scans_folded_key UNIQUE (session_id, folded),
    CONSTRAINT scans_counts_check CHECK (instances >= 0 AND bytes >= 0),
    CONSTRAINT scans_manifest_check CHECK (manifest_sha256 IS NULL OR octet_length(manifest_sha256) = 32)
);

-- One row per SOP Instance UID in the tenant: an instance a reception claims
-- before it writes the file to a storage, or an instance held, once the
-- reception confirmed it with its scan's catalog rows; never both. A claim
-- not confirmed is removed when its reception ends; what is held stays, and is
-- never replaced by reception. sha256 is the checksum the file was received
-- with; reception_id is the reception that claims it, then the one that
-- confirmed it. The storage, key, place and archived checksum are set when it
-- is held, and only then.
CREATE TABLE instances (
    sop_instance_uid text        COLLATE "C" NOT NULL,
    study_uid        text        COLLATE "C" NOT NULL,
    series_uid       text        COLLATE "C" NOT NULL,
    state            text        COLLATE "C" NOT NULL,
    sha256           bytea       NOT NULL,
    reception_id     text        COLLATE "C" NOT NULL,
    scan_id          bigint,
    storage          text        COLLATE "C" NOT NULL,
    storage_key      text        COLLATE "C" NOT NULL,
    byte_offset      bigint      NOT NULL,
    byte_length      bigint      NOT NULL,
    size             bigint      NOT NULL,
    archived_sha256  bytea,
    claimed_at       timestamptz NOT NULL,
    held_at          timestamptz,
    CONSTRAINT instances_pkey PRIMARY KEY (sop_instance_uid),
    CONSTRAINT instances_state_check CHECK (state IN ('claimed', 'held')),
    CONSTRAINT instances_sha256_check CHECK (octet_length(sha256) = 32),
    CONSTRAINT instances_archived_check CHECK (archived_sha256 IS NULL OR octet_length(archived_sha256) = 32),
    CONSTRAINT instances_reception_check CHECK (reception_id <> ''),
    CONSTRAINT instances_place_check CHECK (byte_offset >= 0 AND byte_length >= 0 AND size >= 0),
    CONSTRAINT instances_held_check CHECK ((state = 'held') = (scan_id IS NOT NULL) AND
        (state = 'held') = (archived_sha256 IS NOT NULL) AND (state = 'held') = (held_at IS NOT NULL) AND
        (state = 'held') = (storage <> '' AND storage_key <> ''))
);

-- A reception's claims, to release them; the instances held on a storage,
-- which make it hold data; the instances a reception confirmed, by scan, for
-- its list of scans; the sessions on a storage, which make it in use.
CREATE INDEX instances_claimed    ON instances (reception_id) WHERE state = 'claimed';
CREATE INDEX instances_on_storage ON instances (storage) WHERE state = 'held';
CREATE INDEX instances_held_by    ON instances (reception_id, scan_id) WHERE state = 'held';
CREATE INDEX scans_by_session     ON scans (session_id);
CREATE INDEX sessions_by_storage  ON sessions (storage);

-- The catalog's running totals, changed in the transaction of every commit
-- by an UPDATE that adds, so they equal a count over the catalog after each.
CREATE TABLE catalog_totals (
    only_row  boolean NOT NULL DEFAULT true,
    subjects  bigint  NOT NULL,
    sessions  bigint  NOT NULL,
    scans     bigint  NOT NULL,
    instances bigint  NOT NULL,
    CONSTRAINT catalog_totals_pkey PRIMARY KEY (only_row),
    CONSTRAINT catalog_totals_one_row CHECK (only_row),
    CONSTRAINT catalog_totals_check CHECK (subjects >= 0 AND sessions >= 0 AND scans >= 0 AND instances >= 0)
);
INSERT INTO catalog_totals (only_row, subjects, sessions, scans, instances) VALUES (true, 0, 0, 0, 0);

-- A unit of work. due is when a queued unit may be claimed, and when a leased
-- unit's lease ends; attempt counts claims and fences every write of the
-- worker holding the lease. unique_key is NULL for none. reception_id names
-- the reception the unit carries, '' for none: a unit held as failed fails its
-- reception in the same transaction.
CREATE TABLE work (
    id           text        COLLATE "C" NOT NULL,
    kind         text        COLLATE "C" NOT NULL,
    subject      text        NOT NULL,
    reception_id text        COLLATE "C" NOT NULL,
    unique_key   text        COLLATE "C",
    state        text        COLLATE "C" NOT NULL,
    due          timestamptz NOT NULL,
    attempt      integer     NOT NULL,
    max_attempts integer     NOT NULL,
    owner        text        COLLATE "C" NOT NULL,
    last_error   text        NOT NULL,
    created      timestamptz NOT NULL,
    updated      timestamptz NOT NULL,
    CONSTRAINT work_pkey PRIMARY KEY (id),
    CONSTRAINT work_state_check CHECK (state IN ('queued', 'leased', 'done', 'failed')),
    CONSTRAINT work_attempt_check CHECK (attempt >= 0 AND max_attempts >= 1)
);

-- A key queues its work once while the work is queued or leased; a finished
-- unit frees it.
CREATE UNIQUE INDEX work_by_key ON work (unique_key) WHERE unique_key IS NOT NULL AND state IN ('queued', 'leased');
-- A claim reads the earliest due of a kind among the units not finished, and
-- only those: finished units, however many, are not in this index.
CREATE INDEX work_claimable ON work (kind, due, id) WHERE state IN ('queued', 'leased');
-- The receptions with a live unit, for the search at start of those left
-- without one.
CREATE INDEX work_by_reception ON work (reception_id) WHERE state IN ('queued', 'leased');
