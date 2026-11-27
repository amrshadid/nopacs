package tenanttest

import (
	"errors"
	"strings"
	"time"

	"github.com/amrshadid/nopacs/internal/core/record"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// The tests of batch deletes.

// batchTargets are the three sessions batchSetup archives, in order.
var batchTargets = []tenant.CorrectionTarget{{Subject: "sub-01", Session: "ses-1"}, {Subject: "sub-01", Session: "ses-2"},
	{Subject: "sub-01", Session: "ses-3"}}

// batchSetup archives three sessions of one scan each: ses-1 and ses-2
// through recv, ses-3 through recv2, which a rule keeps for ten years; it
// places a hold on ses-2 and attaches the replica "copy" to "store".
func (e *tenv) batchSetup() {
	e.Helper()
	e.addReceiver("recv", "store", 11112)
	e.addReceiver("recv2", "store", 11113)
	e.archiveScans("recv", 1, []int{100}, 100, at(1), "ses-1", nil)
	e.archiveScans("recv", 2, []int{200}, 200, at(2), "ses-2", nil)
	e.archiveScans("recv2", 3, []int{300}, 300, at(3), "ses-3", nil)
	e.addRule(tenant.RetentionRule{Name: "k", Selector: tenant.RetentionSelector{Receiver: "recv2"}, KeepAtLeast: tenant.RetentionPeriod{Years: 10},
		Action: tenant.RetentionReview}, change(5))
	e.place(tenant.HoldTarget{Subject: "sub-01", Session: "ses-2"}, "litigation", 10)
	e.addStorage("copy")
	if _, _, _, err := e.p.AttachReplica(e.ctx, "copy", "store", at(11), e.audit(), nil); err != nil {
		e.Fatalf("AttachReplica: %v", err)
	}
}

// queueBatch queues a batch of targets with reason and token at now, its
// bound the contract's.
func (e *tenv) queueBatch(id string, targets []tenant.CorrectionTarget, reason, confirm string, now time.Time) (tenant.Job, bool, error) {
	return e.p.QueueDeleteBatch(e.ctx, tenant.DeleteBatchQueue{ID: id, Targets: targets, Reason: reason, Confirm: confirm, KeyID: "k-admin",
		Max: tenant.MaxDeleteBatchTargets, Now: now}, e.audit(), nil)
}

// batchTargetsOf returns every target of the batch job, in order.
func (e *tenv) batchTargetsOf(id string) []tenant.DeleteBatchTarget {
	e.Helper()
	ts, err := e.p.DeleteBatchTargets(e.ctx, id, 0, tenant.MaxBatchTargetPage)
	e.ok("DeleteBatchTargets", err)
	return ts
}

// prepareBatchDelete previews target at second s and prepares its delete
// for the batch job's target at ord.
func (e *tenv) prepareBatchDelete(job string, ord int, target tenant.CorrectionTarget, s int) (tenant.Correction, error) {
	e.Helper()
	p, err := e.p.PreviewDelete(e.ctx, target, at(s))
	e.ok("PreviewDelete", err)
	c, _, err := e.p.PrepareCorrection(e.ctx, tenant.CorrectionPrepare{ID: e.newID(s), Kind: record.KindDelete, Target: target,
		Reason: "consent withdrawn", Confirm: p.Confirm, KeyID: "k-admin", Batch: &tenant.BatchOrigin{JobID: job, Ord: ord}, Now: at(s)},
		e.audit(), nil)
	return c, err
}

// testBatchPreview guards a batch delete's preview: every target in order
// with what its own delete preview answers (a hold, a rule's keep-at-least
// and its time, the replicas the purge reaches), the totals and the held
// and kept counts; a target the tenant does not hold is refused naming its
// place; a target named twice (case folded) or inside another is refused.
func testBatchPreview(e *tenv) {
	e.batchSetup()
	p, err := e.p.PreviewDeleteBatch(e.ctx, batchTargets, "consent withdrawn", at(100))
	e.ok("PreviewDeleteBatch", err)
	if len(p.Items) != 3 || p.Held != 1 || p.Kept != 1 || p.Counts.Sessions != 3 || p.Counts.Instances != 3 || p.Reason != "consent withdrawn" ||
		!strings.HasPrefix(p.Confirm, "b1.") {
		e.Fatalf("the preview is %+v", p)
	}
	for i, it := range p.Items {
		if it.Target != batchTargets[i] || it.Fingerprint == "" || len(it.Replicas) != 1 || it.Replicas[0].Replica != "copy" ||
			it.Replicas[0].Primary != "store" {
			e.Fatalf("item %d is %+v", i+1, it)
		}
	}
	if len(p.Items[0].Holds) != 0 || p.Items[0].SessionsUnderMinimum != 0 || len(p.Items[1].Holds) != 1 ||
		p.Items[2].SessionsUnderMinimum != 1 || p.Items[2].KeepUntil.IsZero() {
		e.Fatalf("the items' holds and keeps are %+v", p.Items)
	}
	_, err = e.p.PreviewDeleteBatch(e.ctx, []tenant.CorrectionTarget{batchTargets[0], {Subject: "sub-01", Session: "ses-9"}}, "consent withdrawn",
		at(101))
	if fault.KindOf(err) != fault.NotFound || !strings.Contains(fault.Message(err), "target 2") {
		e.Fatalf("a batch naming a session not held answers %v, want not found naming target 2", err)
	}
	_, err = e.p.PreviewDeleteBatch(e.ctx, []tenant.CorrectionTarget{batchTargets[0], {Subject: "SUB-01", Session: "SES-1"}}, "x", at(102))
	e.wantKind("a batch naming one session twice", err, fault.Invalid)
	_, err = e.p.PreviewDeleteBatch(e.ctx, []tenant.CorrectionTarget{batchTargets[0], {Subject: "sub-01"}}, "x", at(103))
	e.wantKind("a batch naming a session inside a subject it names", err, fault.Invalid)
	_, err = e.p.PreviewDeleteBatch(e.ctx, []tenant.CorrectionTarget{{Subject: "sub-01", Session: "ses-1", Scan: "s", Instance: uidOf(3, 1)}}, "x",
		at(104))
	e.wantKind("a batch naming one instance", err, fault.Invalid)
}

// testBatchQueue guards a batch delete job's making: the token makes a job
// of every target, queued, with each target's fingerprint; the same token
// answers that job; a token sent with other targets or another reason, an
// old token, a token of before a change of a target, a second batch while
// one has not ended, and a list over the bound are refused.
func testBatchQueue(e *tenv) {
	e.batchSetup()
	reason := "consent withdrawn"
	p, err := e.p.PreviewDeleteBatch(e.ctx, batchTargets, reason, at(100))
	e.ok("PreviewDeleteBatch", err)
	_, _, err = e.queueBatch(e.id("j"), batchTargets, "another reason", p.Confirm, at(101))
	e.wantKind("a token sent with another reason", err, fault.Precondition)
	_, _, err = e.queueBatch(e.id("j"), batchTargets[:2], reason, p.Confirm, at(101))
	e.wantKind("a token sent with fewer targets", err, fault.Precondition)
	_, _, err = e.queueBatch(e.id("j"), []tenant.CorrectionTarget{batchTargets[1], batchTargets[0], batchTargets[2]}, reason, p.Confirm, at(101))
	e.wantKind("a token sent with the targets in another order", err, fault.Precondition)
	_, _, err = e.queueBatch(e.id("j"), batchTargets, reason, p.Confirm, at(100).Add(2*time.Hour))
	e.wantKind("a token more than an hour old", err, fault.Precondition)
	_, _, err = e.p.QueueDeleteBatch(e.ctx, tenant.DeleteBatchQueue{ID: e.id("j"), Targets: batchTargets, Reason: reason, Confirm: p.Confirm, Max: 2,
		Now: at(101)}, e.audit(), nil)
	e.wantKind("a batch over its bound", err, fault.Invalid)
	j, made, err := e.queueBatch(e.id("j"), batchTargets, reason, p.Confirm, at(102))
	e.ok("QueueDeleteBatch", err)
	if !made || j.Kind != tenant.JobDeleteBatch || j.State != tenant.JobQueued || j.Counts.Scans != 3 || j.Counts.Instances != 3 ||
		!j.Counts.Listed || j.CreatedByKeyID != "k-admin" {
		e.Fatalf("the batch job is %+v (made %v)", j, made)
	}
	bj, err := e.p.DeleteBatchJob(e.ctx, j.ID)
	e.ok("DeleteBatchJob", err)
	if bj.Reason != reason || bj.Paused || bj.Targets != 3 {
		e.Fatalf("the batch job's own part is %+v", bj)
	}
	for i, t := range e.batchTargetsOf(j.ID) {
		if t.Ord != i+1 || t.Target != batchTargets[i] || t.Fingerprint != p.Items[i].Fingerprint || t.Outcome != tenant.BatchPending ||
			t.Correction != "" || t.Instances != 1 {
			e.Fatalf("target %d is %+v", i+1, t)
		}
	}
	again, made, err := e.queueBatch(e.id("j"), batchTargets, reason, p.Confirm, at(103))
	e.ok("QueueDeleteBatch with the token used", err)
	if made || again.ID != j.ID {
		e.Fatalf("the token used again answers %s (made %v), want %s", again.ID, made, j.ID)
	}
	_, _, err = e.queueBatch(e.id("j"), batchTargets, "another reason", p.Confirm, at(103))
	e.wantKind("the token used, sent with another reason", err, fault.Precondition)
	p2, err := e.p.PreviewDeleteBatch(e.ctx, batchTargets[:1], reason, at(104))
	e.ok("PreviewDeleteBatch", err)
	_, _, err = e.queueBatch(e.id("j"), batchTargets[:1], reason, p2.Confirm, at(105))
	if fault.KindOf(err) != fault.Precondition || !strings.Contains(fault.Message(err), "one runs at a time") {
		e.Fatalf("a second batch while one has not ended answers %v", err)
	}
	if _, err := e.p.CancelJob(e.ctx, j.ID, at(106), e.audit(), nil); err != nil {
		e.Fatalf("CancelJob: %v", err)
	}
	// ses-1 changes between the preview and the queue: an instance arrives.
	p3, err := e.p.PreviewDeleteBatch(e.ctx, batchTargets[:1], reason, at(107))
	e.ok("PreviewDeleteBatch", err)
	e.archiveScans("recv", 1, []int{101}, 110, at(108), "ses-1", nil)
	_, _, err = e.queueBatch(e.id("j"), batchTargets[:1], reason, p3.Confirm, at(109))
	e.wantKind("a token of before a target changed", err, fault.Precondition)
	p4, err := e.p.PreviewDeleteBatch(e.ctx, batchTargets[:1], reason, at(110))
	e.ok("PreviewDeleteBatch", err)
	if _, made, err := e.queueBatch(e.id("j"), batchTargets[:1], reason, p4.Confirm, at(111)); err != nil || !made {
		e.Fatalf("a batch once the first was canceled: made %v, %v", made, err)
	}
}

// testBatchDeletes guards a batch's deletes against its job: a delete made
// for a target is recorded on it in the prepare's transaction, so a second
// is refused while the first was not abandoned; an outcome deleted names
// that delete; outcomes move the job's counts and start it; a pause
// refuses the next delete until a resume; the job ends partial once every
// target has its outcome, and refuses an end before; a cancel refuses the
// next delete and outcome; a stop ends the job failed with its reason, once.
func testBatchDeletes(e *tenv) {
	e.batchSetup()
	p, err := e.p.PreviewDeleteBatch(e.ctx, batchTargets, "consent withdrawn", at(100))
	e.ok("PreviewDeleteBatch", err)
	j, _, err := e.queueBatch(e.id("j"), batchTargets, "consent withdrawn", p.Confirm, at(101))
	e.ok("QueueDeleteBatch", err)
	_, err = e.prepareBatchDelete(j.ID, 2, batchTargets[0], 110)
	e.wantKind("a batch delete naming another target than the job's at that place", err, fault.Precondition)
	c, err := e.prepareBatchDelete(j.ID, 1, batchTargets[0], 111)
	e.ok("PrepareCorrection for the batch", err)
	if got := e.batchTargetsOf(j.ID)[0]; got.Correction != c.ID || got.Outcome != tenant.BatchPending {
		e.Fatalf("after its delete was prepared target 1 is %+v, want it naming %s", got, c.ID)
	}
	if _, err := e.p.AbandonCorrection(e.ctx, c.ID, "storage does not answer", at(112)); err != nil {
		e.Fatalf("AbandonCorrection: %v", err)
	}
	c, err = e.prepareBatchDelete(j.ID, 1, batchTargets[0], 113)
	e.ok("PrepareCorrection for the batch after the first was abandoned", err)
	_, err = e.prepareBatchDelete(j.ID, 1, batchTargets[0], 114)
	e.wantKind("a second delete of a target whose delete is prepared", err, fault.Precondition)
	e.commit(c.ID, 115, nil)
	_, err = e.p.RecordBatchTarget(e.ctx, tenant.BatchRecord{JobID: j.ID, Ord: 1, Outcome: tenant.BatchDeleted, Correction: e.newID(116), Now: at(116)})
	e.wantKind("an outcome naming another delete", err, fault.Precondition)
	run, err := e.p.RecordBatchTarget(e.ctx, tenant.BatchRecord{JobID: j.ID, Ord: 1, Outcome: tenant.BatchDeleted, Correction: c.ID, Now: at(117)})
	e.ok("RecordBatchTarget", err)
	if run.State != tenant.JobRunning || run.Counts.ScansDone != 1 || run.Counts.InstancesSent != 1 || run.Started.IsZero() {
		e.Fatalf("after its first outcome the job is %+v", run)
	}
	if _, err := e.p.RecordBatchTarget(e.ctx, tenant.BatchRecord{JobID: j.ID, Ord: 2, Outcome: tenant.BatchSkippedHeld, Now: at(118)}); err != nil {
		e.Fatalf("RecordBatchTarget of a skip: %v", err)
	}
	_, err = e.p.EndDeleteBatchJob(e.ctx, j.ID, at(119))
	e.wantKind("the end of a batch with a target left", err, fault.Precondition)
	paused, err := e.p.PauseDeleteBatch(e.ctx, j.ID, true, at(120), e.audit(), nil)
	e.ok("PauseDeleteBatch", err)
	if !paused.Paused || paused.Job.State != tenant.JobRunning {
		e.Fatalf("the paused job is %+v", paused)
	}
	// ses-3 is kept by a rule, so its delete is refused by retention once
	// the pause no longer refuses it first.
	_, err = e.prepareBatchDelete(j.ID, 3, batchTargets[2], 121)
	if !errors.Is(err, tenant.ErrBatchStopped) || fault.KindOf(err) != fault.Precondition {
		e.Fatalf("a delete of a paused batch answers %v, want the batch stopped", err)
	}
	if _, err := e.p.PauseDeleteBatch(e.ctx, j.ID, false, at(122), e.audit(), nil); err != nil {
		e.Fatalf("PauseDeleteBatch to resume: %v", err)
	}
	_, err = e.prepareBatchDelete(j.ID, 3, batchTargets[2], 123)
	if !errors.Is(err, tenant.ErrRetentionMinimum) {
		e.Fatalf("a delete of a kept target of a resumed batch answers %v, want the rule's keep", err)
	}
	if _, err := e.p.RecordBatchTarget(e.ctx, tenant.BatchRecord{JobID: j.ID, Ord: 3, Outcome: tenant.BatchSkippedKept, Now: at(124)}); err != nil {
		e.Fatalf("RecordBatchTarget of a kept target: %v", err)
	}
	ended, err := e.p.EndDeleteBatchJob(e.ctx, j.ID, at(125))
	e.ok("EndDeleteBatchJob", err)
	if ended.State != tenant.JobPartial || ended.Counts.ScansDone != 3 || ended.Counts.InstancesSent != 1 || ended.Counts.InstancesRefused != 2 {
		e.Fatalf("the ended batch job is %+v, want partial with one deleted and two skipped", ended)
	}
	tallies, err := e.p.JobTallies(e.ctx, j.ID)
	e.ok("JobTallies", err)
	want := map[string]int64{"deleted": 1, "skipped|held": 1, "skipped|kept": 1}
	if len(tallies) != len(want) {
		e.Fatalf("the job's tallies are %+v, want %v", tallies, want)
	}
	for _, t := range tallies {
		if want[t.Key] != t.Count {
			e.Fatalf("the job's tallies are %+v, want %v", tallies, want)
		}
	}
	if got := steps(e.jobEvents(""), j.ID); len(got) != 3 || got[0] != tenant.JobQueued || got[1] != tenant.JobRunning || got[2] != tenant.JobPartial {
		e.Fatalf("the batch job reached %v, want queued, running, partial", got)
	}
	_, err = e.p.PauseDeleteBatch(e.ctx, j.ID, true, at(126), e.audit(), nil)
	e.wantKind("a pause of an ended job", err, fault.Precondition)

	p2, err := e.p.PreviewDeleteBatch(e.ctx, batchTargets[2:], "consent withdrawn", at(130))
	e.ok("PreviewDeleteBatch", err)
	j2, _, err := e.queueBatch(e.id("j"), batchTargets[2:], "consent withdrawn", p2.Confirm, at(131))
	e.ok("QueueDeleteBatch", err)
	if _, err := e.p.CancelJob(e.ctx, j2.ID, at(132), e.audit(), nil); err != nil {
		e.Fatalf("CancelJob: %v", err)
	}
	_, err = e.prepareBatchDelete(j2.ID, 1, batchTargets[2], 133)
	if !errors.Is(err, tenant.ErrBatchStopped) {
		e.Fatalf("a delete of a canceled batch answers %v, want the batch stopped", err)
	}
	_, err = e.p.RecordBatchTarget(e.ctx, tenant.BatchRecord{JobID: j2.ID, Ord: 1, Outcome: tenant.BatchSkippedKept, Now: at(134)})
	if !errors.Is(err, tenant.ErrBatchStopped) {
		e.Fatalf("an outcome of a canceled batch answers %v, want the batch stopped", err)
	}
	_, err = e.prepareBatchDelete("j-none", 1, batchTargets[2], 135)
	if !errors.Is(err, tenant.ErrBatchStopped) {
		e.Fatalf("a delete for a batch the tenant does not have answers %v, want the batch stopped", err)
	}

	p3, err := e.p.PreviewDeleteBatch(e.ctx, batchTargets[2:], "consent withdrawn", at(140))
	e.ok("PreviewDeleteBatch", err)
	j3, _, err := e.queueBatch(e.id("j"), batchTargets[2:], "consent withdrawn", p3.Confirm, at(141))
	e.ok("QueueDeleteBatch", err)
	why := "stopped: the key that made the batch may no longer delete (key revoked)"
	stopped, err := e.p.StopDeleteBatchJob(e.ctx, j3.ID, why, at(142))
	e.ok("StopDeleteBatchJob", err)
	again, err := e.p.StopDeleteBatchJob(e.ctx, j3.ID, "another reason", at(143))
	e.ok("StopDeleteBatchJob of a job stopped", err)
	if stopped.State != tenant.JobFailed || stopped.Reason != why || again.Reason != why || !again.Ended.Equal(stopped.Ended) {
		e.Fatalf("the stopped batch is %+v, and stopped again %+v", stopped, again)
	}
	_, err = e.prepareBatchDelete(j3.ID, 1, batchTargets[2], 144)
	if !errors.Is(err, tenant.ErrBatchStopped) {
		e.Fatalf("a delete of a stopped batch answers %v, want the batch stopped", err)
	}
}
