package servecmd

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/amrshadid/nopacs/internal/correct"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/server"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// The pace of retention's pass.
const (
	// retentionCheckInterval is how often a tenant's retention clock is
	// checked (tenant.Retention.CheckRetentionClock): at least hourly, so a
	// clock that jumped pauses retention, and with it every purge started by
	// its time (tenant.Corrections.CheckPurgeClock), within the hour.
	retentionCheckInterval = time.Hour
	// retentionPassInterval is how often the check is the daily pass, which
	// queues a dispose job for each delete rule with sessions due and works
	// the dispose jobs not ended.
	retentionPassInterval = 24 * time.Hour
	// disposeMethod is the method of the audit entry of a disposal the
	// daily pass queues, and of each delete it makes.
	disposeMethod = "retention.dispose"
)

// tenantRetention runs one tenant's retention: the clock checked every
// retentionCheckInterval, and once a day the pass that disposes of what
// the tenant's delete rules make due, each session through the ordinary
// delete of the corrections engine (records, grace, undo, a purge on every
// copy), never a second path.
type tenantRetention struct {
	w    *tenantWork
	name string
	// st is the tenant's database as the background work writes it, each
	// write fenced (fencedDB); base is the database as the registry
	// answered it, by which the corrections engines know the tenant's
	// engine (correct.Engines.Use). Nil base is st.
	st   tenantDB
	base tenantDB
	log  *slog.Logger

	// owed is set from the check that decides on a daily pass until a pass
	// succeeds: the store records the pass's time when the check is made,
	// before the pass runs, so a pass that failed would otherwise wait a
	// whole day for its next try rather than the next hourly check.
	owed bool
}

// run checks the clock, and passes when a day has gone since the last
// pass, until ctx ends.
func (r *tenantRetention) run(ctx context.Context) {
	t := time.NewTicker(retentionCheckInterval)
	defer t.Stop()
	for {
		if err := r.once(ctx); err != nil && ctx.Err() == nil {
			r.log.Warn("retention: the pass could not run; tried again within the hour", "kind", fault.KindOf(err).String(),
				"reason", fault.Message(err))
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// once checks the tenant's retention clock and, when the last pass is a
// day old or more, makes this check the pass and disposes.
func (r *tenantRetention) once(ctx context.Context) error {
	values := r.w.values.OnTenant(r.st)
	step, err := values.MaxClockStep(ctx)
	if err != nil {
		return err
	}
	notice, err := values.ShortenNotice(ctx)
	if err != nil {
		return err
	}
	cur, err := r.st.RetentionClock(ctx)
	if err != nil {
		return err
	}
	now := r.w.now()
	pass := r.owed || cur.LastPass.IsZero() || now.Sub(cur.LastPass) >= retentionPassInterval
	if pass {
		r.owed = true
	}
	rc, err := r.st.CheckRetentionClock(ctx, tenant.RetentionClockCheck{Now: now, MaxStep: step, Notice: notice, Pass: pass})
	if err != nil {
		return err
	}
	if rc.JustPaused {
		r.log.Error("retention: the server's clock jumped forward past retention.max-clock-step; retention and the purges started by "+
			"time are paused until an admin confirms the time", "step", rc.Step.String(), "paused_at", rc.PausedAt)
	}
	if rc.Paused {
		return nil
	}
	es := r.w.engines.Load()
	if es == nil {
		return fault.New(fault.Unavailable, "the corrections engines are not ready")
	}
	var errs []error
	if pass {
		err := r.pass(ctx, now)
		if err == nil {
			r.owed = false
		}
		errs = append(errs, err)
	}
	// Every check, not only the pass: a person's disposal is worked within
	// the hour it was queued.
	errs = append(errs, r.workDisposals(ctx, es))
	return errors.Join(errs...)
}

// pass queues a dispose job for each delete rule with sessions due: one per
// rule at a time, at most retention.max-per-pass sessions.
func (r *tenantRetention) pass(ctx context.Context, now time.Time) error {
	max, err := r.w.values.OnTenant(r.st).MaxPerPass(ctx)
	if err != nil {
		return err
	}
	var errs []error
	page := master.Page{Limit: master.MaxPageLimit}
	for {
		rules, next, err := r.st.ListRetentionRules(ctx, now, page)
		if err != nil {
			return err
		}
		for _, rule := range rules {
			if rule.Action != tenant.RetentionDelete {
				continue
			}
			at := now.UTC().Truncate(time.Microsecond)
			id := server.NewID()
			_, _, made, err := r.st.QueueDispose(ctx, tenant.DisposeQueue{ID: id, Rule: rule.Name, Scheduled: true, Max: max, Now: at},
				serverAudit(r.name, disposeMethod, rule.Name, at), nil)
			switch {
			case fault.KindOf(err) == fault.Precondition:
				// Nothing due, a dispose job of the rule not ended, or
				// retention paused: nothing to queue now.
			case err != nil:
				errs = append(errs, err)
			case made:
				r.log.Info("retention: the daily pass queued a disposal", "rule", rule.Name, "job", id)
			}
		}
		if next == "" {
			break
		}
		page.Cursor = next
	}
	return errors.Join(errs...)
}

// workDisposals works every dispose job not ended: each session it lists
// whose outcome is not recorded yet is deleted through the engine, or
// skipped for the store's reason.
func (r *tenantRetention) workDisposals(ctx context.Context, es *correct.Engines) error {
	var errs []error
	page := master.Page{Limit: master.MaxPageLimit}
	for {
		jobs, next, err := r.st.ListJobs(ctx, tenant.JobFilter{InProgress: true}, page)
		if err != nil {
			return err
		}
		for _, j := range jobs {
			if j.Kind == tenant.JobDispose {
				errs = append(errs, r.workDisposal(ctx, es, j.ID))
			}
		}
		if next == "" {
			break
		}
		page.Cursor = next
	}
	return errors.Join(errs...)
}

// workDisposal works one dispose job.
func (r *tenantRetention) workDisposal(ctx context.Context, es *correct.Engines, id string) error {
	dj, err := r.st.DisposeJob(ctx, id)
	if err != nil {
		return err
	}
	var errs []error
	for _, s := range dj.Sessions {
		if s.Outcome != tenant.DisposePending {
			continue
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		outcome, correction, err := r.dispose(ctx, es, dj, s)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if err := r.st.RecordDisposed(ctx, id, s.StudyUID, outcome, correction); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// dispose deletes one session of a dispose job through the corrections
// engine, as a person's delete is made, with the rule and due time it was
// listed under; the store checks again, in the delete's transaction, that
// it is due under the rule and not held. A session held, not due, or gone
// is a skip; any other failure is answered, and the session is tried again
// at the next pass.
func (r *tenantRetention) dispose(ctx context.Context, es *correct.Engines, dj tenant.DisposeJob, s tenant.DisposeSession) (tenant.DisposeOutcome, string, error) {
	base := r.base
	if base == nil {
		base = r.st
	}
	e, release, err := es.Use(ctx, r.name, base)
	if err != nil {
		return "", "", err
	}
	defer release()
	target := tenant.CorrectionTarget{Subject: s.SubjectLabel, Session: s.SessionLabel}
	p, err := e.Preview(ctx, target)
	switch {
	case fault.KindOf(err) == fault.NotFound:
		return tenant.DisposeSkippedDeleted, "", nil
	case err != nil:
		return "", "", err
	}
	reason := dj.Reason
	if reason == "" {
		reason = "retention rule " + dj.Rule + ", due " + s.Due.UTC().Format(time.DateOnly)
	}
	now := r.w.now()
	c, err := e.Delete(ctx, correct.DeleteRequest{Target: target, Reason: reason, Confirm: p.Confirm, KeyID: dj.Job.CreatedByKeyID,
		Retention: &tenant.RetentionOrigin{Rule: dj.Rule, Due: s.Due, Scheduled: dj.Scheduled},
		Audit:     serverAudit(r.name, disposeMethod, s.StudyUID, now)})
	switch {
	case errors.Is(err, tenant.ErrHeld):
		return tenant.DisposeSkippedHeld, "", nil
	case errors.Is(err, tenant.ErrNotDue):
		return tenant.DisposeSkippedNotDue, "", nil
	case fault.KindOf(err) == fault.NotFound:
		return tenant.DisposeSkippedDeleted, "", nil
	case err != nil:
		return "", "", err
	}
	return tenant.DisposeDeleted, c.ID, nil
}
