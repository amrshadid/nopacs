package tenanttest

import (
	"errors"
	"strings"
	"time"

	"github.com/amrshadid/nopacs/internal/core/record"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// testDisposalEnds guards the end of a dispose job: refused while a session
// it lists has no outcome; once every session is deleted or skipped it
// ends done, counting them, with its event, and answers the same when
// asked again; and the rule's daily pass, refused while the job had not
// ended, then queues the rule's next disposal.
func testDisposalEnds(e *tenv) {
	now := e.disposalSetup()
	p, err := e.p.PreviewDisposal(e.ctx, "d", 5, now)
	e.ok("PreviewDisposal", err)
	j, _, made, err := e.queue(e.id("j"), p.Confirm, now.Add(time.Minute))
	e.ok("QueueDispose", err)
	if !made || len(p.Sessions) != 3 {
		e.Fatalf("the queued disposal lists %+v (made %v), want three sessions", p.Sessions, made)
	}
	_, err = e.p.EndDisposeJob(e.ctx, j.ID, now.Add(2*time.Minute))
	e.wantKind("the end of a dispose job with no outcome recorded", err, fault.Precondition)
	outcomes := []tenant.DisposeOutcome{tenant.DisposeSkippedHeld, tenant.DisposeSkippedNotDue, tenant.DisposeSkippedUndone}
	for i, s := range p.Sessions {
		if i == len(p.Sessions)-1 {
			_, err = e.p.EndDisposeJob(e.ctx, j.ID, now.Add(3*time.Minute))
			e.wantKind("the end of a dispose job with one session left", err, fault.Precondition)
		}
		if err := e.p.RecordDisposed(e.ctx, j.ID, s.StudyUID, outcomes[i], ""); err != nil {
			e.Fatalf("RecordDisposed: %v", err)
		}
	}
	_, _, _, err = e.p.QueueDispose(e.ctx, tenant.DisposeQueue{ID: e.id("j"), Rule: "d", Scheduled: true, Max: 10, Now: now.Add(4 * time.Minute)},
		e.audit(), nil)
	e.wantKind("the daily pass before the rule's job ended", err, fault.Precondition)
	ended, err := e.p.EndDisposeJob(e.ctx, j.ID, now.Add(5*time.Minute))
	e.ok("EndDisposeJob", err)
	if ended.State != tenant.JobDone || !ended.State.Ended() || ended.Counts.ScansDone != 3 || !ended.Ended.Equal(now.Add(5*time.Minute)) {
		e.Fatalf("the ended dispose job is %+v, want done with its three sessions counted", ended)
	}
	again, err := e.p.EndDisposeJob(e.ctx, j.ID, now.Add(6*time.Minute))
	e.ok("EndDisposeJob of a job ended", err)
	if again.State != tenant.JobDone || !again.Ended.Equal(ended.Ended) {
		e.Fatalf("a dispose job ended again answers %+v, want it as it ended", again)
	}
	if got := steps(e.jobEvents(""), j.ID); len(got) != 2 || got[0] != tenant.JobQueued || got[1] != tenant.JobDone {
		e.Fatalf("the dispose job reached %v, want queued then done once", got)
	}
	next, _, made, err := e.p.QueueDispose(e.ctx, tenant.DisposeQueue{ID: e.id("j"), Rule: "d", Scheduled: true, Max: 10,
		Now: now.Add(7 * time.Minute)}, e.audit(), nil)
	e.ok("QueueDispose of the pass after the job ended", err)
	if !made || next.ID == j.ID {
		e.Fatalf("the pass after the job ended answers %+v (made %v), want a new job", next, made)
	}
	_, err = e.p.EndDisposeJob(e.ctx, "j-none", now)
	e.wantKind("the end of a dispose job the tenant does not have", err, fault.NotFound)
}

// testDisposalAfterRelabel guards that a disposal deletes only the session
// its job listed: once relabels give the listed labels to another session,
// a retention delete naming the listed session is refused (not due) and
// leaves the other session in the catalog, while one naming the session
// the labels now hold, due under the rule, is prepared.
func testDisposalAfterRelabel(e *tenv) {
	now := e.disposalSetup()
	due := e.due("d", now, false)
	if len(due) != 3 || due[0].SessionLabel != "ses-1" || due[1].SessionLabel != "ses-2" {
		e.Fatalf("the due sessions are %+v", due)
	}
	first, second := due[0], due[1]
	r1, _, err := e.prepareRelabel(tenant.CorrectionTarget{Subject: "sub-01", Session: "ses-1"}, "ses-9", 20)
	e.ok("PrepareCorrection of the first relabel", err)
	e.commit(r1.ID, 21, nil)
	e.purgeNow(r1.ID, 22)
	r2, _, err := e.prepareRelabel(tenant.CorrectionTarget{Subject: "sub-01", Session: "ses-2"}, "ses-1", 30)
	e.ok("PrepareCorrection of the second relabel", err)
	e.commit(r2.ID, 31, nil)
	e.purgeNow(r2.ID, 32)

	target := tenant.CorrectionTarget{Subject: "sub-01", Session: "ses-1"}
	prep := func(s tenant.DueSession, at time.Time) (tenant.Correction, error) {
		e.Helper()
		p, err := e.p.PreviewDelete(e.ctx, target, at)
		e.ok("PreviewDelete", err)
		c, _, err := e.p.PrepareCorrection(e.ctx, tenant.CorrectionPrepare{ID: e.newID(int(at.Sub(T0).Seconds())), Kind: record.KindDelete,
			Target: target, Reason: "retention rule d, due " + s.Due.Format("2006-01-02"), Confirm: p.Confirm,
			Retention: &tenant.RetentionOrigin{Rule: "d", StudyUID: s.StudyUID, Due: s.Due, Scheduled: true}, Now: at}, e.audit(), nil)
		return c, err
	}
	_, err = prep(first, now)
	if !errors.Is(err, tenant.ErrNotDue) || !strings.Contains(fault.Message(err), "no longer holds") {
		e.Fatalf("a retention delete of labels relabels gave to another session answers %v, want not due", err)
	}
	held, err := e.p.CatalogSession(e.ctx, "sub-01", "ses-1")
	e.ok("CatalogSession", err)
	if held.StudyUID != second.StudyUID {
		e.Fatalf("the labels now hold %s, want %s", held.StudyUID, second.StudyUID)
	}
	if _, err := prep(second, now.Add(time.Minute)); err != nil {
		e.Fatalf("a retention delete naming the session the labels hold: %v", err)
	}
}

// testDisposalStopsWhenEnded guards a dispose job's end against its
// runner: once the job is canceled, a delete made for it is refused in its
// prepare's transaction (ErrDisposeJobEnded, nothing prepared) though the
// runner read the job before the cancel, and no outcome is recorded for it
// any more; a delete for a job not ended is prepared, and a skip for
// another correction overlapping the session, or for a refusal of no known
// reason, is an outcome the job takes.
func testDisposalStopsWhenEnded(e *tenv) {
	now := e.disposalSetup()
	p, err := e.p.PreviewDisposal(e.ctx, "d", 5, now)
	e.ok("PreviewDisposal", err)
	j, _, _, err := e.queue(e.id("j"), p.Confirm, now.Add(time.Minute))
	e.ok("QueueDispose", err)
	s := p.Sessions[0]
	target := tenant.CorrectionTarget{Subject: s.SubjectLabel, Session: s.SessionLabel}
	prep := func(at time.Time, job string) (tenant.Correction, error) {
		e.Helper()
		pv, err := e.p.PreviewDelete(e.ctx, target, at)
		e.ok("PreviewDelete", err)
		c, _, err := e.p.PrepareCorrection(e.ctx, tenant.CorrectionPrepare{ID: e.newID(int(at.Sub(T0).Seconds())), Kind: record.KindDelete,
			Target: target, Reason: "trial closed", Confirm: pv.Confirm, KeyID: "k-admin",
			Retention: &tenant.RetentionOrigin{Rule: "d", StudyUID: s.StudyUID, Due: s.Due, JobID: job}, Now: at}, e.audit(), nil)
		return c, err
	}
	if err := e.p.RecordDisposed(e.ctx, j.ID, p.Sessions[1].StudyUID, tenant.DisposeSkippedOverlap, ""); err != nil {
		e.Fatalf("RecordDisposed of a session another correction holds: %v", err)
	}
	if err := e.p.RecordDisposed(e.ctx, j.ID, p.Sessions[2].StudyUID, tenant.DisposeSkippedRefused, ""); err != nil {
		e.Fatalf("RecordDisposed of a session whose delete was refused: %v", err)
	}
	if _, err := e.p.CancelJob(e.ctx, j.ID, now.Add(2*time.Minute), e.audit(), nil); err != nil {
		e.Fatalf("CancelJob: %v", err)
	}
	_, err = prep(now.Add(3*time.Minute), j.ID)
	if !errors.Is(err, tenant.ErrDisposeJobEnded) || fault.KindOf(err) != fault.Precondition {
		e.Fatalf("a delete for a canceled dispose job answers %v, want the job ended", err)
	}
	if held, err := e.p.CatalogSession(e.ctx, s.SubjectLabel, s.SessionLabel); err != nil || held.StudyUID != s.StudyUID {
		e.Fatalf("after the refused delete the session is %+v (%v), want it held", held, err)
	}
	err = e.p.RecordDisposed(e.ctx, j.ID, s.StudyUID, tenant.DisposeSkippedHeld, "")
	if !errors.Is(err, tenant.ErrDisposeJobEnded) {
		e.Fatalf("an outcome recorded for a canceled dispose job answers %v, want the job ended", err)
	}
	dj, err := e.p.DisposeJob(e.ctx, j.ID)
	e.ok("DisposeJob", err)
	if dj.Sessions[0].Outcome != tenant.DisposePending || dj.Sessions[1].Outcome != tenant.DisposeSkippedOverlap ||
		dj.Sessions[2].Outcome != tenant.DisposeSkippedRefused {
		e.Fatalf("the canceled job's sessions are %+v", dj.Sessions)
	}
	_, err = prep(now.Add(4*time.Minute), "j-none")
	if !errors.Is(err, tenant.ErrDisposeJobEnded) {
		e.Fatalf("a delete for a dispose job the tenant does not have answers %v, want the job ended", err)
	}
	p2, err := e.p.PreviewDisposal(e.ctx, "d", 5, now.Add(5*time.Minute))
	e.ok("PreviewDisposal", err)
	open, _, _, err := e.queue(e.id("j"), p2.Confirm, now.Add(6*time.Minute))
	e.ok("QueueDispose", err)
	if _, err := prep(now.Add(7*time.Minute), open.ID); err != nil {
		e.Fatalf("a delete for a dispose job not ended: %v", err)
	}
}
