package tenanttest

import (
	"context"
	"encoding/base64"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/core/reception"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// runImporting runs the contract suite's group of import jobs
// (tenant.Importing) against the stores open makes, each test on its own
// store, in parallel. Run runs it. Every test checks both identities of
// the counts (tenant.ImportCounts.Reconcile), on the job and on each unit,
// and that the units' counts add up to the job's, after every write.
func runImporting(t *testing.T, open OpenEvents) {
	tests := []struct {
		name string
		fn   func(*ienv)
	}{
		{"ImportCreate", testImportCreate},
		{"ImportWalk", testImportWalk},
		{"ImportOrder", testImportOrder},
		{"ImportAlreadyHeld", testImportAlreadyHeld},
		{"ImportFailedRetried", testImportFailedRetried},
		{"ImportDiscardedAndLost", testImportDiscardedAndLost},
		{"ImportCrashResumes", testImportCrashResumes},
		{"ImportCanceled", testImportCanceled},
		{"ImportUnitFails", testImportUnitFails},
		{"ImportUnitGivesUp", testImportUnitGivesUp},
		{"ImportSettlingTimesOut", testImportSettlingTimesOut},
		{"ImportFilesBounded", testImportFilesBounded},
		{"ImportHeldAtArrival", testImportHeldAtArrival},
		{"ImportReleaseKeepsOutcome", testImportReleaseKeepsOutcome},
		{"ImportPathBound", testImportPathBound},
		{"ImportArrivalAfterCancel", testImportArrivalAfterCancel},
		{"ImportQuarantinedEnd", testImportQuarantinedEnd},
		{"ImportEndEventLast", testImportEndEventLast},
		{"ImportNameHeld", testImportNameHeld},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s := open(t, SuiteTenant)
			tt.fn(&ienv{senv: &senv{eenv: &eenv{penv: &penv{env: &env{T: t, s: s, ctx: context.Background()}, p: s}, ev: s}}})
		})
	}
}

// runImportingShared runs the import tests that need two instances of the
// store on one database, as two processes would work one job.
func runImportingShared(t *testing.T, open OpenEventsPair) {
	t.Run("ImportUnitsAcrossInstances", func(t *testing.T) {
		t.Parallel()
		first, second := open(t, SuiteTenant)
		e := &ienv{senv: &senv{eenv: &eenv{penv: &penv{env: &env{T: t, s: first, ctx: context.Background()}, p: first}, ev: first}}}
		testImportUnitsShared(e, second)
	})
}

// ienv is an import test's store and its helpers; it has senv's too.
type ienv struct {
	*senv
}

// importReceiver is the receiver every import test's jobs name.
const importReceiver = "imp"

// setupImport adds the receiver the jobs name, on storage "store".
func (e *ienv) setupImport() {
	e.Helper()
	e.addReceiver(importReceiver, "store", 11112)
}

// newImport makes an import job of the path, at T0, keeping 100 files.
func (e *ienv) newImport(id, path string) tenant.Job {
	e.Helper()
	j, err := e.p.CreateImportJob(e.ctx, tenant.NewImportJob{ID: id, Receiver: importReceiver, Path: path,
		FailuresKept: tenant.MinFailuresKept, Created: T0}, e.audit(), nil)
	e.ok("CreateImportJob", err)
	return j
}

// claimImport claims the next import unit due at now.
func (e *ienv) claimImport(owner string, now time.Time) tenant.Work {
	e.Helper()
	w, err := e.p.ClaimWork(e.ctx, tenant.ImportWorkKind, owner, now, time.Minute)
	e.ok("ClaimWork import", err)
	return w
}

// start starts the leased unit.
func (e *ienv) start(w tenant.Work, now time.Time) tenant.ImportUnit {
	e.Helper()
	u, _, err := e.p.StartImportUnit(e.ctx, w.Fence(), now)
	e.ok("StartImportUnit", err)
	return u
}

// claimUnits claims and starts n units, and returns their leases by folder.
func (e *ienv) claimUnits(n int, now time.Time) map[string]tenant.Work {
	e.Helper()
	out := map[string]tenant.Work{}
	for range n {
		w := e.claimImport("w", now)
		out[e.start(w, now).Folder] = w
	}
	return out
}

// skip records a skip of the path by the unit.
func (e *ienv) skip(w tenant.Work, path string, code tenant.ImportSkipCode, detail string) tenant.Job {
	e.Helper()
	j, err := e.p.RecordImportSkip(e.ctx, tenant.ImportSkip{Fence: w.Fence(), Path: path, Code: code, Size: 10, Detail: detail, Now: at(5)})
	e.ok("RecordImportSkip", err)
	return j
}

// endWalk ends the unit's walk.
func (e *ienv) endWalk(w tenant.Work, now time.Time) tenant.Job {
	e.Helper()
	j, err := e.p.EndImportWalk(e.ctx, w.Fence(), now)
	e.ok("EndImportWalk", err)
	return j
}

// importFile returns received file n of study for the reception, handed in
// by the unit w from path: no association brought it.
func importFile(r tenant.Reception, study, n int, w tenant.Work, path string) tenant.ReceivedFile {
	f := file(r, study, n)
	f.Association = ""
	f.Import = &tenant.ImportArrival{Fence: w.Fence(), Path: path, Size: f.Size}
	return f
}

// handIn records the files, failing the test on an error.
func (e *ienv) handIn(files ...tenant.ReceivedFile) {
	e.Helper()
	_, err := e.p.AddReceivedFiles(e.ctx, files, at(12))
	e.ok("AddReceivedFiles", err)
}

// held returns the record of received file f held with the reason.
func held(id string, f tenant.ReceivedFile, reason tenant.QuarantineReason, details map[string]string) tenant.QuarantinedFile {
	q := tenant.QuarantinedFile{ID: id, Reason: reason, Details: details, Receiver: importReceiver, ReceptionID: f.ReceptionID,
		Caller: f.Caller, Size: f.Size, SHA256: f.SHA256, SOPClassUID: f.SOPClassUID, SOPInstanceUID: f.SOPInstanceUID,
		StudyUID: f.StudyUID, SeriesUID: f.SeriesUID, TransferSyntaxUID: f.TransferSyntaxUID, Incoming: f.Incoming,
		ContentKept: true, Quarantined: at(30)}
	if reason == tenant.QuarantineIncomingLost {
		q.Incoming, q.ContentKept = "", false // no bytes left to keep
	}
	return q
}

// badLabel is the details of a file held for a subject label it lacks.
var badLabel = map[string]string{"label": "subject", "fault": "empty"}

// hold records the records, failing the test on an error.
func (e *ienv) hold(files ...tenant.QuarantinedFile) {
	e.Helper()
	_, err := e.p.HoldFiles(e.ctx, files)
	e.ok("HoldFiles", err)
}

// reconciled reads the import job and its units and fails unless every
// count reconciles (tenant.ImportCounts.Reconcile) on the job and each
// unit, the units' counts add up to the job's, nothing was handed in
// twice, and the job's counts are the import's as the contract maps them.
// It returns the job.
func (e *ienv) reconciled(what, id string) tenant.Job {
	e.Helper()
	j, err := e.p.ImportJob(e.ctx, id)
	e.ok(what+": ImportJob", err)
	c := j.Import.Counts
	if s := c.Reconcile(); s != "" {
		e.Errorf("%s: the job's counts %+v do not reconcile: %s", what, c, s)
	}
	if c.HandedAgain != 0 {
		e.Errorf("%s: %d files handed in again, want 0", what, c.HandedAgain)
	}
	jc := j.Counts
	if jc != c.JobCounts(j.Import.Listed) || jc.Instances != jc.InstancesSent+jc.InstancesWarned+jc.InstancesRefused+jc.InstancesRemaining() ||
		jc.InstancesRemaining() != c.Processing {
		e.Errorf("%s: the job's counts %+v are not its import's %+v as the contract maps them", what, jc, c)
	}
	units := allPages(e.penv, 2, func(p master.Page) ([]tenant.ImportUnit, string, error) {
		return e.p.ListImportUnits(e.ctx, id, tenant.ImportUnitFilter{}, p)
	})
	var sum tenant.ImportCounts
	sum.Skipped, sum.Quarantined = map[tenant.ImportSkipCode]int64{}, map[tenant.QuarantineReason]int64{}
	for _, u := range units {
		uc := u.Counts
		if s := uc.Reconcile(); s != "" {
			e.Errorf("%s: unit %s's counts %+v do not reconcile: %s", what, u.ID, uc, s)
		}
		sum.Found, sum.HandedIn, sum.Archived, sum.AlreadyHeld = sum.Found+uc.Found, sum.HandedIn+uc.HandedIn, sum.Archived+uc.Archived, sum.AlreadyHeld+uc.AlreadyHeld
		sum.Lost, sum.Failed, sum.Discarded, sum.Processing = sum.Lost+uc.Lost, sum.Failed+uc.Failed, sum.Discarded+uc.Discarded, sum.Processing+uc.Processing
		sum.BytesHandedIn, sum.Units, sum.UnitsWalked = sum.BytesHandedIn+uc.BytesHandedIn, sum.Units+uc.Units, sum.UnitsWalked+uc.UnitsWalked
		for k, v := range uc.Skipped {
			sum.Skipped[k] += v
		}
		for k, v := range uc.Quarantined {
			sum.Quarantined[k] += v
		}
	}
	if sum.Found != c.Found || sum.HandedIn != c.HandedIn || sum.Archived != c.Archived || sum.AlreadyHeld != c.AlreadyHeld ||
		sum.Lost != c.Lost || sum.Failed != c.Failed || sum.Discarded != c.Discarded || sum.Processing != c.Processing ||
		sum.BytesHandedIn != c.BytesHandedIn || sum.Units != c.Units || sum.UnitsWalked != c.UnitsWalked ||
		!sameCounts(sum.Skipped, c.Skipped) || !sameCounts(sum.Quarantined, c.Quarantined) {
		e.Errorf("%s: the units' counts add up to %+v, the job's are %+v", what, sum, c)
	}
	return j
}

// sameCounts reports whether two counts by key hold the same keys with
// counts above zero and the same counts.
func sameCounts[K comparable](a, b map[K]int64) bool {
	for k, v := range a {
		if v != b[k] {
			return false
		}
	}
	for k, v := range b {
		if v != a[k] {
			return false
		}
	}
	return true
}

// wantImport fails unless the job's import counts hold the values given.
func (e *ienv) wantImport(what string, j tenant.Job, found, handedIn, archived, alreadyHeld, processing int64) {
	e.Helper()
	c := j.Import.Counts
	if c.Found != found || c.HandedIn != handedIn || c.Archived != archived || c.AlreadyHeld != alreadyHeld || c.Processing != processing {
		e.Errorf("%s: counts %+v, want found %d, handed in %d, archived %d, already held %d, processing %d",
			what, c, found, handedIn, archived, alreadyHeld, processing)
	}
}

// files returns every listed file of the job the filter keeps.
func (e *ienv) files(id string, f tenant.ImportFileFilter) []tenant.ImportFile {
	e.Helper()
	return allPages(e.penv, 3, func(p master.Page) ([]tenant.ImportFile, string, error) {
		return e.p.ListImportFiles(e.ctx, id, f, p)
	})
}

// paths returns the files' paths and kinds, "path kind" each.
func paths(files []tenant.ImportFile) []string {
	out := make([]string, len(files))
	for i, f := range files {
		out[i] = f.Path + " " + string(f.Kind)
	}
	return out
}

// testImportCreate guards CreateImportJob: the job is queued with its
// listing unit, its counts reconcile at zero, its event is written; a job
// whose path overlaps one of the same receiver in progress, a receiver
// missing and an id taken are refused; a path breaking the rule is Invalid
// with a message naming no segment's text; the inline map is kept and
// answered by ImportJob alone.
func testImportCreate(e *ienv) {
	e.setupImport()
	j := e.newImport("imp1", "proj")
	if j.Kind != tenant.JobImport || j.State != tenant.JobQueued || j.Receiver != importReceiver || j.Sender != "" ||
		j.Import == nil || j.Import.Path != "proj" || j.Import.Counts.Units != 1 || j.Counts.Scans != 1 || j.Counts.Listed {
		e.Fatalf("CreateImportJob = %+v (import %+v), want a queued import job of one unit", j, j.Import)
	}
	e.reconciled("a job made", "imp1")
	if got := steps(e.jobEvents(""), "imp1"); !slices.Equal(got, []tenant.JobState{tenant.JobQueued}) {
		e.Errorf("the job's events reached %v, want [queued]", got)
	}
	mk := func(id, receiver, path string) error {
		_, err := e.p.CreateImportJob(e.ctx, tenant.NewImportJob{ID: id, Receiver: receiver, Path: path,
			FailuresKept: tenant.MinFailuresKept, Created: T0}, e.audit(), nil)
		return err
	}
	e.wantPrecondition("a path under one in progress", mk("imp2", importReceiver, "proj/s1"))
	e.wantPrecondition("the whole root, over one in progress", mk("imp2", importReceiver, ""))
	e.ok("a path beside it", mk("imp3", importReceiver, "projx"))
	e.wantPrecondition("a receiver the tenant does not have", mk("imp4", "nope", "other"))
	e.wantKind("an id taken", mk("imp1", importReceiver, "other"), fault.Conflict)
	err := mk("imp5", importReceiver, "PATIENT^NAME/..")
	e.wantKind("a path with a dot segment", err, fault.Invalid)
	if err != nil && strings.Contains(err.Error(), "PATIENT") {
		e.Errorf("the refusal of a path repeats it: %v", err)
	}
	m := tenant.ImportMap{KeyColumns: []string{"label"}, ValueColumn: "subject_label", KeyParts: []string{"session"}, Gives: "subject"}
	csv := []byte("label,subject_label\nS1,P1\n")
	_, err = e.p.CreateImportJob(e.ctx, tenant.NewImportJob{ID: "imp6", Receiver: importReceiver, Path: "maps", MapCSV: csv, Map: m,
		FailuresKept: tenant.MinFailuresKept, Created: T0}, e.audit(), nil)
	e.ok("CreateImportJob with a map", err)
	full, err := e.p.ImportJob(e.ctx, "imp6")
	e.ok("ImportJob", err)
	if string(full.Import.MapCSV) != string(csv) || !slices.Equal(full.Import.Map.KeyColumns, m.KeyColumns) ||
		full.Import.Map.Gives != "subject" || !slices.Equal(full.Import.Map.KeyParts, m.KeyParts) {
		e.Errorf("ImportJob's import = %+v, want the map as given", full.Import)
	}
	plain, err := e.p.Job(e.ctx, "imp6")
	e.ok("Job", err)
	if plain.Import == nil || plain.Import.MapCSV != nil {
		e.Errorf("Job's import = %+v, want it without the inline map", plain.Import)
	}
	_, err = e.p.ImportJob(e.ctx, "nope")
	e.wantKind("ImportJob of no job", err, fault.NotFound)
	jobs, _, err := e.p.ListJobs(e.ctx, tenant.JobFilter{}, master.Page{Limit: 10})
	e.ok("ListJobs", err)
	for _, l := range jobs {
		if l.Kind != tenant.JobImport || l.Import == nil || l.Counts.Scans != 1 {
			e.Errorf("ListJobs answered %+v, want import jobs with their counts", l)
		}
	}
}

// testImportWalk guards one job's whole life: a listing unit that skips a
// file and queues two units; a unit that hands in files with its cursor,
// refuses a path before its cursor or outside its folder, takes a resend
// at its cursor as recorded, and skips a file too large; a unit that skips
// a directory too wide; the job settling while files are processing, then
// ending partial in the transaction of the last outcome; the files listed
// in the walk's order, the quarantine record naming the job and the path,
// and the units listed by folder. The counts reconcile after every write.
func testImportWalk(e *ienv) {
	e.setupImport()
	e.newImport("j", "proj")
	lw := e.claimImport("w", at(1))
	u, job, err := e.p.StartImportUnit(e.ctx, lw.Fence(), at(1))
	e.ok("StartImportUnit", err)
	if !u.Listing || u.Folder != "proj" || u.Cursor != "" || u.State != tenant.ImportUnitWalking || job.State != tenant.JobRunning {
		e.Fatalf("the first unit is %+v of a job %s, want the walking listing unit of a running job", u, job.State)
	}
	e.skip(lw, "proj/a-readme.txt", tenant.SkipNotPart10, "")
	e.reconciled("a skip", "j")
	_, err = e.p.AddImportUnits(e.ctx, lw.Fence(), []string{"proj/s1", "proj/s2"}, at(2))
	e.ok("AddImportUnits", err)
	_, err = e.p.AddImportUnits(e.ctx, lw.Fence(), []string{"proj/b"}, at(2))
	e.wantPrecondition("a folder before the listing's cursor", err)
	_, err = e.p.AddImportUnits(e.ctx, lw.Fence(), []string{"proj/s2"}, at(2))
	e.ok("a folder queued again", err)
	j := e.endWalk(lw, at(2))
	if c := j.Import.Counts; c.Units != 3 || c.UnitsWalked != 1 || !j.Import.Listed || j.State != tenant.JobRunning {
		e.Errorf("after the listing's walk: %+v listed %v, want 3 units, 1 walked, listed, running", c, j.Import.Listed)
	}
	e.reconciled("the listing's walk ended", "j")

	ws := e.claimUnits(2, at(3))
	w1, w2 := ws["proj/s1"], ws["proj/s2"]
	r := e.openReception(importReceiver, 1, at(10))
	f1 := importFile(r, 1, 1, w1, "proj/s1/a/b/c.dcm")
	f2 := importFile(r, 1, 2, w1, "proj/s1/a/b.d")
	f3 := importFile(r, 1, 3, w1, "proj/s1/z.dcm")
	e.handIn(f1)
	e.reconciled("a file handed in", "j")
	e.handIn(f2, f3)
	e.wantImport("three handed in", e.reconciled("three handed in", "j"), 4, 3, 0, 0, 3)
	_, err = e.p.AddReceivedFiles(e.ctx, []tenant.ReceivedFile{importFile(r, 1, 4, w1, "proj/s1/a")}, at(12))
	e.wantPrecondition("a path before the cursor", err)
	_, err = e.p.AddReceivedFiles(e.ctx, []tenant.ReceivedFile{importFile(r, 1, 4, w1, "proj/s2/x.dcm")}, at(12))
	e.wantPrecondition("a path outside the unit's folder", err)
	e.handIn(f3)
	e.wantImport("a resend at the cursor", e.reconciled("a resend at the cursor", "j"), 4, 3, 0, 0, 3)
	e.skip(w1, "proj/s1/zz-big.dcm", tenant.SkipTooLarge, tenant.ImportDetailMaxFileBytes)
	j = e.endWalk(w1, at(13))
	if j.State != tenant.JobRunning || j.Reason != "" {
		e.Errorf("with a unit walking the job is %s %q, want running with no reason", j.State, j.Reason)
	}
	e.skip(w2, "proj/s2/wide", tenant.SkipDirectoryTooWide, tenant.ImportDetailMaxDirEntries)
	j = e.endWalk(w2, at(14))
	if j.State != tenant.JobRunning || j.Reason != tenant.ImportReasonSettling || j.Import.SettlingSince.IsZero() {
		e.Errorf("every walk ended, files processing: %s %q since %v, want running, settling", j.State, j.Reason, j.Import.SettlingSince)
	}
	e.reconciled("settling", "j")

	r = e.stepTo(r, reception.Identified, "store")
	e.commit(r, f1, f2)
	e.wantImport("two archived", e.reconciled("two archived", "j"), 6, 3, 2, 0, 1)
	e.hold(held("q1", f3, tenant.QuarantineBadLabel, badLabel))
	j = e.reconciled("the last outcome", "j")
	if j.State != tenant.JobPartial || j.Import.Counts.Quarantined[tenant.QuarantineBadLabel] != 1 || j.Import.Counts.Processing != 0 {
		e.Errorf("after the last outcome the job is %s with %+v, want partial with one quarantined bad_label", j.State, j.Import.Counts)
	}
	e.stepTo(r, reception.Done, "store")
	e.reconciled("the reception done", "j")
	if got := steps(e.jobEvents(""), "j"); !slices.Equal(got, []tenant.JobState{tenant.JobQueued, tenant.JobRunning, tenant.JobPartial}) {
		e.Errorf("the job's events reached %v, want queued, running, partial", got)
	}
	q, err := e.p.QuarantinedFile(e.ctx, "q1")
	e.ok("QuarantinedFile", err)
	if q.ImportJob != "j" || q.ImportPath != "proj/s1/z.dcm" {
		e.Errorf("the record names job %q and path %q, want j and the file's path", q.ImportJob, q.ImportPath)
	}
	files := e.files("j", tenant.ImportFileFilter{})
	want := []string{"proj/a-readme.txt skipped", "proj/s1/z.dcm quarantined", "proj/s1/zz-big.dcm skipped", "proj/s2/wide skipped"}
	if got := paths(files); !slices.Equal(got, want) {
		e.Errorf("the files listed are %q, want %q", got, want)
	}
	for _, f := range files {
		switch f.Path {
		case "proj/s1/z.dcm":
			if f.QuarantinedFile != "q1" || f.QuarantineReason != tenant.QuarantineBadLabel || f.Reception != r.ID || f.Unit == "" {
				e.Errorf("the quarantined file is listed %+v, want its record, reason, reception and unit", f)
			}
		case "proj/s2/wide":
			if f.SkipCode != tenant.SkipDirectoryTooWide || f.Detail != tenant.ImportDetailMaxDirEntries {
				e.Errorf("the directory too wide is listed %+v, want its code and the setting's name", f)
			}
		}
	}
	if got := paths(e.files("j", tenant.ImportFileFilter{Kinds: []tenant.ImportFileKind{tenant.ImportFileSkipped}})); len(got) != 3 {
		e.Errorf("the skipped files listed are %q, want 3", got)
	}
	units := allPages(e.penv, 1, func(p master.Page) ([]tenant.ImportUnit, string, error) {
		return e.p.ListImportUnits(e.ctx, "j", tenant.ImportUnitFilter{}, p)
	})
	var folders []string
	for _, u := range units {
		folders = append(folders, u.Folder+" "+string(u.State))
	}
	if want := []string{"proj done", "proj/s1 done", "proj/s2 done"}; !slices.Equal(folders, want) {
		e.Errorf("the units listed are %q, want %q", folders, want)
	}
	s1 := units[1].ID
	if got := paths(e.files("j", tenant.ImportFileFilter{Unit: s1})); len(got) != 2 {
		e.Errorf("unit s1's files are %q, want 2", got)
	}
}

// testImportOrder guards the walk's order in the store: a unit takes
// "x/a/b/c", "x/a/b.d", "x/a-c" in that order (segment by segment, a
// directory's subtree before the next name), which plain byte order of
// whole paths would reverse; the list answers them in it, a page at a
// time; and a page's cursor holds no path.
func testImportOrder(e *ienv) {
	e.setupImport()
	e.newImport("j", "x")
	w := e.claimImport("w", at(1))
	e.start(w, at(1))
	order := []string{"x/a/b/c", "x/a/b.d", "x/a-c"}
	for _, p := range order {
		e.skip(w, p, tenant.SkipNotPart10, "")
	}
	page := master.Page{Limit: 1}
	var got []string
	for range 10 {
		files, next, err := e.p.ListImportFiles(e.ctx, "j", tenant.ImportFileFilter{}, page)
		e.ok("ListImportFiles", err)
		for _, f := range files {
			got = append(got, f.Path)
		}
		if next == "" {
			break
		}
		raw, _ := base64.RawURLEncoding.DecodeString(next)
		if strings.Contains(string(raw), "x/a") || strings.Contains(next, "x/a") || strings.Contains(string(raw), "x\x00a") {
			e.Errorf("a page's cursor holds a path: %q", raw)
		}
		page.Cursor = next
	}
	if !slices.Equal(got, order) {
		e.Errorf("the files are listed %q, want %q", got, order)
	}
	e.reconciled("three skipped", "j")
}

// testImportUnitsShared guards two instances working two units of one job
// at once, each handing in and skipping files one at a time: every write is
// counted once on the job, and the counts reconcile.
func testImportUnitsShared(e *ienv, other EventStore) {
	e.setupImport()
	e.newImport("j", "p")
	lw := e.claimImport("w", at(1))
	e.start(lw, at(1))
	_, err := e.p.AddImportUnits(e.ctx, lw.Fence(), []string{"p/a", "p/b"}, at(1))
	e.ok("AddImportUnits", err)
	e.endWalk(lw, at(1))
	ws := e.claimUnits(2, at(2))
	stores := []tenant.Pipeline{e.p, other}
	const n = 15
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i, folder := range []string{"p/a", "p/b"} {
		r := e.openReception(importReceiver, 100+i, at(10))
		wg.Add(1)
		go func() {
			defer wg.Done()
			for k := range n {
				f := importFile(r, 100+i, 1000*(i+1)+k, ws[folder], folder+"/f"+string(rune('a'+k)))
				if _, err := stores[i].AddReceivedFiles(e.ctx, []tenant.ReceivedFile{f}, at(12)); err != nil {
					errs[i] = err
					return
				}
				if _, err := stores[i].RecordImportSkip(e.ctx, tenant.ImportSkip{Fence: ws[folder].Fence(),
					Path: folder + "/f" + string(rune('a'+k)) + "x", Code: tenant.SkipNotPart10, Now: at(12)}); err != nil {
					errs[i] = err
					return
				}
			}
		}()
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil && !errors.Is(err, context.Canceled) {
			e.ok("worker", err)
		}
	}
	e.wantImport("two units at once", e.reconciled("two units at once", "j"), 4*n, 2*n, 0, 0, 2*n)
}
