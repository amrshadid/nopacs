package tenanttest

import (
	"errors"

	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// seriesOf returns the Series Instance UID of the scan at p.
func (e *renv) seriesOf(p label.Path) string {
	e.Helper()
	sc, err := e.p.CatalogScan(e.ctx, p)
	e.ok("CatalogScan", err)
	return sc.SeriesUID
}

// scrubFindingsOf lists the findings the filter keeps, cleared ones too.
func (e *renv) scrubFindingsOf(f tenant.ScrubFindingFilter) []tenant.ScrubFinding {
	e.Helper()
	f.IncludeCleared = true
	got, _, err := e.p.ListScrubFindings(e.ctx, f, master.Page{Limit: master.MaxPageLimit})
	e.ok("ListScrubFindings", err)
	return got
}

// testScrubOfAReplica guards the listing of a scrub job that names a
// replica: it lists its primary's scans, which the replica holds under the
// same keys, each unit a scan of the primary.
func testScrubOfAReplica(e *renv) {
	e.replicaSetup()
	e.attach("copy", "store", 10)
	e.copyAll(20)
	e.createScrub("r1", "copy", at(1000))
	j := e.listScrub(at(1010))
	if j.Counts.Scans != 2 || j.Counts.Instances != 4 {
		e.Errorf("a scrub of a replica listed %d scans of %d instances; want its primary's 2 of 4", j.Counts.Scans, j.Counts.Instances)
	}
	_, scan, ins := e.scrubScan(at(1020))
	if scan.Storage != "store" || len(ins) != 2 {
		e.Errorf("a scrub of a replica's scan unit reads %+v with %d instances", scan, len(ins))
	}
}

// testScrubOfOneScan guards a scrub job of one scan: made on a storage or
// on every one, it lists that scan alone; a target that is not one scan is
// refused, and so is a scan the tenant does not hold.
func testScrubOfOneScan(e *renv) {
	e.replicaSetup()
	scans, err := e.p.ListScans(e.ctx, tenant.ScanFilter{Order: tenant.OrderLabel}, master.Page{Limit: 10})
	e.ok("ListScans", err)
	sc := scans.Items[0]
	target := tenant.JobTarget{SubjectLabel: sc.SubjectLabel, SessionLabel: sc.SessionLabel, ScanLabel: sc.Label}
	mk := func(id string, t tenant.JobTarget) (tenant.Job, error) {
		return e.p.CreateJob(e.ctx, tenant.NewJob{ID: id, Kind: tenant.JobScrub, Storage: "store", Target: t, Scheduled: true,
			Created: at(1000)}, e.audit(), nil)
	}
	_, err = mk("bad1", tenant.JobTarget{SubjectLabel: sc.SubjectLabel, SessionLabel: sc.SessionLabel})
	e.wantKind("a scrub of a session", err, fault.Invalid)
	_, err = mk("bad2", tenant.JobTarget{SubjectLabel: sc.SubjectLabel, SessionLabel: sc.SessionLabel, ScanLabel: "scan-none"})
	e.wantKind("a scrub of a scan the tenant does not hold", err, fault.NotFound)
	j, err := mk("one", target)
	e.ok("CreateJob of a scrub of one scan", err)
	if j.Target != target || j.Storage != "store" {
		e.Errorf("a scrub of one scan reads %+v", j)
	}
	listed := e.listScrub(at(1010))
	if listed.Counts.Scans != 1 || listed.Counts.Instances != 2 {
		e.Errorf("a scrub of one scan listed %d scans of %d instances; want 1 of 2", listed.Counts.Scans, listed.Counts.Instances)
	}
	_, scan, _ := e.scrubScan(at(1020))
	if scan.Path != (label.Path{Subject: sc.SubjectLabel, Session: sc.SessionLabel, Scan: sc.Label}) {
		e.Errorf("the scan unit of a scrub of one scan reads %+v", scan.Path)
	}
}

// testScrubOfOneScanLeavesTheSchedule guards that a scrub of one scan,
// which a read that found bad bytes asks for, never stands for the
// storage's whole scrub: LastScrub answers the whole scrub made before it,
// ended or not, so the next scheduled scrub stays due from that one; a
// scrub of one scan queued does not refuse a whole scrub of its storage;
// and a scrub of one scan is still refused while a whole one is queued.
func testScrubOfOneScanLeavesTheSchedule(e *renv) {
	e.replicaSetup()
	e.createScrub("w1", "store", at(100))
	_, err := e.p.CancelJob(e.ctx, "w1", at(110), e.audit(), nil)
	e.ok("CancelJob of the whole scrub", err)
	scans, err := e.p.ListScans(e.ctx, tenant.ScanFilter{Order: tenant.OrderLabel}, master.Page{Limit: 10})
	e.ok("ListScans", err)
	sc := scans.Items[0]
	target := tenant.JobTarget{SubjectLabel: sc.SubjectLabel, SessionLabel: sc.SessionLabel, ScanLabel: sc.Label}
	mk := func(id string, s int) error {
		_, err := e.p.CreateJob(e.ctx, tenant.NewJob{ID: id, Kind: tenant.JobScrub, Storage: "store", Target: target, Scheduled: true,
			Created: at(s)}, e.audit(), nil)
		return err
	}
	e.ok("CreateJob of a scrub of one scan", mk("one", 200))
	_, err = e.p.CancelJob(e.ctx, "one", at(210), e.audit(), nil)
	e.ok("CancelJob of the scrub of one scan", err)
	last, has, err := e.p.LastScrub(e.ctx, "store")
	if e.ok("LastScrub", err); !has || last.ID != "w1" {
		e.Errorf("LastScrub after a scrub of one scan ended answers %q (%v); want the whole scrub w1", last.ID, has)
	}
	e.ok("CreateJob of a second scrub of one scan", mk("two", 300))
	e.createScrub("w2", "store", at(310))
	e.wantPrecondition("a scrub of one scan while a whole scrub is queued", mk("three", 320))
	if last, _, err := e.p.LastScrub(e.ctx, "store"); err != nil || last.ID != "w2" {
		e.Errorf("LastScrub with a whole scrub queued = %q, %v; want w2", last.ID, err)
	}
}

// testScrubVerifications guards what is recorded of each scan's reads:
// under a scrub unit's fence, by storage and series, a whole read and a
// read by presence each keep their own time; a page of a storage's scans
// answers each with its objects and when it was archived, a replica's
// pages its primary's scans; a fence lost records nothing; and the
// arguments are checked.
func testScrubVerifications(e *renv) {
	e.replicaSetup()
	e.attach("copy", "store", 10)
	e.createScrub("v1", "", at(1000))
	e.listScrub(at(1010))
	w, scan, _ := e.scrubScan(at(1020))
	e.ok("RecordScanVerified", e.p.RecordScanVerified(e.ctx, tenant.ScanVerified{Fence: w.Fence(), Storage: "store", Path: scan.Path,
		At: at(200)}))
	e.ok("RecordScanVerified by presence", e.p.RecordScanVerified(e.ctx, tenant.ScanVerified{Fence: w.Fence(), Storage: "copy",
		Path: scan.Path, ByPresence: true, At: at(201)}))
	e.ok("RecordScanVerified again", e.p.RecordScanVerified(e.ctx, tenant.ScanVerified{Fence: w.Fence(), Storage: "store",
		Path: scan.Path, At: at(202)}))
	sc, err := e.p.CatalogScan(e.ctx, scan.Path)
	e.ok("CatalogScan", err)
	for _, storage := range []string{"store", "copy"} {
		page, next, err := e.p.ScanVerifications(e.ctx, storage, "", 10)
		e.ok("ScanVerifications", err)
		if len(page) != 2 || next != "" {
			e.Fatalf("the verifications of %s are %+v, %q; want the 2 scans of the primary", storage, page, next)
		}
		read := 0
		for _, v := range page {
			if v.Objects != 2 || v.Held.IsZero() {
				e.Errorf("a verification of %s is %+v; want 2 objects and when they were archived", storage, v)
			}
			if v.Verified.IsZero() && v.Presence.IsZero() {
				continue
			}
			read++
			if !v.Held.Equal(sc.Created) {
				e.Errorf("the scan read is answered held at %v; want %v", v.Held, sc.Created)
			}
			switch storage {
			case "store":
				if !v.Verified.Equal(at(202)) || !v.Presence.IsZero() {
					e.Errorf("the scan read whole on the primary is answered %+v; want verified at its last read", v)
				}
			case "copy":
				if !v.Verified.IsZero() || !v.Presence.Equal(at(201)) {
					e.Errorf("the scan checked by presence on the replica is answered %+v", v)
				}
			}
		}
		if read != 1 {
			e.Errorf("%d scans of %s are answered read; want 1", read, storage)
		}
	}
	first, next, err := e.p.ScanVerifications(e.ctx, "store", "", 1)
	e.ok("ScanVerifications of one", err)
	rest, end, err := e.p.ScanVerifications(e.ctx, "store", next, 1)
	e.ok("ScanVerifications after", err)
	if len(first) != 1 || next == "" || len(rest) != 1 || end == "" {
		e.Errorf("pages of one answered %d with %q, then %d with %q", len(first), next, len(rest), end)
	}
	if last, _, err := e.p.ScanVerifications(e.ctx, "store", end, 1); err != nil || len(last) != 0 {
		e.Errorf("the page past the last = %+v, %v; want none", last, err)
	}
	lost := w.Fence()
	lost.Attempt++
	err = e.p.RecordScanVerified(e.ctx, tenant.ScanVerified{Fence: lost, Storage: "store", Path: scan.Path, At: at(300)})
	if !errors.Is(err, tenant.ErrLeaseLost) {
		e.Errorf("RecordScanVerified under a lost fence = %v; want the lease lost", err)
	}
	gone := scan.Path
	gone.Scan = "scan-gone"
	e.ok("RecordScanVerified of a scan gone", e.p.RecordScanVerified(e.ctx, tenant.ScanVerified{Fence: w.Fence(), Storage: "store",
		Path: gone, At: at(301)}))
	_, _, err = e.p.ScanVerifications(e.ctx, "nowhere", "", 10)
	e.wantKind("ScanVerifications of a storage the tenant does not have", err, fault.NotFound)
	_, _, err = e.p.ScanVerifications(e.ctx, "store", "", 0)
	e.wantKind("ScanVerifications of no scan", err, fault.Invalid)
	_, _, err = e.p.ScanVerifications(e.ctx, "store", "x", 10)
	e.wantKind("ScanVerifications after a cursor no page answered", err, fault.Invalid)
}

// testReplicaDamageFindings guards a replica's damage a scrub records with
// its finding: the finding is on the replica's name, names its primary,
// marks no instance, is listed by the scan's labels and answered by
// ReplicaDamaged; the scrub's clear of the damage clears it by its job; the
// copy that mends damage clears it by no job; a detach clears what is left;
// a finding without a scrub unit's fence is refused.
func testReplicaDamageFindings(e *renv) {
	e.replicaSetup()
	e.attach("copy", "store", 10)
	e.copyAll(20)
	e.createScrub("d1", "", at(1000))
	e.listScrub(at(1010))
	w, scan, ins := e.scrubScan(at(1020))
	fence := w.Fence()
	series := e.seriesOf(scan.Path)
	in := ins[0]
	found := in.Size + 1
	rec := func(s int) tenant.ReplicaDamageRecord {
		size := in.Size
		return tenant.ReplicaDamageRecord{Fence: &fence, Replica: "copy", SeriesUID: series,
			Damage:  tenant.ReplicaDamage{SOPInstanceUID: in.SOPInstanceUID, Revision: 1, FoundSize: &found, FoundSHA256: sum(77), Found: at(s)},
			Finding: &tenant.ReplicaFinding{Kind: tenant.FindingDamaged, Key: in.Key, Path: scan.Path, ExpectedSize: &size, ExpectedSHA256: in.ArchivedSHA256}}
	}
	made, err := e.p.RecordReplicaDamage(e.ctx, rec(130))
	e.ok("RecordReplicaDamage with its finding", err)
	if !made {
		e.Error("the damage was not made")
	}
	all := e.scrubFindingsOf(tenant.ScrubFindingFilter{Storage: "copy"})
	if len(all) != 1 {
		e.Fatalf("the findings on the replica are %+v; want one", all)
	}
	f := all[0]
	if f.Kind != tenant.FindingDamaged || f.ReplicaOf != "store" || f.Key != in.Key || f.Path != scan.Path || f.Job != "d1" ||
		f.SOPInstanceUID != in.SOPInstanceUID || *f.FoundSize != found || f.FoundSHA256 != sum(77) || !f.Holds() {
		e.Errorf("the finding of the replica's damage is %+v", f)
	}
	if got := e.marked(scan.Path, ins); got[0] {
		e.Error("a finding on a replica marks its instance")
	}
	if marks, err := e.p.DamagedInstances(e.ctx, []string{in.SOPInstanceUID}); err != nil || len(marks) != 0 {
		e.Errorf("DamagedInstances = %v, %v; want no mark", marks, err)
	}
	if got := e.scrubFindingsOf(tenant.ScrubFindingFilter{Path: scan.Path}); len(got) != 1 {
		e.Errorf("the findings of the scan's labels are %+v; want the one", got)
	}
	other := scan.Path
	other.Scan = "scan-other"
	if got := e.scrubFindingsOf(tenant.ScrubFindingFilter{Storage: "copy", Path: other}); len(got) != 0 {
		e.Errorf("the findings of other labels are %+v; want none", got)
	}
	_, _, err = e.p.ListScrubFindings(e.ctx, tenant.ScrubFindingFilter{Path: label.Path{Subject: "sub-01"}}, firstPage(10))
	e.wantKind("a filter of labels that name no scan", err, fault.Invalid)
	damaged, err := e.p.ReplicaDamaged(e.ctx, "copy", series)
	e.ok("ReplicaDamaged", err)
	if len(damaged) != 1 || damaged[in.SOPInstanceUID] != f.ID {
		e.Errorf("ReplicaDamaged = %v; want the instance with its finding %s", damaged, f.ID)
	}
	cleared, err := e.p.ClearReplicaDamage(e.ctx, &fence, "copy", series, in.SOPInstanceUID, at(140))
	e.ok("ClearReplicaDamage", err)
	if all := e.scrubFindingsOf(tenant.ScrubFindingFilter{Storage: "copy"}); !cleared || len(all) != 1 || all[0].Holds() ||
		all[0].ClearJob != "d1" || !all[0].Cleared.Equal(at(140)) {
		e.Errorf("after the scrub's clear (%v) the findings are %+v; want the one cleared by d1", cleared, all)
	}
	// Found again, then mended by a copy.
	_, err = e.p.RecordReplicaDamage(e.ctx, rec(150))
	e.ok("RecordReplicaDamage again", err)
	e.copyAll(160)
	if open := e.scrubFindingsOf(tenant.ScrubFindingFilter{Storage: "copy"}); len(open) != 2 || open[0].Holds() || open[1].Holds() {
		e.Errorf("after the copy the findings are %+v; want both cleared", open)
	} else {
		byCopy := 0
		for _, g := range open {
			if g.ClearJob == "" && g.Cleared.Equal(at(161)) {
				byCopy++
			}
		}
		if byCopy != 1 {
			e.Errorf("after the copy the findings are %+v; want one cleared by no job at the copy's end", open)
		}
	}
	if damaged, err := e.p.ReplicaDamaged(e.ctx, "copy", series); err != nil || len(damaged) != 0 {
		e.Errorf("ReplicaDamaged after the copy = %v, %v; want none", damaged, err)
	}
	// Found once more, then the replica detached.
	_, err = e.p.RecordReplicaDamage(e.ctx, rec(170))
	e.ok("RecordReplicaDamage once more", err)
	_, _, err = e.p.DetachReplica(e.ctx, tenant.ReplicaDetach{Replica: "copy", Now: at(180)}, e.audit(), nil)
	e.ok("DetachReplica", err)
	for _, g := range e.scrubFindingsOf(tenant.ScrubFindingFilter{Storage: "copy"}) {
		if g.Holds() {
			e.Errorf("after the detach a finding of the replica's damage holds: %+v", g)
		}
	}
	bad := rec(190)
	bad.Fence = nil
	_, err = e.p.RecordReplicaDamage(e.ctx, bad)
	e.wantKind("a replica's finding without a scrub unit's fence", err, fault.Invalid)
}

// testSidecarDigest guards the digest of a scan's metadata file: a commit
// keeps it with its manifest's, and CatalogScan and a replica's claim
// answer it; a commit of a new manifest without one keeps none; a metadata
// file rewritten without a new manifest records its digest and queues the
// scan's replicas again, but not for a scan on another storage or one the
// catalog does not hold.
func testSidecarDigest(e *renv) {
	e.replicaSetup()
	series := uidOf(2, 900)
	a := e.arrive(series, 20, 30)
	a.c.SidecarSHA256 = sum(500)
	e.ok("commit with a metadata digest", a.commit(31))
	p := label.Path{Subject: "sub-01", Session: "ses-1", Scan: "scan-" + series}
	sc, err := e.p.CatalogScan(e.ctx, p)
	e.ok("CatalogScan", err)
	if sc.SidecarSHA256 != sum(500) || sc.ManifestSHA256 != sum(20) {
		e.Errorf("the scan committed with a metadata digest reads %v beside manifest %v", sc.SidecarSHA256, sc.ManifestSHA256)
	}
	e.attach("copy", "store", 40)
	claims := e.claimAll(50)
	seen := false
	for _, c := range claims {
		if c.Fence.SeriesUID == series {
			seen = true
			if c.SidecarSHA256 != sum(500) {
				e.Errorf("the claim of the scan carries metadata digest %v; want the catalog's", c.SidecarSHA256)
			}
		} else if !c.SidecarSHA256.IsZero() {
			e.Errorf("the claim of a scan with no metadata digest carries %v", c.SidecarSHA256)
		}
		if _, err := e.p.EndReplicaScan(e.ctx, c.Fence, c.ManifestSHA256, at(51)); err != nil {
			e.Fatal(err)
		}
	}
	if !seen {
		e.Fatalf("the claims %+v hold no row of the scan", claims)
	}
	recorded, err := e.p.RecordSidecarRewritten(e.ctx, series, "store", sum(501), at(60))
	e.ok("RecordSidecarRewritten", err)
	if sc, _ := e.p.CatalogScan(e.ctx, p); !recorded || sc.SidecarSHA256 != sum(501) {
		e.Errorf("after the rewrite (%v) the scan reads metadata digest %v; want the new one", recorded, sc.SidecarSHA256)
	}
	if st := e.rowState("copy", series); st != tenant.ReplicaPending {
		e.Errorf("after the rewrite the replica's row of the scan is %s; want pending", st)
	}
	if st := e.rowState("copy", seriesA); st != tenant.ReplicaInStep {
		e.Errorf("the rewrite of one scan's metadata moved another's row to %s", st)
	}
	if recorded, err := e.p.RecordSidecarRewritten(e.ctx, series, "copy", sum(502), at(61)); err != nil || recorded {
		e.Errorf("RecordSidecarRewritten on another storage = %v, %v; want nothing recorded", recorded, err)
	}
	if recorded, err := e.p.RecordSidecarRewritten(e.ctx, uidOf(2, 999), "store", sum(502), at(61)); err != nil || recorded {
		e.Errorf("RecordSidecarRewritten of a series the catalog does not hold = %v, %v; want nothing recorded", recorded, err)
	}
	_, err = e.p.RecordSidecarRewritten(e.ctx, series, "store", tenant.SHA256{}, at(62))
	e.wantKind("RecordSidecarRewritten with no digest", err, fault.Invalid)
	// A later commit of the scan with a new manifest and no metadata digest.
	b := e.arrive(series, 21, 70)
	e.ok("commit without a metadata digest", b.commit(71))
	if sc, _ := e.p.CatalogScan(e.ctx, p); !sc.SidecarSHA256.IsZero() || sc.ManifestSHA256 != sum(21) {
		e.Errorf("a commit of a new manifest without a metadata digest left %v beside %v; want none", sc.SidecarSHA256, sc.ManifestSHA256)
	}
}

// testRebuildFromStorage guards a rebuild compare that reads one named
// storage: it is kept with the job and read back, a replace that names one
// is refused, and so is a storage the tenant does not have.
func testRebuildFromStorage(e *renv) {
	e.replicaSetup()
	e.attach("copy", "store", 10)
	mk := func(id string, mode tenant.RebuildMode, from string) (tenant.Job, error) {
		return e.p.CreateJob(e.ctx, tenant.NewJob{ID: id, Kind: tenant.JobRebuild, Rebuild: &tenant.RebuildJob{Mode: mode,
			Scratch: e.scratch, FromStorage: from}, Created: at(100)}, e.audit(), nil)
	}
	_, err := mk("bad1", tenant.RebuildReplace, "copy")
	e.wantKind("a replace from one storage", err, fault.Invalid)
	_, err = mk("bad2", tenant.RebuildCompare, "nowhere")
	e.wantPrecondition("a compare from a storage the tenant does not have", err)
	j, err := mk("c1", tenant.RebuildCompare, "copy")
	e.ok("CreateJob of a compare from a replica", err)
	if j.Rebuild == nil || j.Rebuild.FromStorage != "copy" {
		e.Errorf("the compare made reads %+v", j.Rebuild)
	}
	got, err := e.p.Job(e.ctx, "c1")
	e.ok("Job", err)
	if got.Rebuild == nil || got.Rebuild.FromStorage != "copy" || got.Rebuild.Mode != tenant.RebuildCompare {
		e.Errorf("the compare read back is %+v", got.Rebuild)
	}
}
