package sqlkit

import (
	"context"
	"time"

	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// jobScanColumns are a job's scan's columns, with its scan's labels,
// storage and Series Instance UID, read from job_scans joined to scans:
// empty when a delete hid the scan whole (jobScanOf then reads them from
// its hidden row), since a storage is never empty for a scan held.
const jobScanColumns = `js.job_id, js.scan_id, js.start_after, js.cursor, js.instances, js.bytes, js.sent, js.sent_bytes,
	js.warned, js.warned_bytes, js.refused, js.refused_bytes, js.refused_not_listed, js.ended,
	coalesce(s.subject_label, ''), coalesce(s.session_label, ''), coalesce(s.label, ''), coalesce(s.storage, ''),
	coalesce(s.series_uid, '')`

// jobScanRow is a job's scan as its row holds it.
type jobScanRow struct {
	jobID            string
	scanID           int64
	startAfter       string
	refusedNotListed int64
	// series is the scan's Series Instance UID.
	series string
	scan   tenant.JobScan
}

// scanJobScan reads one row of jobScanColumns.
func scanJobScan(row Row) (jobScanRow, error) {
	var r jobScanRow
	c := &r.scan.Counts
	err := row.Scan(&r.jobID, &r.scanID, &r.startAfter, &r.scan.Cursor, &c.Instances, &c.Bytes, &c.InstancesSent, &c.BytesSent,
		&c.InstancesWarned, &c.BytesWarned, &c.InstancesRefused, &c.BytesRefused, &r.refusedNotListed, &r.scan.Ended,
		&r.scan.Path.Subject, &r.scan.Path.Session, &r.scan.Path.Scan, &r.scan.Storage, &r.series)
	c.Scans, c.Listed = 1, true
	if r.scan.Ended {
		c.ScansDone = 1
	}
	return r, err
}

// jobScanOf reads the scan a unit sends, with the lock given on its
// job_scans row; false when the unit sends none. A scan a delete hid whole
// is still the unit's, with its labels as the delete hid them
// (hiddenScanOf), so its unit starts, meets its instances deleted and
// ends, rather than being refused as no job's on each attempt.
func jobScanOf(ctx context.Context, tx SendingTx, workID, lock string) (jobScanRow, bool, error) {
	var out jobScanRow
	found := false
	err := tx.Query(ctx, jobScanByWorkSQL+lock, []any{workID}, func(r Row) error {
		found = true
		var err error
		out, err = scanJobScan(r)
		return err
	})
	if err != nil || !found || out.scan.Storage != "" {
		return out, found, err
	}
	return out, true, hiddenScanOf(ctx, tx, &out)
}

// listingJobOf returns the id of the job whose listing unit is workID, or
// "": a listing unit's subject is its job's id.
func listingJobOf(ctx context.Context, tx SendingTx, workID string) (string, error) {
	var subject string
	found, err := tx.QueryRow(ctx, `SELECT subject FROM work WHERE id = ?`, []any{workID}, &subject)
	if err != nil || !found || master.ValidateID("job id", subject) != nil {
		return "", err
	}
	var id string
	found, err = tx.QueryRow(ctx, `SELECT id FROM jobs WHERE id = ? AND list_work = ?`, []any{subject, workID}, &id)
	if err != nil || !found {
		return "", err
	}
	return id, nil
}

// errNotJobUnit is the refusal of a unit no job queued.
func errNotJobUnit() error {
	return tenant.Precondition("the unit of work is not one of a job's")
}

// completeUnit marks the leased unit done, fenced by its attempt.
func (k Sending) completeUnit(ctx context.Context, tx SendingTx, f tenant.WorkFence, now time.Time) error {
	_, err := tx.Exec(ctx, `UPDATE work SET state = 'done', owner = '', updated = ? WHERE id = ? AND attempt = ?`,
		k.D.Time(now), f.ID, f.Attempt)
	return err
}

// StartJobUnit implements tenant.JobWork.
func (k Sending) StartJobUnit(ctx context.Context, fence tenant.WorkFence, now time.Time) (tenant.JobUnit, error) {
	if err := fence.Validate(); err != nil {
		return tenant.JobUnit{}, err
	}
	if err := master.ValidateTime("time", now); err != nil {
		return tenant.JobUnit{}, err
	}
	var out tenant.JobUnit
	err := k.Host.Write(ctx, "the job's unit cannot be started", func(tx SendingTx) error {
		out = tenant.JobUnit{}
		if err := tx.CheckFence(ctx, fence); err != nil {
			return err
		}
		scan, isScan, err := jobScanOf(ctx, tx, fence.ID, "")
		if err != nil {
			return err
		}
		jobID := scan.jobID
		if !isScan {
			if jobID, err = listingJobOf(ctx, tx, fence.ID); err != nil {
				return err
			}
			if jobID == "" {
				return errNotJobUnit()
			}
			out.Listing = true
		}
		r, err := jobInTx(ctx, tx, jobID, k.D.Lock)
		if err != nil {
			return err
		}
		out.Scan = scan.scan
		out.Job = r.job
		if isScan {
			if out.Scan.Owed, err = k.owedInstances(ctx, tx, scan); err != nil {
				return err
			}
		}
		if r.job.State != tenant.JobQueued {
			return nil
		}
		if _, err := tx.Exec(ctx, `UPDATE jobs SET state = 'running', started = ?, state_at = ? WHERE id = ? AND state = 'queued'`,
			k.D.Time(now), k.D.Time(now), jobID); err != nil {
			return err
		}
		after, err := jobInTx(ctx, tx, jobID, "")
		if err != nil {
			return err
		}
		out.Job = after.job
		e, err := tenant.JobStepEvent(tenant.JobQueued, after.job, now)
		if err != nil {
			return err
		}
		return tx.WriteEvents(ctx, e)
	})
	if err != nil {
		return tenant.JobUnit{}, err
	}
	return out, nil
}

// JobScanInstances implements tenant.JobWork.
func (k Sending) JobScanInstances(ctx context.Context, fence tenant.WorkFence, after string, limit int) ([]tenant.ScanInstance, error) {
	if err := fence.Validate(); err != nil {
		return nil, err
	}
	if after != "" {
		if err := tenant.ValidateInstanceUID(after); err != nil {
			return nil, err
		}
	}
	if limit < 1 || limit > tenant.MaxJobInstancePage {
		return nil, fault.New(fault.Invalid, "a page of a job's instances is 1 to 1000")
	}
	var out []tenant.ScanInstance
	err := k.Host.Read(ctx, "the job's instances cannot be read", func(tx SendingTx) error {
		out = nil
		s, found, err := jobScanOf(ctx, tx, fence.ID, "")
		if err != nil {
			return err
		}
		if !found {
			return errNotJobUnit()
		}
		r, err := jobInTx(ctx, tx, s.jobID, "")
		if err != nil {
			return err
		}
		query, args := k.D.StreamPage(r.job, r.bound, s.scanID, s.startAfter, after, limit)
		out, err = streamInstances(ctx, tx, s, query, args)
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// streamInstances reads the instances of a scan's stream query returns,
// placed as a catalog read places them.
func streamInstances(ctx context.Context, tx SendingTx, s jobScanRow, query string, args []any) ([]tenant.ScanInstance, error) {
	var out []tenant.ScanInstance
	if err := tx.Query(ctx, query, args, func(row Row) error {
		var in tenant.ScanInstance
		var sum, archived []byte
		var held DBTime
		var mark string
		err := row.Scan(&in.SOPInstanceUID, &in.ReceptionID, &in.Key, &in.Offset, &in.Length, &in.Size, &sum, &archived, &held,
			&in.Profile.Name, &in.Profile.Version, &mark, &in.PixelRiskAccepted)
		in.SHA256, in.ArchivedSHA256, in.Held, in.Damaged = ToSHA256(sum), ToSHA256(archived), held.T, mark != ""
		out = append(out, in)
		return err
	}); err != nil {
		return nil, err
	}
	err := placeHeld(tenant.CatalogScan{SubjectLabel: s.scan.Path.Subject, SessionLabel: s.scan.Path.Session,
		Label: s.scan.Path.Scan, Storage: s.scan.Storage}, out)
	return out, err
}

// owedInstances reads the instances a scan's unit owes, placed, in order.
func (k Sending) owedInstances(ctx context.Context, tx SendingTx, s jobScanRow) ([]tenant.ScanInstance, error) {
	return streamInstances(ctx, tx, s, owedInstancesSQL, []any{s.jobID, s.scanID, s.scanID, tenant.MaxJobOwed})
}

// RecordJobProgress implements tenant.JobWork.
func (k Sending) RecordJobProgress(ctx context.Context, p tenant.JobProgress) (tenant.Job, error) {
	if err := p.Validate(); err != nil {
		return tenant.Job{}, err
	}
	var out tenant.Job
	err := k.Host.Write(ctx, "the job's progress cannot be recorded", func(tx SendingTx) error {
		if err := tx.CheckFence(ctx, p.Fence); err != nil {
			return err
		}
		job, _, events, err := k.recordInTx(ctx, tx, p)
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

// EndJobScan implements tenant.JobWork.
func (k Sending) EndJobScan(ctx context.Context, p tenant.JobProgress) (tenant.Job, error) {
	if err := p.Validate(); err != nil {
		return tenant.Job{}, err
	}
	var out tenant.Job
	err := k.Host.Write(ctx, "the job's scan cannot be ended", func(tx SendingTx) error {
		if err := tx.CheckFence(ctx, p.Fence); err != nil {
			return err
		}
		job, scan, events, err := k.recordInTx(ctx, tx, p)
		if err != nil {
			return err
		}
		out = job
		if err := k.completeUnit(ctx, tx, p.Fence, p.Now); err != nil {
			return err
		}
		if !scan.scan.Ended {
			if _, err := tx.Exec(ctx, `UPDATE job_scans SET ended = ? WHERE job_id = ? AND scan_id = ?`,
				k.D.Bool(true), scan.jobID, scan.scanID); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE jobs SET scans_done = scans_done + 1 WHERE id = ?`, scan.jobID); err != nil {
				return err
			}
			ended, more, err := k.endIfDone(ctx, tx, scan.jobID, p.Now)
			if err != nil {
				return err
			}
			out, events = ended, append(events, more...)
		}
		return tx.WriteEvents(ctx, events...)
	})
	if err != nil {
		return tenant.Job{}, err
	}
	return out, nil
}

// recordInTx records p on the scan its unit sends and on its job, after the
// fence was checked, and returns the job and the scan as they are after,
// with the progress event when one is due.
func (k Sending) recordInTx(ctx context.Context, tx SendingTx, p tenant.JobProgress) (tenant.Job, jobScanRow, []tenant.EventRecord, error) {
	first, found, err := jobScanOf(ctx, tx, p.Fence.ID, "")
	if err != nil {
		return tenant.Job{}, jobScanRow{}, nil, err
	}
	if !found {
		return tenant.Job{}, jobScanRow{}, nil, errNotJobUnit()
	}
	// The job's row before its scan's, in the order of the locks.
	r, err := jobInTx(ctx, tx, first.jobID, k.D.Lock)
	if err != nil {
		return tenant.Job{}, jobScanRow{}, nil, err
	}
	s, _, err := jobScanOf(ctx, tx, p.Fence.ID, k.D.lockOf("js"))
	if err != nil {
		return tenant.Job{}, jobScanRow{}, nil, err
	}
	if !r.job.State.TakesProgress() {
		return r.job, s, nil, nil // its counts are final
	}
	if s.scan.Cursor != p.From {
		if p.From != p.To && s.scan.Cursor == p.To {
			return r.job, s, nil, nil // recorded before: its answer was lost
		}
		return tenant.Job{}, jobScanRow{}, nil, tenant.Precondition("the unit's cursor is not the one the progress follows")
	}
	settled, err := k.settle(ctx, tx, s, p.Settled)
	if err != nil {
		return tenant.Job{}, jobScanRow{}, nil, err
	}
	if settled < int64(len(p.Settled)) {
		if p.From == p.To && settled == 0 {
			return r.job, s, nil, nil // recorded before: its answer was lost
		}
		return tenant.Job{}, jobScanRow{}, nil, tenant.Precondition("the progress settles an instance the unit does not owe")
	}
	if p.From == p.To && len(p.Settled) == 0 {
		return r.job, s, nil, nil
	}
	if err := k.owe(ctx, tx, s, p.Owed); err != nil {
		return tenant.Job{}, jobScanRow{}, nil, err
	}
	moreInstances, moreBytes := tenant.ProgressExcess(s.scan.Counts, p)
	listed, notListed, refusedNotListed := r.listed, int64(0), int64(0)
	for _, f := range p.Failures {
		if listed >= int64(p.FailuresKept) {
			notListed++
			if f.Code.Refused() {
				refusedNotListed++
			}
			continue
		}
		// No ON CONFLICT: an instance answered once per job is listed
		// once, and a second row would be a caller's bug that must fail
		// the write rather than vanish from the counts.
		if _, err := tx.Exec(ctx, `INSERT INTO job_failures (job_id, subject_label, session_label, scan_label, sop_instance_uid,
			scan_id, warning, code, peer_status, sop_class_uid, transfer_syntax_uid, detail, at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, r.job.ID, s.scan.Path.Subject, s.scan.Path.Session, s.scan.Path.Scan,
			f.SOPInstanceUID, s.scanID, k.D.Bool(!f.Code.Refused()), string(f.Code), f.PeerStatus, f.SOPClassUID,
			f.TransferSyntaxUID, f.Detail, k.D.Time(f.Time)); err != nil {
			return tenant.Job{}, jobScanRow{}, nil, err
		}
		listed++
	}
	if err := k.addTallies(ctx, tx, r.job.ID, p.Tallies); err != nil {
		return tenant.Job{}, jobScanRow{}, nil, err
	}
	if _, err := tx.Exec(ctx, `UPDATE job_scans SET cursor = ?, instances = instances + ?, bytes = bytes + ?, sent = sent + ?,
		sent_bytes = sent_bytes + ?, warned = warned + ?, warned_bytes = warned_bytes + ?, refused = refused + ?,
		refused_bytes = refused_bytes + ?, refused_not_listed = refused_not_listed + ? WHERE job_id = ? AND scan_id = ?
		AND cursor = ?`, p.To, moreInstances, moreBytes, p.Sent, p.SentBytes, p.Warned, p.WarnedBytes, p.Refused,
		p.RefusedBytes, refusedNotListed, s.jobID, s.scanID, p.From); err != nil {
		return tenant.Job{}, jobScanRow{}, nil, err
	}
	if _, err := tx.Exec(ctx, `UPDATE jobs SET instances = instances + ?, bytes = bytes + ?, sent = sent + ?,
		sent_bytes = sent_bytes + ?, warned = warned + ?, warned_bytes = warned_bytes + ?, refused = refused + ?,
		refused_bytes = refused_bytes + ?, failures_listed = ?, failures_not_listed = failures_not_listed + ? WHERE id = ?`,
		moreInstances, moreBytes, p.Sent, p.SentBytes, p.Warned, p.WarnedBytes, p.Refused, p.RefusedBytes, listed, notListed,
		r.job.ID); err != nil {
		return tenant.Job{}, jobScanRow{}, nil, err
	}
	after, err := jobInTx(ctx, tx, r.job.ID, "")
	if err != nil {
		return tenant.Job{}, jobScanRow{}, nil, err
	}
	s.scan.Cursor = p.To
	if !tenant.ProgressEventDue(after.job, after.progressMark, after.progressAt, p.Now) {
		return after.job, s, nil, nil
	}
	e, err := tenant.JobProgressEvent(after.job, p.Now)
	if err != nil {
		return tenant.Job{}, jobScanRow{}, nil, err
	}
	if _, err := tx.Exec(ctx, `UPDATE jobs SET progress_at = ?, progress_mark = ? WHERE id = ?`,
		k.D.Time(p.Now), tenant.ProgressMark(after.job.Counts), r.job.ID); err != nil {
		return tenant.Job{}, jobScanRow{}, nil, err
	}
	return after.job, s, []tenant.EventRecord{e}, nil
}

// settle removes the instances given from what the scan's unit owes and
// returns how many it owed.
func (k Sending) settle(ctx context.Context, tx SendingTx, s jobScanRow, uids []string) (int64, error) {
	var n int64
	for _, u := range uids {
		m, err := tx.Exec(ctx, `DELETE FROM job_owed WHERE job_id = ? AND scan_id = ? AND sop_instance_uid = ?`, s.jobID, s.scanID, u)
		if err != nil {
			return 0, err
		}
		n += m
	}
	return n, nil
}

// owe keeps the instances given owed to the scan's unit, at most
// tenant.MaxJobOwed at once. An instance owed already fails the write: the
// cursor passes an instance once.
func (k Sending) owe(ctx context.Context, tx SendingTx, s jobScanRow, uids []string) error {
	if len(uids) == 0 {
		return nil
	}
	var held int64
	if _, err := tx.QueryRow(ctx, owedCountSQL, []any{s.jobID, s.scanID}, &held); err != nil {
		return err
	}
	if held+int64(len(uids)) > tenant.MaxJobOwed {
		return tenant.Precondition("a unit owes at most 1000 instances at once; settle some first")
	}
	for _, u := range uids {
		if _, err := tx.Exec(ctx, `INSERT INTO job_owed (job_id, scan_id, sop_instance_uid) VALUES (?, ?, ?)`,
			s.jobID, s.scanID, u); err != nil {
			return err
		}
	}
	return nil
}

// lockOf returns the lock clause for the rows of one table of a join, so a
// join locks only the row it changes: " FOR NO KEY UPDATE OF js".
func (d SendingDialect) lockOf(table string) string {
	if d.Lock == "" {
		return ""
	}
	return d.Lock + " OF " + table
}

// endIfDone ends the job, listed, once every scan of it has ended, in the
// state EndState gives, and returns it with its event; a job ended already
// or with work left is returned as it is.
func (k Sending) endIfDone(ctx context.Context, tx SendingTx, jobID string, now time.Time) (tenant.Job, []tenant.EventRecord, error) {
	r, err := jobInTx(ctx, tx, jobID, "")
	if err != nil {
		return tenant.Job{}, nil, err
	}
	c := r.job.Counts
	if r.job.State.Ended() || !c.Listed || c.ScansDone < c.Scans {
		return r.job, nil, nil
	}
	state, reason := tenant.EndState(c)
	return k.endJob(ctx, tx, r.job, state, reason, now)
}

// endJob moves a job that has not ended to state with the reason.
func (k Sending) endJob(ctx context.Context, tx SendingTx, j tenant.Job, state tenant.JobState, reason string, now time.Time) (tenant.Job, []tenant.EventRecord, error) {
	if _, err := tx.Exec(ctx, `UPDATE jobs SET state = ?, ended = ?, reason = ?, state_at = ?, ended_at = ? WHERE id = ? AND state = ?`,
		string(state), k.D.Bool(true), reason, k.D.Time(now), k.D.Time(now), j.ID, string(j.State)); err != nil {
		return tenant.Job{}, nil, err
	}
	after, err := jobInTx(ctx, tx, j.ID, "")
	if err != nil {
		return tenant.Job{}, nil, err
	}
	e, err := tenant.JobStepEvent(j.State, after.job, now)
	if err != nil {
		return tenant.Job{}, nil, err
	}
	return after.job, []tenant.EventRecord{e}, nil
}

// GiveUpJobUnit ends what a unit of a job carried when the unit is held as
// failed, its attempts run out, in the transaction that holds it: a scan's
// unit ends its scan with what it did not send remaining, and ends the job
// (failed) when it was its last; a listing's unit ends its job failed,
// since what it did not list can no longer be sent, and with it, as a
// cancel does, the job's queued units, and every scan of it, so the job's
// counts are final and a unit still held records nothing more
// (tenant.JobState.TakesProgress). It returns the events, which the
// driver writes; a unit of no job changes nothing. A driver calls it where
// a unit is held as failed (FailWork, a claim).
func (k Sending) GiveUpJobUnit(ctx context.Context, tx SendingTx, workID string, now time.Time) ([]tenant.EventRecord, error) {
	// An import job's unit ends its walk failed (giveUpImportUnit).
	if isImport, events, err := k.giveUpImportUnit(ctx, tx, workID, now); err != nil || isImport {
		return events, err
	}
	s, found, err := jobScanOf(ctx, tx, workID, "")
	if err != nil {
		return nil, err
	}
	if found {
		r, err := jobInTx(ctx, tx, s.jobID, k.D.Lock)
		if err != nil {
			return nil, err
		}
		n, err := tx.Exec(ctx, `UPDATE job_scans SET ended = ? WHERE job_id = ? AND scan_id = ? AND ended = ?`,
			k.D.Bool(true), s.jobID, s.scanID, k.D.Bool(false))
		if err != nil || n == 0 {
			return nil, err
		}
		if _, err := tx.Exec(ctx, `UPDATE jobs SET scans_done = scans_done + 1 WHERE id = ?`, r.job.ID); err != nil {
			return nil, err
		}
		_, events, err := k.endIfDone(ctx, tx, r.job.ID, now)
		return events, err
	}
	jobID, err := listingJobOf(ctx, tx, workID)
	if err != nil || jobID == "" {
		return nil, err
	}
	r, err := jobInTx(ctx, tx, jobID, k.D.Lock)
	if err != nil || r.job.State.Ended() {
		return nil, err
	}
	if _, err := k.endQueuedUnits(ctx, tx, r, tenant.WorkReasonJobFailed, now); err != nil {
		return nil, err
	}
	n, err := tx.Exec(ctx, `UPDATE job_scans SET ended = ? WHERE job_id = ? AND ended = ?`, k.D.Bool(true), jobID, k.D.Bool(false))
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `UPDATE jobs SET scans_done = scans_done + ? WHERE id = ?`, n, jobID); err != nil {
		return nil, err
	}
	_, events, err := k.endJob(ctx, tx, r.job, tenant.JobFailed, tenant.JobReasonAttemptsRanOut, now)
	return events, err
}

// endQueuedUnits ends the units of a job that are queued and no
// transaction holds, with reason as their last error, and returns them. A
// unit a transaction holds is skipped, never waited for: it is leased, or
// being claimed, and its worker learns of its job's end at its next write,
// so this never waits on a unit's row, which a worker locks before the
// job's.
func (k Sending) endQueuedUnits(ctx context.Context, tx SendingTx, r jobRow, reason string, now time.Time) ([]string, error) {
	var units []string
	if err := tx.Query(ctx, queuedUnitsSQL+k.D.SkipLocked, []any{r.listWork, r.job.ID}, func(row Row) error {
		var u string
		err := row.Scan(&u)
		units = append(units, u)
		return err
	}); err != nil {
		return nil, err
	}
	for _, u := range units {
		if _, err := tx.Exec(ctx, `UPDATE work SET state = 'done', owner = '', last_error = ?, updated = ?
			WHERE id = ? AND state = 'queued'`, reason, k.D.Time(now), u); err != nil {
			return nil, err
		}
	}
	return units, nil
}

// ReleaseJobUnit implements tenant.JobWork. The attempt the claim counted is
// given back by allowing one more, never by lowering the attempt, which
// fences every write: a lease released stays lost. A unit whose job has
// ended (canceled while the unit sent, as removing its sender with its jobs
// canceled does) is completed instead, and its scan ended, in the same
// transaction: queued again, it would be a unit nobody claims for a job
// that never counts that scan done.
func (k Sending) ReleaseJobUnit(ctx context.Context, fence tenant.WorkFence, due time.Time, reason string, now time.Time) error {
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
	return k.Host.Write(ctx, "the job's unit cannot be given back", func(tx SendingTx) error {
		if err := tx.CheckFence(ctx, fence); err != nil {
			return err
		}
		scan, isScan, err := jobScanOf(ctx, tx, fence.ID, "")
		if err != nil {
			return err
		}
		jobID := scan.jobID
		if !isScan {
			if jobID, err = listingJobOf(ctx, tx, fence.ID); err != nil {
				return err
			}
			if jobID == "" {
				return errNotJobUnit()
			}
		}
		// The job's row before its scan's, in the order of the locks.
		r, err := jobInTx(ctx, tx, jobID, k.D.Lock)
		if err != nil {
			return err
		}
		if r.job.State.Ended() {
			return k.endReleasedUnit(ctx, tx, fence, isScan, now)
		}
		_, err = tx.Exec(ctx, `UPDATE work SET state = 'queued', owner = '', due = ?, last_error = ?,
			max_attempts = max_attempts + 1, updated = ? WHERE id = ? AND attempt = ?`,
			k.D.Time(due), reason, k.D.Time(now), fence.ID, fence.Attempt)
		return err
	})
}

// endReleasedUnit completes a unit given back after its job ended, and
// ends its scan when it is a scan's unit and the scan is still open, so the
// ended job's scans_done counts it. The job has ended, so no event is due.
func (k Sending) endReleasedUnit(ctx context.Context, tx SendingTx, fence tenant.WorkFence, isScan bool, now time.Time) error {
	if err := k.completeUnit(ctx, tx, fence, now); err != nil {
		return err
	}
	if !isScan {
		return nil
	}
	s, _, err := jobScanOf(ctx, tx, fence.ID, k.D.lockOf("js"))
	if err != nil || s.scan.Ended {
		return err
	}
	n, err := tx.Exec(ctx, `UPDATE job_scans SET ended = ? WHERE job_id = ? AND scan_id = ? AND ended = ?`,
		k.D.Bool(true), s.jobID, s.scanID, k.D.Bool(false))
	if err != nil || n == 0 {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE jobs SET scans_done = scans_done + 1 WHERE id = ?`, s.jobID)
	return err
}

// JobsInProgressSQL counts the jobs not ended by state, from
// jobs_in_progress, for a status.
func (d SendingDialect) JobsInProgressSQL() string {
	return `SELECT state, count(*) FROM jobs WHERE ` + d.NotEnded + ` GROUP BY state`
}
