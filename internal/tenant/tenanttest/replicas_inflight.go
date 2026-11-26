package tenanttest

import (
	"errors"
	"time"

	"github.com/amrshadid/nopacs/internal/core/record"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// Copies to a replica under way when a correction removes what they copy:
// their leases end, and the correction keeps the last moment a put of one
// of them can land.

// testPurgeEndsCopies guards a purge's start over a scan a replica is
// copying: the copy's lease ends (its renewal and its end are refused as a
// lost lease, so it puts nothing more and never ends in step), its row is
// due again with the change counted, and the correction keeps the copy's
// lease due plus the grace as the moment its purge waits for; the next
// claim does not take the scan while the purge runs.
func testPurgeEndsCopies(e *renv) {
	e.replicaSetup()
	e.attach("copy", "store", 10)
	e.copyAll(20)
	target := tenant.CorrectionTarget{Subject: "sub-01", Session: "ses-1", Scan: "scan-" + seriesA, Instance: uidOf(3, 0)}
	pv, err := e.p.PreviewDelete(e.ctx, target, at(30))
	e.ok("PreviewDelete", err)
	id, err := tenant.NewCorrectionID(at(30))
	e.ok("NewCorrectionID", err)
	_, _, err = e.p.PrepareCorrection(e.ctx, tenant.CorrectionPrepare{ID: id, Kind: record.KindDelete, Target: target,
		Reason: "consent withdrawn", Confirm: pv.Confirm, Now: at(31)}, e.audit(), nil)
	e.ok("PrepareCorrection", err)
	_, err = e.p.CommitCorrection(e.ctx, tenant.CorrectionCommit{ID: id, Now: at(32), Grace: time.Hour})
	e.ok("CommitCorrection", err)
	claims := e.claimAll(40)
	if len(claims) != 1 || claims[0].Fence.SeriesUID != seriesA {
		e.Fatalf("the claims after the delete's publish are %+v; want the scan of the instance", claims)
	}
	if got := e.correction(id).CopiesInFlightUntil; !got.IsZero() {
		e.Errorf("an instance delete's publish, which drops no row, kept %v as copies in flight; want none", got)
	}
	e.startPurge(id, 50)
	err = e.p.RenewReplicaScan(e.ctx, claims[0].Fence, at(51), time.Minute)
	if !errors.Is(err, tenant.ErrLeaseLost) {
		e.Errorf("the renewal of a copy claimed before the purge began = %v; want its lease lost", err)
	}
	if _, err := e.p.EndReplicaScan(e.ctx, claims[0].Fence, claims[0].ManifestSHA256, at(52)); !errors.Is(err, tenant.ErrLeaseLost) {
		e.Errorf("the end of a copy claimed before the purge began = %v; want its lease lost", err)
	}
	if st := e.rowState("copy", seriesA); st != tenant.ReplicaPending {
		e.Errorf("the row of a copy the purge's start ended is %s, want pending", st)
	}
	e.wantReplicaCounts("after the purge's start", "copy", 1, 0, 1, 0, 0)
	want := at(40).Add(time.Minute).Add(tenant.ReplicaPutGrace)
	if c := e.correction(id); !c.CopiesInFlightUntil.Equal(want) {
		e.Errorf("the purge's start kept %v as copies in flight; want the copy's lease due plus the grace, %v", c.CopiesInFlightUntil, want)
	}
	if again := e.claimAll(53); len(again) != 0 {
		e.Fatalf("the claim after the purge's start is %+v; want none while the purge runs", again)
	}
}

// testPurgingScanNotClaimed guards the claim against an instance delete in
// state purging: until FinishPurge the catalog still names the manifest the
// purge is replacing, so a copy claimed then would put that manifest, which
// names the deleted instance, on the replica after the purge rewrote it
// there. The scan's row stays pending, due, and is not claimed while the
// purge runs (the replica's other scans are); FinishPurge queues it again,
// and the next claim takes it at the new manifest.
func testPurgingScanNotClaimed(e *renv) {
	e.replicaSetup()
	e.attach("copy", "store", 10)
	e.copyAll(20)
	target := tenant.CorrectionTarget{Subject: "sub-01", Session: "ses-1", Scan: "scan-" + seriesA, Instance: uidOf(3, 0)}
	pv, err := e.p.PreviewDelete(e.ctx, target, at(30))
	e.ok("PreviewDelete", err)
	id, err := tenant.NewCorrectionID(at(30))
	e.ok("NewCorrectionID", err)
	c, _, err := e.p.PrepareCorrection(e.ctx, tenant.CorrectionPrepare{ID: id, Kind: record.KindDelete, Target: target,
		Reason: "consent withdrawn", Confirm: pv.Confirm, Now: at(31)}, e.audit(), nil)
	e.ok("PrepareCorrection", err)
	_, err = e.p.CommitCorrection(e.ctx, tenant.CorrectionCommit{ID: id, Now: at(32), Grace: time.Hour})
	e.ok("CommitCorrection", err)
	if st := e.rowState("copy", seriesA); st != tenant.ReplicaPending {
		e.Fatalf("the scan of the deleted instance is %s on the replica after the publish, want pending", st)
	}
	e.startPurge(id, 40)
	// The other scan, queued again, is claimed beside the purge.
	_, err = e.p.BackfillReplica(e.ctx, "copy", at(41), e.audit(), nil)
	e.ok("BackfillReplica", err)
	claims := e.claimAll(42)
	if len(claims) != 1 || claims[0].Fence.SeriesUID != seriesB {
		e.Fatalf("the claims while the instance delete purges are %+v; want the other scan alone", claims)
	}
	_, err = e.p.EndReplicaScan(e.ctx, claims[0].Fence, claims[0].ManifestSHA256, at(43))
	e.ok("EndReplicaScan", err)
	if again := e.claimAll(44); len(again) != 0 {
		e.Fatalf("a later claim while the purge runs is %+v; want none", again)
	}
	if st := e.rowState("copy", seriesA); st != tenant.ReplicaPending {
		e.Errorf("the scan left unclaimed while its purge runs is %s, want pending", st)
	}
	e.wantReplicaCounts("while the purge runs", "copy", 1, 0, 1, 0, 0)
	e.ok("RecordPurgedObject", e.p.RecordPurgedObject(e.ctx, tenant.PurgedObject{ID: id, Storage: "store", Key: c.InstanceKey,
		Size: c.InstanceSize, SHA256: sum(5), Disposition: tenant.DispositionRemoved, Now: at(45)}))
	scan, err := e.p.CatalogScan(e.ctx, target.Path())
	e.ok("CatalogScan", err)
	_, err = e.p.FinishPurge(e.ctx, tenant.PurgeFinish{ID: id, Now: at(46), Manifest: sum(77), ManifestBefore: scan.ManifestSHA256})
	e.ok("FinishPurge", err)
	after := e.claimAll(47)
	if len(after) != 1 || after[0].Fence.SeriesUID != seriesA || after[0].ManifestSHA256 != sum(77) {
		e.Fatalf("the claims after the purge are %+v; want the scan at the manifest written without the instance", after)
	}
}

// testDeletePublishEndsCopies guards a delete's publish over a scan a
// replica is copying: the scan's row goes, so the copy's renewal is
// refused as a lost lease, and the correction keeps the copy's lease due
// plus the grace; the purge's start, which finds no copy of its scans
// left, keeps it as it was.
func testDeletePublishEndsCopies(e *renv) {
	e.replicaSetup()
	e.attach("copy", "store", 10)
	claims := e.claimAll(20)
	if len(claims) != 2 {
		e.Fatalf("the backfill's claims are %+v; want both scans", claims)
	}
	id := e.commitDeleteOf(seriesA, 30)
	for _, c := range claims {
		err := e.p.RenewReplicaScan(e.ctx, c.Fence, at(33), time.Minute)
		switch {
		case c.Fence.SeriesUID == seriesA && !errors.Is(err, tenant.ErrLeaseLost):
			e.Errorf("the renewal of a copy of the deleted scan = %v; want its lease lost", err)
		case c.Fence.SeriesUID != seriesA && err != nil:
			e.Errorf("the renewal of a copy of the other scan = %v", err)
		}
	}
	want := at(20).Add(time.Minute).Add(tenant.ReplicaPutGrace)
	if got := e.correction(id).CopiesInFlightUntil; !got.Equal(want) {
		e.Errorf("the delete's publish kept %v as copies in flight; want the copy's lease due plus the grace, %v", got, want)
	}
	e.startPurge(id, 40)
	if got := e.correction(id).CopiesInFlightUntil; !got.Equal(want) {
		e.Errorf("the purge's start moved copies in flight to %v; want %v kept", got, want)
	}
}
