package repair

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// ErrNotDue is a Run that found no repair job's unit due: none is queued,
// another worker holds it, or it waits for its next attempt after a failed
// one. Nothing was done, and a later Run takes it.
var ErrNotDue = errors.New("repair: no repair job's unit is due")

// ErrGaveUp is a Run whose job's unit used its last attempt and is held as
// failed, its job ended failed, for a person to see.
var ErrGaveUp = errors.New("repair: the job's unit used its last attempt and is held as failed")

// ErrTryAgain is in the chain of a Run that met findings a storage or the
// database did not let it finish: the attempt is recorded failed and the
// unit is due again later.
var ErrTryAgain = errors.New("repair: some objects could not be repaired now; the job is tried again")

// Run claims the next repair job's unit due (tenant.RepairWorkKind, queued
// with the job), starts its job, mends each finding the job names that
// holds on its storage, and ends the job with its unit. A job canceled
// before is ended doing nothing. A stop (ctx ending) or a lost lease leaves
// the unit leased, and another Run takes it over once its lease ends.
func (r *Runner) Run(ctx context.Context) (Result, error) {
	cctx, cancel := context.WithTimeout(ctx, opTimeout)
	w, err := r.cfg.DB.ClaimWork(cctx, tenant.RepairWorkKind, r.cfg.Owner, r.cfg.Now(), r.cfg.Lease)
	cancel()
	if fault.KindOf(err) == fault.NotFound {
		return Result{}, ErrNotDue
	}
	if err != nil {
		return Result{}, err
	}
	l := r.newLease(ctx, w.Fence(), w.Due)
	defer l.cancel(nil)
	stop := l.hold()
	defer stop()
	ru := &run{r: r, fence: w.Fence(), ctx: l.ctx, lease: l, res: Result{Job: w.Subject, Counts: map[OutcomeKind]int64{}},
		replicas: map[string]tenant.Storage{}, sources: map[string]blob.Store{}}
	walkErr := ru.start()
	if walkErr == nil && !ru.ended {
		walkErr = ru.walk()
	}
	ru.close()
	stop()
	res := ru.res
	if errors.Is(walkErr, errJobEnded) && l.ctx.Err() == nil {
		// The job was canceled while it ran: its unit is ended with it, every
		// hold of its repairs let go, and nothing more is put.
		ectx, ecancel := context.WithTimeout(context.WithoutCancel(ctx), opTimeout)
		defer ecancel()
		if _, err := r.cfg.DB.EndRepairJob(ectx, w.Fence(), r.cfg.Now()); err != nil {
			return res, err
		}
		res.Done = true
		return res, nil
	}
	if l.ctx.Err() != nil || errors.Is(walkErr, errStop) {
		// A stop, or a lost lease: the unit is left to its lease, and
		// whoever claims it next walks the findings again.
		if ctx.Err() != nil {
			return res, ctx.Err()
		}
		return res, tenant.ErrLeaseLost
	}
	now := r.cfg.Now()
	ectx, ecancel := context.WithTimeout(context.WithoutCancel(ctx), opTimeout)
	defer ecancel()
	if walkErr != nil || res.Counts[Retry] > 0 {
		reason := "some objects could not be repaired now; the first: " + ru.firstRetry
		if walkErr != nil {
			reason = "the job could not read what it repairs (" + fault.KindOf(walkErr).String() + ")"
		}
		if len(reason) > tenant.MaxWorkTextLength {
			reason = reason[:tenant.MaxWorkTextLength]
		}
		failed, err := r.cfg.DB.FailWork(ectx, w.Fence(), reason, now)
		switch {
		case err != nil && !errors.Is(err, tenant.ErrLeaseLost):
			r.log.Warn("repairs: a failed attempt could not be recorded; the unit's lease runs out", "job", res.Job,
				"kind", fault.KindOf(err).String())
		case err == nil && failed.State == tenant.WorkFailed:
			return res, errors.Join(ErrGaveUp, walkErr)
		}
		if walkErr != nil {
			return res, errors.Join(ErrTryAgain, walkErr)
		}
		return res, ErrTryAgain
	}
	if _, err := r.cfg.DB.EndRepairJob(ectx, w.Fence(), now); err != nil {
		return res, err
	}
	res.Done = true
	return res, nil
}

// run is one Run's walk of the job's findings under its lease.
type run struct {
	r     *Runner
	job   Job
	fence tenant.WorkFence
	ctx   context.Context
	lease *lease
	res   Result
	// ended says the job had ended (canceled) when its unit started: the
	// unit is ended and nothing walked.
	ended bool
	// firstRetry is the reason of the first finding left for the next
	// attempt, kept for the attempt's failure.
	firstRetry string
	// storage is the primary's row and store, read and opened on first
	// use; replicas the replicas' rows read so far.
	storage  *tenant.Storage
	primary  blob.Store
	replicas map[string]tenant.Storage
	sources  map[string]blob.Store
}

// errStop ends a walk: the run's context ended (a stop or a lost lease).
var errStop = errors.New("repair: the run stopped")

// errJobEnded ends a walk: the job ended or was canceled while it ran, as a
// repair's record answered.
var errJobEnded = errors.New("repair: the job ended while it ran")

// start starts the unit's job (queued to running) and reads it from its
// row. A job ended already is ended with its unit here.
func (u *run) start() error {
	ctx, cancel := context.WithTimeout(u.ctx, opTimeout)
	unit, err := u.r.cfg.DB.StartJobUnit(ctx, u.fence, u.r.cfg.Now())
	cancel()
	if err != nil {
		return u.stopOr(err)
	}
	if unit.Job.State.Ended() {
		ctx, cancel := context.WithTimeout(u.ctx, opTimeout)
		_, err := u.r.cfg.DB.EndRepairJob(ctx, u.fence, u.r.cfg.Now())
		cancel()
		if err != nil {
			return u.stopOr(err)
		}
		u.ended, u.res.Done = true, true
		return nil
	}
	u.job, err = jobOf(unit.Job)
	return err
}

// walk mends each finding the job names that holds on its storage: a job
// naming findings reads those alone, in pages; one of all, or automatic,
// reads the storage's findings that hold, page by page.
func (u *run) walk() error {
	kinds := map[tenant.ScrubFindingKind]bool{}
	for _, k := range u.job.kinds() {
		kinds[k] = true
	}
	if len(u.job.Findings) > 0 {
		for start := 0; start < len(u.job.Findings); start += findingsPage {
			ids := u.job.Findings[start:min(start+findingsPage, len(u.job.Findings))]
			ctx, cancel := context.WithTimeout(u.ctx, opTimeout)
			page, err := u.r.cfg.DB.RepairFindings(ctx, ids)
			cancel()
			if err != nil {
				return u.stopOr(err)
			}
			met := map[string]bool{}
			for _, f := range page {
				met[f.ID] = true
				if !f.Holds() || f.Storage != u.job.Storage || !kinds[f.Kind] {
					u.tell(Outcome{Finding: f.ID, Kind: Skipped, Reason: "the finding no longer holds on the storage, or is of a kind a copy does not mend"})
					continue
				}
				if err := u.meet(f); err != nil {
					return err
				}
			}
			for _, id := range ids {
				if !met[id] {
					u.tell(Outcome{Finding: id, Kind: Skipped, Reason: "the tenant no longer has the finding"})
				}
			}
		}
		return nil
	}
	filter := tenant.ScrubFindingFilter{Kinds: u.job.kinds(), Storage: u.job.Storage}
	cursor := ""
	for {
		ctx, cancel := context.WithTimeout(u.ctx, opTimeout)
		page, next, err := u.r.cfg.DB.ListScrubFindings(ctx, filter, master.Page{Cursor: cursor, Limit: findingsPage})
		cancel()
		if err != nil {
			return u.stopOr(err)
		}
		for _, f := range page {
			if err := u.meet(f); err != nil {
				return err
			}
		}
		if next == "" {
			return nil
		}
		cursor = next
	}
}

// meet mends f and counts what became of it.
func (u *run) meet(f tenant.ScrubFinding) error {
	out, err := u.mend(f)
	if err != nil {
		return err
	}
	if out.Kind == NoGoodCopy {
		// Counted on the job's row with its tally; a repaired or good
		// object was counted in the transaction that ended its repair.
		ctx, cancel := context.WithTimeout(u.ctx, opTimeout)
		_, err := u.r.cfg.DB.RecordRepairOutcome(ctx, tenant.RepairOutcome{Fence: u.fence, Finding: f.ID, Kind: tenant.RepairNoGoodCopy,
			Now: u.r.cfg.Now()})
		cancel()
		if err != nil {
			if errors.Is(u.stopOr(err), errStop) {
				return errStop
			}
			out = Outcome{Finding: f.ID, Kind: Retry, Reason: retryReason("the outcome could not be counted", err)}
		}
	}
	u.tell(out)
	return nil
}

// stopOr returns errStop when the run's context ended, err otherwise.
func (u *run) stopOr(err error) error {
	if u.ctx.Err() != nil || errors.Is(err, tenant.ErrLeaseLost) {
		return errStop
	}
	return err
}

// tell counts an outcome and hands it to the caller's hook.
func (u *run) tell(o Outcome) {
	u.res.Counts[o.Kind]++
	u.res.Bytes += o.Bytes
	switch o.Kind {
	case Repaired:
		u.r.log.Info("repairs: an object was put back from a replica and its finding cleared", "job", u.job.ID, "finding", o.Finding)
	case AlreadyGood:
		u.r.log.Info("repairs: an object read whole again; its finding is cleared", "job", u.job.ID, "finding", o.Finding)
	default:
		if o.Kind == Retry && u.firstRetry == "" {
			u.firstRetry = o.Reason
		}
		u.r.log.Warn("repairs: an object was not repaired", "job", u.job.ID, "finding", o.Finding, "outcome", string(o.Kind),
			"reason", o.Reason)
	}
	if u.r.cfg.Outcome != nil {
		u.r.cfg.Outcome(o)
	}
}

// close closes every storage the run opened.
func (u *run) close() {
	if u.primary != nil {
		_ = u.primary.Close() // every put was answered before; nothing to lose
	}
	for _, s := range u.sources {
		_ = s.Close() // only read from; nothing to lose
	}
}

// lease is the unit's lease, renewed from a timer every third of it while
// the run holds the unit. A renewal answered tenant.ErrLeaseLost (another
// attempt holds the unit) cancels ctx with that cause, so no later write is
// made. One that fails otherwise (the database not answering) is tried
// again at the next tick, until the lease's own time has run out since the
// last renewal that landed: then ctx is canceled as lost too, so a put in
// flight stops, and every write of a repair carries the fence all the same.
// A put does not rest on that cancel, which a timer makes late: each put is
// cut by its own deadline before the last due the lease was renewed to
// (run.put), so it ends before the hold its repair recorded runs out.
type lease struct {
	r      *Runner
	fence  tenant.WorkFence
	ctx    context.Context
	cancel context.CancelCauseFunc
	// last is the due the claim or the latest renewal that landed
	// returned, in Unix nanoseconds.
	last atomic.Int64
}

// newLease returns the lease of fence under parent, claimed with due.
func (r *Runner) newLease(parent context.Context, fence tenant.WorkFence, due time.Time) *lease {
	ctx, cancel := context.WithCancelCause(parent)
	l := &lease{r: r, fence: fence, ctx: ctx, cancel: cancel}
	l.last.Store(due.UnixNano())
	return l
}

// due returns the due the claim or the latest renewal that landed returned.
func (l *lease) due() time.Time { return time.Unix(0, l.last.Load()).UTC() }

// hold renews the lease until the returned stop is called; stop waits for a
// renewal in flight, so none runs after it returns, and may be called more
// than once.
func (l *lease) hold() (stop func()) {
	done, finished := make(chan struct{}), make(chan struct{})
	var once sync.Once
	go func() {
		defer close(finished)
		t := time.NewTicker(max(l.r.cfg.Lease/3, time.Millisecond))
		defer t.Stop()
		renewed := l.r.cfg.Now()
		for {
			select {
			case <-done:
				return
			case <-l.ctx.Done():
				return
			case <-t.C:
				ctx, cancel := context.WithTimeout(l.ctx, opTimeout)
				w, err := l.r.cfg.DB.RenewWork(ctx, l.fence, l.r.cfg.Now(), l.r.cfg.Lease)
				cancel()
				if errors.Is(err, tenant.ErrLeaseLost) {
					l.cancel(tenant.ErrLeaseLost)
					return
				}
				if err == nil {
					renewed = l.r.cfg.Now()
					l.last.Store(w.Due.UnixNano())
					continue
				}
				if l.ctx.Err() != nil {
					return
				}
				if !l.r.cfg.Now().Before(renewed.Add(l.r.cfg.Lease)) {
					l.r.log.Warn("repairs: the job's lease ran out unrenewed; the run stops", "kind", fault.KindOf(err).String())
					l.cancel(tenant.ErrLeaseLost)
					return
				}
				l.r.log.Warn("repairs: the job's lease could not be renewed; tried again", "kind", fault.KindOf(err).String())
			}
		}
	}()
	return func() {
		once.Do(func() { close(done) })
		<-finished
	}
}

// retryReason returns reason with the kind of the error that made it, the
// fault package's word for it: never the storage's or the database's own
// text, which may carry an address or a credential's name.
func retryReason(reason string, err error) string {
	if err == nil {
		return reason
	}
	return reason + " (" + fault.KindOf(err).String() + ")"
}
