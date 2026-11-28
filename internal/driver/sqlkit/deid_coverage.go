package sqlkit

import (
	"context"
	"strconv"
	"time"

	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// The tenant's coverage reports (tenant.DeidCoverages), as SQL both drivers
// run: the job's row of jobs, its own row of deid_coverage_jobs (the last
// scan counted and what was read), and the elements it counted in
// deid_coverage_elements. The scans are counted as the job's instances, one
// each, so its counts say how far it is.

// QueueDeidCoverage implements tenant.DeidCoverages.
func (k Sending) QueueDeidCoverage(ctx context.Context, q tenant.DeidCoverageQueue, audit master.AuditEntry, guard tenant.Guard) (tenant.Job, error) {
	if err := q.Validate(); err != nil {
		return tenant.Job{}, err
	}
	if err := audit.Validate(); err != nil {
		return tenant.Job{}, err
	}
	var out tenant.Job
	err := k.Host.Write(ctx, "the coverage job cannot be queued", func(tx SendingTx) error {
		if err := tx.LockConfig(ctx); err != nil {
			return err
		}
		if err := k.checkNoRebuild(ctx, tx); err != nil {
			return err
		}
		busy, err := exists(ctx, tx, `SELECT 1 FROM jobs WHERE kind = 'deid_coverage' AND `+k.D.NotEnded)
		if err != nil {
			return err
		}
		if busy {
			return tenant.Precondition("the tenant has a coverage job not ended; one runs at a time")
		}
		if taken, err := exists(ctx, tx, `SELECT 1 FROM jobs WHERE id = ?`, q.ID); err != nil || taken {
			if err != nil {
				return err
			}
			return fault.New(fault.Conflict, "a job with this id exists")
		}
		var scans int64
		if _, err := tx.QueryRow(ctx, `SELECT count(*) FROM scans`, nil, &scans); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO jobs (id, kind, sender, subject_label, session_label, scan_label, reception_id,
			receiver, unique_key, state, ended, reason, created_by_key, created, started, state_at, ended_at, bound_at, retry_of,
			retry_job, list_work, listed, retry_listed, target_listed, retry_after, list_session, list_scan, list_id, scans,
			scans_done, instances, bytes, sent, sent_bytes, warned, warned_bytes, refused, refused_bytes, failures_listed,
			failures_not_listed, progress_at, progress_mark, deid_mode, profile_name, profile_version)
			VALUES (?, 'deid_coverage', '', '', '', '', '', '', NULL, 'queued', ?, '', ?, ?, NULL, ?, NULL, ?, '', '', '', ?, ?, ?, 0, '', '', 0,
			?, 0, ?, 0, 0, 0, 0, 0, 0, 0, 0, 0, NULL, 0, '', '', 0)`,
			q.ID, k.D.Bool(false), q.KeyID, k.D.Time(q.Now), k.D.Time(q.Now), k.D.Time(q.Now), k.D.Bool(true), k.D.Bool(true), k.D.Bool(true),
			scans, scans); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO deid_coverage_jobs (job_id, after_scan_id, skipped, instances, recorded, capped, not_listed, elements, reported)
			VALUES (?, 0, 0, 0, 0, 0, 0, 0, 0)`, q.ID); err != nil {
			return err
		}
		r, err := jobInTx(ctx, tx, q.ID, "")
		if err != nil {
			return err
		}
		e, err := tenant.JobStepEvent("", r.job, q.Now)
		if err != nil {
			return err
		}
		out = r.job
		if err := tx.WriteEvents(ctx, e); err != nil {
			return err
		}
		return tx.Audited(ctx, audit, guard)
	})
	return out, err
}

// coverageIn reads a coverage job's own part with the lock given, or
// NotFound.
func (k Sending) coverageIn(ctx context.Context, tx SendingTx, id, lock string) (tenant.DeidCoverageJob, error) {
	var out tenant.DeidCoverageJob
	found, err := tx.QueryRow(ctx, `SELECT after_scan_id, skipped, instances, recorded, capped, not_listed, elements FROM deid_coverage_jobs WHERE job_id = ?`+lock,
		[]any{id}, &out.After, &out.Skipped, &out.Instances, &out.Recorded, &out.Capped, &out.NotListed, &out.Elements)
	if err != nil {
		return out, err
	}
	if !found {
		return out, notFound("the tenant has no coverage job with this id")
	}
	r, err := jobInTx(ctx, tx, id, lock)
	if err != nil {
		return out, err
	}
	out.Job = r.job
	return out, nil
}

// DeidCoverageJob implements tenant.DeidCoverages.
func (k Sending) DeidCoverageJob(ctx context.Context, id string) (tenant.DeidCoverageJob, error) {
	if err := master.ValidateID("job id", id); err != nil {
		return tenant.DeidCoverageJob{}, err
	}
	var out tenant.DeidCoverageJob
	err := k.Host.Read(ctx, "the coverage job cannot be read", func(tx SendingTx) error {
		var err error
		out, err = k.coverageIn(ctx, tx, id, "")
		return err
	})
	return out, err
}

// DeidCoverageInProgress implements tenant.DeidCoverages.
func (k Sending) DeidCoverageInProgress(ctx context.Context) (tenant.Job, bool, error) {
	var out tenant.Job
	var found bool
	err := k.Host.Read(ctx, "the coverage job cannot be read", func(tx SendingTx) error {
		var id string
		var err error
		found, err = tx.QueryRow(ctx, `SELECT id FROM jobs WHERE kind = 'deid_coverage' AND `+k.D.NotEnded+` ORDER BY id LIMIT 1`, nil, &id)
		if err != nil || !found {
			return err
		}
		r, err := jobInTx(ctx, tx, id, "")
		out = r.job
		return err
	})
	return out, found, err
}

// DeidCoverageScans implements tenant.DeidCoverages.
func (k Sending) DeidCoverageScans(ctx context.Context, after int64, limit int) ([]tenant.DeidCoverageScan, error) {
	if limit < 1 || limit > tenant.MaxDeidCoveragePage {
		return nil, fault.New(fault.Invalid, "a page of scans holds 1 to "+strconv.Itoa(tenant.MaxDeidCoveragePage))
	}
	var out []tenant.DeidCoverageScan
	err := k.Host.Read(ctx, "the scans cannot be listed", func(tx SendingTx) error {
		out = nil
		return tx.Query(ctx, `SELECT id, subject_label, session_label, label, storage FROM scans WHERE id > ? ORDER BY id LIMIT ?`,
			[]any{after, limit}, func(r Row) error {
				var s tenant.DeidCoverageScan
				err := r.Scan(&s.ID, &s.Path.Subject, &s.Path.Session, &s.Path.Scan, &s.Storage)
				out = append(out, s)
				return err
			})
	})
	return out, err
}

// ApplyDeidCoverage implements tenant.DeidCoverages.
func (k Sending) ApplyDeidCoverage(ctx context.Context, a tenant.DeidCoverageApply) (tenant.DeidCoverageJob, error) {
	if err := a.Validate(); err != nil {
		return tenant.DeidCoverageJob{}, err
	}
	var out tenant.DeidCoverageJob
	err := k.Host.Write(ctx, "the coverage job's page cannot be written", func(tx SendingTx) error {
		cj, err := k.coverageIn(ctx, tx, a.JobID, k.D.Lock)
		if err != nil {
			return err
		}
		if cj.Job.State.Ended() {
			return tenant.Precondition("the coverage job has ended " + string(cj.Job.State) + "; it counts nothing more")
		}
		if cj.After != a.After {
			return tenant.Precondition("the page begins at scan " + strconv.FormatInt(a.After, 10) + " and the job stands at scan " +
				strconv.FormatInt(cj.After, 10) + "; another runner wrote it")
		}
		var read, skipped, instances, recorded, capped, notListed int64
		for _, s := range a.Scans {
			if s.Skipped {
				skipped++
				continue
			}
			read++
			instances += s.Instances
			recorded += s.Recorded
			capped += s.Capped
			notListed += s.NotListed
		}
		elements := cj.Elements
		for _, e := range a.Elements {
			n, err := tx.Exec(ctx, `UPDATE deid_coverage_elements SET instances = instances + ?, occurrences = occurrences + ?
				WHERE job_id = ? AND tag = ? AND creator = ? AND withheld = ? AND vr = ?`,
				e.Instances, e.Occurrences, a.JobID, int64(e.Tag), e.Creator, withheldOf(e.Withheld), e.VR)
			if err != nil {
				return err
			}
			if n > 0 {
				continue
			}
			if elements >= tenant.MaxCoverageElements {
				// A report holds a bounded number of distinct elements; what is
				// past it is counted, never dropped without a trace.
				notListed += e.Occurrences
				continue
			}
			if _, err := tx.Exec(ctx, `INSERT INTO deid_coverage_elements (job_id, tag, creator, withheld, vr, instances, occurrences) VALUES (?, ?, ?, ?, ?, ?, ?)`,
				a.JobID, int64(e.Tag), e.Creator, withheldOf(e.Withheld), e.VR, e.Instances, e.Occurrences); err != nil {
				return err
			}
			elements++
		}
		if _, err := tx.Exec(ctx, `UPDATE deid_coverage_jobs SET after_scan_id = ?, skipped = skipped + ?, instances = instances + ?, recorded = recorded + ?,
			capped = capped + ?, not_listed = not_listed + ?, elements = ? WHERE job_id = ?`,
			a.Reached(), skipped, instances, recorded, capped, notListed, elements, a.JobID); err != nil {
			return err
		}
		c := cj.Job.Counts
		done := c.ScansDone + int64(len(a.Scans))
		sent, refused := c.InstancesSent+read, c.InstancesRefused+skipped
		if _, err := tx.Exec(ctx, `UPDATE jobs SET scans_done = ?, scans = ?, sent = ?, refused = ?, instances = ? WHERE id = ?`,
			done, max(c.Scans, done), sent, refused, max(c.Instances, sent+refused), a.JobID); err != nil {
			return err
		}
		events, err := k.batchProgressed(ctx, tx, cj.Job, a.Now)
		if err != nil {
			return err
		}
		if out, err = k.coverageIn(ctx, tx, a.JobID, ""); err != nil {
			return err
		}
		return tx.WriteEvents(ctx, events...)
	})
	return out, err
}

// withheldOf returns the number a report stores for a withheld creator.
func withheldOf(b bool) int {
	if b {
		return 1
	}
	return 0
}

// EndDeidCoverage implements tenant.DeidCoverages.
func (k Sending) EndDeidCoverage(ctx context.Context, id string, now time.Time) (tenant.Job, error) {
	if err := master.ValidateID("job id", id); err != nil {
		return tenant.Job{}, err
	}
	if err := master.ValidateTime("time", now); err != nil {
		return tenant.Job{}, err
	}
	var out tenant.Job
	err := k.Host.Write(ctx, "the coverage job cannot be ended", func(tx SendingTx) error {
		cj, err := k.coverageIn(ctx, tx, id, k.D.Lock)
		if err != nil {
			return err
		}
		out = cj.Job
		if cj.Job.State.Ended() {
			return nil
		}
		if more, err := exists(ctx, tx, `SELECT 1 FROM scans WHERE id > ?`, cj.After); err != nil || more {
			if err != nil {
				return err
			}
			return tenant.Precondition("the coverage job has scans past its place left to read")
		}
		c := cj.Job.Counts
		if _, err := tx.Exec(ctx, `UPDATE jobs SET instances = ?, scans = ? WHERE id = ?`, c.InstancesSent+c.InstancesRefused, c.ScansDone, id); err != nil {
			return err
		}
		after, err := jobInTx(ctx, tx, id, "")
		if err != nil {
			return err
		}
		state, reason := tenant.EndState(after.job.Counts)
		if state == tenant.JobDone || state == tenant.JobPartial {
			// The report is this job's elements: those of every job before
			// are dropped, and it is marked the report, in the transaction
			// that ends it, so a reader never sees none or two, and two jobs
			// that end at one instant are not told apart by their times.
			if _, err := tx.Exec(ctx, `DELETE FROM deid_coverage_elements WHERE job_id <> ?`, id); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE deid_coverage_jobs SET reported = CASE WHEN job_id = ? THEN 1 ELSE 0 END`, id); err != nil {
				return err
			}
		}
		ended, events, err := k.endJob(ctx, tx, after.job, state, reason, now)
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

// StopDeidCoverage implements tenant.DeidCoverages.
func (k Sending) StopDeidCoverage(ctx context.Context, id, reason string, now time.Time) (tenant.Job, error) {
	if err := master.ValidateID("job id", id); err != nil {
		return tenant.Job{}, err
	}
	if reason == "" || len(reason) > tenant.MaxFailureLength {
		return tenant.Job{}, fault.New(fault.Invalid, "a coverage job is stopped with a reason of 1 to "+strconv.Itoa(tenant.MaxFailureLength)+" bytes")
	}
	if err := master.ValidateTime("time", now); err != nil {
		return tenant.Job{}, err
	}
	var out tenant.Job
	err := k.Host.Write(ctx, "the coverage job cannot be stopped", func(tx SendingTx) error {
		cj, err := k.coverageIn(ctx, tx, id, k.D.Lock)
		if err != nil {
			return err
		}
		out = cj.Job
		if cj.Job.State.Ended() {
			return nil
		}
		if _, err := tx.Exec(ctx, `DELETE FROM deid_coverage_elements WHERE job_id = ?`, id); err != nil {
			return err
		}
		ended, events, err := k.endJob(ctx, tx, cj.Job, tenant.JobFailed, reason, now)
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

// DeidCoverageReport implements tenant.DeidCoverages.
func (k Sending) DeidCoverageReport(ctx context.Context) (tenant.DeidCoverageReport, bool, error) {
	var out tenant.DeidCoverageReport
	var found bool
	err := k.Host.Read(ctx, "the coverage report cannot be read", func(tx SendingTx) error {
		var id string
		var err error
		found, err = tx.QueryRow(ctx, `SELECT job_id FROM deid_coverage_jobs WHERE reported = 1`, nil, &id)
		if err != nil || !found {
			return err
		}
		cj, err := k.coverageIn(ctx, tx, id, "")
		if err != nil {
			return err
		}
		out = tenant.DeidCoverageReport{
			JobID: id, Ended: cj.Job.Ended, Partial: cj.Job.State == tenant.JobPartial,
			Scans: cj.Job.Counts.InstancesSent, ScansSkipped: cj.Skipped,
			Instances: cj.Instances, Recorded: cj.Recorded, Capped: cj.Capped, NotListed: cj.NotListed, Elements: cj.Elements,
		}
		return nil
	})
	return out, found, err
}

// DeidCoverageElements implements tenant.DeidCoverages.
func (k Sending) DeidCoverageElements(ctx context.Context, jobID string, after *tenant.CoverageKey, limit int) ([]tenant.CoverageElement, error) {
	if err := master.ValidateID("job id", jobID); err != nil {
		return nil, err
	}
	if limit < 1 || limit > tenant.MaxCoverageElementsPage {
		return nil, fault.New(fault.Invalid, "a page of elements holds 1 to "+strconv.Itoa(tenant.MaxCoverageElementsPage))
	}
	query := `SELECT tag, creator, withheld, vr, instances, occurrences FROM deid_coverage_elements WHERE job_id = ?`
	args := []any{jobID}
	if after != nil {
		query += ` AND (tag, creator, withheld, vr) > (?, ?, ?, ?)`
		args = append(args, int64(after.Tag), after.Creator, withheldOf(after.Withheld), after.VR)
	}
	query += ` ORDER BY tag, creator, withheld, vr LIMIT ?`
	args = append(args, limit)
	var out []tenant.CoverageElement
	err := k.Host.Read(ctx, "the coverage report cannot be read", func(tx SendingTx) error {
		out = nil
		return tx.Query(ctx, query, args, func(r Row) error {
			var e tenant.CoverageElement
			var tag int64
			var withheld int
			err := r.Scan(&tag, &e.Creator, &withheld, &e.VR, &e.Instances, &e.Occurrences)
			e.Tag, e.Withheld = uint32(tag), withheld != 0 //nolint:gosec // G115: a column holding 0 to 2^32-1, checked on writing.
			out = append(out, e)
			return err
		})
	})
	return out, err
}
