package sqlkit

import (
	"context"
	"strconv"
	"time"

	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// Batch deletes (tenant.DeleteBatches) on a SQL database: the preview of
// many targets under one token, the job a confirmed preview makes, and
// what became of each target. Each target is deleted by the ordinary
// delete (PrepareCorrection with its Batch set, checkBatchTarget), never
// here.

// batchPreviewIn makes the preview of a batch delete of targets with
// reason at now, in one read of the catalog: each target's delete preview
// and its fingerprint, the totals, and the token.
func (k Sending) batchPreviewIn(ctx context.Context, tx SendingTx, targets []tenant.CorrectionTarget, reason string, now time.Time) (tenant.DeleteBatchPreview, error) {
	p := tenant.DeleteBatchPreview{Reason: reason, Issued: now.UTC().Truncate(time.Microsecond), Items: make([]tenant.BatchItem, 0, len(targets))}
	for i, t := range targets {
		dp, err := k.previewIn(ctx, tx, t, p.Issued)
		if fault.KindOf(err) == fault.NotFound {
			return p, fault.Wrap(fault.NotFound, err, "target "+strconv.Itoa(i+1)+": "+fault.Message(err))
		}
		if err != nil {
			return p, err
		}
		p.Items = append(p.Items, tenant.BatchItem{DeletePreview: dp, Fingerprint: tenant.DeleteFingerprint(dp)})
		c := dp.Counts
		p.Counts.Subjects += c.Subjects
		p.Counts.Sessions += c.Sessions
		p.Counts.Scans += c.Scans
		p.Counts.Instances += c.Instances
		p.Counts.Bytes += c.Bytes
		if len(dp.Holds) > 0 {
			p.Held++
		} else if dp.SessionsUnderMinimum > 0 {
			p.Kept++
		}
	}
	p.Confirm = tenant.BatchConfirm(p)
	return p, nil
}

// PreviewDeleteBatch implements tenant.DeleteBatches.
func (k Sending) PreviewDeleteBatch(ctx context.Context, targets []tenant.CorrectionTarget, reason string, now time.Time) (tenant.DeleteBatchPreview, error) {
	if err := tenant.ValidateBatchTargets(targets, tenant.MaxDeleteBatchTargets); err != nil {
		return tenant.DeleteBatchPreview{}, err
	}
	if err := tenant.ValidateUndoReason(reason); err != nil {
		return tenant.DeleteBatchPreview{}, err
	}
	if err := master.ValidateTime("preview time", now); err != nil {
		return tenant.DeleteBatchPreview{}, err
	}
	var out tenant.DeleteBatchPreview
	err := k.Host.Read(ctx, "the batch delete cannot be previewed", func(tx SendingTx) error {
		var err error
		out, err = k.batchPreviewIn(ctx, tx, targets, reason, now)
		return err
	})
	return out, err
}

// QueueDeleteBatch implements tenant.DeleteBatches. The preview is made
// again in a read of its own, not in the transaction that makes the job: a
// preview of ten thousand targets would hold the database's one writer for
// as long as it reads. What changes between that read and the job's row is
// met by the runner, which makes each target's preview again when its turn
// comes and skips one whose fingerprint differs.
func (k Sending) QueueDeleteBatch(ctx context.Context, q tenant.DeleteBatchQueue, audit master.AuditEntry, guard tenant.Guard) (tenant.Job, bool, error) {
	if err := q.Validate(); err != nil {
		return tenant.Job{}, false, err
	}
	if err := audit.Validate(); err != nil {
		return tenant.Job{}, false, err
	}
	if job, used, err := k.batchOfToken(ctx, q, audit, guard); err != nil || used {
		return job, false, err
	}
	issued, _ := tenant.BatchConfirmIssued(q.Confirm)
	if q.Now.Sub(issued) > tenant.MaxConfirmAge || issued.After(q.Now.Add(time.Minute)) {
		return tenant.Job{}, false, tenant.Precondition("the confirm token is not one of a batch delete's preview made within the hour")
	}
	var preview tenant.DeleteBatchPreview
	err := k.Host.Read(ctx, "the batch delete cannot be queued", func(tx SendingTx) error {
		var err error
		preview, err = k.batchPreviewIn(ctx, tx, q.Targets, q.Reason, issued)
		return err
	})
	if fault.KindOf(err) == fault.NotFound {
		err = fault.Wrap(fault.Precondition, err, "the confirm token does not match what the batch would now delete: "+fault.Message(err))
	}
	if err != nil {
		return tenant.Job{}, false, err
	}
	if preview.Confirm != q.Confirm {
		return tenant.Job{}, false, tenant.Precondition("the confirm token does not match the targets, the reason, or what they hold now; preview the batch again")
	}
	var job tenant.Job
	made := false
	err = k.Host.Write(ctx, "the batch delete cannot be queued", func(tx SendingTx) error {
		made = false
		if err := tx.LockConfig(ctx); err != nil {
			return err
		}
		id, err := k.batchJobOfToken(ctx, tx, q)
		if err != nil {
			return err
		}
		if id != "" {
			r, err := jobInTx(ctx, tx, id, "")
			if err != nil {
				return err
			}
			job = r.job
			return tx.Audited(ctx, answeredEntry(audit, q.ID, id), guard)
		}
		if err := k.checkNoRebuild(ctx, tx); err != nil {
			return err
		}
		busy, err := exists(ctx, tx, `SELECT 1 FROM jobs WHERE kind = 'delete_batch' AND `+k.D.NotEnded)
		if err != nil {
			return err
		}
		if busy {
			return tenant.Precondition("the tenant has a batch delete job not ended; one runs at a time")
		}
		if taken, err := exists(ctx, tx, `SELECT 1 FROM jobs WHERE id = ?`, q.ID); err != nil || taken {
			if err != nil {
				return err
			}
			return fault.New(fault.Conflict, "a job with this id exists")
		}
		if job, err = k.insertBatchJob(ctx, tx, q, preview); err != nil {
			return err
		}
		made = true
		return tx.Audited(ctx, audit, guard)
	})
	return job, made, err
}

// batchOfToken answers the job a used token made, read before any preview
// is made again: once the job deleted its targets no preview would match.
func (k Sending) batchOfToken(ctx context.Context, q tenant.DeleteBatchQueue, audit master.AuditEntry, guard tenant.Guard) (tenant.Job, bool, error) {
	var job tenant.Job
	used := false
	err := k.Host.Write(ctx, "the batch delete cannot be queued", func(tx SendingTx) error {
		used = false
		id, err := k.batchJobOfToken(ctx, tx, q)
		if err != nil || id == "" {
			return err
		}
		r, err := jobInTx(ctx, tx, id, "")
		if err != nil {
			return err
		}
		job, used = r.job, true
		return tx.Audited(ctx, answeredEntry(audit, q.ID, id), guard)
	})
	return job, used, err
}

// batchJobOfToken returns the job q's token made, "" for none. A token
// answers only the batch it was given for: a repeat naming other targets or
// another reason would otherwise be told a job it did not ask for was made
// for it.
func (k Sending) batchJobOfToken(ctx context.Context, tx SendingTx, q tenant.DeleteBatchQueue) (string, error) {
	id, reason := "", ""
	if _, err := tx.QueryRow(ctx, `SELECT job_id, reason FROM delete_batch_jobs WHERE confirm_token = ?`, []any{q.Confirm}, &id, &reason); err != nil {
		return "", err
	}
	if id == "" {
		return "", nil
	}
	same := reason == q.Reason
	n := 0
	err := tx.Query(ctx, `SELECT subject_label, session_label, scan_label FROM delete_batch_targets WHERE job_id = ? ORDER BY ord`, []any{id},
		func(row Row) error {
			var t tenant.CorrectionTarget
			if err := row.Scan(&t.Subject, &t.Session, &t.Scan); err != nil {
				return err
			}
			if n >= len(q.Targets) || q.Targets[n] != t {
				same = false
			}
			n++
			return nil
		})
	if err != nil {
		return "", err
	}
	if !same || n != len(q.Targets) {
		return "", tenant.Precondition("the confirm token was used for a batch delete of other targets or with another reason")
	}
	return id, nil
}

// insertBatchJob writes a batch delete job of the preview's targets,
// queued and every target listed, with its event.
func (k Sending) insertBatchJob(ctx context.Context, tx SendingTx, q tenant.DeleteBatchQueue, p tenant.DeleteBatchPreview) (tenant.Job, error) {
	if _, err := tx.Exec(ctx, `INSERT INTO jobs (id, kind, sender, subject_label, session_label, scan_label, reception_id,
		receiver, unique_key, state, ended, reason, created_by_key, created, started, state_at, ended_at, bound_at, retry_of,
		retry_job, list_work, listed, retry_listed, target_listed, retry_after, list_session, list_scan, list_id, scans,
		scans_done, instances, bytes, sent, sent_bytes, warned, warned_bytes, refused, refused_bytes, failures_listed,
		failures_not_listed, progress_at, progress_mark, deid_mode, profile_name, profile_version)
		VALUES (?, 'delete_batch', '', '', '', '', '', '', NULL, 'queued', ?, '', ?, ?, NULL, ?, NULL, ?, '', '', '', ?, ?, ?, 0, '', '', 0,
		?, 0, ?, ?, 0, 0, 0, 0, 0, 0, 0, 0, NULL, 0, '', '', 0)`,
		q.ID, k.D.Bool(false), q.KeyID, k.D.Time(q.Now), k.D.Time(q.Now), k.D.Time(q.Now), k.D.Bool(true), k.D.Bool(true), k.D.Bool(true),
		int64(len(p.Items)), p.Counts.Instances, p.Counts.Bytes); err != nil {
		return tenant.Job{}, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO delete_batch_jobs (job_id, reason, confirm_token, paused) VALUES (?, ?, ?, ?)`, q.ID, q.Reason,
		q.Confirm, k.D.Bool(false)); err != nil {
		return tenant.Job{}, err
	}
	for i, it := range p.Items {
		t := it.Target
		if _, err := tx.Exec(ctx, `INSERT INTO delete_batch_targets (job_id, ord, subject_label, session_label, scan_label, fingerprint,
			instances, bytes, outcome, correction_id) VALUES (?, ?, ?, ?, ?, ?, ?, ?, '', '')`, q.ID, i+1, t.Subject, t.Session, t.Scan,
			it.Fingerprint, it.Counts.Instances, it.Counts.Bytes); err != nil {
			return tenant.Job{}, err
		}
	}
	r, err := jobInTx(ctx, tx, q.ID, "")
	if err != nil {
		return tenant.Job{}, err
	}
	e, err := tenant.JobStepEvent("", r.job, q.Now)
	if err != nil {
		return tenant.Job{}, err
	}
	return r.job, tx.WriteEvents(ctx, e)
}

// batchJobIn reads a batch delete job's own part with the lock given, or
// NotFound.
func (k Sending) batchJobIn(ctx context.Context, tx SendingTx, id, lock string) (tenant.DeleteBatchJob, error) {
	var out tenant.DeleteBatchJob
	found, err := tx.QueryRow(ctx, `SELECT reason, paused FROM delete_batch_jobs WHERE job_id = ?`+lock, []any{id}, &out.Reason, &out.Paused)
	if err != nil {
		return out, err
	}
	if !found {
		return out, notFound("the tenant has no batch delete job with this id")
	}
	r, err := jobInTx(ctx, tx, id, lock)
	if err != nil {
		return out, err
	}
	out.Job, out.Targets = r.job, r.job.Counts.Scans
	return out, nil
}

// DeleteBatchJob implements tenant.DeleteBatches.
func (k Sending) DeleteBatchJob(ctx context.Context, id string) (tenant.DeleteBatchJob, error) {
	if err := master.ValidateID("job id", id); err != nil {
		return tenant.DeleteBatchJob{}, err
	}
	var out tenant.DeleteBatchJob
	err := k.Host.Read(ctx, "the batch delete job cannot be read", func(tx SendingTx) error {
		var err error
		out, err = k.batchJobIn(ctx, tx, id, "")
		return err
	})
	return out, err
}

// DeleteBatchTargets implements tenant.DeleteBatches.
func (k Sending) DeleteBatchTargets(ctx context.Context, id string, after, limit int) ([]tenant.DeleteBatchTarget, error) {
	if err := master.ValidateID("job id", id); err != nil {
		return nil, err
	}
	if after < 0 || limit < 1 || limit > tenant.MaxBatchTargetPage {
		return nil, fault.New(fault.Invalid, "a page of a batch's targets starts at 0 or after and holds 1 to "+
			strconv.Itoa(tenant.MaxBatchTargetPage))
	}
	var out []tenant.DeleteBatchTarget
	err := k.Host.Read(ctx, "the batch delete job's targets cannot be read", func(tx SendingTx) error {
		out = nil
		if has, err := exists(ctx, tx, `SELECT 1 FROM delete_batch_jobs WHERE job_id = ?`, id); err != nil || !has {
			if err != nil {
				return err
			}
			return notFound("the tenant has no batch delete job with this id")
		}
		return tx.Query(ctx, `SELECT ord, subject_label, session_label, scan_label, fingerprint, instances, bytes, outcome, correction_id
			FROM delete_batch_targets WHERE job_id = ? AND ord > ? ORDER BY ord LIMIT ?`, []any{id, after, limit}, func(row Row) error {
			var t tenant.DeleteBatchTarget
			var outcome string
			err := row.Scan(&t.Ord, &t.Target.Subject, &t.Target.Session, &t.Target.Scan, &t.Fingerprint, &t.Instances, &t.Bytes, &outcome,
				&t.Correction)
			t.Outcome = tenant.BatchOutcome(outcome)
			out = append(out, t)
			return err
		})
	})
	return out, err
}

// checkBatchTarget refuses a batch's delete (ErrBatchStopped) unless its
// job exists, has not ended and is not paused, and its target is the one
// the job lists at that place, not yet met, with no delete recorded on it
// but one abandoned. The job's rows are read under the dialect's row lock,
// which a cancel's and a pause's updates of them take too, so a cancel or a
// pause committed before this transaction reads them stops the delete, and
// one after it waits for the delete's commit.
func (k Sending) checkBatchTarget(ctx context.Context, tx SendingTx, p tenant.CorrectionPrepare) error {
	b := p.Batch
	bj, err := k.batchJobIn(ctx, tx, b.JobID, k.D.Lock)
	if fault.KindOf(err) == fault.NotFound {
		return tenant.BatchStoppedError("does not exist")
	}
	if err != nil {
		return err
	}
	switch {
	case bj.Job.State.Ended():
		return tenant.BatchStoppedError("has ended " + string(bj.Job.State))
	case bj.Paused:
		return tenant.BatchStoppedError("is paused")
	}
	var t tenant.CorrectionTarget
	var outcome, correction string
	found, err := tx.QueryRow(ctx, `SELECT subject_label, session_label, scan_label, outcome, correction_id FROM delete_batch_targets
		WHERE job_id = ? AND ord = ?`+k.D.Lock, []any{b.JobID, b.Ord}, &t.Subject, &t.Session, &t.Scan, &outcome, &correction)
	if err != nil {
		return err
	}
	switch {
	case !found || t != p.Target:
		return tenant.Precondition("the batch delete job lists no such target at that place")
	case outcome != "":
		return tenant.BatchTargetTakenError("the batch delete job met that target already")
	}
	return checkBatchDelete(ctx, tx, correction)
}

// checkBatchDelete refuses a step on a batch's target whose recorded delete
// (correction, "" for none) was not abandoned: a second delete of it, or a
// skip recorded over it, would misstate what became of it.
func checkBatchDelete(ctx context.Context, tx SendingTx, correction string) error {
	if correction == "" {
		return nil
	}
	var state string
	if _, err := tx.QueryRow(ctx, `SELECT state FROM corrections WHERE id = ?`, []any{correction}, &state); err != nil {
		return err
	}
	if state != "abandoned" {
		return tenant.BatchTargetTakenError("the batch delete job made a delete of that target already")
	}
	return nil
}

// recordBatchDelete records on a batch's target the delete prepared for
// it, in the prepare's transaction.
func recordBatchDelete(ctx context.Context, tx SendingTx, p tenant.CorrectionPrepare) error {
	_, err := tx.Exec(ctx, `UPDATE delete_batch_targets SET correction_id = ? WHERE job_id = ? AND ord = ?`, p.ID, p.Batch.JobID, p.Batch.Ord)
	return err
}

// RecordBatchTarget implements tenant.DeleteBatches.
func (k Sending) RecordBatchTarget(ctx context.Context, r tenant.BatchRecord) (tenant.Job, error) {
	if err := r.Validate(); err != nil {
		return tenant.Job{}, err
	}
	var out tenant.Job
	err := k.Host.Write(ctx, "the batch target's outcome cannot be recorded", func(tx SendingTx) error {
		bj, err := k.batchJobIn(ctx, tx, r.JobID, k.D.Lock)
		if err != nil {
			return err
		}
		out = bj.Job
		// An ended job's counts are final: a cancel stops what the runner
		// records, as it stops what it deletes. A pause does not: a delete
		// made before it is recorded.
		if bj.Job.State.Ended() {
			return tenant.BatchStoppedError("has ended " + string(bj.Job.State))
		}
		var outcome, correction string
		var instances, bytes int64
		found, err := tx.QueryRow(ctx, `SELECT outcome, correction_id, instances, bytes FROM delete_batch_targets WHERE job_id = ? AND ord = ?`+
			k.D.Lock, []any{r.JobID, r.Ord}, &outcome, &correction, &instances, &bytes)
		if err != nil {
			return err
		}
		if !found {
			return notFound("the batch delete job lists no target at that place")
		}
		if outcome != "" {
			return nil
		}
		if r.Outcome == tenant.BatchDeleted && r.Correction != correction {
			return tenant.Precondition("a target is recorded deleted only by the delete its prepare recorded on it")
		}
		if r.Outcome != tenant.BatchDeleted {
			if err := checkBatchDelete(ctx, tx, correction); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, `UPDATE delete_batch_targets SET outcome = ? WHERE job_id = ? AND ord = ?`, string(r.Outcome), r.JobID,
			r.Ord); err != nil {
			return err
		}
		var sent, sentBytes, refused, refusedBytes int64
		if r.Outcome == tenant.BatchDeleted {
			sent, sentBytes = instances, bytes
		} else {
			refused, refusedBytes = instances, bytes
		}
		if _, err := tx.Exec(ctx, `UPDATE jobs SET scans_done = scans_done + 1, sent = sent + ?, sent_bytes = sent_bytes + ?,
			refused = refused + ?, refused_bytes = refused_bytes + ? WHERE id = ?`, sent, sentBytes, refused, refusedBytes, r.JobID); err != nil {
			return err
		}
		if err := k.addTallies(ctx, tx, r.JobID, map[string]int64{string(r.Outcome): 1}); err != nil {
			return err
		}
		events, err := k.batchProgressed(ctx, tx, bj.Job, r.Now)
		if err != nil {
			return err
		}
		after, err := jobInTx(ctx, tx, r.JobID, "")
		if err != nil {
			return err
		}
		out = after.job
		return tx.WriteEvents(ctx, events...)
	})
	if err != nil {
		return out, err
	}
	return out, nil
}

// batchProgressed moves a batch job queued when its first target was met
// to running, with its event, or writes its progress event when one is
// due.
func (k Sending) batchProgressed(ctx context.Context, tx SendingTx, before tenant.Job, now time.Time) ([]tenant.EventRecord, error) {
	if before.State == tenant.JobQueued {
		if _, err := tx.Exec(ctx, `UPDATE jobs SET state = 'running', started = ?, state_at = ? WHERE id = ? AND state = 'queued'`,
			k.D.Time(now), k.D.Time(now), before.ID); err != nil {
			return nil, err
		}
		after, err := jobInTx(ctx, tx, before.ID, "")
		if err != nil {
			return nil, err
		}
		e, err := tenant.JobStepEvent(tenant.JobQueued, after.job, now)
		if err != nil {
			return nil, err
		}
		return []tenant.EventRecord{e}, nil
	}
	after, err := jobInTx(ctx, tx, before.ID, "")
	if err != nil {
		return nil, err
	}
	if !tenant.ProgressEventDue(after.job, after.progressMark, after.progressAt, now) {
		return nil, nil
	}
	e, err := tenant.JobProgressEvent(after.job, now)
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `UPDATE jobs SET progress_at = ?, progress_mark = ? WHERE id = ?`, k.D.Time(now),
		tenant.ProgressMark(after.job.Counts), before.ID); err != nil {
		return nil, err
	}
	return []tenant.EventRecord{e}, nil
}

// EndDeleteBatchJob implements tenant.DeleteBatches.
func (k Sending) EndDeleteBatchJob(ctx context.Context, id string, now time.Time) (tenant.Job, error) {
	if err := master.ValidateID("job id", id); err != nil {
		return tenant.Job{}, err
	}
	if err := master.ValidateTime("time", now); err != nil {
		return tenant.Job{}, err
	}
	var out tenant.Job
	err := k.Host.Write(ctx, "the batch delete job cannot be ended", func(tx SendingTx) error {
		bj, err := k.batchJobIn(ctx, tx, id, k.D.Lock)
		if err != nil {
			return err
		}
		out = bj.Job
		if bj.Job.State.Ended() {
			return nil
		}
		if pending, err := exists(ctx, tx, `SELECT 1 FROM delete_batch_targets WHERE job_id = ? AND outcome = ''`, id); err != nil || pending {
			if err != nil {
				return err
			}
			return tenant.Precondition("the batch delete job lists targets whose outcome is not recorded yet")
		}
		// A skip is an outcome: the target was looked at and left for the
		// reason recorded, so a job that skipped any ends partial.
		state, reason := tenant.EndState(bj.Job.Counts)
		ended, events, err := k.endJob(ctx, tx, bj.Job, state, reason, now)
		if err != nil {
			return err
		}
		out = ended
		return tx.WriteEvents(ctx, events...)
	})
	if err != nil {
		return tenant.Job{}, err
	}
	return out, nil
}

// StopDeleteBatchJob implements tenant.DeleteBatches.
func (k Sending) StopDeleteBatchJob(ctx context.Context, id, reason string, now time.Time) (tenant.Job, error) {
	if err := master.ValidateID("job id", id); err != nil {
		return tenant.Job{}, err
	}
	if reason == "" || len(reason) > tenant.MaxFailureLength {
		return tenant.Job{}, fault.New(fault.Invalid, "a batch delete job is stopped with a reason of 1 to "+strconv.Itoa(tenant.MaxFailureLength)+" bytes")
	}
	if err := master.ValidateTime("time", now); err != nil {
		return tenant.Job{}, err
	}
	var out tenant.Job
	err := k.Host.Write(ctx, "the batch delete job cannot be stopped", func(tx SendingTx) error {
		bj, err := k.batchJobIn(ctx, tx, id, k.D.Lock)
		if err != nil {
			return err
		}
		out = bj.Job
		if bj.Job.State.Ended() {
			return nil
		}
		ended, events, err := k.endJob(ctx, tx, bj.Job, tenant.JobFailed, reason, now)
		if err != nil {
			return err
		}
		out = ended
		return tx.WriteEvents(ctx, events...)
	})
	if err != nil {
		return tenant.Job{}, err
	}
	return out, nil
}

// PauseDeleteBatch implements tenant.DeleteBatches.
func (k Sending) PauseDeleteBatch(ctx context.Context, id string, paused bool, now time.Time, audit master.AuditEntry,
	guard tenant.Guard) (tenant.DeleteBatchJob, error) {
	if err := master.ValidateID("job id", id); err != nil {
		return tenant.DeleteBatchJob{}, err
	}
	if err := master.ValidateTime("time", now); err != nil {
		return tenant.DeleteBatchJob{}, err
	}
	if err := audit.Validate(); err != nil {
		return tenant.DeleteBatchJob{}, err
	}
	var out tenant.DeleteBatchJob
	err := k.Host.Write(ctx, "the batch delete job cannot be paused or resumed", func(tx SendingTx) error {
		bj, err := k.batchJobIn(ctx, tx, id, k.D.Lock)
		if err != nil {
			return err
		}
		out = bj
		if bj.Job.State.Ended() {
			return tenant.Precondition("the job has ended " + string(bj.Job.State) + "; only a job queued or running is paused or resumed")
		}
		if bj.Paused != paused {
			if _, err := tx.Exec(ctx, `UPDATE delete_batch_jobs SET paused = ? WHERE job_id = ?`, k.D.Bool(paused), id); err != nil {
				return err
			}
			out.Paused = paused
		}
		return tx.Audited(ctx, audit, guard)
	})
	return out, err
}
