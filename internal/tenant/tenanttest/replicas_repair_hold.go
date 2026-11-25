package tenanttest

import (
	"errors"
	"slices"
	"time"

	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/core/layout"
	"github.com/amrshadid/nopacs/internal/core/reception"
	"github.com/amrshadid/nopacs/internal/core/record"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// repairLease is the lease of the repair unit these tests claim.
const repairLease = 10 * time.Minute

// repairFixture archives two scans on "store", makes "copy" its replica in
// step, and records two findings of the first scan a scrub unit read
// (damaged instances 0 and 1); it returns the findings, the scan and its
// instances.
func (e *renv) repairFixture() (f0, f1 tenant.ScrubFinding, scan tenant.JobScan, ins []tenant.ScanInstance) {
	e.Helper()
	e.replicaSetup()
	e.attach("copy", "store", 10)
	e.copyAll(20)
	e.createScrub("sj-1", "store", at(200))
	e.listScrub(at(200))
	w, scan, ins := e.scrubScan(at(210))
	f0, _ = e.record(w, finding("sj-1", tenant.FindingDamaged, scan.Path, ins[0]), at(220))
	f1, _ = e.record(w, finding("sj-1", tenant.FindingDamaged, scan.Path, ins[1]), at(221))
	return f0, f1, scan, ins
}

// claimRepair makes the repair job id of "store" naming the findings, at
// second s, claims its unit with repairLease and starts it.
func (e *renv) claimRepair(id string, s int, findings ...string) tenant.Work {
	e.Helper()
	j, err := e.p.CreateJob(e.ctx, tenant.NewJob{ID: id, Kind: tenant.JobRepair, Created: at(s),
		Repair: &tenant.RepairJob{Storage: "store", Findings: findings}}, e.audit(), nil)
	e.ok("CreateJob of a repair", err)
	if j.Kind != tenant.JobRepair || j.State != tenant.JobQueued || j.Repair == nil || j.Repair.Storage != "store" ||
		!slices.Equal(j.Repair.Findings, findings) || j.Repair.All || j.Repair.Automatic {
		e.Errorf("the repair job made reads %+v (repair %+v)", j, j.Repair)
	}
	w, err := e.p.ClaimWork(e.ctx, tenant.RepairWorkKind, "repairer", at(s+5), repairLease)
	e.ok("ClaimWork of a repair", err)
	if w.Subject != id {
		e.Fatalf("the repair unit claimed is of %q, want %q", w.Subject, id)
	}
	u, err := e.p.StartJobUnit(e.ctx, w.Fence(), at(s+5))
	e.ok("StartJobUnit of a repair", err)
	if u.Job.State != tenant.JobRunning {
		e.Errorf("the repair job started reads %s", u.Job.State)
	}
	return w
}

// deleteInstance prepares a delete of the instance at second s; with
// commit, it publishes it too. It returns the first error.
func (e *renv) deleteInstance(p label.Path, uid string, s int, commit bool) error {
	e.Helper()
	target := tenant.CorrectionTarget{Subject: p.Subject, Session: p.Session, Scan: p.Scan, Instance: uid}
	pv, err := e.p.PreviewDelete(e.ctx, target, at(s))
	if err != nil {
		return err
	}
	id, err := tenant.NewCorrectionID(at(s))
	e.ok("NewCorrectionID", err)
	c, _, err := e.p.PrepareCorrection(e.ctx, tenant.CorrectionPrepare{ID: id, Kind: record.KindDelete, Target: target,
		Reason: "consent withdrawn", Confirm: pv.Confirm, Now: at(s)}, e.audit(), nil)
	if err != nil || !commit {
		return err
	}
	_, err = e.p.CommitCorrection(e.ctx, tenant.CorrectionCommit{ID: c.ID, Now: at(s + 1), Grace: time.Hour})
	return err
}

// arrival is a reception of one instance, claimed and identified, whose
// commit a test makes, and may make again as a retried step does.
type arrival struct {
	e *renv
	r tenant.Reception
	c tenant.ScanCommit
}

// commit commits the arrival's instance at second s and, when it lands,
// takes the reception to done.
func (a *arrival) commit(s int) error {
	e := a.e
	e.Helper()
	a.c.At = at(s)
	if _, err := e.p.CommitScan(e.ctx, a.c); err != nil {
		return err
	}
	r := a.r
	var err error
	for _, st := range []struct {
		from reception.State
		ev   reception.Event
	}{{reception.Identified, reception.Archive}, {reception.Archived, reception.Verify}, {reception.Verified, reception.Finish}} {
		r, err = e.p.StepReception(e.ctx, tenant.ReceptionStep{ID: r.ID, From: st.from, Event: st.ev, At: at(s + 1)}, nil)
		e.ok("StepReception "+string(st.ev), err)
	}
	return nil
}

// commitInto receives one new instance n into the scan of series of study
// 1 in the session "ses-1", as a reception does, at second s, and returns
// the arrival, whose commit may be made again, and its commit's error.
func (e *renv) commitInto(series string, n, s int) (*arrival, error) {
	e.Helper()
	a := e.arrive(series, n, s)
	return a, a.commit(s + 2)
}

// arrive receives one new instance n into the scan of series of study 1
// in the session "ses-1" at second s, identified and claimed.
func (e *renv) arrive(series string, n, s int) *arrival {
	e.Helper()
	r, _, err := e.p.OpenReception(e.ctx, "recv", uidOf(1, 1), e.id("rc"), at(s))
	e.ok("OpenReception", err)
	f := file(r, 1, n)
	f.SeriesUID = series
	_, err = e.p.AddReceivedFiles(e.ctx, []tenant.ReceivedFile{f}, at(s))
	e.ok("AddReceivedFiles", err)
	for _, st := range []struct {
		from reception.State
		ev   reception.Event
	}{{reception.Receiving, reception.Quiet}, {reception.Complete, reception.Identify}} {
		step := tenant.ReceptionStep{ID: r.ID, From: st.from, Event: st.ev, At: r.StepTime.Add(time.Second)}
		if st.ev == reception.Identify {
			step.SubjectLabel, step.SessionLabel, step.Storage = "sub-01", "ses-1", "store"
		}
		r, err = e.p.StepReception(e.ctx, step, nil)
		e.ok("StepReception "+string(st.ev), err)
	}
	_, err = e.p.ClaimInstances(e.ctx, r.ID, []tenant.InstanceClaim{{SOPInstanceUID: f.SOPInstanceUID, StudyUID: f.StudyUID,
		SeriesUID: series, SHA256: f.SHA256}}, r.StepTime, nil)
	e.ok("ClaimInstances", err)
	p := label.Path{Subject: "sub-01", Session: "ses-1", Scan: "scan-" + series}
	key, err := layout.InstanceKey(p, f.SOPInstanceUID)
	e.ok("InstanceKey", err)
	return &arrival{e: e, r: r, c: tenant.ScanCommit{ReceptionID: r.ID, StudyUID: r.StudyUID, SeriesUID: series, SubjectLabel: p.Subject,
		SessionLabel: p.Session, ScanLabel: p.Scan, Storage: "store", ManifestSHA256: sum(n),
		Instances: []tenant.ConfirmedInstance{{SOPInstanceUID: f.SOPInstanceUID, Key: key, Length: f.Size, Size: f.Size,
			ArchivedSHA256: f.SHA256}}}}
}

// wantRepairing fails unless err is the refusal of a change to a scan a
// repair holds: a precondition with tenant.ErrScanRepairing in its chain.
func (e *renv) wantRepairing(what string, err error) {
	e.Helper()
	if !errors.Is(err, tenant.ErrScanRepairing) || fault.KindOf(err) != fault.Precondition {
		e.Errorf("%s: %v; want refused as the scan being repaired", what, err)
	}
}

// testRepairHoldsScan guards a repair's hold of its scan. A record is
// refused when the catalog no longer names the bytes it expects. Recorded
// under its unit's fence, it holds the scan: a correction's prepare of the
// scan and a reception's commit to it are refused as retryable, while a
// change of another scan is not. The hold stands past the unit's due for
// the grace, and no more once that ran out too, so an abandoned repair
// never blocks its scan for longer; a record is then refused while a
// correction of the scan is under way, and the repair ended after the scan
// changed ends changed, its finding left holding, nothing counted, and the
// key it may have put recorded as an orphan.
func testRepairHoldsScan(e *renv) {
	f0, f1, scan, ins := e.repairFixture()
	cat, err := e.p.CatalogScan(e.ctx, scan.Path)
	e.ok("CatalogScan", err)
	w := e.claimRepair("rj-1", 225, f0.ID)
	fence := w.Fence()
	stale := tenant.Repair{ID: tenant.RepairID("rj-1", f0.ID), Finding: f0.ID, Storage: "store", From: "copy", Job: "rj-1",
		ExpectedSize: ins[0].Size, ExpectedSHA256: sum(77)}
	_, _, err = e.p.RecordRepair(e.ctx, tenant.RepairRecord{Fence: &fence, Repair: stale, Now: at(231)})
	e.wantPrecondition("RecordRepair expecting bytes the catalog does not name", err)
	rec := stale
	rec.ExpectedSHA256 = ins[0].ArchivedSHA256
	got, made, err := e.p.RecordRepair(e.ctx, tenant.RepairRecord{Fence: &fence, Repair: rec, Now: at(232)})
	e.ok("RecordRepair", err)
	if !made || got.SeriesUID != cat.SeriesUID || got.Done() {
		e.Errorf("the repair recorded reads %+v (made %v); want it of the scan %s, not ended", got, made, cat.SeriesUID)
	}
	e.wantRepairing("PrepareCorrection of the scan a repair holds", e.deleteInstance(scan.Path, ins[1].SOPInstanceUID, 240, false))
	held, err := e.commitInto(cat.SeriesUID, 500, 241)
	e.wantRepairing("CommitScan to the scan a repair holds", err)
	other := seriesB
	if cat.SeriesUID == seriesB {
		other = seriesA
	}
	_, err = e.commitInto(other, 501, 245)
	e.ok("CommitScan to another scan", err)
	// The unit's lease (claimed at 230 for ten minutes) ran out at 830, but
	// the hold stands a grace longer (to 950): a worker cut off from the
	// database may still be putting until its lease's due, and a storage may
	// land a put cut then a little later.
	if !got.HeldUntil.Equal(at(830).Add(tenant.RepairHoldGrace)) {
		e.Errorf("the repair holds until %v; want its unit's due plus the grace, %v", got.HeldUntil, at(830).Add(tenant.RepairHoldGrace))
	}
	e.wantRepairing("CommitScan past the lease's due, within the grace", held.commit(900))
	if held, err := e.p.ScanRepairHeld(e.ctx, cat.SeriesUID, at(949)); err != nil || !held {
		e.Errorf("ScanRepairHeld at the grace's last second = %v, %v; want held", held, err)
	}
	// Once the grace too has passed, the refused step, tried again, lands.
	e.ok("CommitScan tried again once the hold ran out", held.commit(951))
	e.ok("a delete of the instance once the hold ran out", e.deleteInstance(scan.Path, ins[0].SOPInstanceUID, 1000, true))
	second := tenant.Repair{ID: tenant.RepairID("rj-9", f1.ID), Finding: f1.ID, Storage: "store", From: "copy", Job: "rj-9",
		ExpectedSize: ins[1].Size, ExpectedSHA256: ins[1].ArchivedSHA256}
	_, _, err = e.p.RecordRepair(e.ctx, tenant.RepairRecord{Repair: second, Now: at(1010)})
	e.wantPrecondition("RecordRepair while a correction of the scan is under way", err)
	if errors.Is(err, tenant.ErrScanRepairing) {
		e.Error("a repair refused for a correction under way reads as the scan being repaired")
	}
	entry := e.serverAudit()
	ended, err := e.p.CompleteRepair(e.ctx, &fence, rec.ID, at(1020), entry)
	e.ok("CompleteRepair after the scan changed", err)
	if !ended.Done() || !ended.Changed || !ended.HeldUntil.IsZero() {
		e.Errorf("the repair ended after its instance was deleted reads %+v; want ended, changed, holding nothing", ended)
	}
	if !e.findingOf(f0.ID).Holds() {
		e.Error("a repair ended changed cleared its finding")
	}
	if !e.auditIDs()[entry.ID] {
		e.Error("a repair ended changed wrote no audit entry")
	}
	// What its put may have left at the key is no catalog row's now: it is
	// recorded as an orphan of the scan, which marks nothing.
	stray := e.findingOf(tenant.ScrubFindingID(rec.ID, "store", f0.Key, tenant.FindingOrphan))
	if stray.Kind != tenant.FindingOrphan || !stray.Holds() || stray.Key != f0.Key || stray.Path != scan.Path || stray.Job != "rj-1" ||
		stray.SOPInstanceUID != "" {
		e.Errorf("the key a repair ended changed may have put is recorded as %+v; want an orphan of the scan, holding", stray)
	}
	j, err := e.p.EndRepairJob(e.ctx, fence, at(1030))
	e.ok("EndRepairJob", err)
	if j.State != tenant.JobDone || j.Counts.Instances != 0 || j.Counts.InstancesSent != 0 {
		e.Errorf("the repair job ended reads %s with %+v; want done, nothing counted", j.State, j.Counts)
	}
	if w, err := e.p.Work(e.ctx, fence.ID); err != nil || w.State != tenant.WorkDone {
		e.Errorf("the repair job's unit reads %+v, %v; want done", w, err)
	}
}

// testRepairReleasedAndCanceled guards the ways a hold ends before it runs
// out, and a job canceled while it runs. ScanRepairHeld answers the hold
// for the scan held alone; ReleaseRepair under another fence is the lease
// lost and leaves the hold, and under the record's fence ends it, so a
// delete of the scan is taken at once; a record under another attempt's
// fence while the first attempt's hold stands is refused as the scan being
// repaired; and once the job is canceled, a record under its unit is
// refused with ErrRepairJobEnded, writing nothing.
func testRepairReleasedAndCanceled(e *renv) {
	f0, f1, scan, ins := e.repairFixture()
	cat, err := e.p.CatalogScan(e.ctx, scan.Path)
	e.ok("CatalogScan", err)
	w := e.claimRepair("rj-1", 225, f0.ID, f1.ID)
	fence := w.Fence()
	rec := tenant.Repair{ID: tenant.RepairID("rj-1", f0.ID), Finding: f0.ID, Storage: "store", From: "copy", Job: "rj-1",
		ExpectedSize: ins[0].Size, ExpectedSHA256: ins[0].ArchivedSHA256}
	_, _, err = e.p.RecordRepair(e.ctx, tenant.RepairRecord{Fence: &fence, Repair: rec, Now: at(232)})
	e.ok("RecordRepair", err)
	for series, want := range map[string]bool{cat.SeriesUID: true, "1.2.826.0.1.3680043.2.1125.99.1": false} {
		held, err := e.p.ScanRepairHeld(e.ctx, series, at(233))
		e.ok("ScanRepairHeld", err)
		if held != want {
			e.Errorf("ScanRepairHeld(%s) = %v; want %v", series, held, want)
		}
	}
	other := fence
	other.Attempt++
	if err := e.p.ReleaseRepair(e.ctx, other, rec.ID); !errors.Is(err, tenant.ErrLeaseLost) {
		e.Errorf("ReleaseRepair under another attempt's fence: %v; want the lease lost", err)
	}
	e.wantRepairing("PrepareCorrection while the hold stands", e.deleteInstance(scan.Path, ins[1].SOPInstanceUID, 234, false))
	e.ok("ReleaseRepair", e.p.ReleaseRepair(e.ctx, fence, rec.ID))
	if held, err := e.p.ScanRepairHeld(e.ctx, cat.SeriesUID, at(235)); err != nil || held {
		e.Errorf("ScanRepairHeld once the hold was let go = %v, %v; want false", held, err)
	}
	if r, err := e.p.Repair(e.ctx, rec.ID); err != nil || r.Done() || !r.HeldUntil.IsZero() {
		e.Errorf("the repair let go reads %+v, %v; want not ended, holding nothing", r, err)
	}
	target := tenant.CorrectionTarget{Subject: scan.Path.Subject, Session: scan.Path.Session, Scan: scan.Path.Scan, Instance: ins[1].SOPInstanceUID}
	pv, err := e.p.PreviewDelete(e.ctx, target, at(236))
	e.ok("PreviewDelete", err)
	id, err := tenant.NewCorrectionID(at(236))
	e.ok("NewCorrectionID", err)
	_, _, err = e.p.PrepareCorrection(e.ctx, tenant.CorrectionPrepare{ID: id, Kind: record.KindDelete, Target: target,
		Reason: "consent withdrawn", Confirm: pv.Confirm, Now: at(236)}, e.audit(), nil)
	e.ok("a delete of the scan prepared at once once the hold was let go", err)
	if _, err := e.p.AbandonCorrection(e.ctx, id, "abandoned for the test", at(237)); err != nil {
		e.Fatalf("AbandonCorrection: %v", err)
	}
	// Nothing else stands in the way of a record of the other finding but
	// the job's state: canceled, the record is refused as ended.
	_, err = e.p.CancelJob(e.ctx, "rj-1", at(240), e.audit(), nil)
	e.ok("CancelJob", err)
	rec1 := tenant.Repair{ID: tenant.RepairID("rj-1", f1.ID), Finding: f1.ID, Storage: "store", From: "copy", Job: "rj-1",
		ExpectedSize: ins[1].Size, ExpectedSHA256: ins[1].ArchivedSHA256}
	_, _, err = e.p.RecordRepair(e.ctx, tenant.RepairRecord{Fence: &fence, Repair: rec1, Now: at(241)})
	if !errors.Is(err, tenant.ErrRepairJobEnded) || fault.KindOf(err) != fault.Precondition {
		e.Errorf("RecordRepair under a canceled job's unit: %v; want refused as the job ended", err)
	}
	if rs, err := e.p.FindingRepairs(e.ctx, f1.ID); err != nil || len(rs) != 0 {
		e.Errorf("the refused record left %d repairs, %v", len(rs), err)
	}
}

// testRepairWaitsForEarlierAttempt guards a unit taken over while its
// first attempt held an object: the second attempt's record of that object
// is refused as the scan being repaired until the first attempt's hold ran
// out (its worker may still be putting it), and made after.
func testRepairWaitsForEarlierAttempt(e *renv) {
	f0, _, _, ins := e.repairFixture()
	w := e.claimRepair("rj-1", 225, f0.ID)
	first := w.Fence()
	rec := tenant.Repair{ID: tenant.RepairID("rj-1", f0.ID), Finding: f0.ID, Storage: "store", From: "copy", Job: "rj-1",
		ExpectedSize: ins[0].Size, ExpectedSHA256: ins[0].ArchivedSHA256}
	got, _, err := e.p.RecordRepair(e.ctx, tenant.RepairRecord{Fence: &first, Repair: rec, Now: at(232)})
	e.ok("RecordRepair", err)
	// The first lease's due is 830; another worker claims the unit at 840.
	w2, err := e.p.ClaimWork(e.ctx, tenant.RepairWorkKind, "repairer-2", at(840), repairLease)
	e.ok("ClaimWork of the unit taken over", err)
	second := w2.Fence()
	_, _, err = e.p.RecordRepair(e.ctx, tenant.RepairRecord{Fence: &second, Repair: rec, Now: at(841)})
	e.wantRepairing("RecordRepair while an earlier attempt's hold stands", err)
	after := int(got.HeldUntil.Sub(at(0)).Seconds()) + 1
	again, _, err := e.p.RecordRepair(e.ctx, tenant.RepairRecord{Fence: &second, Repair: rec, Now: at(after)})
	e.ok("RecordRepair once the earlier attempt's hold ran out", err)
	if !again.HeldUntil.Equal(w2.Due.Add(tenant.RepairHoldGrace)) {
		e.Errorf("the record taken over holds until %v; want the new lease's due plus the grace, %v", again.HeldUntil,
			w2.Due.Add(tenant.RepairHoldGrace))
	}
}

// testRepairHoldsCorrectionSteps guards the two sides of the exclusion: a
// repair's record is refused while a correction of its scan is prepared
// (as a precondition, not as the scan being repaired), and made once that
// correction ended; then a new correction of the scan is refused as
// retryable until the repair ended, its finding cleared, and taken after.
func testRepairHoldsCorrectionSteps(e *renv) {
	f0, _, scan, ins := e.repairFixture()
	target := tenant.CorrectionTarget{Subject: scan.Path.Subject, Session: scan.Path.Session, Scan: scan.Path.Scan, Instance: ins[1].SOPInstanceUID}
	pv, err := e.p.PreviewDelete(e.ctx, target, at(222))
	e.ok("PreviewDelete", err)
	id, err := tenant.NewCorrectionID(at(222))
	e.ok("NewCorrectionID", err)
	_, _, err = e.p.PrepareCorrection(e.ctx, tenant.CorrectionPrepare{ID: id, Kind: record.KindDelete, Target: target,
		Reason: "consent withdrawn", Confirm: pv.Confirm, Now: at(222)}, e.audit(), nil)
	e.ok("PrepareCorrection", err)
	w := e.claimRepair("rj-1", 225, f0.ID)
	fence := w.Fence()
	rec := tenant.Repair{ID: tenant.RepairID("rj-1", f0.ID), Finding: f0.ID, Storage: "store", From: "copy", Job: "rj-1",
		ExpectedSize: ins[0].Size, ExpectedSHA256: ins[0].ArchivedSHA256}
	_, _, err = e.p.RecordRepair(e.ctx, tenant.RepairRecord{Fence: &fence, Repair: rec, Now: at(232)})
	e.wantPrecondition("RecordRepair while a correction of the scan is prepared", err)
	if errors.Is(err, tenant.ErrScanRepairing) {
		e.Error("a repair refused for a correction prepared reads as the scan being repaired")
	}
	if _, err := e.p.AbandonCorrection(e.ctx, id, "abandoned for the test", at(233)); err != nil {
		e.Fatalf("AbandonCorrection: %v", err)
	}
	_, _, err = e.p.RecordRepair(e.ctx, tenant.RepairRecord{Fence: &fence, Repair: rec, Now: at(234)})
	e.ok("RecordRepair once the correction ended", err)
	pv, err = e.p.PreviewDelete(e.ctx, target, at(240))
	e.ok("PreviewDelete", err)
	e.wantRepairing("PrepareCorrection while the repair holds", func() error {
		id2, err := tenant.NewCorrectionID(at(240))
		e.ok("NewCorrectionID", err)
		_, _, err = e.p.PrepareCorrection(e.ctx, tenant.CorrectionPrepare{ID: id2, Kind: record.KindDelete, Target: target,
			Reason: "consent withdrawn", Confirm: pv.Confirm, Now: at(240)}, e.audit(), nil)
		return err
	}())
	_, err = e.p.CompleteRepair(e.ctx, &fence, rec.ID, at(250), e.serverAudit())
	e.ok("CompleteRepair", err)
	if e.findingOf(f0.ID).Holds() {
		e.Error("the repair ended did not clear its finding")
	}
	e.ok("a delete once the repair ended", e.deleteInstance(scan.Path, ins[1].SOPInstanceUID, 260, true))
}

// testRepairRefusedWhileSwitching guards a switch of the storage: neither a
// repair job nor a repair's record is made while one is accepted.
func testRepairRefusedWhileSwitching(e *renv) {
	f0, _, _, ins := e.repairFixture()
	_, _, err := e.p.BeginSwitch(e.ctx, tenant.SwitchStart{ID: "sw-1", Primary: "store", Replica: "copy", Token: "tok-1", Now: at(230)},
		e.audit(), nil)
	e.ok("BeginSwitch", err)
	_, err = e.p.CreateJob(e.ctx, tenant.NewJob{ID: "rj-1", Kind: tenant.JobRepair, Created: at(231),
		Repair: &tenant.RepairJob{Storage: "store", All: true}}, e.audit(), nil)
	e.wantPrecondition("CreateJob of a repair while the storage is switched", err)
	rec := tenant.Repair{ID: tenant.RepairID("rj-1", f0.ID), Finding: f0.ID, Storage: "store", From: "copy", Job: "rj-1",
		ExpectedSize: ins[0].Size, ExpectedSHA256: ins[0].ArchivedSHA256}
	_, _, err = e.p.RecordRepair(e.ctx, tenant.RepairRecord{Repair: rec, Now: at(232)})
	e.wantPrecondition("RecordRepair while the storage is switched", err)
}

// testRepairJobCounts guards a repair job's row: what it mends read back,
// the refusals of a job the tenant cannot run, each finding counted once
// whatever the number of writes, one counted no good copy and repaired
// later moved from refused to sent with its tallies, and the job ended
// with its unit.
func testRepairJobCounts(e *renv) {
	f0, _, _, ins := e.repairFixture()
	bad := []tenant.NewJob{
		{ID: "rj-x", Kind: tenant.JobRepair, Created: at(224), Repair: &tenant.RepairJob{Storage: "copy", All: true}},
		{ID: "rj-x", Kind: tenant.JobRepair, Created: at(224), Repair: &tenant.RepairJob{Storage: "store", From: "store", All: true}},
		{ID: "rj-x", Kind: tenant.JobRepair, Created: at(224), Repair: &tenant.RepairJob{Storage: "store", Findings: []string{"sf-none"}}},
	}
	for i, want := range []fault.Kind{fault.Precondition, fault.Precondition, fault.NotFound} {
		_, err := e.p.CreateJob(e.ctx, bad[i], e.audit(), nil)
		e.wantKind("CreateJob of a repair the tenant cannot run", err, want)
	}
	_, err := e.p.CreateJob(e.ctx, tenant.NewJob{ID: "rj-x", Kind: tenant.JobRepair, Created: at(224)}, e.audit(), nil)
	e.wantKind("CreateJob of a repair naming nothing", err, fault.Invalid)
	w := e.claimRepair("rj-1", 225, f0.ID)
	fence := w.Fence()
	_, err = e.p.CreateJob(e.ctx, tenant.NewJob{ID: "rj-2", Kind: tenant.JobRepair, Created: at(231),
		Repair: &tenant.RepairJob{Storage: "store", All: true}}, e.audit(), nil)
	e.wantPrecondition("a second repair of the storage while one runs", err)
	for range 2 {
		j, err := e.p.RecordRepairOutcome(e.ctx, tenant.RepairOutcome{Fence: fence, Finding: f0.ID, Kind: tenant.RepairNoGoodCopy, Now: at(232)})
		e.ok("RecordRepairOutcome", err)
		if j.Counts.Instances != 1 || j.Counts.InstancesRefused != 1 || j.Counts.InstancesSent != 0 {
			e.Errorf("the job after a no good copy reads %+v; want one refused, once", j.Counts)
		}
	}
	rec := tenant.Repair{ID: tenant.RepairID("rj-1", f0.ID), Finding: f0.ID, Storage: "store", From: "copy", Job: "rj-1",
		ExpectedSize: ins[0].Size, ExpectedSHA256: ins[0].ArchivedSHA256}
	_, _, err = e.p.RecordRepair(e.ctx, tenant.RepairRecord{Fence: &fence, Repair: rec, Now: at(233)})
	e.ok("RecordRepair", err)
	_, err = e.p.CompleteRepair(e.ctx, &fence, rec.ID, at(234), e.serverAudit())
	e.ok("CompleteRepair", err)
	j, err := e.p.EndRepairJob(e.ctx, fence, at(235))
	e.ok("EndRepairJob", err)
	c := j.Counts
	if j.State != tenant.JobDone || c.Instances != 1 || c.InstancesSent != 1 || c.InstancesRefused != 0 || c.BytesSent != ins[0].Size ||
		c.Bytes != ins[0].Size {
		e.Errorf("the job ended reads %s with %+v; want done, the one object repaired", j.State, c)
	}
	tallies, err := e.p.JobTallies(e.ctx, "rj-1")
	e.ok("JobTallies", err)
	byKey := map[string]int64{}
	for _, t := range tallies {
		byKey[t.Key] = t.Count
	}
	if byKey["repaired"] != 1 || byKey["no_good_copy"] != 0 {
		e.Errorf("the job's tallies are %+v; want repaired 1 and no good copy 0", tallies)
	}
	_, err = e.p.RecordRepairOutcome(e.ctx, tenant.RepairOutcome{Fence: fence, Finding: f0.ID, Kind: tenant.RepairNoGoodCopy, Now: at(236)})
	if !errors.Is(err, tenant.ErrLeaseLost) {
		e.Errorf("RecordRepairOutcome after the unit ended: %v; want the lease lost", err)
	}
	if _, err := e.p.CreateJob(e.ctx, tenant.NewJob{ID: "rj-3", Kind: tenant.JobRepair, Created: at(240),
		Repair: &tenant.RepairJob{Storage: "store", All: true, Automatic: false}}, e.audit(), nil); err != nil {
		e.Errorf("a repair of the storage once the last ended: %v", err)
	}
}
