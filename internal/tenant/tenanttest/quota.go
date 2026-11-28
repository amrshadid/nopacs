package tenanttest

import (
	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/core/record"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// quotaOf returns the store's counts of a tenant's use, failing the test
// when it keeps none.
func (e *penv) quotaOf() tenant.Quotas {
	e.Helper()
	q, ok := e.p.(tenant.Quotas)
	if !ok {
		e.Fatal("the driver's store keeps no counts of a tenant's use (tenant.Quotas)")
	}
	return q
}

// wantUse fails unless the tenant's use is want, read through the port.
func (e *penv) wantUse(when string, want tenant.QuotaUse) {
	e.Helper()
	got, err := e.quotaOf().QuotaUse(e.ctx)
	e.ok("QuotaUse", err)
	if got != want {
		e.Fatalf("%s: the use is %+v, want %+v", when, got, want)
	}
}

// sizes sums the sizes of files.
func sizes(files ...tenant.ReceivedFile) int64 {
	var n int64
	for _, f := range files {
		n += f.Size
	}
	return n
}

// testQuotaCounts guards the running totals a tenant's use against its
// quota is read from (tenant.Quotas), at each change that moves them:
// files recorded as received count as receiving; a commit moves them to
// held; a replace and an edit hold their new revisions while each revision
// they supersede counts as awaiting purge until its purge; a delete moves
// its instances from held to awaiting purge, an undo moves them back, and a
// purge takes them out; what a reception quarantines stops counting.
func testQuotaCounts(e *cenv) {
	e.wantUse("an empty tenant", tenant.QuotaUse{})
	a, b := e.setupTwo()
	all := append(append([]tenant.ReceivedFile{}, a.files...), b.files...)
	use := tenant.QuotaUse{HeldInstances: 8, HeldBytes: sizes(all...)}
	e.wantUse("after two sessions archived", use)

	// Recorded and not archived: counted as receiving from the record on.
	r := e.openReception("recv", 3, at(200))
	late := []tenant.ReceivedFile{file(r, 3, 30000), file(r, 3, 30001)}
	_, err := e.p.AddReceivedFiles(e.ctx, late, at(200), nil)
	e.ok("AddReceivedFiles", err)
	e.wantUse("with two files received", use.Plus(2, sizes(late...)))
	// One of them quarantined by its reception stops counting.
	q := tenant.QuarantinedFile{ID: e.id("q"), Reason: tenant.QuarantineDifferentFile, Receiver: "recv", ReceptionID: r.ID,
		Size: late[1].Size, SHA256: late[1].SHA256, KeptSHA256: sum(31001), SOPClassUID: late[1].SOPClassUID, SOPInstanceUID: late[1].SOPInstanceUID,
		StudyUID: late[1].StudyUID, SeriesUID: late[1].SeriesUID, TransferSyntaxUID: late[1].TransferSyntaxUID, Incoming: late[1].Incoming,
		ContentKept: true, Quarantined: at(210)}
	_, err = e.p.HoldFiles(e.ctx, []tenant.QuarantinedFile{q}, nil)
	e.ok("HoldFiles", err)
	e.wantUse("with one of them quarantined", use.Plus(1, late[0].Size))
	use = use.Plus(1, late[0].Size)

	// A replace: its new revision held, the old one awaiting its purge.
	in := a.files[0]
	rq := tenant.QuarantinedFile{ID: e.id("q"), Reason: tenant.QuarantineDifferentFile, Receiver: "recv", ReceptionID: a.r.ID,
		Size: 777, SHA256: sum(4242), KeptSHA256: sum(4243), SOPClassUID: in.SOPClassUID, SOPInstanceUID: in.SOPInstanceUID,
		StudyUID: in.StudyUID, SeriesUID: in.SeriesUID, TransferSyntaxUID: in.TransferSyntaxUID, Incoming: "quarantine/q1",
		ContentKept: true, Quarantined: at(400)}
	_, err = e.p.HoldFiles(e.ctx, []tenant.QuarantinedFile{rq}, nil)
	e.ok("HoldFiles", err)
	rep, _, err := e.p.PrepareCorrection(e.ctx, tenant.CorrectionPrepare{ID: e.newID(500), Kind: record.KindReplace, Quarantine: rq.ID,
		NewSHA256: sum(5000), NewSize: 780, RestoreLine: []byte(`{"line":1}`), Reason: "corrected re-send", Now: at(500)}, e.audit(), nil)
	e.ok("PrepareCorrection of a replace", err)
	e.wantUse("with a replace prepared", use)
	_, err = e.p.CommitCorrection(e.ctx, tenant.CorrectionCommit{ID: rep.ID, Now: at(600), Grace: grace,
		Manifests: map[string]tenant.SHA256{in.SeriesUID: sum(6000)}, Replaced: &tenant.InstanceProvenance{}})
	e.ok("CommitCorrection of a replace", err)
	use.HeldBytes += 780 - in.Size
	use.PurgeInstances, use.PurgeBytes = 1, in.Size
	e.wantUse("after a replace", use)

	// An edit of the other scan of the first session: three new revisions
	// of 900 bytes held, the three it supersedes awaiting their purge.
	target := tenant.CorrectionTarget{Subject: "sub-01", Session: "ses-1", Scan: "scan-" + uidOf(2, 101)}
	ed, targets, err := e.prepareEdit(target, 700)
	e.ok("PrepareCorrection of an edit", err)
	if len(targets) != 3 {
		e.Fatalf("the edit's targets are %+v, want the scan's three instances", targets)
	}
	e.commitEdit(ed, targets, 710)
	old := sizes(a.files[3:]...)
	use.HeldBytes += 3*900 - old
	use.PurgeInstances, use.PurgeBytes = use.PurgeInstances+3, use.PurgeBytes+old
	e.wantUse("after an edit", use)

	// A delete of one instance: from held to awaiting purge.
	gone := b.files[0]
	del := e.prepareDelete(tenant.CorrectionTarget{Subject: "sub-01", Session: "ses-2", Scan: "scan-" + uidOf(2, 200),
		Instance: gone.SOPInstanceUID}, 800)
	e.wantUse("with a delete prepared", use)
	e.commit(del.ID, 810, nil)
	use.HeldInstances, use.HeldBytes = use.HeldInstances-1, use.HeldBytes-gone.Size
	use.PurgeInstances, use.PurgeBytes = use.PurgeInstances+1, use.PurgeBytes+gone.Size
	e.wantUse("after an instance's delete", use)

	// Its purge, each object settled one at a time: the instance stops
	// counting when its object is settled, before the purge ends.
	_, err = e.p.StartPurge(e.ctx, del.ID, at(820), &master.AuditEntry{ID: e.id("a"), Time: at(820), Method: "PurgeCorrection",
		Outcome: master.OutcomeDone}, nil)
	e.ok("StartPurge", err)
	pt, err := e.p.PurgeTargets(e.ctx, del.ID, 100)
	e.ok("PurgeTargets", err)
	var settled []tenant.PurgeSettled
	for _, t := range pt {
		if t.Ord == tenant.TargetInstance {
			settled = append(settled, tenant.PurgeSettled{Target: t, Outcome: tenant.OutcomePurged})
		}
	}
	if len(settled) != 1 {
		e.Fatalf("the delete's purge targets are %+v, want one instance", pt)
	}
	e.ok("SettlePurgeTargets", e.p.SettlePurgeTargets(e.ctx, del.ID, settled))
	use.PurgeInstances, use.PurgeBytes = use.PurgeInstances-1, use.PurgeBytes-gone.Size
	e.wantUse("after the delete's instance was purged", use)
	_, err = e.p.FinishPurge(e.ctx, tenant.PurgeFinish{ID: del.ID, Now: at(830)})
	e.ok("FinishPurge", err)
	e.wantUse("after the delete's purge", use)

	// The replace's purge, ended whole: its old revision stops counting.
	_, err = e.p.StartPurge(e.ctx, rep.ID, at(840), &master.AuditEntry{ID: e.id("a"), Time: at(840), Method: "PurgeCorrection",
		Outcome: master.OutcomeDone}, nil)
	e.ok("StartPurge of a replace", err)
	_, err = e.p.FinishPurge(e.ctx, tenant.PurgeFinish{ID: rep.ID, Now: at(841)})
	e.ok("FinishPurge of a replace", err)
	use.PurgeInstances, use.PurgeBytes = use.PurgeInstances-1, use.PurgeBytes-in.Size
	e.wantUse("after the replace's purge", use)

	// A session's delete and its undo: everything back where it was.
	ses := e.prepareDelete(tenant.CorrectionTarget{Subject: "sub-01", Session: "ses-2"}, 900)
	e.commit(ses.ID, 910, nil)
	moved := use
	moved.HeldInstances, moved.HeldBytes = moved.HeldInstances-1, moved.HeldBytes-b.files[1].Size
	moved.PurgeInstances, moved.PurgeBytes = moved.PurgeInstances+1, moved.PurgeBytes+b.files[1].Size
	e.wantUse("after a session's delete", moved)
	_, err = e.p.UndoCorrection(e.ctx, ses.ID, "the wrong session", at(920), e.audit(), nil)
	e.ok("UndoCorrection", err)
	e.wantUse("after the session's delete was undone", use)

	// The edit's purge: its three old revisions stop counting.
	_, err = e.p.StartPurge(e.ctx, ed.ID, at(930), &master.AuditEntry{ID: e.id("a"), Time: at(930), Method: "PurgeCorrection",
		Outcome: master.OutcomeDone}, nil)
	e.ok("StartPurge of an edit", err)
	_, err = e.p.FinishPurge(e.ctx, tenant.PurgeFinish{ID: ed.ID, Now: at(931)})
	e.ok("FinishPurge of an edit", err)
	use.PurgeInstances, use.PurgeBytes = 0, 0
	e.wantUse("after every purge", use)
	e.wantTotalsRecounted("after every purge")
	held := allPages(e.penv, 1000, func(pg master.Page) ([]tenant.ScanInstance, string, error) {
		return e.p.ListHeldInstances(e.ctx, label.Path{Subject: "sub-01", Session: "ses-1", Scan: "scan-" + uidOf(2, 101)}, pg)
	})
	if len(held) != 3 || held[0].Size != 900 {
		e.Fatalf("the edited scan holds %+v, want three instances of 900 bytes", held)
	}
}

// testQuotaRebuilt guards that a rebuild that replaces the catalog leaves a
// tenant's use what the rebuilt catalog and the corrections not purged
// hold, the deleted instance still awaiting its purge.
func testQuotaRebuilt(e *renv) {
	e.rebuildSetup()
	before := e.liveScans()
	var heldBytes int64
	for _, s := range before {
		for _, in := range s.instances {
			heldBytes += in.Size
		}
	}
	gone := before[0].instances[0]
	c := (&cenv{senv: e.senv}).prepareDelete(tenant.CorrectionTarget{Subject: before[0].scan.Path.Subject,
		Session: before[0].scan.Path.Session, Scan: before[0].scan.Path.Scan, Instance: gone.SOPInstanceUID}, 50)
	(&cenv{senv: e.senv}).commit(c.ID, 60, nil)
	want := tenant.QuotaUse{HeldInstances: 7, HeldBytes: heldBytes - gone.Size, PurgeInstances: 1, PurgeBytes: gone.Size}
	e.wantUse("after a delete", want)
	e.createRebuild("rb-q", tenant.RebuildReplace)
	w := e.claimRebuild(at(100))
	e.build(w, e.liveScans(), 1000)
	_, err := e.p.ActivateCatalog(e.ctx, w.Fence(), at(110))
	e.ok("ActivateCatalog", err)
	e.wantUse("after the rebuild", want)
}
