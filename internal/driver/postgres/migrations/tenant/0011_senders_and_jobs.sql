-- What a tenant keeps to send archived data out: its senders, its jobs, each
-- job's scans with how far each scan's unit got, the instances a retry sends
-- again below its scan's starting point, the instances a scan's unit owes
-- a second try, and the instances a job's peer refused or kept with a
-- warning. The work itself is units of the work
-- table, of the kind send-<sender>. Only tables are added, empty, so the
-- migration is safe on a database that holds data.
--
-- The rules of 0002_pipeline.sql hold: times are timestamptz to the
-- microsecond, text a caller did not give is '' rather than NULL, compared
-- text is COLLATE "C", and there are no foreign keys: a job keeps the name of
-- a sender removed, and every other reference is checked by the change that
-- makes it, under the row locks the driver's documentation lists.

-- A sender. Its protocol's configuration (a DIMSE peer's host, port, AE
-- titles and TLS files; a stow peer's URL, authentication and request
-- bounds) is JSON text, stored and returned whole. The credential is sealed
-- by the caller and never read back by a read operation: it is selected only
-- as whether it is set.
CREATE TABLE senders (
    name                 text        COLLATE "C" NOT NULL,
    type                 text        COLLATE "C" NOT NULL,
    enabled              boolean     NOT NULL,
    config               text        NOT NULL,
    credential           bytea,
    max_associations     integer     NOT NULL,
    max_bytes_per_second bigint      NOT NULL,
    connect_us           bigint      NOT NULL,
    response_us          bigint      NOT NULL,
    created              timestamptz NOT NULL,
    updated              timestamptz NOT NULL,
    CONSTRAINT senders_pkey PRIMARY KEY (name),
    CONSTRAINT senders_type_check CHECK (type IN ('dimse', 'stow')),
    CONSTRAINT senders_bounds_check CHECK (max_associations BETWEEN 1 AND 16 AND max_bytes_per_second >= 0)
);

-- A job: what it sends (a target by labels, or a reception's instances),
-- where it is, and its running counts, kept in the transaction of every
-- write of its units. bound_at is the moment whose catalog it sends (its
-- creation, or the creation of the job a retry sends again). Its listing
-- has two parts: a retry's first lists the ended job's scans (retry_after,
-- retry_listed), then every job lists what its target holds that is not
-- listed yet (list_*, target_listed); listed is both. progress_at and progress_mark are the time and the
-- instances answered at its last progress event, which is written at most
-- once in a period.
CREATE TABLE jobs (
    id                  text        COLLATE "C" NOT NULL,
    kind                text        COLLATE "C" NOT NULL,
    sender              text        COLLATE "C" NOT NULL,
    subject_label       text        COLLATE "C" NOT NULL,
    session_label       text        COLLATE "C" NOT NULL,
    scan_label          text        COLLATE "C" NOT NULL,
    reception_id        text        COLLATE "C" NOT NULL,
    receiver            text        COLLATE "C" NOT NULL,
    unique_key          text        COLLATE "C",
    state               text        COLLATE "C" NOT NULL,
    ended               boolean     NOT NULL,
    reason              text        NOT NULL,
    created_by_key      text        COLLATE "C" NOT NULL,
    created             timestamptz NOT NULL,
    started             timestamptz,
    state_at            timestamptz NOT NULL,
    ended_at            timestamptz,
    bound_at            timestamptz NOT NULL,
    retry_of            text        COLLATE "C" NOT NULL,
    retry_job           text        COLLATE "C" NOT NULL,
    list_work           text        COLLATE "C" NOT NULL,
    listed              boolean     NOT NULL,
    retry_listed        boolean     NOT NULL,
    target_listed       boolean     NOT NULL,
    retry_after         bigint      NOT NULL,
    list_session        text        COLLATE "C" NOT NULL,
    list_scan           text        COLLATE "C" NOT NULL,
    list_id             bigint      NOT NULL,
    scans               bigint      NOT NULL,
    scans_done          bigint      NOT NULL,
    instances           bigint      NOT NULL,
    bytes               bigint      NOT NULL,
    sent                bigint      NOT NULL,
    sent_bytes          bigint      NOT NULL,
    warned              bigint      NOT NULL,
    warned_bytes        bigint      NOT NULL,
    refused             bigint      NOT NULL,
    refused_bytes       bigint      NOT NULL,
    failures_listed     bigint      NOT NULL,
    failures_not_listed bigint      NOT NULL,
    progress_at         timestamptz,
    progress_mark       bigint      NOT NULL,
    CONSTRAINT jobs_pkey PRIMARY KEY (id),
    CONSTRAINT jobs_key UNIQUE (unique_key),
    CONSTRAINT jobs_kind_check CHECK (kind = 'send'),
    CONSTRAINT jobs_state_check CHECK (state IN ('queued', 'running', 'done', 'partial', 'failed', 'canceled')),
    CONSTRAINT jobs_scans_check CHECK (scans >= 0 AND scans_done >= 0 AND scans_done <= scans),
    CONSTRAINT jobs_counts_check CHECK (sent >= 0 AND warned >= 0 AND refused >= 0 AND sent + warned + refused <= instances),
    CONSTRAINT jobs_bytes_check CHECK (sent_bytes >= 0 AND warned_bytes >= 0 AND refused_bytes >= 0
        AND sent_bytes + warned_bytes + refused_bytes <= bytes)
);

-- The lists, newest first: every job, by state, by sender, and those not
-- ended (also what a sender's removal and a status read).
CREATE INDEX jobs_by_created            ON jobs (created, id);
CREATE INDEX jobs_by_state              ON jobs (state, created, id);
CREATE INDEX jobs_by_sender             ON jobs (sender, created, id);
CREATE INDEX jobs_in_progress           ON jobs (created, id) WHERE NOT ended;
CREATE INDEX jobs_of_sender_in_progress ON jobs (sender, created, id) WHERE NOT ended;

-- A job's scan, one unit of work each: the instances its stream holds after
-- start_after (and those of job_resends), the last one the peer answered
-- (cursor), and its counts.
CREATE TABLE job_scans (
    job_id             text    COLLATE "C" NOT NULL,
    scan_id            bigint  NOT NULL,
    work_id            text    COLLATE "C" NOT NULL,
    start_after        text    COLLATE "C" NOT NULL,
    cursor             text    COLLATE "C" NOT NULL,
    instances          bigint  NOT NULL,
    bytes              bigint  NOT NULL,
    sent               bigint  NOT NULL,
    sent_bytes         bigint  NOT NULL,
    warned             bigint  NOT NULL,
    warned_bytes       bigint  NOT NULL,
    refused            bigint  NOT NULL,
    refused_bytes      bigint  NOT NULL,
    refused_not_listed bigint  NOT NULL,
    ended              boolean NOT NULL,
    CONSTRAINT job_scans_pkey PRIMARY KEY (job_id, scan_id),
    CONSTRAINT job_scans_counts_check CHECK (sent >= 0 AND warned >= 0 AND refused >= 0 AND sent + warned + refused <= instances)
);

-- A unit's writes find its scan by the unit.
CREATE UNIQUE INDEX job_scans_by_work ON job_scans (work_id);

-- The instances a retry's scan sends again that lie at or before its
-- start_after: the ended job's refused instances.
CREATE TABLE job_resends (
    job_id           text   COLLATE "C" NOT NULL,
    scan_id          bigint NOT NULL,
    sop_instance_uid text   COLLATE "C" NOT NULL,
    CONSTRAINT job_resends_pkey PRIMARY KEY (job_id, scan_id, sop_instance_uid)
);

-- An instance a scan's unit owes: the peer left it unaccounted, and the
-- unit's cursor moved past it, so a crash sends at most one batch again; it
-- is tried again by name, and a retry sends it while it is still owed.
CREATE TABLE job_owed (
    job_id           text   COLLATE "C" NOT NULL,
    scan_id          bigint NOT NULL,
    sop_instance_uid text   COLLATE "C" NOT NULL,
    CONSTRAINT job_owed_pkey PRIMARY KEY (job_id, scan_id, sop_instance_uid)
);

-- An instance a job's peer refused, or kept with a warning, listed by its
-- scan's labels; at most the setting job.failures-kept a job.
CREATE TABLE job_failures (
    job_id              text        COLLATE "C" NOT NULL,
    subject_label       text        COLLATE "C" NOT NULL,
    session_label       text        COLLATE "C" NOT NULL,
    scan_label          text        COLLATE "C" NOT NULL,
    sop_instance_uid    text        COLLATE "C" NOT NULL,
    scan_id             bigint      NOT NULL,
    warning             boolean     NOT NULL,
    code                text        COLLATE "C" NOT NULL,
    peer_status         integer     NOT NULL,
    sop_class_uid       text        NOT NULL,
    transfer_syntax_uid text        NOT NULL,
    detail              text        NOT NULL,
    at                  timestamptz NOT NULL,
    CONSTRAINT job_failures_pkey PRIMARY KEY (job_id, subject_label, session_label, scan_label, sop_instance_uid)
);

-- The refused alone, in the same order.
CREATE INDEX job_failures_by_kind ON job_failures (job_id, warning, subject_label, session_label, scan_label, sop_instance_uid);
-- A scan's failures by the scan's key, for a retry.
CREATE INDEX job_failures_by_scan ON job_failures (job_id, scan_id, sop_instance_uid);
