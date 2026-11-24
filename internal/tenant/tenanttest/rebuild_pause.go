package tenanttest

import (
	"time"

	"github.com/amrshadid/nopacs/internal/core/record"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// wantReplacing fails unless RebuildReplacing answers want: the pause a
// replace holds the tenant's receivers in, read from the tenant's database
// so it holds across a restart.
func (e *renv) wantReplacing(when string, want bool) {
	e.Helper()
	got, err := e.p.RebuildReplacing(e.ctx)
	e.ok("RebuildReplacing "+when, err)
	if got != want {
		e.Errorf("RebuildReplacing %s answers %v, want %v", when, got, want)
	}
}

// testRebuildPauseUntilActivated guards the pause of a replace from its
// making to its activation: reported replacing while queued and while its
// shadow is built (also as read by another connection's look, which is what
// a restarted server makes), and no longer once the activation ended it; a
// compare never pauses anything.
func testRebuildPauseUntilActivated(e *renv) {
	e.rebuildSetup()
	e.wantReplacing("with no rebuild", false)
	e.createRebuild("rb-c", tenant.RebuildCompare)
	e.wantReplacing("with a compare queued", false)
	_, err := e.p.CancelJob(e.ctx, "rb-c", at(1), e.audit(), nil)
	e.ok("CancelJob of the compare", err)
	// The canceled compare's unit is still queued: its worker drops its
	// shadow and completes it before the replace's unit is claimed.
	w, err := e.p.ClaimWork(e.ctx, tenant.RebuildWorkKind, "rebuilder", at(2), time.Minute)
	e.ok("ClaimWork of the canceled compare", err)
	e.ok("DropShadow of the canceled compare", e.p.DropShadow(e.ctx, "rb-c"))
	e.ok("CompleteWork of the canceled compare", e.p.CompleteWork(e.ctx, w.Fence(), at(3), nil))

	e.createRebuild("rb-1", tenant.RebuildReplace)
	e.wantReplacing("with a replace queued", true)
	w = e.claimRebuild(at(10))
	e.wantReplacing("with a replace running", true)
	e.build(w, e.liveScans(), 1000)
	e.wantReplacing("with a replace built, not yet activated", true)
	j, err := e.p.ActivateCatalog(e.ctx, w.Fence(), at(20))
	e.ok("ActivateCatalog", err)
	if j.State != tenant.JobDone {
		e.Fatalf("the activated replace is %s, want done", j.State)
	}
	e.wantReplacing("once the replace is activated", false)
}

// testRebuildPauseEndsAnyWay guards that a replace which ends without its
// activation lifts the pause as well: ended failed by its worker
// (FailRebuild), and ended failed by its unit's attempts running out
// (FailWork's last), with no worker left to lift anything.
func testRebuildPauseEndsAnyWay(e *renv) {
	e.rebuildSetup()
	e.createRebuild("rb-1", tenant.RebuildReplace)
	w := e.claimRebuild(at(10))
	_, err := e.p.PrepareShadow(e.ctx, w.Fence())
	e.ok("PrepareShadow", err)
	j, err := e.p.FailRebuild(e.ctx, w.Fence(), "the scratch schema is not empty", at(11))
	e.ok("FailRebuild", err)
	if j.State != tenant.JobFailed {
		e.Fatalf("the replace failed is %s", j.State)
	}
	e.wantReplacing("once the replace failed", false)
	e.ok("DropShadow", e.p.DropShadow(e.ctx, "rb-1"))
	e.ok("CompleteWork", e.p.CompleteWork(e.ctx, w.Fence(), at(12), nil))

	e.createRebuild("rb-2", tenant.RebuildReplace)
	e.wantReplacing("with a second replace queued", true)
	now := at(20)
	w, err = e.p.ClaimWork(e.ctx, tenant.RebuildWorkKind, "rebuilder", now, time.Minute)
	e.ok("ClaimWork", err)
	for i := 0; ; i++ {
		got, err := e.p.FailWork(e.ctx, w.Fence(), "the tenant's receptions did not stop in time", now)
		e.ok("FailWork", err)
		if got.State == tenant.WorkFailed {
			break
		}
		if i > tenant.JobUnitAttempts {
			e.Fatalf("the unit is not held failed after %d attempts", i)
		}
		e.wantReplacing("while the replace's unit is tried again", true)
		now = now.Add(48 * time.Hour)
		w, err = e.p.ClaimWork(e.ctx, tenant.RebuildWorkKind, "rebuilder", now, time.Minute)
		e.ok("ClaimWork", err)
	}
	e.wantReplacing("once the replace's attempts ran out", false)
}

// wantRunning fails unless RebuildRunning answers want.
func (e *renv) wantRunning(when string, want bool) {
	e.Helper()
	got, err := e.p.RebuildRunning(e.ctx)
	e.ok("RebuildRunning "+when, err)
	if got != want {
		e.Errorf("RebuildRunning %s answers %v, want %v", when, got, want)
	}
}

// testRebuildHoldsPurges guards the purges while a rebuild of either mode
// is queued or running (RebuildRunning): the server's start of a purge
// whose grace has ended is refused as an admin's is, and a purge already
// going on can settle no page of its objects, record no object and cannot
// end, each a precondition that
// changes nothing; once the rebuild has ended both go on.
func testRebuildHoldsPurges(e *renv) {
	e.rebuildSetup()
	c := &cenv{senv: e.senv}
	due := c.prepareDelete(tenant.CorrectionTarget{Subject: "sub-01", Session: "ses-2"}, 100)
	c.commit(due.ID, 110, nil)
	going := c.prepareDelete(tenant.CorrectionTarget{Subject: "sub-01", Session: "ses-1"}, 120)
	c.commit(going.ID, 130, nil)
	after := at(130).Add(grace + time.Hour)
	_, err := e.p.StartPurge(e.ctx, going.ID, after, nil, nil)
	e.ok("StartPurge before the rebuild", err)
	e.wantRunning("with no rebuild", false)
	e.createRebuild("rb-1", tenant.RebuildCompare)
	e.wantRunning("with a compare queued", true)
	e.wantReplacing("with a compare queued", false)
	_, err = e.p.StartPurge(e.ctx, due.ID, after, nil, nil)
	e.wantKind("the server's StartPurge while a rebuild is queued", err, fault.Precondition)
	err = e.p.RecordPurgedObject(e.ctx, tenant.PurgedObject{ID: going.ID, Storage: "store", Key: "data/sub-01/x.dcm", Size: 1,
		SHA256: sum(1), Disposition: tenant.DispositionRemoved, Now: after})
	e.wantKind("RecordPurgedObject while a rebuild is queued", err, fault.Precondition)
	e.wantKind("SettlePurgeTargets while a rebuild is queued", e.p.SettlePurgeTargets(e.ctx, going.ID, nil), fault.Precondition)
	_, err = e.p.FinishPurge(e.ctx, tenant.PurgeFinish{ID: going.ID, Now: after})
	e.wantKind("FinishPurge while a rebuild is queued", err, fault.Precondition)
	c.wantState(due.ID, record.StateCommitted)
	if g := c.wantState(going.ID, record.StatePurging); g.Counts.ObjectsRemoved != 0 {
		e.Fatalf("a purge refused during a rebuild recorded %d objects", g.Counts.ObjectsRemoved)
	}
	_, err = e.p.CancelJob(e.ctx, "rb-1", at(140), e.audit(), nil)
	e.ok("CancelJob", err)
	e.wantRunning("once the compare is canceled", false)
	_, err = e.p.StartPurge(e.ctx, due.ID, after, nil, nil)
	e.ok("StartPurge once the rebuild ended", err)
	e.ok("SettlePurgeTargets once the rebuild ended", e.p.SettlePurgeTargets(e.ctx, going.ID, nil))
	e.ok("RecordPurgedObject once the rebuild ended", e.p.RecordPurgedObject(e.ctx, tenant.PurgedObject{ID: going.ID, Storage: "store",
		Key: "data/sub-01/x.dcm", Size: 1, SHA256: sum(1), Disposition: tenant.DispositionRemoved, Now: after}))
	f, err := e.p.FinishPurge(e.ctx, tenant.PurgeFinish{ID: going.ID, Now: after})
	e.ok("FinishPurge once the rebuild ended", err)
	if f.State != record.StatePurged {
		e.Fatalf("the purge ended is %s", f.State)
	}
}

// testRebuildHoldsCorrections guards the corrections while a rebuild is
// queued or running: a new one is refused, and one prepared before the
// rebuild was made is not published, both as a precondition that changes
// nothing (the catalog's totals as they were), since the shadow read from
// the storages meanwhile would miss the change; once the rebuild has ended
// the prepared one is published.
func testRebuildHoldsCorrections(e *renv) {
	e.rebuildSetup()
	c := &cenv{senv: e.senv}
	prepared := c.prepareDelete(tenant.CorrectionTarget{Subject: "sub-01", Session: "ses-2"}, 100)
	before := c.totals()
	e.createRebuild("rb-1", tenant.RebuildReplace)
	_, err := e.p.CommitCorrection(e.ctx, tenant.CorrectionCommit{ID: prepared.ID, Now: at(200), Grace: grace})
	e.wantKind("CommitCorrection while a replace is queued", err, fault.Precondition)
	w := e.claimRebuild(at(210))
	_, err = e.p.CommitCorrection(e.ctx, tenant.CorrectionCommit{ID: prepared.ID, Now: at(220), Grace: grace})
	e.wantKind("CommitCorrection while a replace is running", err, fault.Precondition)
	p, err := e.p.PreviewDelete(e.ctx, tenant.CorrectionTarget{Subject: "sub-01", Session: "ses-1"}, at(230))
	e.ok("PreviewDelete while a replace is running", err)
	_, _, err = e.p.PrepareCorrection(e.ctx, tenant.CorrectionPrepare{ID: c.newID(231), Kind: record.KindDelete,
		Target: tenant.CorrectionTarget{Subject: "sub-01", Session: "ses-1"}, Reason: "consent withdrawn", Confirm: p.Confirm, Now: at(231)},
		e.audit(), nil)
	e.wantKind("PrepareCorrection while a replace is running", err, fault.Precondition)
	if got := c.totals(); got != before {
		e.Fatalf("a correction refused during a rebuild changed the catalog: %+v, want %+v", got, before)
	}
	c.wantState(prepared.ID, record.StatePrepared)
	_, err = e.p.FailRebuild(e.ctx, w.Fence(), "the scratch schema is not empty", at(240))
	e.ok("FailRebuild", err)
	c.commit(prepared.ID, 250, nil)
}
