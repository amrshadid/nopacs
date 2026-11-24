package dirimport

import (
	"context"

	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// settle ends partial, at most once an Interval, every import job of the
// tenant whose walks have all ended and whose files have stayed in a
// reception past the receiver's longest open time and SettlingGrace
// (tenant.ImportWork.TimeOutImportSettling): a reception ends by its
// longest open time at the latest, so a file still processing an hour past
// it is held up by something a person must look at, and the job reports
// it rather than run forever. Its counts go on moving after.
func (t *tenantRun) settle(p tenant.Pipeline) {
	now := t.sup.cfg.Now()
	if !t.settled.IsZero() && now.Sub(t.settled) < t.sup.cfg.Interval {
		return
	}
	t.settled = now
	page := master.Page{Limit: master.MaxPageLimit}
	for {
		// Each read and each job has a bound of its own: one bound for the
		// whole pass would leave the jobs after a slow page or job unread,
		// every pass, on a tenant with many jobs in progress.
		ctx, cancel := context.WithTimeout(t.sup.hard, opTimeout)
		jobs, next, err := p.ListJobs(ctx, tenant.JobFilter{InProgress: true}, page)
		cancel()
		if err != nil {
			if t.sup.hard.Err() == nil {
				t.log.Warn("imports: the jobs in progress could not be listed to look for settling ones", "reason", fault.Message(err))
			}
			return
		}
		for _, j := range jobs {
			if t.sup.hard.Err() != nil {
				return
			}
			if j.Kind == tenant.JobImport {
				ctx, cancel := context.WithTimeout(t.sup.hard, opTimeout)
				t.settleJob(ctx, p, j.ID)
				cancel()
			}
		}
		if next == "" {
			return
		}
		page.Cursor = next
	}
}

// settleJob times out the settling of the import job id when it is due.
func (t *tenantRun) settleJob(ctx context.Context, p tenant.Pipeline, id string) {
	j, err := p.ImportJob(ctx, id)
	if err != nil || j.Import == nil || j.Import.SettlingSince.IsZero() || j.State.Ended() {
		return
	}
	open := tenant.DefaultMaxOpenTime
	if rcv, err := p.Receiver(ctx, j.Import.Receiver); err == nil {
		open = rcv.MaxOpenTime
	}
	now := t.sup.cfg.Now()
	if now.Before(j.Import.SettlingSince.Add(open + SettlingGrace)) {
		return
	}
	ended, err := p.TimeOutImportSettling(ctx, id, now)
	if err != nil {
		t.log.Warn("imports: a job past its settling time could not be ended", "job", id, "reason", fault.Message(err))
		return
	}
	t.sup.cfg.Wake(t.name)
	if ended.State.Ended() && ended.Import != nil {
		t.log.Info("imports: a job's files stayed in a reception past its settling time; it ended partial",
			"job", id, "processing", ended.Import.Counts.Processing)
	}
}
