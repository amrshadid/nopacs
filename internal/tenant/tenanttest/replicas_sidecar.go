package tenanttest

import (
	"time"

	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/core/layout"
	"github.com/amrshadid/nopacs/internal/core/record"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// sidecarOfScan returns the catalog's digest of the metadata file of the
// scan at p.
func (e *renv) sidecarOfScan(p label.Path) tenant.SHA256 {
	e.Helper()
	sc, err := e.p.CatalogScan(e.ctx, p)
	e.ok("CatalogScan", err)
	return sc.SidecarSHA256
}

// testForgetSidecar guards ForgetSidecar, which a writer calls before it
// puts a new metadata file: it drops the digest of a scan on the storage
// named and answers that there was one; on another storage, for a series
// the catalog does not hold, or once dropped, it drops nothing.
func testForgetSidecar(e *renv) {
	e.replicaSetup()
	series := uidOf(2, 910)
	a := e.arrive(series, 20, 30)
	a.c.SidecarSHA256 = sum(500)
	e.ok("commit with a metadata digest", a.commit(31))
	p := label.Path{Subject: "sub-01", Session: "ses-1", Scan: "scan-" + series}
	if dropped, err := e.p.ForgetSidecar(e.ctx, series, "copy"); err != nil || dropped {
		e.Errorf("ForgetSidecar on another storage = %v, %v; want nothing dropped", dropped, err)
	}
	if got := e.sidecarOfScan(p); got != sum(500) {
		e.Errorf("after a forget on another storage the digest reads %v; want it kept", got)
	}
	if dropped, err := e.p.ForgetSidecar(e.ctx, series, "store"); err != nil || !dropped {
		e.Errorf("ForgetSidecar = %v, %v; want the digest dropped", dropped, err)
	}
	if got := e.sidecarOfScan(p); !got.IsZero() {
		e.Errorf("after the forget the digest reads %v; want none", got)
	}
	if dropped, err := e.p.ForgetSidecar(e.ctx, series, "store"); err != nil || dropped {
		e.Errorf("ForgetSidecar again = %v, %v; want nothing dropped", dropped, err)
	}
	if dropped, err := e.p.ForgetSidecar(e.ctx, uidOf(2, 999), "store"); err != nil || dropped {
		e.Errorf("ForgetSidecar of a series the catalog does not hold = %v, %v", dropped, err)
	}
	_, err := e.p.ForgetSidecar(e.ctx, "not a uid", "store")
	e.wantKind("ForgetSidecar of a series that is not a UID", err, fault.Invalid)
}

// verifyOneScan records, under a scrub unit's fence, the first scan the
// unit reads as read whole on storage at second s, and returns its series.
func (e *renv) verifyOneScan(id, storage string, s int) string {
	e.Helper()
	e.createScrub(id, "", at(s))
	e.listScrub(at(s + 10))
	w, scan, _ := e.scrubScan(at(s + 20))
	e.ok("RecordScanVerified", e.p.RecordScanVerified(e.ctx, tenant.ScanVerified{Fence: w.Fence(), Storage: storage, Path: scan.Path,
		At: at(s + 30)}))
	_, err := e.p.CancelJob(e.ctx, id, at(s+40), e.audit(), nil)
	e.ok("CancelJob of the scrub", err)
	_, err = e.p.EndJobScan(e.ctx, tenant.JobProgress{Fence: w.Fence(), FailuresKept: tenant.DefaultFailuresKept, Now: at(s + 41)})
	e.ok("EndJobScan of the canceled scrub's unit", err)
	return e.seriesOf(scan.Path)
}

// verifiedCount counts the scans of storage answered read whole.
func (e *renv) verifiedCount(storage string) int {
	e.Helper()
	page, _, err := e.p.ScanVerifications(e.ctx, storage, "", 100)
	e.ok("ScanVerifications", err)
	n := 0
	for _, v := range page {
		if !v.Verified.IsZero() {
			n++
		}
	}
	return n
}

// testVerifiedFollowsTheScan guards what is recorded of a scan's reads: a
// commit that gives the scan more instances drops it (no read covered
// them); a scan that leaves the catalog for good, its delete purged, takes
// it along, so a scan of the same series received again starts unread;
// and a storage removed takes its records along, so a storage added again
// under its name starts unread.
func testVerifiedFollowsTheScan(e *renv) {
	e.replicaSetup()
	const b = 1000 // after the setup's archive
	series := e.verifyOneScan("v1", "store", b+100)
	if n := e.verifiedCount("store"); n != 1 {
		e.Fatalf("%d scans read whole after the record; want 1", n)
	}
	_, err := e.commitInto(series, 30, b+160)
	e.ok("commit of one more instance", err)
	if n := e.verifiedCount("store"); n != 0 {
		e.Errorf("after a commit gave the scan more instances %d scans read whole; want none", n)
	}

	series = e.verifyOneScan("v2", "store", b+200)
	id := e.commitDeleteOf(series, b+260)
	e.startPurge(id, b+270)
	_, err = e.p.FinishPurge(e.ctx, tenant.PurgeFinish{ID: id, Now: at(b + 280)})
	e.ok("FinishPurge", err)
	a := e.arrive(series, 40, b+290)
	e.ok("commit of the series received again", a.commit(b+295))
	if n := e.verifiedCount("store"); n != 0 {
		e.Errorf("after the scan was purged and received again %d scans read whole; want none", n)
	}

	e.attach("copy", "store", b+400)
	e.copyAll(b + 401)
	e.verifyOneScan("v3", "copy", b+410)
	if n := e.verifiedCount("copy"); n != 1 {
		e.Fatalf("%d scans of the replica read whole after the record; want 1", n)
	}
	_, _, err = e.p.DetachReplica(e.ctx, tenant.ReplicaDetach{Replica: "copy", Now: at(b + 460)}, e.audit(), nil)
	e.ok("DetachReplica", err)
	_, err = e.p.RemoveStorage(e.ctx, "copy", e.audit(), nil)
	e.ok("RemoveStorage", err)
	r := dirStorage("copy", "copy")
	r.ReplicaOf = "store"
	_, err = e.p.AddStorage(e.ctx, r, at(b+480), e.audit(), nil)
	e.ok("AddStorage of the replica again", err)
	if n := e.verifiedCount("copy"); n != 0 {
		e.Errorf("a storage added again under a removed one's name answers %d scans read whole; want none", n)
	}
}

// testReplicaScanDamageKeepsBothFindings guards the replica's damage of a
// scan's own files, its manifest and its metadata file, which share the
// scan's damage row: each keeps its finding, and the copy that mends the
// scan clears both.
func testReplicaScanDamageKeepsBothFindings(e *renv) {
	e.replicaSetup()
	e.attach("copy", "store", 10)
	e.copyAll(20)
	e.createScrub("d1", "", at(1000))
	e.listScrub(at(1010))
	w, scan, _ := e.scrubScan(at(1020))
	fence := w.Fence()
	series := e.seriesOf(scan.Path)
	mk, err := layout.ManifestKey(scan.Path)
	e.ok("ManifestKey", err)
	sk, err := layout.MetadataKey(scan.Path)
	e.ok("MetadataKey", err)
	for i, f := range []tenant.ReplicaFinding{{Kind: tenant.FindingManifestDamaged, Key: mk, Path: scan.Path, ExpectedSHA256: sum(1)},
		{Kind: tenant.FindingDisagreeing, Key: sk, Path: scan.Path, ExpectedSHA256: sum(2)}} {
		f := f
		_, err := e.p.RecordReplicaDamage(e.ctx, tenant.ReplicaDamageRecord{Fence: &fence, Replica: "copy", SeriesUID: series,
			Damage: tenant.ReplicaDamage{Found: at(1030 + i)}, Finding: &f})
		e.ok("RecordReplicaDamage of the scan's "+string(f.Kind), err)
	}
	open := 0
	for _, f := range e.scrubFindingsOf(tenant.ScrubFindingFilter{Storage: "copy"}) {
		if f.Holds() {
			open++
		}
	}
	if open != 2 {
		e.Fatalf("%d findings hold on the replica; want the manifest's and the metadata file's", open)
	}
	e.copyAll(1100)
	for _, f := range e.scrubFindingsOf(tenant.ScrubFindingFilter{Storage: "copy"}) {
		if f.Holds() {
			e.Errorf("after the copy that mended the scan a finding holds: %s of %s", f.Kind, f.Key)
		}
	}
}

// testPurgeKeepsNoMetadataDigest guards the digest of a metadata file an
// instance delete's purge rewrites: the purge puts the new file on every
// copy before its end, so from its start the scan keeps no digest (a read
// turning to a replica, or a scrub, then checks nothing against the file
// replaced), a pass's record of the file records nothing while it runs, and
// so does a commit into the scan meanwhile; another scan keeps its digest.
func testPurgeKeepsNoMetadataDigest(e *renv) {
	e.replicaSetup()
	series, other := uidOf(2, 920), uidOf(2, 921)
	a := e.arrive(series, 20, 30)
	a.c.SidecarSHA256 = sum(500)
	e.ok("commit with a metadata digest", a.commit(31))
	b := e.arrive(series, 21, 40)
	b.c.SidecarSHA256 = sum(501)
	e.ok("second commit with a metadata digest", b.commit(41))
	o := e.arrive(other, 22, 42)
	o.c.SidecarSHA256 = sum(600)
	e.ok("commit of another scan with a metadata digest", o.commit(43))
	p := label.Path{Subject: "sub-01", Session: "ses-1", Scan: "scan-" + series}
	po := label.Path{Subject: "sub-01", Session: "ses-1", Scan: "scan-" + other}
	held, _, err := e.p.ListHeldInstances(e.ctx, p, master.Page{Limit: 10})
	e.ok("ListHeldInstances", err)
	if len(held) != 2 {
		e.Fatalf("the scan holds %d instances; want 2", len(held))
	}
	target := tenant.CorrectionTarget{Subject: p.Subject, Session: p.Session, Scan: p.Scan, Instance: held[0].SOPInstanceUID}
	pv, err := e.p.PreviewDelete(e.ctx, target, at(50))
	e.ok("PreviewDelete", err)
	id, err := tenant.NewCorrectionID(at(50))
	e.ok("NewCorrectionID", err)
	_, _, err = e.p.PrepareCorrection(e.ctx, tenant.CorrectionPrepare{ID: id, Kind: record.KindDelete, Target: target,
		Reason: "consent withdrawn", Confirm: pv.Confirm, Now: at(51)}, e.audit(), nil)
	e.ok("PrepareCorrection", err)
	_, err = e.p.CommitCorrection(e.ctx, tenant.CorrectionCommit{ID: id, Now: at(52), Grace: time.Hour})
	e.ok("CommitCorrection", err)
	if got := e.sidecarOfScan(p); got != sum(501) {
		e.Fatalf("after the delete's publish the digest reads %v; want the last commit's", got)
	}
	e.startPurge(id, 60)
	if got := e.sidecarOfScan(p); !got.IsZero() {
		e.Errorf("while the purge runs the digest reads %v; want none", got)
	}
	if got := e.sidecarOfScan(po); got != sum(600) {
		e.Errorf("the purge dropped another scan's digest: %v", got)
	}
	if recorded, err := e.p.RecordSidecarRewritten(e.ctx, series, "store", sum(502), at(61)); err != nil || recorded {
		e.Errorf("RecordSidecarRewritten while the purge runs = %v, %v; want nothing recorded", recorded, err)
	}
	c := e.arrive(series, 23, 70)
	c.c.SidecarSHA256 = sum(503)
	e.ok("commit into the scan while the purge runs", c.commit(71))
	if got := e.sidecarOfScan(p); !got.IsZero() {
		e.Errorf("after a commit while the purge runs the digest reads %v; want none", got)
	}
	// Started again after a stop, it drops a digest kept since once more.
	if recorded, err := e.p.RecordSidecarRewritten(e.ctx, other, "store", sum(601), at(72)); err != nil || !recorded {
		e.Errorf("RecordSidecarRewritten of a scan the purge does not touch = %v, %v; want it recorded", recorded, err)
	}
	e.startPurge(id, 73)
	if got := e.sidecarOfScan(po); got != sum(601) {
		e.Errorf("the purge started again dropped another scan's digest: %v", got)
	}
}
