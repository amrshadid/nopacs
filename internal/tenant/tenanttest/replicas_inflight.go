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
// claim takes the scan naming the instance the purge removes, so that copy
// skips it.
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
	again := e.claimAll(53)
	if len(again) != 1 || again[0].Fence.SeriesUID != seriesA || len(again[0].Purging) != 1 || again[0].Purging[0] != uidOf(3, 0) {
		e.Fatalf("the claim after the purge's start is %+v; want the scan, naming the instance the purge removes", again)
	}
}

// testPurgingScanClaimedWithoutItsManifest guards the claim of a scan an
// instance delete in state purging touches, which may stay purging for
// years under a hold: the scan is claimed, so new arrivals in it reach the
// replica, and the claim names the instance the purge removes, which the
// copy skips. Until FinishPurge the catalog still names the manifest the
// purge is replacing, so such a copy puts no manifest, and its end never
// records the row in step: the row waits, pending and not due, a new
// arrival making it due at once; FinishPurge queues it again, and the next
// claim takes it at the new manifest, naming nothing purging, and ends it
// in step.
func testPurgingScanClaimedWithoutItsManifest(e *renv) {
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
	purgingOne := func(what string, claims []tenant.ReplicaClaim) tenant.ReplicaClaim {
		e.Helper()
		if len(claims) != 1 || claims[0].Fence.SeriesUID != seriesA || len(claims[0].Purging) != 1 || claims[0].Purging[0] != uidOf(3, 0) {
			e.Fatalf("the claims %s are %+v; want the scan, naming the instance the purge removes", what, claims)
		}
		return claims[0]
	}
	endPending := func(what string, c tenant.ReplicaClaim, s int) {
		e.Helper()
		st, err := e.p.EndReplicaScan(e.ctx, c.Fence, c.ManifestSHA256, at(s))
		e.ok("EndReplicaScan", err)
		if st != tenant.ReplicaPending || e.rowState("copy", seriesA) != tenant.ReplicaPending {
			e.Fatalf("the end of a copy %s is %s; want pending, never in step while the purge is unfinished", what, st)
		}
		if again := e.claimAll(s + 1); len(again) != 0 {
			e.Fatalf("the claims just after the copy %s are %+v; want none until the scan changes", what, again)
		}
	}
	endPending("while the purge runs", purgingOne("while the instance delete purges", e.claimAll(42)), 43)
	e.wantReplicaCounts("while the purge runs", "copy", 1, 0, 1, 0, 0)
	// A new arrival in the scan: due at once, claimed at its new manifest.
	_, err = e.commitInto(seriesA, 9, 50)
	e.ok("CommitScan into the scan whose purge runs", err)
	arrived := purgingOne("after an arrival", e.claimAll(53))
	if arrived.ManifestSHA256 != sum(9) {
		e.Errorf("the claim after an arrival is at manifest %v; want the arrival's %v", arrived.ManifestSHA256, sum(9))
	}
	endPending("after an arrival", arrived, 54)
	e.ok("RecordPurgedObject", e.p.RecordPurgedObject(e.ctx, tenant.PurgedObject{ID: id, Storage: "store", Key: c.InstanceKey,
		Size: c.InstanceSize, SHA256: sum(5), Disposition: tenant.DispositionRemoved, Now: at(60)}))
	scan, err := e.p.CatalogScan(e.ctx, target.Path())
	e.ok("CatalogScan", err)
	_, err = e.p.FinishPurge(e.ctx, tenant.PurgeFinish{ID: id, Now: at(61), Manifest: sum(77), ManifestBefore: scan.ManifestSHA256})
	e.ok("FinishPurge", err)
	after := e.claimAll(62)
	if len(after) != 1 || after[0].Fence.SeriesUID != seriesA || after[0].ManifestSHA256 != sum(77) || len(after[0].Purging) != 0 {
		e.Fatalf("the claims after the purge are %+v; want the scan at the manifest written without the instance, nothing purging", after)
	}
	st, err := e.p.EndReplicaScan(e.ctx, after[0].Fence, after[0].ManifestSHA256, at(63))
	e.ok("EndReplicaScan", err)
	if st != tenant.ReplicaInStep {
		e.Errorf("the copy after the purge ended %s; want in step", st)
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
