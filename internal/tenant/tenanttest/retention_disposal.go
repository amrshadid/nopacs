package tenanttest

import (
	"errors"
	"time"

	"github.com/amrshadid/nopacs/internal/core/record"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// The tests of disposals.

// disposalSetup archives three sessions of one scan each through recv at
// second 0 and adds the delete rule "d" that disposes after a day; it
// returns the time all three are due.
func (e *tenv) disposalSetup() time.Time {
	e.Helper()
	e.addReceiver("recv", "store", 11112)
	for i := 1; i <= 3; i++ {
		e.archiveScans("recv", i, []int{i * 100}, i*100, at(i), "ses-"+string(rune('0'+i)), nil)
	}
	d := rule("d", 0, 1)
	d.Action = tenant.RetentionDelete
	e.addRule(d, change(5))
	return e.sessionTimes(3).Due.Add(time.Hour)
}

// queue queues a person's disposal of rule d with the token.
func (e *tenv) queue(id, confirm string, now time.Time) (tenant.Job, tenant.DisposalPreview, bool, error) {
	return e.p.QueueDispose(e.ctx, tenant.DisposeQueue{ID: id, Rule: "d", Reason: "trial closed", Confirm: confirm, KeyID: "k-admin",
		Now: now}, e.audit(), nil)
}

// testDisposalPreview guards a disposal's preview and its token: due
// sessions oldest first, a held one counted and never listed, a bound
// leaving the rest counted; the token queues a dispose job listing exactly
// those sessions, and a token used again answers that job; a token a
// preview made now no longer matches, an old one, a paused retention and a
// rule not held are refused; the daily pass takes none while the rule's
// job has not ended.
func testDisposalPreview(e *tenv) {
	now := e.disposalSetup()
	h := e.place(tenant.HoldTarget{Subject: "sub-01", Session: "ses-2"}, "audit", 10)
	p, err := e.p.PreviewDisposal(e.ctx, "d", 1, now)
	e.ok("PreviewDisposal", err)
	if len(p.Sessions) != 1 || p.Sessions[0].StudyUID != uidOf(1, 1) || p.NotListed != 1 || p.Held != 1 || p.Counts.Sessions != 1 ||
		p.Counts.Instances != 1 || p.Confirm == "" {
		e.Fatalf("a preview of one is %+v", p)
	}
	_, err = e.p.PreviewDisposal(e.ctx, "nope", 1, now)
	e.wantKind("a preview of a rule not held", err, fault.NotFound)
	_, err = e.p.PreviewDisposal(e.ctx, "d", 1001, now)
	e.wantKind("a preview of 1001", err, fault.Invalid)
	_, _, _, err = e.queue(e.id("j"), p.Confirm, now.Add(2*time.Hour))
	e.wantKind("a token more than an hour old", err, fault.Precondition)
	j, listed, made, err := e.queue(e.id("j"), p.Confirm, now.Add(time.Minute))
	e.ok("QueueDispose", err)
	if !made || j.Kind != tenant.JobDispose || j.State != tenant.JobQueued || j.Counts.Scans != 1 || len(listed.Sessions) != 1 {
		e.Fatalf("a queued disposal is %+v (made %v) listing %+v", j, made, listed.Sessions)
	}
	dj, err := e.p.DisposeJob(e.ctx, j.ID)
	e.ok("DisposeJob", err)
	if dj.Rule != "d" || dj.Reason != "trial closed" || dj.Scheduled || len(dj.Sessions) != 1 || dj.Sessions[0].StudyUID != uidOf(1, 1) ||
		dj.Sessions[0].Outcome != tenant.DisposePending {
		e.Fatalf("the dispose job is %+v", dj)
	}
	again, _, made, err := e.queue(e.id("j"), p.Confirm, now.Add(2*time.Minute))
	e.ok("QueueDispose with a token used", err)
	if made || again.ID != j.ID {
		e.Fatalf("a token used again answers %s (made %v), want %s", again.ID, made, j.ID)
	}
	// The hold released: a preview made now lists another, so a token of
	// before does not match.
	p2, err := e.p.PreviewDisposal(e.ctx, "d", 5, now)
	e.ok("PreviewDisposal", err)
	if _, err := e.p.ReleaseHold(e.ctx, tenant.HoldRelease{ID: h.ID, Reason: "done", Now: at(20)}, e.audit(), nil); err != nil {
		e.Fatalf("ReleaseHold: %v", err)
	}
	_, _, _, err = e.queue(e.id("j"), p2.Confirm, now.Add(time.Minute))
	e.wantKind("a token a preview made now no longer matches", err, fault.Precondition)
	_, _, _, err = e.p.QueueDispose(e.ctx, tenant.DisposeQueue{ID: e.id("j"), Rule: "d", Scheduled: true, Max: 10, Now: now}, e.audit(), nil)
	e.wantKind("the daily pass while the rule's job has not ended", err, fault.Precondition)
	if _, err := e.p.CheckRetentionClock(e.ctx, tenant.RetentionClockCheck{Now: now, MaxStep: tenant.DefaultMaxClockStep, Notice: notice}); err != nil {
		e.Fatalf("CheckRetentionClock: %v", err)
	}
	if _, err := e.p.CheckRetentionClock(e.ctx, tenant.RetentionClockCheck{Now: now.Add(10 * day), MaxStep: tenant.DefaultMaxClockStep,
		Notice: notice}); err != nil {
		e.Fatalf("CheckRetentionClock: %v", err)
	}
	_, err = e.p.PreviewDisposal(e.ctx, "d", 5, now.Add(10*day))
	if !errors.Is(err, tenant.ErrRetentionPaused) {
		e.Fatalf("a preview while paused answers %v, want the pause's refusal", err)
	}
}

// testDisposalTokenKeepsItsTime guards that a person's token is matched at
// the time it was issued: a session that only became due between the
// preview and the queue does not refuse it (the job lists what the person
// saw), while a hold placed in between does.
func testDisposalTokenKeepsItsTime(e *tenv) {
	e.addReceiver("recv", "store", 11112)
	for i := 1; i <= 3; i++ {
		e.archiveScans("recv", i, []int{i * 100}, i*100, at(i*10), "ses-"+string(rune('0'+i)), nil)
	}
	// Longer than the notice, so each session is due its own time after
	// its start rather than all at the notice's end.
	d := rule("d", 0, 40)
	d.Action = tenant.RetentionDelete
	e.addRule(d, change(50))
	second, third := e.sessionTimes(2).Due, e.sessionTimes(3).Due
	if !third.After(second) {
		e.Fatalf("the third session is due at %v, not after the second's %v", third, second)
	}
	p, err := e.p.PreviewDisposal(e.ctx, "d", 5, second)
	e.ok("PreviewDisposal", err)
	if len(p.Sessions) != 2 {
		e.Fatalf("a preview at the second's due time lists %+v, want two", p.Sessions)
	}
	j, listed, made, err := e.queue(e.id("j"), p.Confirm, third.Add(time.Minute))
	e.ok("QueueDispose after another session became due", err)
	if !made || j.Counts.Scans != 2 || len(listed.Sessions) != 2 || listed.Sessions[1].StudyUID != uidOf(1, 2) {
		e.Fatalf("the job queued is %+v (made %v) listing %+v, want the two the person saw", j, made, listed.Sessions)
	}
	p2, err := e.p.PreviewDisposal(e.ctx, "d", 1, second)
	e.ok("PreviewDisposal", err)
	e.place(tenant.HoldTarget{Subject: "sub-01", Session: p2.Sessions[0].SessionLabel}, "audit", 20)
	_, _, _, err = e.queue(e.id("j"), p2.Confirm, third.Add(2*time.Minute))
	e.wantKind("a token after a hold was placed on what it lists", err, fault.Precondition)
}

// testDisposalDeletes guards that a disposal deletes through the ordinary
// delete: a delete with the rule's origin is prepared only for a session
// due under the rule and not held, and keeps its origin, rule and due
// time; undone, it exempts the session from the rule's daily pass (the
// pass's delete refused, the pass's job not listing it) while a person's
// preview lists it marked undone; a change of the rule ends the exemption.
func testDisposalDeletes(e *tenv) {
	now := e.disposalSetup()
	due := e.due("d", now, false)
	if len(due) != 3 {
		e.Fatalf("the due sessions are %+v, want three", due)
	}
	s := due[0]
	target := tenant.CorrectionTarget{Subject: s.SubjectLabel, Session: s.SessionLabel}
	prep := func(at time.Time, scheduled bool, ruleName string) (tenant.Correction, error) {
		e.Helper()
		p, err := e.p.PreviewDelete(e.ctx, target, at)
		e.ok("PreviewDelete", err)
		c, _, err := e.p.PrepareCorrection(e.ctx, tenant.CorrectionPrepare{ID: e.newID(int(at.Sub(T0).Seconds())), Kind: record.KindDelete,
			Target: target, Reason: "retention rule d, due " + s.Due.Format("2006-01-02"), Confirm: p.Confirm,
			Retention: &tenant.RetentionOrigin{Rule: ruleName, StudyUID: s.StudyUID, Due: s.Due, Scheduled: scheduled}, Now: at}, e.audit(), nil)
		return c, err
	}
	_, err := prep(s.Due.Add(-time.Hour), true, "d")
	if !errors.Is(err, tenant.ErrNotDue) {
		e.Fatalf("a retention delete before the due time answers %v, want not due", err)
	}
	_, err = prep(now, true, "other")
	if !errors.Is(err, tenant.ErrNotDue) {
		e.Fatalf("a retention delete under another rule answers %v, want not due", err)
	}
	c, err := prep(now, true, "d")
	e.ok("PrepareCorrection of a retention delete", err)
	if c.Origin != tenant.OriginRetention || c.RetentionRule != "d" || !c.RetentionDue.Equal(s.Due) || c.KeyID != "" {
		e.Fatalf("a retention delete is %+v", c)
	}
	e.commit(c.ID, int(now.Sub(T0).Seconds())+1, nil)
	if _, err := e.p.UndoCorrection(e.ctx, c.ID, "the trial was extended", now.Add(time.Minute), e.audit(), nil); err != nil {
		e.Fatalf("UndoCorrection: %v", err)
	}
	if got := e.sessionTimes(1); got.ExemptRule != "d" {
		e.Fatalf("an undone retention delete leaves %+v, want its session exempt from d", got)
	}
	_, err = prep(now.Add(2*time.Minute), true, "d")
	if !errors.Is(err, tenant.ErrNotDue) {
		e.Fatalf("the pass's delete of an exempt session answers %v, want not due", err)
	}
	listed := e.due("d", now.Add(3*time.Minute), false)
	if len(listed) != 3 || listed[0].StudyUID != s.StudyUID || !listed[0].Undone {
		e.Fatalf("after the undo the due sessions are %+v, want the first marked undone", listed)
	}
	_, pass, made, err := e.p.QueueDispose(e.ctx, tenant.DisposeQueue{ID: e.id("j"), Rule: "d", Scheduled: true, Max: 10,
		Now: now.Add(4 * time.Minute)}, e.audit(), nil)
	e.ok("QueueDispose of the pass", err)
	if !made || len(pass.Sessions) != 2 || pass.Sessions[0].StudyUID == s.StudyUID {
		e.Fatalf("the pass's job lists %+v, want the two not exempt", pass.Sessions)
	}
	if _, err := prep(now.Add(5*time.Minute), false, "d"); err != nil {
		e.Fatalf("a person's disposal of an exempt session: %v", err)
	}
	r := rule("d", 0, 2)
	r.Action = tenant.RetentionDelete
	if _, err := e.p.SetRetentionRule(e.ctx, r, changeAt(now.Add(6*time.Minute)), e.audit(), nil); err != nil {
		e.Fatalf("SetRetentionRule: %v", err)
	}
	if got := e.sessionTimes(1); got.ExemptRule != "" {
		e.Fatalf("after the rule changed the session is %+v, want no exemption", got)
	}
	e.wantKind("a disposed session's outcome without its delete", e.p.RecordDisposed(e.ctx, "j-none", s.StudyUID, tenant.DisposeDeleted, ""),
		fault.Invalid)
}
