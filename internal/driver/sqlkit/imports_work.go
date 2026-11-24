package sqlkit

import (
	"context"
	"time"

	"github.com/amrshadid/nopacs/internal/core/importpath"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// The work of an import job's units (tenant.ImportWork). Every write checks
// its fence first, then locks the unit's row, the import job's, and last
// the job's, the order every transaction of an import keeps.

// fencedImportUnit checks the fence and reads the unit, locked.
func (k Sending) fencedImportUnit(ctx context.Context, tx SendingTx, fence tenant.WorkFence) (importUnitRow, error) {
	if err := tx.CheckFence(ctx, fence); err != nil {
		return importUnitRow{}, err
	}
	u, found, err := k.importUnitOf(ctx, tx, fence.ID, k.D.lockOf("u"))
	if err != nil {
		return importUnitRow{}, err
	}
	if !found {
		return importUnitRow{}, errNotImportUnit()
	}
	return u, nil
}

// StartImportUnit implements tenant.ImportWork.
func (k Sending) StartImportUnit(ctx context.Context, fence tenant.WorkFence, now time.Time) (tenant.ImportUnit, tenant.Job, error) {
	if err := fence.Validate(); err != nil {
		return tenant.ImportUnit{}, tenant.Job{}, err
	}
	if err := master.ValidateTime("time", now); err != nil {
		return tenant.ImportUnit{}, tenant.Job{}, err
	}
	var unit tenant.ImportUnit
	var job tenant.Job
	err := k.Host.Write(ctx, "the import unit cannot be started", func(tx SendingTx) error {
		u, err := k.fencedImportUnit(ctx, tx, fence)
		if err != nil {
			return err
		}
		r, err := jobInTx(ctx, tx, u.jobID, k.D.Lock)
		if err != nil {
			return err
		}
		job = r.job
		if !r.job.State.Ended() && u.unit.State == tenant.ImportUnitQueued {
			if _, err := tx.Exec(ctx, `UPDATE import_units SET state = 'walking' WHERE job_id = ? AND unit_id = ?`,
				u.jobID, u.unit.ID); err != nil {
				return err
			}
			u.unit.State = tenant.ImportUnitWalking
		}
		if err := withTallies(ctx, tx, &u); err != nil {
			return err
		}
		unit = u.unit
		if r.job.State != tenant.JobQueued {
			return nil
		}
		if _, err := tx.Exec(ctx, `UPDATE jobs SET state = 'running', started = ?, state_at = ? WHERE id = ? AND state = 'queued'`,
			k.D.Time(now), k.D.Time(now), u.jobID); err != nil {
			return err
		}
		after, err := jobInTx(ctx, tx, u.jobID, "")
		if err != nil {
			return err
		}
		job = after.job
		e, err := tenant.JobStepEvent(tenant.JobQueued, after.job, now)
		if err != nil {
			return err
		}
		return tx.WriteEvents(ctx, e)
	})
	if err != nil {
		return tenant.ImportUnit{}, tenant.Job{}, err
	}
	return unit, job, nil
}

// AddImportUnits implements tenant.ImportWork.
func (k Sending) AddImportUnits(ctx context.Context, fence tenant.WorkFence, folders []string, now time.Time) (tenant.Job, error) {
	if err := fence.Validate(); err != nil {
		return tenant.Job{}, err
	}
	if err := tenant.ValidateImportFolders(folders); err != nil {
		return tenant.Job{}, err
	}
	if err := master.ValidateTime("time", now); err != nil {
		return tenant.Job{}, err
	}
	var out tenant.Job
	err := k.Host.Write(ctx, "the import units cannot be queued", func(tx SendingTx) error {
		u, err := k.fencedImportUnit(ctx, tx, fence)
		if err != nil {
			return err
		}
		if !u.unit.Listing {
			return tenant.Precondition("only an import job's listing unit queues units")
		}
		ij, _, err := importJobInTx(ctx, tx, u.jobID, k.D.Lock)
		if err != nil {
			return err
		}
		r, err := jobInTx(ctx, tx, u.jobID, "")
		if err != nil {
			return err
		}
		out = r.job
		if r.job.State.Ended() {
			return nil
		}
		var added int64
		for _, f := range folders {
			if !tenant.ImportPathUnder(u.unit.Folder, f) {
				return tenant.Precondition("a folder is not under the job's path")
			}
			key := tenant.ImportPathKey(f)
			queued, err := exists(ctx, tx, `SELECT 1 FROM import_units WHERE job_id = ? AND folder_key = ?`, u.jobID, key)
			if err != nil {
				return err
			}
			if queued {
				continue // a page written again after a crash
			}
			if u.cursorSet && importpath.Compare(f, u.unit.Cursor) <= 0 {
				return tenant.Precondition("a folder is at or before the listing unit's cursor")
			}
			if err := k.insertImportUnit(ctx, tx, u.jobID, ij.nextUnit, f, false, now); err != nil {
				return err
			}
			ij.nextUnit++
			added++
		}
		last := folders[len(folders)-1]
		if !u.cursorSet || importpath.Compare(last, u.unit.Cursor) > 0 {
			if _, err := tx.Exec(ctx, `UPDATE import_units SET cursor_key = ? WHERE job_id = ? AND unit_id = ?`,
				tenant.ImportPathKey(last), u.jobID, u.unit.ID); err != nil {
				return err
			}
		}
		if added == 0 {
			return nil
		}
		if _, err := tx.Exec(ctx, `UPDATE import_jobs SET units = units + ?, next_unit = ? WHERE job_id = ?`,
			added, ij.nextUnit, u.jobID); err != nil {
			return err
		}
		after, err := jobInTx(ctx, tx, u.jobID, "")
		out = after.job
		return err
	})
	if err != nil {
		return tenant.Job{}, err
	}
	return out, nil
}

// RecordImportSkip implements tenant.ImportWork.
func (k Sending) RecordImportSkip(ctx context.Context, s tenant.ImportSkip) (tenant.Job, error) {
	if err := s.Validate(); err != nil {
		return tenant.Job{}, err
	}
	var out tenant.Job
	err := k.Host.Write(ctx, "the skipped file cannot be recorded", func(tx SendingTx) error {
		u, err := k.fencedImportUnit(ctx, tx, s.Fence)
		if err != nil {
			return err
		}
		fresh, err := judgeImportSkip(u, s.Path)
		if err != nil {
			return err
		}
		if fresh {
			ij, _, err := importJobInTx(ctx, tx, u.jobID, k.D.Lock)
			if err != nil {
				return err
			}
			// An entry too long, or badly named, is listed under its
			// deepest ancestor that fits the rule (ImportSkip.RowPath), so
			// the row's key fits the index; several such entries of one
			// directory share that row and are each counted. The cursor,
			// which no index holds, takes the entry's own path, so each is
			// judged once.
			if _, err := k.listImportFile(ctx, tx, &ij, importListed{pathKey: tenant.ImportPathKey(s.RowPath()),
				kind: tenant.ImportFileSkipped, unitID: u.unit.ID, skipCode: s.Code, size: s.Size, detail: s.Detail, at: s.Now}); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE import_units SET cursor_key = ? WHERE job_id = ? AND unit_id = ?`,
				tenant.ImportPathKey(s.Path), u.jobID, u.unit.ID); err != nil {
				return err
			}
			d := tenant.ImportSkipDelta()
			if err := k.addToUnit(ctx, tx, u.jobID, u.unit.ID, d); err != nil {
				return err
			}
			if err := k.saveImportJob(ctx, tx, &ij, d); err != nil {
				return err
			}
			if err := addImportTally(ctx, tx, u.jobID, u.unit.ID, tallySkip, string(s.Code), 1); err != nil {
				return err
			}
		}
		r, err := jobInTx(ctx, tx, u.jobID, "")
		out = r.job
		return err
	})
	if err != nil {
		return tenant.Job{}, err
	}
	return out, nil
}

// judgeImportSkip is judgeImportPath for a skip, which may also be of a
// unit's own folder before its first entry: a folder that, when its unit
// starts, is no longer a directory reached through no link is recorded so,
// rather than its files lost from the counts. The listing unit's folder is
// the job's path, whose loss fails the unit instead.
func judgeImportSkip(u importUnitRow, p string) (bool, error) {
	if p == "" || p != u.unit.Folder || u.unit.Listing {
		return judgeImportPath(u, p)
	}
	switch {
	case !u.cursorSet:
		return true, nil
	case u.unit.Cursor == p:
		return false, nil
	}
	return false, tenant.Precondition("the path is before the unit's cursor")
}

// EndImportWalk implements tenant.ImportWork.
func (k Sending) EndImportWalk(ctx context.Context, fence tenant.WorkFence, now time.Time) (tenant.Job, error) {
	return k.endWalk(ctx, fence, "", now)
}

// FailImportUnit implements tenant.ImportWork.
func (k Sending) FailImportUnit(ctx context.Context, fence tenant.WorkFence, reason string, now time.Time) (tenant.Job, error) {
	if reason != tenant.ImportReasonRootUnavailable {
		return tenant.Job{}, fault.New(fault.Invalid, "an import unit fails with the reason \"root unavailable\"")
	}
	return k.endWalk(ctx, fence, reason, now)
}

// endWalk ends a unit's walk, failed with reason when it is not empty, and
// completes its unit of work.
func (k Sending) endWalk(ctx context.Context, fence tenant.WorkFence, reason string, now time.Time) (tenant.Job, error) {
	if err := fence.Validate(); err != nil {
		return tenant.Job{}, err
	}
	if err := master.ValidateTime("time", now); err != nil {
		return tenant.Job{}, err
	}
	var out tenant.Job
	err := k.Host.Write(ctx, "the import unit's walk cannot be ended", func(tx SendingTx) error {
		u, err := k.fencedImportUnit(ctx, tx, fence)
		if err != nil {
			return err
		}
		already := u.unit.State.WalkEnded()
		events, err := k.endWalkInTx(ctx, tx, u, reason, now)
		if err != nil {
			return err
		}
		if err := k.completeUnit(ctx, tx, fence, now); err != nil {
			return err
		}
		r, err := jobInTx(ctx, tx, u.jobID, "")
		if err != nil {
			return err
		}
		out = r.job
		if !already && !r.job.State.Ended() {
			// A unit's walk is the import's step a person follows: its
			// job's counts as they stand go out with it, before any end.
			e, err := tenant.JobProgressEvent(r.job, now)
			if err != nil {
				return err
			}
			events = append([]tenant.EventRecord{e}, events...)
		}
		return tx.WriteEvents(ctx, events...)
	})
	if err != nil {
		return tenant.Job{}, err
	}
	return out, nil
}

// endWalkInTx ends the walk of the unit u (its row locked): failed with
// reason when it is not empty; canceled when its job has ended; otherwise
// walked, or done when nothing it handed in is processing. The listing
// unit's end lists the job. It returns the events of the job's end. A walk
// ended already changes nothing.
func (k Sending) endWalkInTx(ctx context.Context, tx SendingTx, u importUnitRow, reason string, now time.Time) ([]tenant.EventRecord, error) {
	if u.unit.State.WalkEnded() {
		return nil, nil
	}
	ij, _, err := importJobInTx(ctx, tx, u.jobID, k.D.Lock)
	if err != nil {
		return nil, err
	}
	r, err := jobInTx(ctx, tx, u.jobID, "")
	if err != nil {
		return nil, err
	}
	state := tenant.ImportWalkEnd(reason, r.job.State.Ended(), u.unit.Counts.Processing)
	if _, err := tx.Exec(ctx, `UPDATE import_units SET state = ?, reason = ?, walk_ended = ? WHERE job_id = ? AND unit_id = ?`,
		string(state), reason, k.D.Time(now), u.jobID, u.unit.ID); err != nil {
		return nil, err
	}
	failReason := ij.failReason
	if reason != "" && failReason == "" {
		failReason = reason
	}
	listed := ij.spec.Listed || (u.unit.Listing && reason == "")
	if _, err := tx.Exec(ctx, `UPDATE import_jobs SET units_walked = units_walked + 1, listed = ?, fail_reason = ? WHERE job_id = ?`,
		k.D.Bool(listed), failReason, u.jobID); err != nil {
		return nil, err
	}
	return k.settleImportJob(ctx, tx, u.jobID, now)
}

// settleImportJob ends an import job whose walks have all ended (it is
// listed, or a unit failed, so no unit will be queued) once nothing is
// processing, or at once when a unit failed (tenant.ImportEndState); while
// files are still processing it stays running with the reason "settling",
// from the time it first was. It returns the end's event; a job ended, or
// with walks still to end, is left as it is.
func (k Sending) settleImportJob(ctx context.Context, tx SendingTx, jobID string, now time.Time) ([]tenant.EventRecord, error) {
	ij, found, err := importJobInTx(ctx, tx, jobID, k.D.Lock)
	if err != nil || !found {
		return nil, err
	}
	c := ij.spec.Counts
	if (!ij.spec.Listed && ij.failReason == "") || c.UnitsWalked < c.Units {
		return nil, nil
	}
	r, err := jobInTx(ctx, tx, jobID, k.D.Lock)
	if err != nil || r.job.State.Ended() {
		return nil, err
	}
	if ij.failReason == "" && c.Processing > 0 {
		if ij.spec.SettlingSince.IsZero() {
			if _, err := tx.Exec(ctx, `UPDATE import_jobs SET settling_since = ? WHERE job_id = ?`, k.D.Time(now), jobID); err != nil {
				return nil, err
			}
		}
		_, err := tx.Exec(ctx, `UPDATE jobs SET reason = ? WHERE id = ? AND reason <> ?`,
			tenant.ImportReasonSettling, jobID, tenant.ImportReasonSettling)
		return nil, err
	}
	state, reason := tenant.ImportEndState(c, ij.failReason)
	_, events, err := k.endJob(ctx, tx, r.job, state, reason, now)
	return events, err
}

// TimeOutImportSettling implements tenant.ImportWork.
func (k Sending) TimeOutImportSettling(ctx context.Context, jobID string, now time.Time) (tenant.Job, error) {
	if err := master.ValidateID("job id", jobID); err != nil {
		return tenant.Job{}, err
	}
	if err := master.ValidateTime("time", now); err != nil {
		return tenant.Job{}, err
	}
	var out tenant.Job
	err := k.Host.Write(ctx, "the import job's settling cannot be timed out", func(tx SendingTx) error {
		first, err := importJobOf(ctx, tx, jobID)
		if err != nil {
			return err
		}
		out = first
		ij, _, err := importJobInTx(ctx, tx, jobID, k.D.Lock)
		if err != nil {
			return err
		}
		r, err := jobInTx(ctx, tx, jobID, k.D.Lock)
		if err != nil {
			return err
		}
		c := ij.spec.Counts
		if r.job.State.Ended() || !ij.spec.Listed || c.UnitsWalked < c.Units || c.Processing == 0 {
			out = r.job
			return nil
		}
		job, events, err := k.endJob(ctx, tx, r.job, tenant.JobPartial, tenant.ImportReasonSettlingTimedOut, now)
		if err != nil {
			return err
		}
		out = job
		return tx.WriteEvents(ctx, events...)
	})
	if err != nil {
		return tenant.Job{}, err
	}
	return out, nil
}

// ReleaseImportUnit implements tenant.ImportWork. As ReleaseJobUnit, the
// attempt the claim counted is given back by allowing one more, never by
// lowering the attempt, which fences every write.
func (k Sending) ReleaseImportUnit(ctx context.Context, fence tenant.WorkFence, due time.Time, reason string, now time.Time) error {
	if err := fence.Validate(); err != nil {
		return err
	}
	if err := tenant.ValidateWorkReason(reason); err != nil {
		return err
	}
	if err := master.ValidateTime("time", now); err != nil {
		return err
	}
	if err := master.ValidateTime("due time", due); err != nil {
		return err
	}
	if due.Before(now) {
		return fault.New(fault.Invalid, "a unit is given back until a time not before now")
	}
	return k.Host.Write(ctx, "the import unit cannot be given back", func(tx SendingTx) error {
		u, err := k.fencedImportUnit(ctx, tx, fence)
		if err != nil {
			return err
		}
		r, err := jobInTx(ctx, tx, u.jobID, "")
		if err != nil {
			return err
		}
		if r.job.State.Ended() {
			events, err := k.endWalkInTx(ctx, tx, u, "", now)
			if err != nil {
				return err
			}
			if err := k.completeUnit(ctx, tx, fence, now); err != nil {
				return err
			}
			return tx.WriteEvents(ctx, events...)
		}
		_, err = tx.Exec(ctx, `UPDATE work SET state = 'queued', owner = '', due = ?, last_error = ?,
			max_attempts = max_attempts + 1, updated = ? WHERE id = ? AND attempt = ?`,
			k.D.Time(due), reason, k.D.Time(now), fence.ID, fence.Attempt)
		return err
	})
}

// cancelImportInTx ends an import job canceled, and with it the units of
// it that are queued and no transaction holds, whose walks end canceled; a
// unit a worker holds learns of the cancel from the job's state its next
// write answers. A job canceled already is returned as it stands with no
// event; one ended otherwise is a precondition failure. The rows are
// locked in the order of every import transaction: the units of work
// (skipping those another holds), their import units in order of unit,
// the import job, then the job. The job's state is judged again once its
// rows are locked, since a walk's end or an outcome may have ended it
// after the first read: the cancel then changes nothing it would keep.
func (k Sending) cancelImportInTx(ctx context.Context, tx SendingTx, id, reason string, now time.Time) (tenant.Job, *tenant.EventRecord, error) {
	first, err := jobInTx(ctx, tx, id, "")
	if err != nil {
		return tenant.Job{}, nil, err
	}
	if canceled, err := cancelVerdict(first.job); err != nil {
		return tenant.Job{}, nil, err
	} else if canceled {
		return first.job, nil, nil
	}
	var queued []string
	if err := tx.Query(ctx, `SELECT w.id FROM work AS w JOIN import_units AS u ON u.work_id = w.id
		WHERE u.job_id = ? AND w.state = 'queued' ORDER BY u.unit_id`+k.D.lockOf("w")+skipLocked(k.D), []any{id}, func(r Row) error {
		var w string
		err := r.Scan(&w)
		queued = append(queued, w)
		return err
	}); err != nil {
		return tenant.Job{}, nil, err
	}
	var ended int64
	for _, w := range queued {
		if _, err := tx.Exec(ctx, `UPDATE work SET state = 'done', owner = '', last_error = ?, updated = ?
			WHERE id = ? AND state = 'queued'`, tenant.WorkReasonJobCanceled, k.D.Time(now), w); err != nil {
			return tenant.Job{}, nil, err
		}
		n, err := tx.Exec(ctx, `UPDATE import_units SET state = 'canceled', walk_ended = ? WHERE work_id = ?
			AND state IN ('queued', 'walking')`, k.D.Time(now), w)
		if err != nil {
			return tenant.Job{}, nil, err
		}
		ended += n
	}
	if _, _, err := importJobInTx(ctx, tx, id, k.D.Lock); err != nil {
		return tenant.Job{}, nil, err
	}
	r, err := jobInTx(ctx, tx, id, k.D.Lock)
	if err != nil {
		return tenant.Job{}, nil, err
	}
	// Ended otherwise meanwhile: the precondition undoes what was changed
	// above. Canceled meanwhile by another cancel: the units ended above
	// were queued and skipped by it, so ending them is still right, and the
	// job is answered with no second event.
	canceled, err := cancelVerdict(r.job)
	if err != nil {
		return tenant.Job{}, nil, err
	}
	if _, err := tx.Exec(ctx, `UPDATE import_jobs SET units_walked = units_walked + ? WHERE job_id = ?`, ended, id); err != nil {
		return tenant.Job{}, nil, err
	}
	if canceled {
		after, err := jobInTx(ctx, tx, id, "")
		return after.job, nil, err
	}
	n, err := tx.Exec(ctx, `UPDATE jobs SET state = 'canceled', ended = ?, reason = ?, state_at = ?, ended_at = ? WHERE id = ?
		AND ended = ?`, k.D.Bool(true), reason, k.D.Time(now), k.D.Time(now), id, k.D.Bool(false))
	if err != nil {
		return tenant.Job{}, nil, err
	}
	if n != 1 {
		return tenant.Job{}, nil, fault.New(fault.Internal, "the import job ended while its rows were locked")
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

// cancelVerdict judges a cancel of j: true when j is canceled already, to
// be answered as it stands; a precondition failure when it ended
// otherwise; false, nil when it may be canceled.
func cancelVerdict(j tenant.Job) (bool, error) {
	switch {
	case j.State == tenant.JobCanceled:
		return true, nil
	case j.State.Ended():
		return false, tenant.Precondition("the job has ended " + string(j.State) + "; only a job queued or running is canceled")
	}
	return false, nil
}

// skipLocked returns the clause that skips rows another transaction
// holds, written after a lock clause: " SKIP LOCKED" where rows are
// locked, "" where one writer runs at a time.
func skipLocked(d SendingDialect) string {
	if d.Lock == "" {
		return ""
	}
	return " SKIP LOCKED"
}

// giveUpImportUnit ends the walk of an import unit whose unit of work is
// held as failed, its attempts run out, in the transaction that holds it:
// failed, with the job ending failed once every walk has ended. It reports
// false for a unit of work that is no import unit's.
func (k Sending) giveUpImportUnit(ctx context.Context, tx SendingTx, workID string, now time.Time) (bool, []tenant.EventRecord, error) {
	u, found, err := k.importUnitOf(ctx, tx, workID, k.D.lockOf("u"))
	if err != nil || !found {
		return false, nil, err
	}
	events, err := k.endWalkInTx(ctx, tx, u, tenant.JobReasonAttemptsRanOut, now)
	return true, events, err
}
