package sqlkit

import (
	"context"
	"time"

	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// EndDisposeJob implements tenant.Disposal. A dispose job not ended keeps
// its rule from the daily pass (QueueDispose refuses a second one of the
// rule), so the job must end once every session it lists has its outcome,
// or the rule never disposes again.
func (k Sending) EndDisposeJob(ctx context.Context, id string, now time.Time) (tenant.Job, error) {
	if err := master.ValidateID("job id", id); err != nil {
		return tenant.Job{}, err
	}
	if err := master.ValidateTime("time", now); err != nil {
		return tenant.Job{}, err
	}
	var out tenant.Job
	err := k.Host.Write(ctx, "the dispose job cannot be ended", func(tx SendingTx) error {
		if has, err := exists(ctx, tx, `SELECT 1 FROM dispose_jobs WHERE job_id = ?`, id); err != nil || !has {
			if err != nil {
				return err
			}
			return notFound("the tenant has no dispose job with this id")
		}
		r, err := jobInTx(ctx, tx, id, k.D.Lock)
		if err != nil {
			return err
		}
		out = r.job
		if r.job.State.Ended() {
			return nil
		}
		var pending, done int64
		if _, err := tx.QueryRow(ctx, `SELECT COALESCE(SUM(CASE WHEN outcome = '' THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN outcome <> '' THEN 1 ELSE 0 END), 0) FROM dispose_sessions WHERE job_id = ?`,
			[]any{id}, &pending, &done); err != nil {
			return err
		}
		if pending > 0 {
			return tenant.Precondition("the dispose job lists sessions whose outcome is not recorded yet")
		}
		if _, err := tx.Exec(ctx, `UPDATE jobs SET scans_done = ?, listed = ? WHERE id = ?`, done, k.D.Bool(true), id); err != nil {
			return err
		}
		// A skip is an outcome, not a failure: the session was looked at
		// and left for the reason recorded (held, not due, gone, undone).
		ended, events, err := k.endJob(ctx, tx, r.job, tenant.JobDone, "", now)
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
