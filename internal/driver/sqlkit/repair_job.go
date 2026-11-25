package sqlkit

import (
	"context"
	"time"

	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// A repair job (tenant.JobRepair): its row of repair_jobs, made with the
// job; its one unit of work, the job's listing unit, queued by insertJob
// with the kind tenant.RepairWorkKind, so a give-up (GiveUpJobUnit) and a
// cancel find it as they find every job's; and its counts, kept per
// finding in repair_outcomes so an attempt that walks the findings again
// counts none twice.

// checkRepairJob refuses a repair job the tenant cannot run: a storage it
// does not have (NotFound) or that is a replica, a replica named that is
// not a replica of it, a finding it does not have (NotFound) or that is on
// another storage, cleared or of a kind a repair does not mend, a switch of
// the storage running, and another repair of the storage queued or running.
// It serializes with the tenant's changes of configuration, so two repairs
// made at once on several writers cannot both pass.
func (k Sending) checkRepairJob(ctx context.Context, tx SendingTx, r tenant.RepairJob) error {
	if err := tx.LockConfig(ctx); err != nil {
		return err
	}
	storage, found, err := storageIn(ctx, tx, r.Storage, k.D.Share)
	if err != nil {
		return err
	}
	if !found {
		return notFound("the tenant has no storage named " + r.Storage)
	}
	if storage.ReplicaOf != "" {
		return tenant.Precondition("the storage " + r.Storage + " is a replica; the server copies its objects again by itself")
	}
	if r.From != "" {
		from, found, err := storageIn(ctx, tx, r.From, k.D.Share)
		if err != nil {
			return err
		}
		if !found {
			return notFound("the tenant has no storage named " + r.From)
		}
		if from.ReplicaOf != r.Storage {
			return tenant.Precondition("the storage " + r.From + " is not a replica of " + r.Storage)
		}
	}
	for _, id := range r.Findings {
		f, found, err := findingIn(ctx, tx, findingByIDSQL, id)
		if err != nil {
			return err
		}
		switch {
		case !found:
			return notFound("the tenant has no scrub finding with the id " + id)
		case f.Storage != r.Storage:
			return tenant.Precondition("the finding " + id + " is on another storage")
		case !f.Holds():
			return tenant.Precondition("the finding " + id + " was cleared")
		case !f.Kind.Mendable():
			return tenant.Precondition("a repair does not mend a finding of kind " + string(f.Kind))
		}
	}
	switching, err := exists(ctx, tx, `SELECT 1 FROM storage_switches WHERE state IN ('accepted', 'swapped')
		AND (primary_name = ? OR replica_name = ?) LIMIT 1`, r.Storage, r.Storage)
	if err != nil {
		return err
	}
	if switching {
		return tenant.Precondition("a switch of the storage " + r.Storage + " is in progress")
	}
	// The condition on ended is written as the dialect writes it (only jobs
	// has the column), so it matches the partial index of jobs not ended.
	busy, err := exists(ctx, tx, `SELECT 1 FROM repair_jobs AS rj JOIN jobs AS j ON j.id = rj.job_id WHERE rj.storage = ?
		AND `+k.D.NotEnded+` LIMIT 1`, r.Storage)
	if err != nil {
		return err
	}
	if busy {
		return tenant.Precondition("another repair of the storage " + r.Storage + " is queued or running")
	}
	return nil
}

// insertRepairJob writes the repair row of the job of id, before the job's
// row, so the job the insert reads back carries it.
func (k Sending) insertRepairJob(ctx context.Context, tx SendingTx, id string, r tenant.RepairJob) error {
	findings := r.Findings
	if findings == nil {
		findings = []string{}
	}
	text, err := JSONText(findings)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO repair_jobs (job_id, storage, from_replica, findings, all_findings, automatic)
		VALUES (?, ?, ?, ?, ?, ?)`, id, r.Storage, r.From, text, k.D.Bool(r.All), k.D.Bool(r.Automatic))
	return err
}

// fillRepair gives a repair job what its repair row holds.
func fillRepair(ctx context.Context, tx SendingTx, j *tenant.Job) error {
	r := &tenant.RepairJob{}
	var findings string
	found, err := tx.QueryRow(ctx, `SELECT storage, from_replica, findings, all_findings, automatic FROM repair_jobs WHERE job_id = ?`,
		[]any{j.ID}, &r.Storage, &r.From, &findings, &r.All, &r.Automatic)
	if err != nil {
		return err
	}
	if !found {
		return fault.New(fault.Internal, "a repair job has no repair row")
	}
	if err := FromJSON(findings, &r.Findings); err != nil {
		return err
	}
	if len(r.Findings) == 0 {
		r.Findings = nil
	}
	j.Repair = r
	return nil
}

// repairJobOfUnit returns the repair job whose unit holds the fence,
// locked, after checking the fence. A unit of another job, or of none, is a
// precondition failure.
func (k Sending) repairJobOfUnit(ctx context.Context, tx SendingTx, fence tenant.WorkFence) (jobRow, error) {
	if err := tx.CheckFence(ctx, fence); err != nil {
		return jobRow{}, err
	}
	id, err := listingJobOf(ctx, tx, fence.ID)
	if err != nil {
		return jobRow{}, err
	}
	if id == "" {
		return jobRow{}, errNotJobUnit()
	}
	r, err := jobInTx(ctx, tx, id, k.D.Lock)
	if err != nil {
		return jobRow{}, err
	}
	if r.job.Kind != tenant.JobRepair {
		return jobRow{}, tenant.Precondition("the unit of work is not a repair job's")
	}
	return r, nil
}

// countJobRepair counts the repair r ended as kind on its job, when its
// job is a repair job; a repair of no job (a test's, a record made by
// hand) counts nothing.
func (k Sending) countJobRepair(ctx context.Context, tx SendingTx, r tenant.Repair, kind tenant.RepairOutcomeKind, now time.Time) error {
	isRepairJob, err := exists(ctx, tx, `SELECT 1 FROM repair_jobs WHERE job_id = ?`, r.Job)
	if err != nil || !isRepairJob {
		return err
	}
	j, err := jobInTx(ctx, tx, r.Job, k.D.Lock)
	if err != nil {
		return err
	}
	var bytes int64
	if kind == tenant.RepairRepaired {
		bytes = r.ExpectedSize
	}
	events, err := k.countOutcome(ctx, tx, j, r.Finding, kind, bytes, now)
	if err != nil {
		return err
	}
	return tx.WriteEvents(ctx, events...)
}

// countOutcome counts what became of one finding on the repair job of row
// j, locked: once per finding, but for one counted no good copy and now
// repaired or good, which moves from refused to sent. It returns the
// progress event when one is due. A job whose counts are final counts
// nothing.
func (k Sending) countOutcome(ctx context.Context, tx SendingTx, j jobRow, finding string, kind tenant.RepairOutcomeKind, bytes int64,
	now time.Time) ([]tenant.EventRecord, error) {
	if !j.job.State.TakesProgress() {
		return nil, nil
	}
	var old string
	var oldBytes int64
	counted, err := tx.QueryRow(ctx, `SELECT outcome, bytes FROM repair_outcomes WHERE job_id = ? AND finding_id = ?`+k.D.Lock,
		[]any{j.job.ID, finding}, &old, &oldBytes)
	if err != nil {
		return nil, err
	}
	var instances, allBytes, sent, sentBytes, refused, refusedBytes int64
	switch {
	case !counted:
		if _, err := tx.Exec(ctx, `INSERT INTO repair_outcomes (job_id, finding_id, outcome, bytes) VALUES (?, ?, ?, ?)`,
			j.job.ID, finding, string(kind), bytes); err != nil {
			return nil, err
		}
		instances, allBytes = 1, bytes
	case old == string(tenant.RepairNoGoodCopy) && kind.Sent():
		if _, err := tx.Exec(ctx, `UPDATE repair_outcomes SET outcome = ?, bytes = ? WHERE job_id = ? AND finding_id = ?`,
			string(kind), bytes, j.job.ID, finding); err != nil {
			return nil, err
		}
		if _, err := tx.Exec(ctx, `UPDATE job_tallies SET count = count - 1 WHERE job_id = ? AND tally = ? AND count > 0`,
			j.job.ID, string(tenant.RepairNoGoodCopy)); err != nil {
			return nil, err
		}
		allBytes, refused, refusedBytes = bytes-oldBytes, -1, -oldBytes
	default:
		return nil, nil
	}
	if kind.Sent() {
		sent, sentBytes = sent+1, sentBytes+bytes
	} else {
		refused, refusedBytes = refused+1, refusedBytes+bytes
	}
	if err := k.addTallies(ctx, tx, j.job.ID, map[string]int64{string(kind): 1}); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `UPDATE jobs SET instances = instances + ?, bytes = bytes + ?, sent = sent + ?, sent_bytes = sent_bytes + ?,
		refused = refused + ?, refused_bytes = refused_bytes + ? WHERE id = ?`,
		instances, allBytes, sent, sentBytes, refused, refusedBytes, j.job.ID); err != nil {
		return nil, err
	}
	after, err := jobInTx(ctx, tx, j.job.ID, "")
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
	if _, err := tx.Exec(ctx, `UPDATE jobs SET progress_at = ?, progress_mark = ? WHERE id = ?`,
		k.D.Time(now), tenant.ProgressMark(after.job.Counts), j.job.ID); err != nil {
		return nil, err
	}
	return []tenant.EventRecord{e}, nil
}

// RecordRepairOutcome implements tenant.Repairs.
func (k Sending) RecordRepairOutcome(ctx context.Context, o tenant.RepairOutcome) (tenant.Job, error) {
	if err := o.Validate(); err != nil {
		return tenant.Job{}, err
	}
	var out tenant.Job
	err := k.Host.Write(ctx, "the repair's outcome cannot be recorded", func(tx SendingTx) error {
		r, err := k.repairJobOfUnit(ctx, tx, o.Fence)
		if err != nil {
			return err
		}
		events, err := k.countOutcome(ctx, tx, r, o.Finding, o.Kind, o.Bytes, o.Now)
		if err != nil {
			return err
		}
		after, err := jobInTx(ctx, tx, r.job.ID, "")
		if err != nil {
			return err
		}
		out = after.job
		return tx.WriteEvents(ctx, events...)
	})
	if err != nil {
		return tenant.Job{}, err
	}
	return out, nil
}

// EndRepairJob implements tenant.Repairs.
func (k Sending) EndRepairJob(ctx context.Context, fence tenant.WorkFence, now time.Time) (tenant.Job, error) {
	if err := fence.Validate(); err != nil {
		return tenant.Job{}, err
	}
	var out tenant.Job
	err := k.Host.Write(ctx, "the repair job cannot be ended", func(tx SendingTx) error {
		r, err := k.repairJobOfUnit(ctx, tx, fence)
		if err != nil {
			return err
		}
		out = r.job
		var events []tenant.EventRecord
		if !r.job.State.Ended() {
			if _, err := tx.Exec(ctx, `UPDATE jobs SET listed = ? WHERE id = ?`, k.D.Bool(true), r.job.ID); err != nil {
				return err
			}
			state, reason := tenant.EndState(r.job.Counts)
			if out, events, err = k.endJob(ctx, tx, r.job, state, reason, now); err != nil {
				return err
			}
		}
		if err := k.completeUnit(ctx, tx, fence, now); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE repairs SET open_work = NULL, open_attempt = NULL, hold_until = NULL
			WHERE job_id = ? AND open_work IS NOT NULL`, r.job.ID); err != nil {
			return err
		}
		return tx.WriteEvents(ctx, events...)
	})
	if err != nil {
		return tenant.Job{}, err
	}
	return out, nil
}
