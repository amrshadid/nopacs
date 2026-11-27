package tenanttest

import (
	"slices"
	"sort"
	"time"

	"github.com/amrshadid/nopacs/internal/core/record"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// testReplicaQueue guards the queue of copies: an attach makes the
// primary's scans pending, a claim leases each once, a copy ended at the
// catalog's manifest puts its scan in step (and a read may turn to it),
// one ended at another digest leaves it pending, a commit to a scan in step
// makes it pending again, the counts follow every change, and a second
// replica keeps rows of its own.
func testReplicaQueue(e *renv) {
	e.replicaSetup()
	e.attach("copy", "store", 10)
	if in, err := e.p.InStepReplicas(e.ctx, []string{seriesA, seriesB}); err != nil || len(in) != 0 {
		e.Errorf("InStepReplicas before any copy = %v, %v", in, err)
	}
	claims := e.claimAll(20)
	if len(claims) != 2 {
		e.Fatalf("a claim took %d rows, want 2", len(claims))
	}
	sort.Slice(claims, func(i, j int) bool { return claims[i].Fence.SeriesUID < claims[j].Fence.SeriesUID })
	a := claims[0]
	if a.Fence != (tenant.ReplicaFence{Replica: "copy", SeriesUID: seriesA, Attempt: 1}) || a.Primary != "store" ||
		a.StudyUID != uidOf(1, 1) || a.Path.Subject != "sub-01" || a.Path.Session != "ses-1" || a.Path.Scan != "scan-"+seriesA ||
		a.ManifestSHA256 != sum(0) || !a.Backfill || len(a.Damaged) != 0 || a.RecopyAll {
		e.Errorf("the claim of the first scan is %+v", a)
	}
	e.wantReplicaCounts("claimed", "copy", 0, 2, 0, 0, 0)
	if more := e.claimAll(21); len(more) != 0 {
		e.Errorf("a second claim took %d rows still leased", len(more))
	}
	state, err := e.p.EndReplicaScan(e.ctx, a.Fence, sum(0), at(25))
	e.ok("EndReplicaScan", err)
	if state != tenant.ReplicaInStep {
		e.Errorf("a copy ended at the catalog's manifest left the scan %s", state)
	}
	state, err = e.p.EndReplicaScan(e.ctx, claims[1].Fence, sum(5), at(25))
	e.ok("EndReplicaScan at another digest", err)
	if state != tenant.ReplicaPending {
		e.Errorf("a copy ended at another digest left the scan %s, want pending", state)
	}
	e.wantReplicaCounts("one copied, one not", "copy", 1, 0, 1, 0, 0)
	row, err := e.p.ReplicaScan(e.ctx, "copy", seriesA)
	e.ok("ReplicaScan", err)
	if row.State != tenant.ReplicaInStep || row.ManifestSHA256 != sum(0) || !row.Verified.Equal(at(25)) || !row.Queued.IsZero() ||
		row.Backfill || row.Attempt != 1 {
		e.Errorf("a row in step reads %+v", row)
	}
	if st := e.status("copy"); st.BackfillPending != 1 || !st.BackfillDone.IsZero() || !st.OldestQueued.Equal(at(10)) {
		e.Errorf("the status with one scan owed is %+v", st)
	}
	in, err := e.p.InStepReplicas(e.ctx, []string{seriesA, seriesB})
	e.ok("InStepReplicas", err)
	if len(in) != 1 || !slices.Equal(in[seriesA], []string{"copy"}) {
		e.Errorf("InStepReplicas = %v, want the first scan on copy", in)
	}
	// Another arrival into the scan in step (committed at its step time, two
	// minutes after it arrived): it is pending again.
	e.finish(e.archive("recv", 1, 1, 1, 500, at(40), "ses-1"))
	if st := e.rowState("copy", seriesA); st != tenant.ReplicaPending {
		e.Errorf("a scan in step committed into again is %s, want pending", st)
	}
	if in, _ := e.p.InStepReplicas(e.ctx, []string{seriesA}); len(in) != 0 {
		e.Errorf("a scan pending is answered in step: %v", in)
	}
	e.copyAll(300)
	e.wantReplicaCounts("all copied", "copy", 0, 0, 2, 0, 0)
	if st := e.status("copy"); st.BackfillPending != 0 || !st.BackfillDone.Equal(at(301)) || !st.OldestQueued.IsZero() {
		e.Errorf("the status with everything in step is %+v", st)
	}
	second := dirStorage("copy2", "copy2")
	second.ReplicaOf = "store"
	_, err = e.p.AddStorage(e.ctx, second, at(60), e.audit(), nil)
	e.ok("AddStorage of a second replica", err)
	e.wantReplicaCounts("the second replica", "copy2", 2, 0, 0, 0, 0)
	if in, _ := e.p.InStepReplicas(e.ctx, []string{seriesA}); !slices.Equal(in[seriesA], []string{"copy"}) {
		e.Errorf("InStepReplicas with a second replica pending = %v", in)
	}
	_, err = e.p.InStepReplicas(e.ctx, []string{"not a uid"})
	e.wantKind("InStepReplicas of a bad UID", err, fault.Invalid)
}

// testReplicaQueueFences guards the claims' fences: a write under an
// attempt not the row's is ErrLeaseLost, a lease renewed holds and one that
// ends is claimed again under the next attempt, a commit while a copy runs
// keeps that copy from ending in step, and a failed copy keeps the scan
// pending until its retry, with the replica's last fault, which a copy that
// ends in step clears.
func testReplicaQueueFences(e *renv) {
	e.replicaSetup()
	e.attach("copy", "store", 10)
	claims := e.claimAll(20)
	if len(claims) != 2 {
		e.Fatalf("a claim took %d rows, want 2", len(claims))
	}
	held, dropped := claims[0], claims[1]
	wrong := held.Fence
	wrong.Attempt = 2
	e.wantIs("RenewReplicaScan under another attempt", e.p.RenewReplicaScan(e.ctx, wrong, at(30), time.Minute), tenant.ErrLeaseLost,
		fault.Conflict)
	e.ok("RenewReplicaScan", e.p.RenewReplicaScan(e.ctx, held.Fence, at(30), time.Minute))
	again := e.claimAll(85)
	if len(again) != 1 || again[0].Fence.SeriesUID != dropped.Fence.SeriesUID || again[0].Fence.Attempt != 2 {
		e.Fatalf("the claim after one lease ended took %+v, want the unrenewed row under attempt 2", again)
	}
	_, err := e.p.EndReplicaScan(e.ctx, dropped.Fence, dropped.ManifestSHA256, at(86))
	e.wantIs("EndReplicaScan under a lease taken over", err, tenant.ErrLeaseLost, fault.Conflict)
	// A commit into the first scan while its copy runs, under whichever
	// lease holds it now.
	e.finish(e.archive("recv", 1, 1, 1, 500, at(87), "ses-1"))
	running := held
	if again[0].Fence.SeriesUID == seriesA {
		running = again[0]
	}
	state, err := e.p.EndReplicaScan(e.ctx, running.Fence, running.ManifestSHA256, at(88))
	e.ok("EndReplicaScan after a commit", err)
	if state != tenant.ReplicaPending {
		e.Errorf("a copy that a commit came during ended %s, want pending", state)
	}
	// The commit's time is its step time, two minutes after it arrived.
	failed := e.claimAll(300)
	if len(failed) != 2 {
		e.Fatalf("the claim before the failures took %d rows, want 2", len(failed))
	}
	for _, c := range failed {
		e.ok("FailReplicaScan", e.p.FailReplicaScan(e.ctx, tenant.ReplicaFailure{Fence: c.Fence, Fault: tenant.FaultUnreachable,
			Reason: "the store did not answer", Retry: at(500), Now: at(301)}))
	}
	if st := e.status("copy"); st.LastFault != tenant.FaultUnreachable || !st.LastFaultTime.Equal(at(301)) || st.Copying != 0 {
		e.Errorf("the status after failed copies is %+v", st)
	}
	row, err := e.p.ReplicaScan(e.ctx, "copy", seriesA)
	e.ok("ReplicaScan", err)
	if row.State != tenant.ReplicaPending || !row.Due.Equal(at(500)) || row.LastError != "the store did not answer" {
		e.Errorf("a row whose copy failed reads %+v", row)
	}
	if c := e.claimAll(499); len(c) != 0 {
		e.Errorf("a claim before the retry took %d rows", len(c))
	}
	e.copyAll(500)
	if st := e.status("copy"); st.LastFault != "" || !st.LastFaultTime.IsZero() || st.InStep != 2 {
		e.Errorf("the status after copies that ended in step is %+v", st)
	}
	err = e.p.FailReplicaScan(e.ctx, tenant.ReplicaFailure{Fence: held.Fence, Fault: "gone", Retry: at(600), Now: at(600)})
	e.wantKind("FailReplicaScan of a fault not in the list", err, fault.Invalid)
	_, err = e.p.ClaimReplicaScans(e.ctx, "copier", at(600), time.Minute, 0)
	e.wantKind("ClaimReplicaScans of none", err, fault.Invalid)
}

// testReplicaDamage guards a replica's damage: recorded on a scan in step it
// makes the scan damaged and no longer read, counted once per object; the
// claim names the damaged objects and the copy that ends in step clears
// them; a damage cleared by a scrub puts a scan back in step; damage
// recorded while a copy runs keeps that copy from ending in step; and a
// scan never copied takes no damage.
func testReplicaDamage(e *renv) {
	e.replicaSetup()
	e.attach("copy", "store", 10)
	e.copyAll(20)
	size := int64(5)
	dmg := tenant.ReplicaDamage{SOPInstanceUID: uidOf(3, 0), Revision: 1, FoundSize: &size, FoundSHA256: sum(7), Found: at(30)}
	made, err := e.p.RecordReplicaDamage(e.ctx, tenant.ReplicaDamageRecord{Replica: "copy", SeriesUID: seriesA, Damage: dmg})
	e.ok("RecordReplicaDamage", err)
	if !made || e.rowState("copy", seriesA) != tenant.ReplicaDamaged {
		e.Errorf("damage recorded: made %v, the scan %s", made, e.rowState("copy", seriesA))
	}
	made, err = e.p.RecordReplicaDamage(e.ctx, tenant.ReplicaDamageRecord{Replica: "copy", SeriesUID: seriesA, Damage: dmg})
	e.ok("RecordReplicaDamage again", err)
	if made {
		e.Error("damage recorded again was made again")
	}
	e.wantReplicaCounts("one object damaged", "copy", 0, 0, 1, 1, 1)
	if in, _ := e.p.InStepReplicas(e.ctx, []string{seriesA}); len(in) != 0 {
		e.Errorf("a damaged scan is answered in step: %v", in)
	}
	claims := e.claimAll(31)
	if len(claims) != 1 || len(claims[0].Damaged) != 1 || claims[0].Damaged[0].SOPInstanceUID != uidOf(3, 0) ||
		*claims[0].Damaged[0].FoundSize != 5 || claims[0].Damaged[0].FoundSHA256 != sum(7) {
		e.Fatalf("the claim of a damaged scan is %+v", claims)
	}
	state, err := e.p.EndReplicaScan(e.ctx, claims[0].Fence, claims[0].ManifestSHA256, at(32))
	e.ok("EndReplicaScan", err)
	if state != tenant.ReplicaInStep {
		e.Errorf("a damaged scan copied again ended %s", state)
	}
	e.wantReplicaCounts("copied again", "copy", 0, 0, 2, 0, 0)
	// The manifest of the other scan, found damaged, then read good.
	_, err = e.p.RecordReplicaDamage(e.ctx, tenant.ReplicaDamageRecord{Replica: "copy", SeriesUID: seriesB,
		Damage: tenant.ReplicaDamage{Found: at(40)}})
	e.ok("RecordReplicaDamage of a manifest", err)
	cleared, err := e.p.ClearReplicaDamage(e.ctx, nil, "copy", seriesB, "", at(41))
	e.ok("ClearReplicaDamage", err)
	if !cleared || e.rowState("copy", seriesB) != tenant.ReplicaInStep {
		e.Errorf("damage cleared: %v, the scan %s; want it in step again", cleared, e.rowState("copy", seriesB))
	}
	if cleared, err := e.p.ClearReplicaDamage(e.ctx, nil, "copy", seriesB, "", at(42)); err != nil || cleared {
		e.Errorf("ClearReplicaDamage again = %v, %v; want nothing cleared", cleared, err)
	}
	// Damage while a copy runs.
	_, err = e.p.RecordReplicaDamage(e.ctx, tenant.ReplicaDamageRecord{Replica: "copy", SeriesUID: seriesA, Damage: dmg})
	e.ok("RecordReplicaDamage", err)
	claims = e.claimAll(50)
	other := dmg
	other.SOPInstanceUID = uidOf(3, 1)
	_, err = e.p.RecordReplicaDamage(e.ctx, tenant.ReplicaDamageRecord{Replica: "copy", SeriesUID: seriesA, Damage: other})
	e.ok("RecordReplicaDamage while copying", err)
	if st := e.rowState("copy", seriesA); st != tenant.ReplicaCopying {
		e.Errorf("damage recorded while copying left the scan %s, want copying", st)
	}
	state, err = e.p.EndReplicaScan(e.ctx, claims[0].Fence, claims[0].ManifestSHA256, at(51))
	e.ok("EndReplicaScan", err)
	if state != tenant.ReplicaDamaged {
		e.Errorf("a copy that damage was recorded during ended %s, want damaged", state)
	}
	e.wantReplicaCounts("damage during a copy", "copy", 0, 0, 1, 1, 2)
	e.addStorage("p3")
	fresh := dirStorage("r3", "r3")
	fresh.ReplicaOf = "p3"
	_, err = e.p.AddStorage(e.ctx, fresh, at(60), e.audit(), nil)
	e.ok("AddStorage", err)
	_, err = e.p.RecordReplicaDamage(e.ctx, tenant.ReplicaDamageRecord{Replica: "r3", SeriesUID: seriesA, Damage: dmg})
	e.wantPrecondition("RecordReplicaDamage of a scan the replica has no row of", err)
	bad := dmg
	bad.FoundSize = nil
	_, err = e.p.RecordReplicaDamage(e.ctx, tenant.ReplicaDamageRecord{Replica: "copy", SeriesUID: seriesA, Damage: bad})
	e.wantKind("RecordReplicaDamage of a missing object with a checksum", err, fault.Invalid)
}

// testReplicaFollowsCorrections guards that a correction's publish and undo
// reach the replicas' rows in their transactions: a delete of a session
// drops the rows of its scans, its undo queues them again, and a delete of
// one instance queues its scan again, leaving the other in step.
func testReplicaFollowsCorrections(e *renv) {
	e.replicaSetup()
	e.attach("copy", "store", 10)
	e.copyAll(20)
	prepare := func(target tenant.CorrectionTarget, s int) string {
		e.Helper()
		p, err := e.p.PreviewDelete(e.ctx, target, at(s))
		e.ok("PreviewDelete", err)
		id, err := tenant.NewCorrectionID(at(s))
		e.ok("NewCorrectionID", err)
		_, _, err = e.p.PrepareCorrection(e.ctx, tenant.CorrectionPrepare{ID: id, Kind: record.KindDelete, Target: target,
			Reason: "consent withdrawn", Confirm: p.Confirm, Now: at(s + 1)}, e.audit(), nil)
		e.ok("PrepareCorrection", err)
		_, err = e.p.CommitCorrection(e.ctx, tenant.CorrectionCommit{ID: id, Now: at(s + 2), Grace: time.Hour})
		e.ok("CommitCorrection", err)
		return id
	}
	id := prepare(tenant.CorrectionTarget{Subject: "sub-01", Session: "ses-1"}, 30)
	if a, b := e.rowState("copy", seriesA), e.rowState("copy", seriesB); a != "" || b != "" {
		e.Errorf("the rows after a delete of the session are %q, %q; want none", a, b)
	}
	e.wantReplicaCounts("after the delete", "copy", 0, 0, 0, 0, 0)
	_, err := e.p.UndoCorrection(e.ctx, id, "a mistake", at(40), e.audit(), nil)
	e.ok("UndoCorrection", err)
	if a, b := e.rowState("copy", seriesA), e.rowState("copy", seriesB); a != tenant.ReplicaPending || b != tenant.ReplicaPending {
		e.Errorf("the rows after the undo are %q, %q; want both pending", a, b)
	}
	e.copyAll(50)
	prepare(tenant.CorrectionTarget{Subject: "sub-01", Session: "ses-1", Scan: "scan-" + seriesA, Instance: uidOf(3, 0)}, 60)
	if a, b := e.rowState("copy", seriesA), e.rowState("copy", seriesB); a != tenant.ReplicaPending || b != tenant.ReplicaInStep {
		e.Errorf("the rows after a delete of one instance are %q, %q; want its scan pending, the other in step", a, b)
	}
	e.wantReplicaCounts("after the instance's delete", "copy", 1, 0, 1, 0, 0)
}

// testReplicaFollowsPurge guards that the end of an instance delete's purge,
// which gives its scan the manifest written without the instance, queues
// the scan again for the replicas: a replica in step at the manifest the
// purge replaced is pending, and no read is sent to it, until its copy of
// the new manifest ends.
func testReplicaFollowsPurge(e *renv) {
	e.replicaSetup()
	e.attach("copy", "store", 10)
	e.copyAll(20)
	target := tenant.CorrectionTarget{Subject: "sub-01", Session: "ses-1", Scan: "scan-" + seriesA, Instance: uidOf(3, 0)}
	p, err := e.p.PreviewDelete(e.ctx, target, at(30))
	e.ok("PreviewDelete", err)
	id, err := tenant.NewCorrectionID(at(30))
	e.ok("NewCorrectionID", err)
	c, _, err := e.p.PrepareCorrection(e.ctx, tenant.CorrectionPrepare{ID: id, Kind: record.KindDelete, Target: target,
		Reason: "consent withdrawn", Confirm: p.Confirm, Now: at(31)}, e.audit(), nil)
	e.ok("PrepareCorrection", err)
	_, err = e.p.CommitCorrection(e.ctx, tenant.CorrectionCommit{ID: id, Now: at(32), Grace: time.Hour})
	e.ok("CommitCorrection", err)
	e.copyAll(40)
	if st := e.rowState("copy", seriesA); st != tenant.ReplicaInStep {
		e.Fatalf("the scan copied after the delete's publish is %s on the replica, want in step", st)
	}
	_, err = e.p.StartPurge(e.ctx, id, at(50), &master.AuditEntry{ID: e.id("a"), Time: at(50), Method: "PurgeCorrection",
		Outcome: master.OutcomeDone}, nil)
	e.ok("StartPurge", err)
	e.ok("RecordPurgedObject", e.p.RecordPurgedObject(e.ctx, tenant.PurgedObject{ID: id, Storage: "store", Key: c.InstanceKey,
		Size: c.InstanceSize, SHA256: sum(5), Disposition: tenant.DispositionRemoved, Now: at(51)}))
	scan, err := e.p.CatalogScan(e.ctx, target.Path())
	e.ok("CatalogScan", err)
	_, err = e.p.FinishPurge(e.ctx, tenant.PurgeFinish{ID: id, Now: at(52), Manifest: sum(77), ManifestBefore: scan.ManifestSHA256})
	e.ok("FinishPurge", err)
	if st := e.rowState("copy", seriesA); st != tenant.ReplicaPending {
		e.Errorf("the scan whose manifest the purge rewrote is %s on the replica, want pending", st)
	}
	if in, _ := e.p.InStepReplicas(e.ctx, []string{seriesA}); len(in[seriesA]) != 0 {
		e.Errorf("InStepReplicas of the scan the purge rewrote = %v before its copy, want none", in)
	}
	e.wantReplicaCounts("after the purge", "copy", 1, 0, 1, 0, 0)
	claims := e.claimAll(60)
	if len(claims) != 1 || claims[0].ManifestSHA256 != sum(77) {
		e.Fatalf("the claims after the purge are %+v; want the scan at the new manifest", claims)
	}
	_, err = e.p.EndReplicaScan(e.ctx, claims[0].Fence, claims[0].ManifestSHA256, at(61))
	e.ok("EndReplicaScan", err)
	if in, _ := e.p.InStepReplicas(e.ctx, []string{seriesA}); !slices.Equal(in[seriesA], []string{"copy"}) {
		e.Errorf("InStepReplicas of the scan copied at the new manifest = %v", in)
	}
}

// testReplicaFollowsRestoreHide guards that a restore's hide of a delete
// known only from its record drops the replicas' rows of the scan it hides
// in the same transaction, as the delete's publish does, so the replica's
// counts never count a scan the catalog no longer shows.
func testReplicaFollowsRestoreHide(e *renv) {
	e.replicaSetup()
	e.attach("copy", "store", 10)
	e.copyAll(20)
	rs, ok := e.p.(tenant.Restoring)
	if !ok {
		e.Fatal("the store does not implement tenant.Restoring")
	}
	id, err := tenant.NewCorrectionID(at(30))
	e.ok("NewCorrectionID", err)
	rec := record.Correction{ID: id, Format: record.Format, Kind: record.KindDelete, Level: record.LevelScan, State: record.StateCommitted,
		ReasonSHA256: record.ReasonDigest("consent withdrawn"), Created: at(30), CommittedAt: at(31), PurgeAfter: at(31).Add(time.Hour),
		Scans: []record.Scan{{Subject: "sub-01", Session: "ses-1", Scan: "scan-" + seriesB, StudyUID: uidOf(1, 1), SeriesUID: seriesB}}}
	_, err = rs.AdoptCorrection(e.ctx, rec, "store", at(32))
	e.ok("AdoptCorrection", err)
	hidden, err := rs.HideDeletes(e.ctx, []tenant.DeleteToHide{{ID: id}})
	e.ok("HideDeletes", err)
	if hidden.Scans != 1 {
		e.Fatalf("HideDeletes hid %+v, want the one scan", hidden)
	}
	if a, b := e.rowState("copy", seriesA), e.rowState("copy", seriesB); a != tenant.ReplicaInStep || b != "" {
		e.Errorf("the rows after the hide are %q, %q; want the scan kept in step, none of the scan hidden", a, b)
	}
	e.wantReplicaCounts("after the hide", "copy", 0, 0, 1, 0, 0)
}

// testReplicaFollowsRebuild guards that a rebuild that makes its shadow the
// catalog in use makes the replicas' rows agree with it in the same
// transaction: the row of a scan the shadow lacks is dropped, a row in step
// at a digest the new catalog does not hold is pending, a row in step at
// the new catalog's digest stays in step, and the counts are counted again.
func testReplicaFollowsRebuild(e *renv) {
	e.rebuildSetup()
	e.addStorage("copy")
	e.attach("copy", "store", 10)
	e.copyAll(20)
	e.createRebuild("rb-1", tenant.RebuildReplace)
	w := e.claimRebuild(at(30))
	var shadow []liveScan
	for _, s := range e.liveScans() {
		switch s.scan.SeriesUID {
		case seriesB:
			continue
		case seriesA:
			s.scan.ManifestSHA256 = sum(777)
		}
		shadow = append(shadow, s)
	}
	e.build(w, shadow, 1000)
	_, err := e.p.ActivateCatalog(e.ctx, w.Fence(), at(40))
	e.ok("ActivateCatalog", err)
	if a, b, c := e.rowState("copy", seriesA), e.rowState("copy", seriesB), e.rowState("copy", uidOf(2, 200)); a != tenant.ReplicaPending ||
		b != "" || c != tenant.ReplicaInStep {
		e.Errorf("the rows after the activation are %q, %q, %q; want pending, none, in step", a, b, c)
	}
	e.wantReplicaCounts("after the activation", "copy", 1, 0, 1, 0, 0)
	if in, _ := e.p.InStepReplicas(e.ctx, []string{uidOf(2, 200)}); !slices.Equal(in[uidOf(2, 200)], []string{"copy"}) {
		e.Errorf("InStepReplicas after the activation = %v", in)
	}
}

// testReplicaBackfill guards BackfillReplica: every scan of the primary is
// owed to the backfill again, those in step pending to be verified again,
// and the backfill is done once each is in step. The attach's backfill and
// a person's are each followed by a replicate job that counts the scans
// marked, is answered again to a repeated call, and ends done once the
// replica owes its backfill nothing.
func testReplicaBackfill(e *renv) {
	e.replicaSetup()
	e.attach("copy", "store", 10)
	sched, open, err := e.p.ReplicateJob(e.ctx, "copy")
	e.ok("ReplicateJob", err)
	if !open || sched.ID != tenant.ReplicateJobID("copy", at(10)) || sched.Kind != tenant.JobReplicate || sched.State != tenant.JobRunning ||
		sched.Replicate == nil || *sched.Replicate != (tenant.ReplicateJob{Replica: "copy", Primary: "store", Scheduled: true}) ||
		sched.Counts.Scans != 2 || sched.Counts.ScansDone != 0 {
		e.Errorf("the attach's replicate job is %+v (open %v), want the scheduled job of copy counting 2 scans", sched, open)
	}
	e.copyAll(20)
	if st := e.status("copy"); !st.BackfillDone.Equal(at(21)) {
		e.Errorf("the first backfill done is %v, want at 21 s", st.BackfillDone)
	}
	if j, err := e.p.Job(e.ctx, sched.ID); err != nil || j.State != tenant.JobDone || j.Counts.ScansDone != 2 {
		e.Errorf("the attach's replicate job after the copy is %+v, %v; want done with 2 scans done", j, err)
	}
	if _, open, err := e.p.ReplicateJob(e.ctx, "copy"); err != nil || open {
		e.Errorf("a replicate job is still open after the backfill (%v, %v)", open, err)
	}
	b := tenant.ReplicaBackfill{Replica: "copy", JobID: "J0000000000b1", Now: at(30)}
	j, made, err := e.p.BackfillReplica(e.ctx, b, e.audit(), nil)
	e.ok("BackfillReplica", err)
	if !made || j.ID != b.JobID || j.State != tenant.JobRunning || j.Counts.Scans != 2 || j.Replicate == nil || j.Replicate.Scheduled ||
		j.Replicate.Primary != "store" {
		e.Errorf("a backfill answered %+v (made %v), want a running job of 2 scans made by a person", j, made)
	}
	if st := e.status("copy"); st.Pending != 2 || st.BackfillPending != 2 || !st.BackfillDone.IsZero() {
		e.Errorf("the status after a backfill is queued is %+v", st)
	}
	again, made, err := e.p.BackfillReplica(e.ctx, tenant.ReplicaBackfill{Replica: "copy", JobID: "J0000000000b2", Now: at(31)}, e.audit(), nil)
	e.ok("BackfillReplica repeated", err)
	if made || again.ID != b.JobID {
		e.Errorf("a repeated backfill answered %s (made %v), want the running job %s", again.ID, made, b.JobID)
	}
	e.copyAll(40)
	if st := e.status("copy"); st.InStep != 2 || st.BackfillPending != 0 || !st.BackfillDone.Equal(at(41)) {
		e.Errorf("the status after the backfill is %+v", st)
	}
	if j, err := e.p.Job(e.ctx, b.JobID); err != nil || j.State != tenant.JobDone || j.Counts.ScansDone != 2 || !j.Ended.Equal(at(41)) {
		e.Errorf("the backfill's job after the copy is %+v, %v; want done at 41 s with 2 scans done", j, err)
	}
	_, _, err = e.p.BackfillReplica(e.ctx, tenant.ReplicaBackfill{Replica: "gone", JobID: "J0000000000b3", Now: at(50)}, e.audit(), nil)
	e.wantKind("BackfillReplica of a storage the tenant does not have", err, fault.NotFound)
	_, _, err = e.p.BackfillReplica(e.ctx, tenant.ReplicaBackfill{Replica: "store", JobID: "J0000000000b4", Now: at(50)}, e.audit(), nil)
	e.wantPrecondition("BackfillReplica of a storage that is not a replica", err)
}
