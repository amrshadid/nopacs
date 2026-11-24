package tenanttest

import (
	"slices"
	"time"

	"github.com/amrshadid/nopacs/internal/core/reception"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// listingOnly makes a job of the path and starts its listing unit, which
// then hands files in itself (files above the layout's unit depth).
func (e *ienv) listingOnly(id, path string) tenant.Work {
	e.Helper()
	e.newImport(id, path)
	w := e.claimImport("w", at(1))
	e.start(w, at(1))
	return w
}

// testImportAlreadyHeld guards the outcome of files the tenant holds: one
// instance in two folders is two files, the first in path order archived
// and the other already held, though the reception keeps one received
// file; a file of an instance archived before is already held, decided
// when its reception ends done; the job then ends done.
func testImportAlreadyHeld(e *ienv) {
	e.setupImport()
	r0, before := e.identified(importReceiver, 2, 7)
	e.commit(r0, before...)
	e.stepTo(r0, reception.Done, "store")

	w := e.listingOnly("j", "dup")
	r := e.openReception(importReceiver, 2, at(10))
	a, b := importFile(r, 2, 5, w, "dup/a/x.dcm"), importFile(r, 2, 5, w, "dup/b/x.dcm")
	b.Incoming += ".copy"
	c := importFile(r, 2, 7, w, "dup/c.dcm")
	e.handIn(a, b, c)
	e.endWalk(w, at(13))
	e.wantImport("three handed in", e.reconciled("three handed in", "j"), 3, 3, 0, 0, 3)
	files, _, err := e.p.ReceivedFiles(e.ctx, r.ID, master.Page{Limit: 10})
	e.ok("ReceivedFiles", err)
	if len(files) != 2 {
		e.Errorf("the reception holds %d received files, want 2: one per UID and checksum", len(files))
	}
	r = e.stepTo(r, reception.Identified, "store")
	e.commit(r, a, c)
	e.wantImport("the instance archived", e.reconciled("the instance archived", "j"), 3, 3, 1, 1, 1)
	e.stepTo(r, reception.Done, "store")
	j := e.reconciled("the reception done", "j")
	e.wantImport("the reception done", j, 3, 3, 1, 2, 0)
	if j.State != tenant.JobDone {
		e.Errorf("the job is %s, want done", j.State)
	}
	if got := e.files("j", tenant.ImportFileFilter{}); len(got) != 0 {
		e.Errorf("a job done lists %q, want none", paths(got))
	}
}

// testImportFailedRetried guards a reception that fails with an import's
// files: they count failed and are listed with their reception, the job
// ends partial; RetryReception moves them back to processing into the new
// reception, so they are listed no more; archived there, they count
// archived after the job ended, which keeps its state.
func testImportFailedRetried(e *ienv) {
	e.setupImport()
	w := e.listingOnly("j", "p")
	r := e.openReception(importReceiver, 4, at(10))
	f1, f2 := importFile(r, 4, 1, w, "p/1.dcm"), importFile(r, 4, 2, w, "p/2.dcm")
	e.handIn(f1, f2)
	e.endWalk(w, at(13))
	r = e.failReception(e.stepTo(r, reception.Complete, "store"))
	j := e.reconciled("the reception failed", "j")
	if c := j.Import.Counts; c.Failed != 2 || c.Processing != 0 || j.State != tenant.JobPartial {
		e.Errorf("after the failure: %s %+v, want partial with 2 failed", j.State, c)
	}
	listed := e.files("j", tenant.ImportFileFilter{})
	if got := paths(listed); !slices.Equal(got, []string{"p/1.dcm failed", "p/2.dcm failed"}) || listed[0].Reception != r.ID {
		e.Errorf("the files listed are %q (%+v), want both failed with their reception", got, listed)
	}
	_, retry, err := e.p.RetryReception(e.ctx, r.ID, "rc-retry", "", at(100), e.audit(), nil)
	e.ok("RetryReception", err)
	j = e.reconciled("the reception retried", "j")
	if c := j.Import.Counts; c.Failed != 0 || c.Processing != 2 || j.State != tenant.JobPartial {
		e.Errorf("after the retry: %s %+v, want still partial with 2 processing", j.State, c)
	}
	if got := e.files("j", tenant.ImportFileFilter{}); len(got) != 0 {
		e.Errorf("after the retry the files listed are %q, want none", paths(got))
	}
	retry = e.stepTo(retry, reception.Identified, "store")
	moved := []tenant.ReceivedFile{f1, f2}
	for i := range moved {
		moved[i].ReceptionID = retry.ID
	}
	e.commit(retry, moved...)
	e.stepTo(retry, reception.Done, "store")
	j = e.reconciled("archived after the job ended", "j")
	e.wantImport("archived after the job ended", j, 2, 2, 2, 0, 0)
	if j.State != tenant.JobPartial {
		e.Errorf("the job is %s, want it to keep partial", j.State)
	}
}

// testImportDiscardedAndLost guards the outcomes discarded (a failed
// reception's files discarded by a person) and lost (a file's copy gone
// from the incoming area), each listed with its kind.
func testImportDiscardedAndLost(e *ienv) {
	e.setupImport()
	w := e.listingOnly("j", "p")
	r := e.openReception(importReceiver, 5, at(10))
	f1 := importFile(r, 5, 1, w, "p/1.dcm")
	r6 := e.openReception(importReceiver, 6, at(10))
	f2 := importFile(r6, 6, 2, w, "p/2.dcm")
	e.handIn(f1, f2)
	e.endWalk(w, at(13))
	r = e.failReception(e.stepTo(r, reception.Complete, "store"))
	_, err := e.p.DiscardReception(e.ctx, r.ID, "the study is a test", at(100), e.audit(), nil)
	e.ok("DiscardReception", err)
	e.reconciled("discarded", "j")
	e.hold(held("lost1", f2, tenant.QuarantineIncomingLost, nil))
	j := e.reconciled("lost", "j")
	if c := j.Import.Counts; c.Discarded != 1 || c.Lost != 1 || c.Failed != 0 || c.Processing != 0 || c.QuarantinedTotal() != 0 ||
		j.State != tenant.JobPartial {
		e.Errorf("the job is %s with %+v, want partial with one discarded and one lost", j.State, c)
	}
	if got := paths(e.files("j", tenant.ImportFileFilter{})); !slices.Equal(got, []string{"p/1.dcm discarded", "p/2.dcm lost"}) {
		e.Errorf("the files listed are %q, want one discarded and one lost", got)
	}
}

// testImportCrashResumes guards a unit whose process died: its lease
// reclaimed, a write of the old lease is ErrLeaseLost and records nothing,
// its batch included; the next attempt resumes after the cursor, a resend
// of the cursor's file changes nothing, and no file is handed in twice.
func testImportCrashResumes(e *ienv) {
	e.setupImport()
	w := e.listingOnly("j", "p")
	r := e.openReception(importReceiver, 7, at(10))
	e.handIn(importFile(r, 7, 1, w, "p/1.dcm"), importFile(r, 7, 2, w, "p/2.dcm"))
	n, err := e.p.ReclaimWorkOfKinds(e.ctx, []string{tenant.ImportWorkKind}, at(20))
	e.ok("ReclaimWorkOfKinds", err)
	if n != 1 {
		e.Errorf("ReclaimWorkOfKinds = %d, want 1", n)
	}
	plain := file(r, 7, 9)
	_, err = e.p.AddReceivedFiles(e.ctx, []tenant.ReceivedFile{plain, importFile(r, 7, 3, w, "p/3.dcm")}, at(21))
	e.wantIs("a hand-off of a lost lease", err, tenant.ErrLeaseLost, fault.Conflict)
	_, err = e.p.RecordImportSkip(e.ctx, tenant.ImportSkip{Fence: w.Fence(), Path: "p/4.txt", Code: tenant.SkipNotPart10, Now: at(21)})
	e.wantIs("a skip of a lost lease", err, tenant.ErrLeaseLost, fault.Conflict)
	e.wantImport("after the crash", e.reconciled("after the crash", "j"), 2, 2, 0, 0, 2)
	files, _, err := e.p.ReceivedFiles(e.ctx, r.ID, master.Page{Limit: 10})
	e.ok("ReceivedFiles", err)
	if len(files) != 2 {
		e.Errorf("the reception holds %d files, want 2: a batch refused records nothing", len(files))
	}
	w2 := e.claimImport("w", at(30))
	u := e.start(w2, at(30))
	if u.Cursor != "p/2.dcm" || u.Attempts != 2 || u.State != tenant.ImportUnitWalking {
		e.Errorf("the unit resumed as %+v, want walking at its cursor p/2.dcm, attempt 2", u)
	}
	e.handIn(importFile(r, 7, 2, w2, "p/2.dcm"))
	e.wantImport("a resend at the cursor", e.reconciled("a resend at the cursor", "j"), 2, 2, 0, 0, 2)
	e.handIn(importFile(r, 7, 3, w2, "p/3.dcm"))
	e.endWalk(w2, at(31))
	e.wantImport("resumed", e.reconciled("resumed", "j"), 3, 3, 0, 0, 3)
}

// testImportNameHeld guards the name of a removed receiver an import job
// names: the job, ended, still names it, so AddReceiver of that name is a
// Conflict, as a name a reception holds is, and another name is taken.
func testImportNameHeld(e *ienv) {
	e.setupImport()
	e.newImport("j", "p")
	_, err := e.p.CancelJob(e.ctx, "j", at(11), e.audit(), nil)
	e.ok("CancelJob", err)
	_, err = e.p.RemoveReceiver(e.ctx, importReceiver, e.audit(), nil)
	e.ok("RemoveReceiver", err)
	if _, err := e.p.AddReceiver(e.ctx, receiverOf(importReceiver, "store", 11112), at(12), e.audit(), nil); fault.KindOf(err) != fault.Conflict {
		e.Errorf("AddReceiver of a name an ended import job names: %v, want a Conflict", err)
	}
	if _, err := e.p.AddReceiver(e.ctx, receiverOf("imp2", "store", 11112), at(12), e.audit(), nil); err != nil {
		e.Errorf("AddReceiver of another name: %v", err)
	}
}

// testImportCanceled guards CancelJob on an import: the job ends canceled
// with its queued unit, whose walk ends canceled; a walking unit's next
// write answers the canceled job and its walk ends canceled; a file handed
// in before the cancel is decided after it and counted, the job keeping
// its state; a second cancel changes nothing.
func testImportCanceled(e *ienv) {
	e.setupImport()
	w := e.listingOnly("j", "p")
	r := e.openReception(importReceiver, 8, at(10))
	f := importFile(r, 8, 1, w, "p/a.dcm")
	e.handIn(f)
	_, err := e.p.AddImportUnits(e.ctx, w.Fence(), []string{"p/s1", "p/s2"}, at(11))
	e.ok("AddImportUnits", err)
	e.endWalk(w, at(12))
	walking := e.claimImport("w", at(13))
	e.start(walking, at(13))
	j, err := e.p.CancelJob(e.ctx, "j", at(14), e.audit(), nil)
	e.ok("CancelJob", err)
	if j.State != tenant.JobCanceled || j.Import.Counts.UnitsWalked != 2 {
		e.Errorf("the job canceled is %s with %d walks ended, want canceled with the listing's and the queued unit's", j.State, j.Import.Counts.UnitsWalked)
	}
	if _, err := e.p.ClaimWork(e.ctx, tenant.ImportWorkKind, "w", at(15), time.Minute); fault.KindOf(err) != fault.NotFound {
		e.Errorf("a unit of a canceled job was claimed: %v", err)
	}
	j = e.skip(walking, "p/s1/x", tenant.SkipNotPart10, "")
	if j.State != tenant.JobCanceled {
		e.Errorf("a walking unit's write answered %s, want canceled", j.State)
	}
	e.endWalk(walking, at(16))
	r = e.stepTo(r, reception.Identified, "store")
	e.commit(r, f)
	j = e.reconciled("decided after the cancel", "j")
	e.wantImport("decided after the cancel", j, 2, 1, 1, 0, 0)
	if c := j.Import.Counts; j.State != tenant.JobCanceled || c.UnitsWalked != c.Units {
		e.Errorf("the job is %s with %+v, want canceled with every walk ended", j.State, c)
	}
	again, err := e.p.CancelJob(e.ctx, "j", at(20), e.audit(), nil)
	e.ok("CancelJob again", err)
	if again.State != tenant.JobCanceled || again.Ended != j.Ended {
		e.Errorf("a second cancel answered %+v, want the job as it stands", again)
	}
	if got := steps(e.jobEvents(""), "j"); !slices.Equal(got, []tenant.JobState{tenant.JobQueued, tenant.JobRunning, tenant.JobCanceled}) {
		e.Errorf("the job's events reached %v, want queued, running, canceled", got)
	}
}

// testImportUnitFails guards FailImportUnit: the unit fails with "root
// unavailable" and, every walk ended, the job ends failed at once with its
// files still processing, which are counted when decided; any other
// reason is Invalid.
func testImportUnitFails(e *ienv) {
	e.setupImport()
	w := e.listingOnly("j", "p")
	r := e.openReception(importReceiver, 9, at(10))
	f := importFile(r, 9, 1, w, "p/a.dcm")
	e.handIn(f)
	_, err := e.p.AddImportUnits(e.ctx, w.Fence(), []string{"p/s1"}, at(11))
	e.ok("AddImportUnits", err)
	e.endWalk(w, at(12))
	u := e.claimImport("w", at(13))
	e.start(u, at(13))
	_, err = e.p.FailImportUnit(e.ctx, u.Fence(), "disk on fire", at(14))
	e.wantKind("a reason not of the fixed phrases", err, fault.Invalid)
	j, err := e.p.FailImportUnit(e.ctx, u.Fence(), tenant.ImportReasonRootUnavailable, at(14))
	e.ok("FailImportUnit", err)
	if j.State != tenant.JobFailed || j.Reason != tenant.ImportReasonRootUnavailable || j.Import.Counts.Processing != 1 {
		e.Errorf("the job is %s %q with %+v, want failed, root unavailable, a file still processing", j.State, j.Reason, j.Import.Counts)
	}
	r = e.stepTo(r, reception.Identified, "store")
	e.commit(r, f)
	e.wantImport("decided after the failure", e.reconciled("decided after the failure", "j"), 1, 1, 1, 0, 0)
	units := allPages(e.penv, 10, func(p master.Page) ([]tenant.ImportUnit, string, error) {
		return e.p.ListImportUnits(e.ctx, "j", tenant.ImportUnitFilter{States: []tenant.ImportUnitState{tenant.ImportUnitFailed}}, p)
	})
	if len(units) != 1 || units[0].Reason != tenant.ImportReasonRootUnavailable || units[0].WalkEnded.IsZero() {
		e.Errorf("the failed units are %+v, want the one, with its reason and when its walk ended", units)
	}
}

// testImportUnitGivesUp guards a unit whose attempts run out: held as
// failed by the queue, its walk ends failed in that transaction, and the
// job ends failed with "attempts ran out".
func testImportUnitGivesUp(e *ienv) {
	e.setupImport()
	e.newImport("j", "p")
	now := at(1)
	for range tenant.JobUnitAttempts {
		w, err := e.p.ClaimWork(e.ctx, tenant.ImportWorkKind, "w", now, time.Minute)
		e.ok("ClaimWork", err)
		got, err := e.p.FailWork(e.ctx, w.Fence(), "the walk failed", now)
		e.ok("FailWork", err)
		now = got.Due.Add(time.Second)
	}
	j := e.reconciled("given up", "j")
	if j.State != tenant.JobFailed || j.Reason != tenant.JobReasonAttemptsRanOut || j.Import.Counts.UnitsWalked != 1 {
		e.Errorf("the job is %s %q with %+v, want failed, attempts ran out, its one walk ended", j.State, j.Reason, j.Import.Counts)
	}
}

// testImportSettlingTimesOut guards TimeOutImportSettling: a job settling
// ends partial with "settling timed out"; one not settling is answered as
// it stands; the counts go on moving after.
func testImportSettlingTimesOut(e *ienv) {
	e.setupImport()
	w := e.listingOnly("j", "p")
	r := e.openReception(importReceiver, 11, at(10))
	f := importFile(r, 11, 1, w, "p/a.dcm")
	e.handIn(f)
	e.newImport("other", "q")
	o, err := e.p.TimeOutImportSettling(e.ctx, "other", at(20))
	e.ok("TimeOutImportSettling of a queued job", err)
	if o.State != tenant.JobQueued {
		e.Errorf("a job not settling became %s, want queued as it stands", o.State)
	}
	e.endWalk(w, at(13))
	j, err := e.p.TimeOutImportSettling(e.ctx, "j", at(5000))
	e.ok("TimeOutImportSettling", err)
	if j.State != tenant.JobPartial || j.Reason != tenant.ImportReasonSettlingTimedOut || j.Import.Counts.Processing != 1 {
		e.Errorf("the job timed out is %s %q with %+v, want partial, settling timed out, one processing", j.State, j.Reason, j.Import.Counts)
	}
	r = e.stepTo(r, reception.Identified, "store")
	e.commit(r, f)
	e.wantImport("decided after the time out", e.reconciled("decided after the time out", "j"), 1, 1, 1, 0, 0)
}

// testImportFilesBounded guards the bound on the files a job lists: past
// its FailuresKept they are counted, not listed.
func testImportFilesBounded(e *ienv) {
	e.setupImport()
	w := e.listingOnly("j", "p")
	for i := range tenant.MinFailuresKept + 1 {
		e.skip(w, "p/f"+string(rune('a'+i/26))+string(rune('a'+i%26)), tenant.SkipNotPart10, "")
	}
	j := e.reconciled("past the bound", "j")
	if got := len(e.files("j", tenant.ImportFileFilter{})); got != tenant.MinFailuresKept || j.Import.FilesNotListed != 1 {
		e.Errorf("%d files listed and %d not, want %d and 1", got, j.Import.FilesNotListed, tenant.MinFailuresKept)
	}
}

// testImportHeldAtArrival guards a file held as it arrived (never a
// received file): handed in and quarantined in the record's transaction,
// the record naming the job and the path; written again it changes
// nothing; with a lease lost it records nothing.
func testImportHeldAtArrival(e *ienv) {
	e.setupImport()
	w := e.listingOnly("j", "p")
	q := tenant.QuarantinedFile{ID: "h1", Reason: tenant.QuarantineNotDICOM, Details: map[string]string{"offset": "0"},
		Receiver: importReceiver, Size: 300, Quarantined: at(5),
		Import: &tenant.ImportArrival{Fence: w.Fence(), Path: "p/x.bin", Size: 300}}
	e.hold(q)
	e.hold(q)
	j := e.reconciled("held as it arrived", "j")
	if c := j.Import.Counts; c.HandedIn != 1 || c.Quarantined[tenant.QuarantineNotDICOM] != 1 || c.Processing != 0 || c.BytesHandedIn != 300 {
		e.Errorf("counts %+v, want one handed in, quarantined not_dicom, 300 bytes", c)
	}
	rec, err := e.p.QuarantinedFile(e.ctx, "h1")
	e.ok("QuarantinedFile", err)
	if rec.ImportJob != "j" || rec.ImportPath != "p/x.bin" {
		e.Errorf("the record names %q %q, want the job and the path", rec.ImportJob, rec.ImportPath)
	}
	if got := e.files("j", tenant.ImportFileFilter{}); len(got) != 1 || got[0].QuarantinedFile != "h1" || got[0].Kind != tenant.ImportFileQuarantined {
		e.Errorf("the files listed are %+v, want the held one", got)
	}
	_, err = e.p.ReclaimWorkOfKinds(e.ctx, []string{tenant.ImportWorkKind}, at(20))
	e.ok("ReclaimWorkOfKinds", err)
	q.ID, q.Import = "h2", &tenant.ImportArrival{Fence: w.Fence(), Path: "p/y.bin", Size: 300}
	_, err = e.p.HoldFiles(e.ctx, []tenant.QuarantinedFile{q})
	e.wantIs("a hold of a lost lease", err, tenant.ErrLeaseLost, fault.Conflict)
	if _, err := e.p.QuarantinedFile(e.ctx, "h2"); fault.KindOf(err) != fault.NotFound {
		e.Errorf("a refused hold left its record: %v", err)
	}
	e.reconciled("a hold refused", "j")
}

// testImportReleaseKeepsOutcome guards a quarantined file released: the
// job keeps its first outcome, quarantined, whatever the release's
// reception makes of it.
func testImportReleaseKeepsOutcome(e *ienv) {
	e.setupImport()
	w := e.listingOnly("j", "p")
	r := e.openReception(importReceiver, 12, at(10))
	f := importFile(r, 12, 1, w, "p/a.dcm")
	e.handIn(f)
	e.endWalk(w, at(11))
	r = e.stepTo(r, reception.Identified, "store")
	e.hold(held("q1", f, tenant.QuarantineBadLabel, badLabel))
	e.stepTo(r, reception.Done, "store")
	before := e.reconciled("quarantined", "j")
	rel, err := e.p.ReleaseFile(e.ctx, tenant.Release{ID: "q1", Mode: tenant.ReleaseAsIs, ReceptionID: "rc-rel", Now: at(100)}, e.audit(), nil)
	e.ok("ReleaseFile", err)
	if rel.ImportJob != "j" || rel.ImportPath != "p/a.dcm" {
		e.Errorf("the released record names %q %q, want the job and the path kept", rel.ImportJob, rel.ImportPath)
	}
	joined, _, err := e.p.Reception(e.ctx, rel.ReleaseReception)
	e.ok("Reception", err)
	joined = e.stepTo(joined, reception.Identified, "store")
	moved := f
	moved.ReceptionID = joined.ID
	e.commit(joined, moved)
	after := e.reconciled("released and archived", "j")
	if after.Import.Counts.Archived != 0 || after.Import.Counts.Quarantined[tenant.QuarantineBadLabel] != 1 ||
		after.State != before.State {
		e.Errorf("after the release the job is %s with %+v, want its first outcome kept", after.State, after.Import.Counts)
	}
}
