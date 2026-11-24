package tenanttest

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// runScrubbing runs the tests of scrub jobs, their findings and the marks of
// damaged instances.
func runScrubbing(t *testing.T, open OpenEvents) {
	tests := []struct {
		name string
		fn   func(*senv)
	}{
		{"ScrubJobs", testScrubJobs},
		{"ScrubFindings", testScrubFindings},
		{"ScrubFindingsSeenAgain", testScrubFindingsSeenAgain},
		{"ScrubFindingsList", testScrubFindingsList},
		{"ScrubFindingsRefused", testScrubFindingsRefused},
		{"ScrubFindingsOfScan", testScrubFindingsOfScan},
		{"ScrubObjectClears", testScrubObjectClears},
		{"ScrubFindingFoundAgainAfterAClear", testScrubFindingFoundAgainAfterAClear},
		{"ScrubTalliesAndLast", testScrubTalliesAndLast},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s := open(t, SuiteTenant)
			tt.fn(&senv{eenv: &eenv{penv: &penv{env: &env{T: t, s: s, ctx: context.Background()}, p: s}, ev: s}})
		})
	}
}

// scrubSetup adds the storage "store" with two scans of three instances,
// and the empty storage "cold".
func (e *senv) scrubSetup() {
	e.Helper()
	e.addReceiver("recv", "store", 11112)
	e.archive("recv", 1, 2, 3, 0, at(-3600), "ses-1")
	e.addStorage("cold")
}

// createScrub makes a scrub job of the storage (empty for every one) at the
// time given, failing the test on an error.
func (e *senv) createScrub(id, storage string, when time.Time) tenant.Job {
	e.Helper()
	j, err := e.p.CreateJob(e.ctx, tenant.NewJob{ID: id, Kind: tenant.JobScrub, Storage: storage, Created: when}, e.audit(), nil)
	e.ok("CreateJob of a scrub", err)
	return j
}

// claimScrub claims the next unit of the tenant's scrub jobs.
func (e *senv) claimScrub(owner string, now time.Time) tenant.Work {
	e.Helper()
	w, err := e.p.ClaimWork(e.ctx, tenant.ScrubWorkKind, owner, now, time.Minute)
	e.ok("ClaimWork of a scrub", err)
	return w
}

// listScrub claims the next scrub unit, which must be a listing, and lists
// every scan of it.
func (e *senv) listScrub(now time.Time) tenant.Job {
	e.Helper()
	w := e.claimScrub("lister", now)
	u, err := e.p.StartJobUnit(e.ctx, w.Fence(), now)
	e.ok("StartJobUnit", err)
	if !u.Listing {
		e.Fatalf("the unit claimed is %+v, want a scrub's listing", u)
	}
	for range 1000 {
		j, more, err := e.p.ListJobScans(e.ctx, w.Fence(), now)
		e.ok("ListJobScans", err)
		if !more {
			return j
		}
	}
	e.Fatal("a listing did not end")
	return tenant.Job{}
}

// scrubScan claims the next scrub unit, which must be a scan's, and returns
// it with its scan and stream.
func (e *senv) scrubScan(now time.Time) (tenant.Work, tenant.JobScan, []tenant.ScanInstance) {
	e.Helper()
	w := e.claimScrub("scrubber", now)
	u, err := e.p.StartJobUnit(e.ctx, w.Fence(), now)
	e.ok("StartJobUnit", err)
	if u.Listing {
		e.Fatal("the unit claimed is a listing, want a scan's")
	}
	return w, u.Scan, e.instances(w, "")
}

// finding returns a finding of the scan unit's job, its id made from it.
func finding(job string, kind tenant.ScrubFindingKind, path label.Path, in tenant.ScanInstance) tenant.ScrubFinding {
	size, found := in.Size, in.Size+1
	f := tenant.ScrubFinding{Kind: kind, Storage: "store", Key: in.Key, Path: path, SOPInstanceUID: in.SOPInstanceUID,
		Revision: 1, ExpectedSize: &size, ExpectedSHA256: in.ArchivedSHA256}
	if kind == tenant.FindingDamaged {
		f.FoundSize, f.FoundSHA256 = &found, sum(9999)
	}
	f.ID = tenant.ScrubFindingID(job, f.Storage, f.Key, kind)
	return f
}

// record records f under the unit's fence, failing the test on an error.
func (e *senv) record(w tenant.Work, f tenant.ScrubFinding, now time.Time) (tenant.ScrubFinding, bool) {
	e.Helper()
	got, made, err := e.p.RecordScrubFinding(e.ctx, tenant.ScrubFindingRecord{Fence: w.Fence(), Finding: f, Now: now})
	e.ok("RecordScrubFinding", err)
	return got, made
}

// tallies returns a job's tallies as text, in the store's order.
func (e *senv) tallies(job string) string {
	e.Helper()
	t, err := e.p.JobTallies(e.ctx, job)
	e.ok("JobTallies", err)
	return fmt.Sprint(t)
}

// marked returns which of the instances given the catalog's read of each
// answers marked, in order.
func (e *senv) marked(path label.Path, ins []tenant.ScanInstance) []bool {
	e.Helper()
	out := make([]bool, len(ins))
	for i, in := range ins {
		got, err := e.p.HeldInstance(e.ctx, path, in.SOPInstanceUID)
		e.ok("HeldInstance", err)
		out[i] = got.Damaged
	}
	return out
}

// testScrubJobs guards making and listing scrub jobs: a scrub names no
// sender, mode, profile or target; it names a storage the tenant has, or
// none for every storage; a second scrub of a storage while one is queued
// or running is refused, and so is a scrub of every storage then, while a
// scrub of another storage is not; the job reads back its storage and
// whether it was scheduled; its units are of the kind of scrubs; a scrub of
// one storage lists that storage's scans only, and one of an empty storage
// ends done with nothing; a scrub is not retried.
func testScrubJobs(e *senv) {
	e.scrubSetup()
	mk := func(j tenant.NewJob) error {
		j.ID, j.Created = "bad", at(100)
		_, err := e.p.CreateJob(e.ctx, j, e.audit(), nil)
		return err
	}
	e.wantKind("a scrub naming a sender", mk(tenant.NewJob{Kind: tenant.JobScrub, Sender: "orthanc"}), fault.Invalid)
	e.wantKind("a scrub naming a target", mk(tenant.NewJob{Kind: tenant.JobScrub, Target: tenant.JobTarget{SubjectLabel: "sub-01"}}), fault.Invalid)
	e.wantKind("a scrub naming a mode", mk(tenant.NewJob{Kind: tenant.JobScrub, Mode: tenant.DeidCheck}), fault.Invalid)
	e.wantKind("a send job naming a storage", mk(tenant.NewJob{Sender: "orthanc", Storage: "store",
		Target: tenant.JobTarget{SubjectLabel: "sub-01"}}), fault.Invalid)
	e.wantKind("a scrub of a malformed storage", mk(tenant.NewJob{Kind: tenant.JobScrub, Storage: "Bad Name"}), fault.Invalid)
	e.wantPrecondition("a scrub of a storage the tenant does not have", mk(tenant.NewJob{Kind: tenant.JobScrub, Storage: "nope"}))
	j, err := e.p.CreateJob(e.ctx, tenant.NewJob{ID: "s1", Kind: tenant.JobScrub, Storage: "store", Scheduled: true, Created: at(100)},
		e.audit(), nil)
	e.ok("CreateJob", err)
	if j.Kind != tenant.JobScrub || j.Storage != "store" || !j.Scheduled || j.Sender != "" || j.State != tenant.JobQueued {
		e.Errorf("a scrub made reads %+v", j)
	}
	if got, err := e.p.Job(e.ctx, "s1"); err != nil || got.Storage != "store" || !got.Scheduled || got.Kind != tenant.JobScrub {
		e.Errorf("Job of a scrub = %+v, %v", got, err)
	}
	e.wantPrecondition("a second scrub of the same storage", mk(tenant.NewJob{Kind: tenant.JobScrub, Storage: "store"}))
	e.wantPrecondition("a scrub of every storage while one runs", mk(tenant.NewJob{Kind: tenant.JobScrub}))
	cold := e.createScrub("s2", "cold", at(110))
	if cold.Storage != "cold" || cold.Scheduled {
		e.Errorf("a scrub of another storage reads %+v", cold)
	}
	jobs, _, err := e.p.ListJobs(e.ctx, tenant.JobFilter{}, firstPage(10))
	e.ok("ListJobs", err)
	for _, l := range jobs {
		if (l.ID == "s1" && l.Storage != "store") || (l.ID == "s2" && l.Storage != "cold") {
			e.Errorf("ListJobs answers a scrub without its storage: %+v", l)
		}
	}
	if _, err := e.p.ClaimWork(e.ctx, tenant.DeidWorkKind, "w", at(120), time.Minute); fault.KindOf(err) != fault.NotFound {
		e.Error("a scrub's unit was claimed under the kind of deid jobs")
	}
	first := e.listScrub(at(120))
	second := e.listScrub(at(120))
	byID := map[string]tenant.Job{first.ID: first, second.ID: second}
	e.wantCounts("a scrub of the storage listed", byID["s1"], 6, 0, 0, 0, 2, 0, true)
	if c := byID["s2"]; c.State != tenant.JobDone || c.Counts.Scans != 0 || !c.Counts.Listed {
		e.Errorf("a scrub of an empty storage ended as %+v, want done with nothing", c)
	}
	e.wantPrecondition("a scrub of every storage while one still runs", mk(tenant.NewJob{Kind: tenant.JobScrub}))
	w, scan, ins := e.scrubScan(at(130))
	if scan.Storage != "store" || len(ins) != 3 {
		e.Errorf("a scrub's scan unit reads %+v with %d instances", scan, len(ins))
	}
	for _, in := range ins {
		if in.Damaged {
			e.Errorf("instance %s of a whole archive reads marked", in.SOPInstanceUID)
		}
	}
	end, err := e.p.EndJobScan(e.ctx, progress(w, "", ins, 0, allSent, at(140)))
	e.ok("EndJobScan", err)
	if end.State.Ended() {
		e.Errorf("a scrub with a scan left ended %s", end.State)
	}
	_, _, err = e.p.RetryJob(e.ctx, "s2", "s3", at(150), e.audit(), nil)
	e.wantPrecondition("a retry of a scrub", err)
}

// testScrubFindings guards recording and clearing findings: a finding is
// recorded once per job, its tallies counted once however often the write
// is sent; one of an instance missing or damaged marks the instance, which
// every catalog read of it then answers (one read, a page of the scan, a
// job's stream) and DamagedInstances names with its finding; an orphan's is
// tallied as an orphan object and marks nothing; each recorded and cleared
// finding writes its event, which names the scan and never the key; a
// clear removes the mark, but an instance stays marked while another finding
// that marks it holds; a clear sent again changes nothing.
func testScrubFindings(e *senv) {
	e.scrubSetup()
	e.createScrub("s1", "store", at(100))
	e.listScrub(at(100))
	start := e.now()
	w, scan, ins := e.scrubScan(at(110))
	path := scan.Path
	damaged := finding("s1", tenant.FindingDamaged, path, ins[0])
	got, made := e.record(w, damaged, at(120))
	if !made || got.ID != damaged.ID || got.Job != "s1" || got.LastJob != "s1" || !got.Holds() || got.Kind != tenant.FindingDamaged ||
		*got.ExpectedSize != ins[0].Size || *got.FoundSize != ins[0].Size+1 || got.FoundSHA256 != sum(9999) || got.Key != ins[0].Key {
		e.Errorf("a damaged finding recorded reads %+v (made %v)", got, made)
	}
	if _, made := e.record(w, damaged, at(121)); made {
		e.Error("a finding written again was made again")
	}
	orphan := tenant.ScrubFinding{Kind: tenant.FindingOrphan, Storage: "store", Path: path,
		Key: "data/" + path.Subject + "/" + path.Session + "/" + path.Scan + "/2.25.9.9.dcm"}
	orphan.ID = tenant.ScrubFindingID("s1", "store", orphan.Key, orphan.Kind)
	e.record(w, orphan, at(122))
	missing := finding("s1", tenant.FindingMissing, path, ins[1])
	e.record(w, missing, at(123))
	if got := fmt.Sprint(e.marked(path, ins)); got != "[true true false]" {
		e.Errorf("the catalog reads the marks as %s, want the damaged and the missing instance marked", got)
	}
	page, _, err := e.p.ListHeldInstances(e.ctx, path, firstPage(10))
	e.ok("ListHeldInstances", err)
	if len(page) != 3 || !page[0].Damaged || !page[1].Damaged || page[2].Damaged {
		e.Errorf("a page of the scan reads the marks as %v", page)
	}
	stream := e.instances(w, "")
	if !stream[0].Damaged || !stream[1].Damaged || stream[2].Damaged {
		e.Errorf("a job's stream reads the marks as %v", stream)
	}
	marks, err := e.p.DamagedInstances(e.ctx, []string{ins[0].SOPInstanceUID, ins[1].SOPInstanceUID, ins[2].SOPInstanceUID, "2.25.9.9"})
	e.ok("DamagedInstances", err)
	if len(marks) != 2 || marks[ins[0].SOPInstanceUID] != damaged.ID || marks[ins[1].SOPInstanceUID] != missing.ID {
		e.Errorf("DamagedInstances = %v", marks)
	}
	if got, want := e.tallies("s1"), "[{finding|damaged 1} {finding|missing 1} {finding|orphan 1} {orphan_objects 1}]"; got != want {
		e.Errorf("the job's tallies = %s, want %s (a write sent again counted once)", got, want)
	}
	// A second finding that marks the missing instance: the mark stays
	// while either holds.
	second := finding("s1", tenant.FindingDamaged, path, ins[1])
	second.Key = ins[1].Key + ".copy"
	second.ID = tenant.ScrubFindingID("s1", "store", second.Key, second.Kind)
	e.record(w, second, at(124))
	cleared, ok, err := e.p.ClearScrubFinding(e.ctx, w.Fence(), missing.ID, at(130))
	e.ok("ClearScrubFinding", err)
	if !ok || cleared.Holds() || cleared.ClearJob != "s1" || !cleared.Cleared.Equal(at(130)) {
		e.Errorf("a finding cleared reads %+v (cleared %v)", cleared, ok)
	}
	if got := fmt.Sprint(e.marked(path, ins)); got != "[true true false]" {
		e.Errorf("after one of two findings of an instance is cleared, the marks read %s, want it still marked", got)
	}
	marks, err = e.p.DamagedInstances(e.ctx, []string{ins[1].SOPInstanceUID})
	e.ok("DamagedInstances", err)
	if marks[ins[1].SOPInstanceUID] != second.ID {
		e.Errorf("the instance is marked by %q, want the finding that still holds", marks[ins[1].SOPInstanceUID])
	}
	for _, id := range []string{damaged.ID, second.ID} {
		_, ok, err := e.p.ClearScrubFinding(e.ctx, w.Fence(), id, at(131))
		if e.ok("ClearScrubFinding", err); !ok {
			e.Errorf("finding %s was not cleared", id)
		}
	}
	if got := fmt.Sprint(e.marked(path, ins)); got != "[false false false]" {
		e.Errorf("after every finding is cleared the marks read %s", got)
	}
	if _, ok, err := e.p.ClearScrubFinding(e.ctx, w.Fence(), damaged.ID, at(132)); err != nil || ok {
		e.Errorf("a clear sent again = %v, %v; want nothing changed", ok, err)
	}
	_, _, err = e.p.ClearScrubFinding(e.ctx, w.Fence(), "sf-none", at(132))
	e.wantKind("a clear of no finding", err, fault.NotFound)
	if got := e.tallies("s1"); !strings.Contains(got, "{cleared 3}") {
		e.Errorf("the job's tallies = %s, want three cleared", got)
	}
	events, _ := e.readAll(start, 100, tenant.EventScrubFinding)
	var recorded, clears int
	for _, ev := range events {
		p, ok := ev.Payload.(*tenant.ScrubFindingChanged)
		if !ok {
			e.Fatalf("a scrub event carries %T", ev.Payload)
		}
		if p.Job != "s1" || p.Storage != "store" || p.ScanLabel != path.Scan || p.SubjectLabel != path.Subject {
			e.Errorf("a scrub event reads %+v", p)
		}
		if text := fmt.Sprintf("%+v", *p); strings.Contains(text, "2.25.3.") || strings.Contains(text, "data/") {
			e.Errorf("a scrub event holds an instance's UID or a key: %s", text)
		}
		if p.Cleared {
			clears++
		} else {
			recorded++
		}
	}
	if recorded != 4 || clears != 3 {
		e.Errorf("the feed holds %d findings recorded and %d cleared, want 4 and 3", recorded, clears)
	}
}

// testScrubFindingsSeenAgain guards a finding found again by a later scrub:
// it is the same finding, seen again by the later job, which counts it in
// its own tallies once; the first job's tallies do not move; the findings
// that hold are counted once by kind.
func testScrubFindingsSeenAgain(e *senv) {
	e.scrubSetup()
	e.createScrub("s1", "store", at(100))
	e.listScrub(at(100))
	w, scan, ins := e.scrubScan(at(110))
	first := finding("s1", tenant.FindingDamaged, scan.Path, ins[0])
	e.record(w, first, at(120))
	_, err := e.p.CancelJob(e.ctx, "s1", at(130), e.audit(), nil)
	e.ok("CancelJob", err)
	_, err = e.p.EndJobScan(e.ctx, tenant.JobProgress{Fence: w.Fence(), FailuresKept: tenant.DefaultFailuresKept, Now: at(131)})
	e.ok("EndJobScan of the canceled job's unit", err)
	e.createScrub("s2", "store", at(200))
	var w2 tenant.Work
	var scan2 tenant.JobScan
	var ins2 []tenant.ScanInstance
	e.listScrub(at(200))
	for range 2 {
		w2, scan2, ins2 = e.scrubScan(at(210))
		if scan2.Path == scan.Path {
			break
		}
	}
	if scan2.Path != scan.Path {
		e.Fatal("the later scrub's units did not reach the scan")
	}
	again := finding("s2", tenant.FindingDamaged, scan2.Path, ins2[0])
	got, made := e.record(w2, again, at(220))
	if made || got.ID != first.ID || got.Job != "s1" || got.LastJob != "s2" || !got.LastSeen.Equal(at(220)) {
		e.Errorf("a finding found again reads %+v (made %v), want the first, seen again by s2", got, made)
	}
	e.record(w2, again, at(221))
	if got := e.tallies("s2"); got != "[{finding|damaged 1}]" {
		e.Errorf("the later job's tallies = %s, want the finding counted once", got)
	}
	if got := e.tallies("s1"); got != "[{finding|damaged 1}]" {
		e.Errorf("the first job's tallies = %s, want them unmoved", got)
	}
	open, err := e.p.OpenScrubFindings(e.ctx)
	e.ok("OpenScrubFindings", err)
	if len(open) != 1 || open[tenant.FindingDamaged] != 1 {
		e.Errorf("OpenScrubFindings = %v, want one damaged", open)
	}
}

// orphanOf returns an orphan finding of the scan unit's job under the
// scan's keys, named by n.
func orphanOf(job string, path label.Path, n int) tenant.ScrubFinding {
	f := tenant.ScrubFinding{Kind: tenant.FindingOrphan, Storage: "store", Path: path,
		Key: fmt.Sprintf("data/%s/%s/%s/2.25.9.%04d.dcm", path.Subject, path.Session, path.Scan, n)}
	f.ID = tenant.ScrubFindingID(job, f.Storage, f.Key, f.Kind)
	return f
}

// testScrubFindingsOfScan guards the read of a scan's findings a unit
// clears at its end: only those of its files (an orphan, a disagreeing
// manifest), never one of an instance's bytes, which the read of the
// instance clears; every one of them, page by page past MaxScanFindings, in
// order of key, each once; none of another storage's.
func testScrubFindingsOfScan(e *senv) {
	e.scrubSetup()
	e.createScrub("s1", "store", at(100))
	e.listScrub(at(100))
	w, scan, ins := e.scrubScan(at(110))
	e.record(w, finding("s1", tenant.FindingDamaged, scan.Path, ins[0]), at(120))
	want := tenant.MaxScanFindings + 2
	for i := range want {
		e.record(w, orphanOf("s1", scan.Path, i), at(120))
	}
	var got []string
	afterKey, afterKind := "", tenant.ScrubFindingKind("")
	for range 10 {
		page, err := e.p.ScanScrubFindings(e.ctx, "store", scan.Path, afterKey, afterKind)
		e.ok("ScanScrubFindings", err)
		for _, f := range page {
			if f.Kind != tenant.FindingOrphan {
				e.Errorf("ScanScrubFindings answers a finding of kind %s", f.Kind)
			}
			got = append(got, f.Key)
		}
		if len(page) < tenant.MaxScanFindings {
			break
		}
		afterKey, afterKind = page[len(page)-1].Key, page[len(page)-1].Kind
	}
	if len(got) != want || !slices.IsSorted(got) || got[0] != orphanOf("s1", scan.Path, 0).Key {
		e.Errorf("ScanScrubFindings answered %d findings (sorted %v), want %d in order of key", len(got), slices.IsSorted(got), want)
	}
	other, err := e.p.ScanScrubFindings(e.ctx, "cold", scan.Path, "", "")
	if e.ok("ScanScrubFindings of another storage", err); len(other) != 0 {
		e.Errorf("ScanScrubFindings of another storage = %+v", other)
	}
	_, err = e.p.ScanScrubFindings(e.ctx, "store", scan.Path, "data/x", "bad")
	e.wantKind("a page after an unknown kind", err, fault.Invalid)
}

// testScrubObjectClears guards ClearObjectFindings: every finding of the
// kinds named that holds of the object is cleared in one write, with its
// mark and its event, counted cleared in the job's tallies; a finding of
// another kind or another object is left as it is; a clear of an object
// with none clears nothing; kinds named twice or none are refused.
func testScrubObjectClears(e *senv) {
	e.scrubSetup()
	e.createScrub("s1", "store", at(100))
	e.listScrub(at(100))
	w, scan, ins := e.scrubScan(at(110))
	missing := finding("s1", tenant.FindingMissing, scan.Path, ins[0])
	damaged := finding("s1", tenant.FindingDamaged, scan.Path, ins[0])
	disagreeing := tenant.ScrubFinding{Kind: tenant.FindingDisagreeing, Storage: "store", Key: ins[0].Key, Path: scan.Path}
	disagreeing.ID = tenant.ScrubFindingID("s1", "store", disagreeing.Key, disagreeing.Kind)
	other := finding("s1", tenant.FindingDamaged, scan.Path, ins[1])
	for _, f := range []tenant.ScrubFinding{missing, damaged, disagreeing, other} {
		e.record(w, f, at(120))
	}
	n, err := e.p.ClearObjectFindings(e.ctx, w.Fence(), "store", ins[0].Key,
		[]tenant.ScrubFindingKind{tenant.FindingMissing, tenant.FindingDamaged}, at(130))
	if e.ok("ClearObjectFindings", err); n != 2 {
		e.Errorf("ClearObjectFindings cleared %d, want the missing and the damaged finding", n)
	}
	if got := fmt.Sprint(e.marked(scan.Path, ins)); got != "[false true false]" {
		e.Errorf("after the clear of an object the marks read %s", got)
	}
	open, _, err := e.p.ListScrubFindings(e.ctx, tenant.ScrubFindingFilter{}, firstPage(10))
	e.ok("ListScrubFindings", err)
	var holding []string
	for _, f := range open {
		holding = append(holding, f.ID)
	}
	slices.Sort(holding)
	want := []string{disagreeing.ID, other.ID}
	slices.Sort(want)
	if !slices.Equal(holding, want) {
		e.Errorf("after the clear of an object the findings that hold are %v, want %v", holding, want)
	}
	if got := e.tallies("s1"); !strings.Contains(got, "{cleared 2}") {
		e.Errorf("the job's tallies = %s, want two cleared", got)
	}
	n, err = e.p.ClearObjectFindings(e.ctx, w.Fence(), "store", ins[2].Key, []tenant.ScrubFindingKind{tenant.FindingMissing}, at(131))
	if e.ok("ClearObjectFindings of an object with none", err); n != 0 {
		e.Errorf("a clear of an object with no finding cleared %d", n)
	}
	_, err = e.p.ClearObjectFindings(e.ctx, w.Fence(), "store", ins[0].Key,
		[]tenant.ScrubFindingKind{tenant.FindingMissing, tenant.FindingMissing}, at(131))
	e.wantKind("a clear naming a kind twice", err, fault.Invalid)
	_, err = e.p.ClearObjectFindings(e.ctx, w.Fence(), "store", ins[0].Key, nil, at(131))
	e.wantKind("a clear naming no kind", err, fault.Invalid)
}

// testScrubFindingFoundAgainAfterAClear guards a finding found again by the
// job that cleared it: its id names the cleared finding, so the new one
// takes ScrubFindingIDAgain and is made, marking the instance again,
// instead of failing on the id; the same write sent again makes nothing.
func testScrubFindingFoundAgainAfterAClear(e *senv) {
	e.scrubSetup()
	e.createScrub("s1", "store", at(100))
	e.listScrub(at(100))
	w, scan, ins := e.scrubScan(at(110))
	f := finding("s1", tenant.FindingDamaged, scan.Path, ins[0])
	e.record(w, f, at(120))
	_, _, err := e.p.ClearScrubFinding(e.ctx, w.Fence(), f.ID, at(121))
	e.ok("ClearScrubFinding", err)
	got, made, err := e.p.RecordScrubFinding(e.ctx, tenant.ScrubFindingRecord{Fence: w.Fence(), Finding: f, Now: at(122)})
	if e.ok("RecordScrubFinding after a clear", err); !made || got.ID != tenant.ScrubFindingIDAgain(f.ID, w.Fence()) || !got.Holds() {
		e.Errorf("a finding found again after a clear reads %+v (made %v)", got, made)
	}
	again, made, err := e.p.RecordScrubFinding(e.ctx, tenant.ScrubFindingRecord{Fence: w.Fence(), Finding: f, Now: at(123)})
	if e.ok("RecordScrubFinding sent again", err); made || again.ID != got.ID {
		e.Errorf("the write sent again reads %+v (made %v)", again, made)
	}
	if got := fmt.Sprint(e.marked(scan.Path, ins)); got != "[true false false]" {
		e.Errorf("the marks read %s, want the instance marked again", got)
	}
}

// testScrubTalliesAndLast guards AddScrubTallies, which adds to a scrub
// job's tallies under its unit's fence and refuses an empty batch; and
// LastScrub, which answers a storage's scrub made last, a scrub of every
// storage counting for each, and none for a storage never scrubbed.
func testScrubTalliesAndLast(e *senv) {
	e.scrubSetup()
	if _, has, err := e.p.LastScrub(e.ctx, "store"); err != nil || has {
		e.Errorf("LastScrub of a storage never scrubbed = %v, %v", has, err)
	}
	e.createScrub("s1", "store", at(100))
	w := e.claimScrub("lister", at(100))
	_, err := e.p.StartJobUnit(e.ctx, w.Fence(), at(100))
	e.ok("StartJobUnit", err)
	e.ok("AddScrubTallies", e.p.AddScrubTallies(e.ctx, w.Fence(), map[string]int64{tenant.TallyStorageUnreadable: 2}))
	if got := e.tallies("s1"); got != "[{unreadable|storage 2}]" {
		e.Errorf("the job's tallies = %s", got)
	}
	e.wantKind("an empty batch of tallies", e.p.AddScrubTallies(e.ctx, w.Fence(), nil), fault.Invalid)
	last, has, err := e.p.LastScrub(e.ctx, "store")
	if e.ok("LastScrub", err); !has || last.ID != "s1" || last.Storage != "store" {
		e.Errorf("LastScrub = %+v, %v", last, has)
	}
	_, err = e.p.CancelJob(e.ctx, "s1", at(110), e.audit(), nil)
	e.ok("CancelJob", err)
	e.createScrub("s2", "", at(120))
	for _, s := range []string{"store", "cold"} {
		last, has, err = e.p.LastScrub(e.ctx, s)
		if e.ok("LastScrub", err); !has || last.ID != "s2" || last.Storage != "" {
			e.Errorf("LastScrub of %s after a scrub of every storage = %+v, %v", s, last, has)
		}
	}
}

// testScrubFindingsList guards the list of findings: by storage, then key,
// then kind; paged by a cursor that holds no key; filtered by kind, by
// storage, and by whether cleared ones are kept; a cursor used with another
// filter is refused.
func testScrubFindingsList(e *senv) {
	e.scrubSetup()
	e.createScrub("s1", "store", at(100))
	e.listScrub(at(100))
	w, scan, ins := e.scrubScan(at(110))
	var ids []string
	for i, in := range ins {
		kind := tenant.FindingDamaged
		if i == 1 {
			kind = tenant.FindingMissing
		}
		f := finding("s1", kind, scan.Path, in)
		e.record(w, f, at(120))
		ids = append(ids, f.ID)
	}
	_, _, err := e.p.ClearScrubFinding(e.ctx, w.Fence(), ids[2], at(130))
	e.ok("ClearScrubFinding", err)
	all := tenant.ScrubFindingFilter{IncludeCleared: true}
	p1, next, err := e.p.ListScrubFindings(e.ctx, all, firstPage(2))
	e.ok("ListScrubFindings", err)
	if len(p1) != 2 || next == "" || p1[0].Key != ins[0].Key || p1[1].Key != ins[1].Key {
		e.Fatalf("the first page = %+v, next %q", p1, next)
	}
	for _, in := range ins {
		if strings.Contains(next, in.SOPInstanceUID) {
			e.Error("a page's cursor holds an instance's UID")
		}
	}
	p2, last, err := e.p.ListScrubFindings(e.ctx, all, master.Page{Limit: 2, Cursor: next})
	e.ok("ListScrubFindings page 2", err)
	if len(p2) != 1 || last != "" || p2[0].ID != ids[2] || p2[0].Holds() {
		e.Errorf("the second page = %+v, next %q", p2, last)
	}
	_, _, err = e.p.ListScrubFindings(e.ctx, tenant.ScrubFindingFilter{}, master.Page{Limit: 2, Cursor: next})
	e.wantKind("a cursor with another filter", err, fault.Invalid)
	holding, _, err := e.p.ListScrubFindings(e.ctx, tenant.ScrubFindingFilter{}, firstPage(10))
	if e.ok("ListScrubFindings that hold", err); len(holding) != 2 {
		e.Errorf("the findings that hold = %d, want 2", len(holding))
	}
	kinds, _, err := e.p.ListScrubFindings(e.ctx, tenant.ScrubFindingFilter{Kinds: []tenant.ScrubFindingKind{tenant.FindingMissing}}, firstPage(10))
	if e.ok("ListScrubFindings of a kind", err); len(kinds) != 1 || kinds[0].ID != ids[1] {
		e.Errorf("the missing findings = %+v", kinds)
	}
	cold, _, err := e.p.ListScrubFindings(e.ctx, tenant.ScrubFindingFilter{Storage: "cold", IncludeCleared: true}, firstPage(10))
	if e.ok("ListScrubFindings of another storage", err); len(cold) != 0 {
		e.Errorf("the findings of an empty storage = %+v", cold)
	}
	_, _, err = e.p.ListScrubFindings(e.ctx, tenant.ScrubFindingFilter{Kinds: []tenant.ScrubFindingKind{"bad"}}, firstPage(10))
	e.wantKind("a filter of an unknown kind", err, fault.Invalid)
}

// testScrubFindingsRefused guards what a finding's write refuses: a unit of
// a job that is not a scrub, a lease taken over, a key past 1024 bytes, an
// orphan that states an expected size, a finding of an instance without its
// scan; and a lookup of marks past its bound.
func testScrubFindingsRefused(e *senv) {
	e.scrubSetup()
	e.addSender("orthanc")
	e.createJob("send", "orthanc", tenant.JobTarget{SubjectLabel: "sub-01"}, at(50))
	sw := e.claim("orthanc", "sender", at(60))
	e.createScrub("s1", "store", at(100))
	e.listScrub(at(100))
	w, scan, ins := e.scrubScan(at(110))
	f := finding("s1", tenant.FindingDamaged, scan.Path, ins[0])
	_, _, err := e.p.RecordScrubFinding(e.ctx, tenant.ScrubFindingRecord{Fence: sw.Fence(), Finding: f, Now: at(120)})
	e.wantPrecondition("a finding of a send job's unit", err)
	stale := w.Fence()
	stale.Attempt++
	_, _, err = e.p.RecordScrubFinding(e.ctx, tenant.ScrubFindingRecord{Fence: stale, Finding: f, Now: at(120)})
	e.wantIs("a finding of a lost lease", err, tenant.ErrLeaseLost, fault.Conflict)
	long := f
	long.Key = strings.Repeat("k", tenant.MaxScrubKeyLength+1)
	_, _, err = e.p.RecordScrubFinding(e.ctx, tenant.ScrubFindingRecord{Fence: w.Fence(), Finding: long, Now: at(120)})
	e.wantKind("a finding of a key too long", err, fault.Invalid)
	orphan := f
	orphan.Kind = tenant.FindingOrphan
	_, _, err = e.p.RecordScrubFinding(e.ctx, tenant.ScrubFindingRecord{Fence: w.Fence(), Finding: orphan, Now: at(120)})
	e.wantKind("an orphan stating what was expected", err, fault.Invalid)
	noScan := f
	noScan.Path = label.Path{}
	_, _, err = e.p.RecordScrubFinding(e.ctx, tenant.ScrubFindingRecord{Fence: w.Fence(), Finding: noScan, Now: at(120)})
	e.wantKind("an instance's finding without its scan", err, fault.Invalid)
	if marks, err := e.p.DamagedInstances(e.ctx, []string{ins[0].SOPInstanceUID}); err != nil || len(marks) != 0 {
		e.Errorf("after refused writes the marks are %v, %v", marks, err)
	}
	big := make([]string, tenant.MaxDamageLookup+1)
	for i := range big {
		big[i] = uidOf(3, i)
	}
	_, err = e.p.DamagedInstances(e.ctx, big)
	e.wantKind("a lookup of marks past its bound", err, fault.Invalid)
}
