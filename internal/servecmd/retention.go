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
	// holdsBack is set once the hold records of every storage of the tenant
	// were read after this start and each hold found active with no row
	// adopted (adoptHolds).
	holdsBack bool
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
	if !r.holdsBack {
		if err := r.adoptHolds(ctx); err != nil {
			return err
		}
		r.holdsBack = true
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

// adoptHolds reads the hold records of every storage of the tenant and
// adopts each hold found active with no row (holds.Writer.AdoptFound), as
// the hold-record worker does at the start, before the first disposal
// after a start: a database lost and made again, or restored from before a
// hold was placed, has no row of that hold until its record is adopted, and
// a disposal before then would delete what the hold keeps. A storage that
// cannot be read is the error, and no disposal runs until every storage
// was read; adopting a hold the worker adopted already changes nothing.
func (r *tenantRetention) adoptHolds(ctx context.Context) error {
	base := r.base
	if base == nil {
		base = r.st
	}
	hw, err := r.w.holdWriterOver(ctx, r.name, base, r.st)
	if err != nil {
		return err
	}
	a, err := hw.AdoptFound(ctx)
	for _, id := range a.Adopted {
		r.log.Warn("holds: a hold record was found active with no row before retention's first disposal; the hold holds again", "hold", id)
	}
	return err
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

// workDisposal works one dispose job. The job is read again before each
// session, and the work stops once it has ended: a cancel stops the job
// within one session, and the delete's prepare refuses, in its own
// transaction, a job that ended after this read (RetentionOrigin.JobID).
func (r *tenantRetention) workDisposal(ctx context.Context, es *correct.Engines, id string) error {
	dj, err := r.st.DisposeJob(ctx, id)
	if err != nil {
		return err
	}
	var errs []error
	for i := range dj.Sessions {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if dj.Sessions[i].Outcome != tenant.DisposePending {
			continue
		}
		if i > 0 {
			if dj, err = r.st.DisposeJob(ctx, id); err != nil {
				return errors.Join(append(errs, err)...)
			}
		}
		if dj.Job.State.Ended() {
			return nil
		}
		s := dj.Sessions[i]
		if s.Outcome != tenant.DisposePending {
			continue
		}
		outcome, correction, err := r.dispose(ctx, es, dj, s)
		if errors.Is(err, tenant.ErrDisposeJobEnded) {
			return nil
		}
		if err != nil {
			errs = append(errs, err)
			continue
		}
		err = r.st.RecordDisposed(ctx, id, s.StudyUID, outcome, correction)
		switch {
		case errors.Is(err, tenant.ErrDisposeJobEnded):
			return nil
		case err != nil:
			errs = append(errs, err)
		}
	}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	// Every session has its outcome: the job ends, so the rule's next
	// daily pass may queue another (QueueDispose refuses one while a job
	// of the rule has not ended).
	if _, err := r.st.EndDisposeJob(ctx, id, r.w.now()); err != nil {
		return err
	}
	return nil
}

// dispose deletes one session of a dispose job through the corrections
// engine, as a person's delete is made, with the rule and due time it was
// listed under; the store checks again, in the delete's transaction, that
// it is due under the rule, not held, and that the job has not ended. A
// dispose job is not tried again, so every refusal that would not pass by
// itself is a skip with its reason (skipOf); only a temporary failure is
// answered, and the session is tried again at the next pass.
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
	if err != nil {
		return r.skipOf(ctx, err)
	}
	reason := dj.Reason
	if reason == "" {
		reason = "retention rule " + dj.Rule + ", due " + s.Due.UTC().Format(time.DateOnly)
	}
	now := r.w.now()
	c, err := e.Delete(ctx, correct.DeleteRequest{Target: target, Reason: reason, Confirm: p.Confirm, KeyID: dj.Job.CreatedByKeyID,
		Retention: &tenant.RetentionOrigin{Rule: dj.Rule, StudyUID: s.StudyUID, Due: s.Due, Scheduled: dj.Scheduled, JobID: dj.Job.ID},
		Audit:     serverAudit(r.name, disposeMethod, s.StudyUID, now)})
	if err != nil {
		return r.skipOf(ctx, err)
	}
	return tenant.DisposeDeleted, c.ID, nil
}

// skipOf answers what a refusal of a session's delete makes of the session:
// the skip for its reason, or the error itself when it may pass by itself
// (a database, storage or engine not answering, the call's own end,
// retention paused for its clock, which keeps the job waiting by design)
// and when the job has ended (the caller stops). Every other refusal is a
// skip: a session left without an outcome would keep the job, and with it
// the rule's next job, from ever ending, for as long as, say, a hold keeps
// another correction of the session purging. A refusal of no known reason
// is logged with its message, which the skip's fixed reason does not keep.
func (r *tenantRetention) skipOf(ctx context.Context, err error) (tenant.DisposeOutcome, string, error) {
	switch {
	case ctx.Err() != nil, errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded),
		errors.Is(err, tenant.ErrDisposeJobEnded), errors.Is(err, tenant.ErrRetentionPaused):
		return "", "", err
	case errors.Is(err, tenant.ErrHeld):
		return tenant.DisposeSkippedHeld, "", nil
	case errors.Is(err, tenant.ErrNotDue):
		return tenant.DisposeSkippedNotDue, "", nil
	case errors.Is(err, tenant.ErrOverlap):
		return tenant.DisposeSkippedOverlap, "", nil
	}
	switch fault.KindOf(err) {
	case fault.NotFound:
		return tenant.DisposeSkippedDeleted, "", nil
	case fault.Unavailable, fault.Exhausted:
		return "", "", err
	}
	r.log.Warn("retention: a session's delete was refused; the dispose job skips it", "kind", fault.KindOf(err).String(),
		"reason", fault.Message(err))
	return tenant.DisposeSkippedRefused, "", nil
}
