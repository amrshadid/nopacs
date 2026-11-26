package sqlkit

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// The jobs' lists' names, the first field of their cursors.
const (
	ListJobs        = "tenant-jobs"
	ListJobFailures = "tenant-job-failures"
)

// jobColumns are a job's columns as the store reads them: what a read
// answers, then what only the store's own work reads.
const jobColumns = `id, kind, sender, subject_label, session_label, scan_label, reception_id, receiver, state, reason,
	created_by_key, created, started, state_at, ended_at, retry_of, retry_job, listed, scans, scans_done, instances, bytes,
	sent, sent_bytes, warned, warned_bytes, refused, refused_bytes, failures_not_listed,
	bound_at, list_work, retry_listed, target_listed, retry_after, list_session, list_scan, list_id, failures_listed,
	progress_at, progress_mark, deid_mode, profile_name, profile_version`

// jobRow is a job as its row holds it: what a read answers, and how far its
// listing got.
type jobRow struct {
	job          tenant.Job
	bound        time.Time
	listWork     string
	retryListed  bool
	targetListed bool
	retryAfter   int64
	listSession  string
	listScan     string
	listID       int64
	listed       int64 // failures listed
	progressAt   time.Time
	progressMark int64
}

// scanJob reads one row of jobColumns.
func scanJob(row Row) (jobRow, error) {
	var r jobRow
	j := &r.job
	c := &j.Counts
	var kind, state, mode string
	var created, started, stateAt, endedAt, bound, progressAt DBTime
	err := row.Scan(&j.ID, &kind, &j.Sender, &j.Target.SubjectLabel, &j.Target.SessionLabel, &j.Target.ScanLabel,
		&j.ReceptionID, &j.Receiver, &state, &j.Reason, &j.CreatedByKeyID, &created, &started, &stateAt, &endedAt,
		&j.RetryOf, &j.RetryJob, &c.Listed, &c.Scans, &c.ScansDone, &c.Instances, &c.Bytes, &c.InstancesSent, &c.BytesSent,
		&c.InstancesWarned, &c.BytesWarned, &c.InstancesRefused, &c.BytesRefused, &j.FailuresNotListed,
		&bound, &r.listWork, &r.retryListed, &r.targetListed, &r.retryAfter, &r.listSession, &r.listScan, &r.listID,
		&r.listed, &progressAt, &r.progressMark, &mode, &j.Profile.Name, &j.Profile.Version)
	j.Kind, j.State, j.Mode = tenant.JobKind(kind), tenant.JobState(state), tenant.DeidMode(mode)
	j.Created, j.Started, j.StateTime, j.Ended = created.T, started.T, stateAt.T, endedAt.T
	r.bound, r.progressAt = bound.T, progressAt.T
	return r, err
}

// jobInTx reads a job inside a transaction with the lock given, or
// NotFound.
func jobInTx(ctx context.Context, tx SendingTx, id, lock string) (jobRow, error) {
	var out jobRow
	found := false
	err := tx.Query(ctx, `SELECT `+jobColumns+` FROM jobs WHERE id = ?`+lock, []any{id}, func(r Row) error {
		found = true
		var err error
		out, err = scanJob(r)
		return err
	})
	if err != nil {
		return out, err
	}
	if !found {
		return out, notFound("the tenant has no job with this id")
	}
	return out, fillJob(ctx, tx, &out.job)
}

// newJob is a job to insert, queued.
type newJob struct {
	kind                      tenant.JobKind
	mode                      tenant.DeidMode
	profile                   tenant.ProfileRef
	id, sender, keyID         string
	target                    tenant.JobTarget
	receptionID, receiver     string
	uniqueKey                 string
	created, bound            time.Time
	retryOf                   string
	retryListed, targetListed bool
	listSession, listScan     string
	listID                    int64
}

// insertJob writes a new job, queued, and queues its listing unit; it
// reports false, writing nothing, when a job holds its unique key. It
// returns the job's event.
func (k Sending) insertJob(ctx context.Context, tx SendingTx, n newJob) (tenant.Job, *tenant.EventRecord, error) {
	var key any
	if n.uniqueKey != "" {
		key = n.uniqueKey
	}
	listWork := tenant.JobUnitID(n.id, "")
	changed, err := tx.Exec(ctx, `INSERT INTO jobs (id, kind, sender, subject_label, session_label, scan_label, reception_id,
		receiver, unique_key, state, ended, reason, created_by_key, created, started, state_at, ended_at, bound_at, retry_of,
		retry_job, list_work, listed, retry_listed, target_listed, retry_after, list_session, list_scan, list_id, scans,
		scans_done, instances, bytes, sent, sent_bytes, warned, warned_bytes, refused, refused_bytes, failures_listed,
		failures_not_listed, progress_at, progress_mark, deid_mode, profile_name, profile_version)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 'queued', ?, '', ?, ?, NULL, ?, NULL, ?, ?, '', ?, ?, ?, ?, 0, ?, ?, ?,
		0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, NULL, 0, ?, ?, ?) ON CONFLICT (unique_key) DO NOTHING`,
		n.id, string(n.kind), n.sender, n.target.SubjectLabel, n.target.SessionLabel, n.target.ScanLabel, n.receptionID, n.receiver, key,
		k.D.Bool(false), n.keyID, k.D.Time(n.created), k.D.Time(n.created), k.D.Time(n.bound), n.retryOf, listWork,
		k.D.Bool(false), k.D.Bool(n.retryListed), k.D.Bool(n.targetListed), n.listSession, n.listScan, n.listID,
		string(n.mode), n.profile.Name, n.profile.Version)
	if err != nil || changed == 0 {
		return tenant.Job{}, nil, err
	}
	if _, _, err := tx.Enqueue(ctx, tenant.Work{ID: listWork, Kind: tenant.JobWorkKind(n.kind, n.sender), Subject: n.id,
		UniqueKey: tenant.JobListKey(n.id), Due: n.created, MaxAttempts: tenant.JobUnitAttempts}, n.created); err != nil {
		return tenant.Job{}, nil, err
	}
	r, err := jobInTx(ctx, tx, n.id, "")
	if err != nil {
		return tenant.Job{}, nil, err
	}
	e, err := tenant.JobStepEvent("", r.job, n.created)
	if err != nil {
		return tenant.Job{}, nil, err
	}
	return r.job, &e, nil
}

// lockSender holds the sender's row from removal to the commit and reports
// whether it is there.
func (k Sending) lockSender(ctx context.Context, tx SendingTx, name string) (bool, error) {
	return exists(ctx, tx, `SELECT 1 FROM senders WHERE name = ?`+k.D.Share, name)
}

// targetExists reports whether the catalog holds the target: its subject,
// its session, or its scan, by the unique indexes on folded labels with the
// labels then compared exactly.
func targetExists(ctx context.Context, tx SendingTx, t tenant.JobTarget) (bool, error) {
	switch {
	case t.Whole():
		return true, nil
	case t.ScanLabel != "":
		return exists(ctx, tx, `SELECT 1 FROM scans WHERE subject_folded = ? AND session_folded = ? AND folded = ?
			AND subject_label = ? AND session_label = ? AND label = ?`, label.Fold(t.SubjectLabel), label.Fold(t.SessionLabel),
			label.Fold(t.ScanLabel), t.SubjectLabel, t.SessionLabel, t.ScanLabel)
	case t.SessionLabel != "":
		return exists(ctx, tx, `SELECT 1 FROM sessions WHERE subject_folded = ? AND folded = ? AND subject_label = ? AND label = ?`,
			label.Fold(t.SubjectLabel), label.Fold(t.SessionLabel), t.SubjectLabel, t.SessionLabel)
	}
	return exists(ctx, tx, `SELECT 1 FROM subjects WHERE folded = ? AND label = ?`, label.Fold(t.SubjectLabel), t.SubjectLabel)
}

// CreateJob implements tenant.Jobs.
func (k Sending) CreateJob(ctx context.Context, j tenant.NewJob, audit master.AuditEntry, guard tenant.Guard) (tenant.Job, error) {
	if err := j.Validate(); err != nil {
		return tenant.Job{}, err
	}
	if err := audit.Validate(); err != nil {
		return tenant.Job{}, err
	}
	var out tenant.Job
	kind := j.KindOrSend()
	err := k.Host.Write(ctx, "the job cannot be made", func(tx SendingTx) error {
		if kind == tenant.JobSend {
			has, err := k.lockSender(ctx, tx, j.Sender)
			if err != nil {
				return err
			}
			if !has {
				return tenant.Precondition("the tenant has no sender named " + j.Sender)
			}
		} else if kind == tenant.JobScrub {
			if err := k.checkScrubJob(ctx, tx, j.Storage, !j.Target.Whole()); err != nil {
				return err
			}
		} else if kind == tenant.JobRebuild {
			if err := k.checkRebuildJob(ctx, tx, j); err != nil {
				return err
			}
		} else if kind == tenant.JobRepair {
			if err := k.checkRepairJob(ctx, tx, *j.Repair); err != nil {
				return err
			}
		} else if !j.Profile.IsZero() {
			has, err := exists(ctx, tx, `SELECT 1 FROM profile_versions WHERE name = ? AND version = ?`, j.Profile.Name, j.Profile.Version)
			if err != nil {
				return err
			}
			if !has {
				return notFound("the tenant has no profile version " + j.Profile.String())
			}
		}
		taken, err := exists(ctx, tx, `SELECT 1 FROM jobs WHERE id = ?`, j.ID)
		if err != nil {
			return err
		}
		if taken {
			return fault.New(fault.Conflict, "a job with this id exists")
		}
		if held, err := targetExists(ctx, tx, j.Target); err != nil || !held {
			if err != nil {
				return err
			}
			return notFound("the tenant holds no " + targetLevel(j.Target) + " with these labels")
		}
		if kind == tenant.JobScrub {
			// Before the job's row, so the job the insert reads back
			// carries what it reads.
			if _, err := tx.Exec(ctx, `INSERT INTO scrub_jobs (job_id, storage, scheduled) VALUES (?, ?, ?)`,
				j.ID, j.Storage, k.D.Bool(j.Scheduled)); err != nil {
				return err
			}
		}
		if kind == tenant.JobRepair {
			if err := k.insertRepairJob(ctx, tx, j.ID, *j.Repair); err != nil {
				return err
			}
		}
		if kind == tenant.JobRebuild {
			if _, err := tx.Exec(ctx, `INSERT INTO rebuild_jobs (job_id, mode, scratch, activated, dropped, from_storage)
				VALUES (?, ?, ?, ?, ?, ?)`, j.ID, string(j.Rebuild.Mode), j.Rebuild.Scratch, k.D.Bool(false), k.D.Bool(false),
				j.Rebuild.FromStorage); err != nil {
				return err
			}
		}
		job, e, err := k.insertJob(ctx, tx, newJob{kind: kind, mode: j.Mode, profile: j.Profile, id: j.ID, sender: j.Sender,
			keyID: audit.KeyID, target: j.Target, created: j.Created, bound: j.Created, retryListed: true})
		if err != nil {
			return err
		}
		out = job
		if err := tx.WriteEvents(ctx, *e); err != nil {
			return err
		}
		return tx.Audited(ctx, audit, guard)
	})
	if err != nil {
		return tenant.Job{}, err
	}
	return out, nil
}

// targetLevel names the level a target names.
func targetLevel(t tenant.JobTarget) string {
	switch {
	case t.ScanLabel != "":
		return "scan"
	case t.SessionLabel != "":
		return "session"
	}
	return "subject"
}

// Job implements tenant.Jobs.
func (k Sending) Job(ctx context.Context, id string) (tenant.Job, error) {
	if err := master.ValidateID("job id", id); err != nil {
		return tenant.Job{}, err
	}
	var out tenant.Job
	err := k.Host.Read(ctx, "the job cannot be read", func(tx SendingTx) error {
		r, err := jobInTx(ctx, tx, id, "")
		out = r.job
		return err
	})
	return out, err
}

// jobBinding is what a list's cursor is bound to: the tenant and the
// filter.
func (k Sending) jobBinding(f tenant.JobFilter) string {
	states := make([]string, len(f.States))
	for i, s := range f.States {
		states[i] = string(s)
	}
	return k.Host.Binding(strings.Join(states, ","), strconv.FormatBool(f.InProgress), f.Sender, MicrosText(f.Since),
		MicrosText(f.Until))
}

// JobsQuery returns the statement and arguments of one page of ListJobs.
// It is put together from fixed fragments by which filters are set, so each
// reads the index named for it in the jobs' migration; every value is a
// parameter. after is the (created, id) of the last row returned, or nil.
// The empty statement means no job can match.
func (d SendingDialect) JobsQuery(f tenant.JobFilter, after *TimePosition, limit int) (string, []any) {
	var where []string
	var args []any
	if f.Sender != "" {
		where, args = append(where, "sender = ?"), append(args, f.Sender)
	}
	states, none := f.NarrowStates()
	if none {
		return "", nil
	}
	if f.InProgress && len(states) == 0 {
		where = append(where, d.NotEnded)
	}
	if !f.Since.IsZero() {
		where, args = append(where, "created >= ?"), append(args, d.Time(f.Since))
	}
	if !f.Until.IsZero() {
		where, args = append(where, "created < ?"), append(args, d.Time(f.Until))
	}
	if after != nil {
		where, args = append(where, "(created, id) < (?, ?)"), append(args, d.Time(after.At), after.ID)
	}
	const order = " ORDER BY created DESC, id DESC LIMIT ?"
	page := func(where []string) string {
		query := "SELECT " + jobColumns + " FROM jobs"
		if len(where) > 0 {
			query += " WHERE " + strings.Join(where, " AND ") // #nosec G202 -- every fragment is a constant above; values are parameters.
		}
		return query + order
	}
	if len(states) <= 1 {
		if len(states) == 1 {
			where, args = append([]string{"state = ?"}, where...), append([]any{string(states[0])}, args...)
		}
		return page(where), append(args, limit)
	}
	// Several states: one page per state, each read from jobs_by_state and
	// bounded by the limit, merged and cut to the limit, as receptions are
	// listed by several steps.
	parts := make([]string, len(states))
	var all []any
	for i, s := range states {
		parts[i] = "SELECT * FROM (" + page(append([]string{"state = ?"}, where...)) + ") AS s" + strconv.Itoa(i)
		all = append(append(append(all, string(s)), args...), limit)
	}
	return strings.Join(parts, " UNION ALL ") + order, append(all, limit)
}

// ListJobs implements tenant.Jobs.
func (k Sending) ListJobs(ctx context.Context, filter tenant.JobFilter, page master.Page) ([]tenant.Job, string, error) {
	if err := filter.Validate(); err != nil {
		return nil, "", err
	}
	binding := k.jobBinding(filter)
	pos, err := PageAfterTime(page, ListJobs, binding)
	if err != nil {
		return nil, "", err
	}
	query, args := k.D.JobsQuery(filter, pos, page.Limit+1)
	var out []tenant.Job
	err = k.Host.Read(ctx, "the jobs cannot be listed", func(tx SendingTx) error {
		out = nil
		if query == "" {
			return nil
		}
		if err := tx.Query(ctx, query, args, func(r Row) error {
			j, err := scanJob(r)
			out = append(out, j.job)
			return err
		}); err != nil {
			return err
		}
		for i := range out {
			if err := fillJob(ctx, tx, &out[i]); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, "", err
	}
	out, last, more := Cut(out, page.Limit)
	if !more {
		return out, "", nil
	}
	return out, TimeCursor(ListJobs, binding, last.Created, last.ID), nil
}

// CancelJob implements tenant.Jobs.
func (k Sending) CancelJob(ctx context.Context, id string, now time.Time, audit master.AuditEntry, guard tenant.Guard) (tenant.Job, error) {
	if err := master.ValidateID("job id", id); err != nil {
		return tenant.Job{}, err
	}
	if err := master.ValidateTime("time", now); err != nil {
		return tenant.Job{}, err
	}
	if err := audit.Validate(); err != nil {
		return tenant.Job{}, err
	}
	var out tenant.Job
	err := k.Host.Write(ctx, "the job cannot be canceled", func(tx SendingTx) error {
		cancel := k.cancelInTx
		if kind, err := jobKind(ctx, tx, id); err != nil {
			return err
		} else if kind == tenant.JobImport {
			cancel = k.cancelImportInTx
		} else if kind == tenant.JobRebuild {
			cancel = k.cancelRebuildInTx
		}
		job, e, err := cancel(ctx, tx, id, tenant.JobReasonCanceled, now)
		if err != nil {
			return err
		}
		out = job
		if e != nil {
			if err := tx.WriteEvents(ctx, *e); err != nil {
				return err
			}
		}
		return tx.Audited(ctx, audit, guard)
	})
	if err != nil {
		return tenant.Job{}, err
	}
	return out, nil
}

// jobKind reads a job's kind, which never changes, without a lock; NotFound
// when the tenant has no job of that id.
func jobKind(ctx context.Context, tx SendingTx, id string) (tenant.JobKind, error) {
	var kind string
	found, err := tx.QueryRow(ctx, `SELECT kind FROM jobs WHERE id = ?`, []any{id}, &kind)
	if err != nil {
		return "", err
	}
	if !found {
		return "", notFound("the tenant has no job with this id")
	}
	return tenant.JobKind(kind), nil
}

// cancelInTx ends a job as canceled with the reason, and the units of it
// that are queued and no transaction holds (endQueuedUnits), and returns it
// with its event; one canceled already is returned as it stands with no
// event, and one ended otherwise is a precondition failure. A unit a
// worker holds keeps its scan open: its next write is recorded and tells
// it of the cancel.
func (k Sending) cancelInTx(ctx context.Context, tx SendingTx, id, reason string, now time.Time) (tenant.Job, *tenant.EventRecord, error) {
	r, err := jobInTx(ctx, tx, id, k.D.Lock)
	if err != nil {
		return tenant.Job{}, nil, err
	}
	if r.job.State == tenant.JobCanceled {
		return r.job, nil, nil
	}
	if r.job.State.Ended() {
		return tenant.Job{}, nil, tenant.Precondition("the job has ended " + string(r.job.State) + "; only a job queued or running is canceled")
	}
	units, err := k.endQueuedUnits(ctx, tx, r, tenant.WorkReasonJobCanceled, now)
	if err != nil {
		return tenant.Job{}, nil, err
	}
	var ended int64
	for _, u := range units {
		n, err := tx.Exec(ctx, `UPDATE job_scans SET ended = ? WHERE work_id = ? AND ended = ?`, k.D.Bool(true), u, k.D.Bool(false))
		if err != nil {
			return tenant.Job{}, nil, err
		}
		ended += n
	}
	if _, err := tx.Exec(ctx, `UPDATE jobs SET state = 'canceled', ended = ?, reason = ?, state_at = ?, ended_at = ?,
		scans_done = scans_done + ? WHERE id = ?`, k.D.Bool(true), reason, k.D.Time(now), k.D.Time(now), ended, id); err != nil {
		return tenant.Job{}, nil, err
	}
	after, err := jobInTx(ctx, tx, id, "")
	if err != nil {
		return tenant.Job{}, nil, err
	}
	e, err := tenant.JobStepEvent(r.job.State, after.job, now)
	if err != nil {
		return tenant.Job{}, nil, err
	}
	return after.job, &e, nil
}

// RetryJob implements tenant.Jobs.
func (k Sending) RetryJob(ctx context.Context, id, newID string, now time.Time, audit master.AuditEntry, guard tenant.Guard) (tenant.Job, tenant.Job, error) {
	if err := master.ValidateID("job id", id); err != nil {
		return tenant.Job{}, tenant.Job{}, err
	}
	if err := master.ValidateID("job id", newID); err != nil {
		return tenant.Job{}, tenant.Job{}, err
	}
	if err := master.ValidateTime("time", now); err != nil {
		return tenant.Job{}, tenant.Job{}, err
	}
	if err := audit.Validate(); err != nil {
		return tenant.Job{}, tenant.Job{}, err
	}
	var ended, retry tenant.Job
	err := k.Host.Write(ctx, "the job cannot be retried", func(tx SendingTx) error {
		// The sender's row before the job's, in the order of the locks.
		first, err := jobInTx(ctx, tx, id, "")
		if err != nil {
			return err
		}
		if first.job.Kind == tenant.JobImport {
			return tenant.Precondition("an import job is not retried by this store; a new job over the same path counts what is held as already held")
		}
		if first.job.Kind == tenant.JobScrub {
			return tenant.Precondition("a scrub job is not retried; a new scrub reads every object again")
		}
		if first.job.Kind == tenant.JobRebuild {
			return tenant.Precondition("a rebuild job is not retried; its shadow is dropped when it ends, and a new rebuild builds again")
		}
		if first.job.Kind == tenant.JobRepair {
			return tenant.Precondition("a repair job is not retried; a new repair mends the findings that still hold")
		}
		has := true
		if first.job.Kind == tenant.JobSend {
			if has, err = k.lockSender(ctx, tx, first.job.Sender); err != nil {
				return err
			}
		}
		old, err := jobInTx(ctx, tx, id, k.D.Lock)
		if err != nil {
			return err
		}
		if old.job.RetryJob != "" {
			r, err := jobInTx(ctx, tx, old.job.RetryJob, "")
			if err != nil {
				return err
			}
			ended, retry = old.job, r.job
			a, err := AddDetails(audit, tenant.DetailRetryJob, r.job.ID)
			if err != nil {
				return err
			}
			return tx.Audited(ctx, a, guard)
		}
		if !old.job.State.Ended() {
			return tenant.Precondition("the job is " + string(old.job.State) + "; only an ended job is retried")
		}
		if !has {
			return tenant.Precondition("the job's sender " + old.job.Sender + " has been removed")
		}
		c := old.job.Counts
		if c.NothingToRetry() {
			return tenant.Precondition("the job sent every instance; there is nothing to send again")
		}
		taken, err := exists(ctx, tx, `SELECT 1 FROM jobs WHERE id = ?`, newID)
		if err != nil {
			return err
		}
		if taken {
			return fault.New(fault.Conflict, "a job with this id exists")
		}
		job, e, err := k.insertJob(ctx, tx, newJob{kind: old.job.Kind, mode: old.job.Mode, profile: old.job.Profile,
			id: newID, sender: old.job.Sender, keyID: audit.KeyID,
			target: old.job.Target, receptionID: old.job.ReceptionID, receiver: old.job.Receiver, created: now,
			bound: old.bound, retryOf: id, targetListed: c.Listed, listSession: old.listSession, listScan: old.listScan,
			listID: old.listID})
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE jobs SET retry_job = ? WHERE id = ?`, newID, id); err != nil {
			return err
		}
		old.job.RetryJob = newID
		ended, retry = old.job, job
		if err := tx.WriteEvents(ctx, *e); err != nil {
			return err
		}
		a, err := AddDetails(audit, tenant.DetailRetryJob, newID)
		if err != nil {
			return err
		}
		return tx.Audited(ctx, a, guard)
	})
	if err != nil {
		return tenant.Job{}, tenant.Job{}, err
	}
	return ended, retry, nil
}

// failureColumns are a failure's columns as a list reads them.
const failureColumns = `subject_label, session_label, scan_label, sop_instance_uid, code, peer_status, sop_class_uid,
	transfer_syntax_uid, detail, at`

// ListJobFailures implements tenant.Jobs.
func (k Sending) ListJobFailures(ctx context.Context, id string, includeWarnings bool, page master.Page) ([]tenant.JobFailure, string, error) {
	if err := master.ValidateID("job id", id); err != nil {
		return nil, "", err
	}
	if err := page.Validate(); err != nil {
		return nil, "", err
	}
	binding := k.Host.Binding(id, strconv.FormatBool(includeWarnings))
	after := []string{"", "", "", ""}
	if page.Cursor != "" {
		f, err := DecodeCursor(page.Cursor, ListJobFailures, binding, 4)
		if err != nil {
			return nil, "", err
		}
		after = f
	}
	query, args := k.D.FailuresQuery(id, includeWarnings, after, page.Limit+1)
	var out []tenant.JobFailure
	err := k.Host.Read(ctx, "the job's failures cannot be listed", func(tx SendingTx) error {
		out = nil
		if has, err := exists(ctx, tx, `SELECT 1 FROM jobs WHERE id = ?`, id); err != nil || !has {
			if err != nil {
				return err
			}
			return notFound("the tenant has no job with this id")
		}
		return tx.Query(ctx, query, args, func(r Row) error {
			var f tenant.JobFailure
			var code string
			var at DBTime
			err := r.Scan(&f.Path.Subject, &f.Path.Session, &f.Path.Scan, &f.SOPInstanceUID, &code, &f.PeerStatus,
				&f.SOPClassUID, &f.TransferSyntaxUID, &f.Detail, &at)
			f.Code, f.Time = tenant.JobFailureCode(code), at.T
			out = append(out, f)
			return err
		})
	})
	if err != nil {
		return nil, "", err
	}
	out, last, more := Cut(out, page.Limit)
	if !more {
		return out, "", nil
	}
	return out, EncodeCursor(ListJobFailures, binding, last.Path.Subject, last.Path.Session, last.Path.Scan, last.SOPInstanceUID), nil
}

// FailuresQuery returns the statement and arguments of one page of a job's
// failures after the (subject, session, scan, instance) given: a range of
// the table's key, or of job_failures_by_kind for the refused alone.
func (d SendingDialect) FailuresQuery(id string, includeWarnings bool, after []string, limit int) (string, []any) {
	where := "job_id = ?"
	args := []any{id}
	if !includeWarnings {
		where += " AND warning = ?"
		args = append(args, d.Bool(false))
	}
	where += " AND (subject_label, session_label, scan_label, sop_instance_uid) > (?, ?, ?, ?)"
	args = append(args, after[0], after[1], after[2], after[3], limit)
	return `SELECT ` + failureColumns + ` FROM job_failures WHERE ` + where +
		` ORDER BY subject_label, session_label, scan_label, sop_instance_uid LIMIT ?`, args
}
