package tenanttest

import (
	"slices"
	"strconv"
	"testing"

	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// RunDeidCoverage runs the contract of the tenant port's coverage reports
// (tenant.DeidCoverages) against the stores open makes.
func RunDeidCoverage(t *testing.T, open OpenPipeline) {
	tests := []struct {
		name string
		fn   func(e *penv)
	}{
		{"DeidCoverageQueue", testDeidCoverageQueue},
		{"DeidCoverageCounts", testDeidCoverageCounts},
		{"DeidCoverageResumes", testDeidCoverageResumes},
		{"DeidCoverageReplacesTheReport", testDeidCoverageReplacesTheReport},
		{"DeidCoverageStops", testDeidCoverageStops},
		{"DeidCoverageValidates", testDeidCoverageValidates},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			p := open(t, SuiteTenant)
			tt.fn(&penv{env: &env{T: t, s: p, ctx: t.Context()}, p: p, open: open})
		})
	}
}

// queueCoverage queues a coverage job.
func (e *penv) queueCoverage(id string, now int) tenant.Job {
	e.Helper()
	j, err := e.p.QueueDeidCoverage(e.ctx, tenant.DeidCoverageQueue{ID: id, KeyID: "K1234567890a", Now: at(now)}, e.audit(), nil)
	e.ok("QueueDeidCoverage", err)
	return j
}

// coverageElement returns an element of the tenant's report for the tests: a
// standard element, a private one with a creator, and a private one whose
// creator is withheld, by n.
func coverageElement(n int) tenant.CoverageElement {
	switch n % 3 {
	case 0:
		return tenant.CoverageElement{CoverageKey: tenant.CoverageKey{Tag: uint32(0x00080070 + n), VR: "LO"}, Instances: 2, Occurrences: 3} //nolint:gosec // G115: n is a small test index.
	case 1:
		return tenant.CoverageElement{CoverageKey: tenant.CoverageKey{Tag: 0x00290010, Creator: "VENDOR " + strconv.Itoa(n), VR: "UN"}, Instances: 1, Occurrences: 1}
	}
	return tenant.CoverageElement{CoverageKey: tenant.CoverageKey{Tag: 0x00290010, Withheld: true, VR: "UN"}, Instances: 1, Occurrences: 2}
}

// coverageScans returns the rows of the next limit scans after the place,
// each read with two instances, one recorded, and one occurrence not listed.
func (e *penv) coverageScans(after int64, limit int) []tenant.DeidCoverageScanRow {
	e.Helper()
	scans, err := e.p.DeidCoverageScans(e.ctx, after, limit)
	e.ok("DeidCoverageScans", err)
	var rows []tenant.DeidCoverageScanRow
	for _, s := range scans {
		rows = append(rows, tenant.DeidCoverageScanRow{ID: s.ID, Instances: 2, Recorded: 1, Capped: 0, NotListed: 1})
	}
	return rows
}

// runCoverage counts every scan, in pages of three, giving each page the
// elements, and returns the ended job.
func (e *penv) runCoverage(id string, now int, elements []tenant.CoverageElement) tenant.Job {
	e.Helper()
	after := int64(0)
	for page := 0; ; page++ {
		rows := e.coverageScans(after, 3)
		if len(rows) == 0 {
			break
		}
		// The elements are given to the first page only: each page adds to what
		// the job holds, so the totals are the elements' once.
		var els []tenant.CoverageElement
		if page == 0 {
			els = elements
		}
		cj, err := e.p.ApplyDeidCoverage(e.ctx, tenant.DeidCoverageApply{JobID: id, After: after, Scans: rows, Elements: els, Now: at(now + page)})
		e.ok("ApplyDeidCoverage", err)
		after = cj.After
	}
	ended, err := e.p.EndDeidCoverage(e.ctx, id, at(now+100))
	e.ok("EndDeidCoverage", err)
	return ended
}

// testDeidCoverageQueue guards the making of a coverage job: a queued job of
// the kind that counts the tenant's scans, a second refused while the first
// has not ended, a job id taken a conflict, and a job that does not exist
// NotFound; with no job ended there is no report.
func testDeidCoverageQueue(e *penv) {
	e.backfillTenant(true)
	if _, found, err := e.p.DeidCoverageReport(e.ctx); err != nil || found {
		e.Fatalf("before any job: report found %v (%v), want none", found, err)
	}
	if _, found, err := e.p.DeidCoverageInProgress(e.ctx); err != nil || found {
		e.Fatalf("before any job: a job in progress %v (%v), want none", found, err)
	}
	j := e.queueCoverage("cv-1", 500)
	if j.Kind != tenant.JobDeidCoverage || j.State != tenant.JobQueued || j.Counts.Scans != int64(len(searchFixture)) {
		e.Fatalf("the job is %+v, want a queued deid_coverage job over %d scans", j, len(searchFixture))
	}
	cj, err := e.p.DeidCoverageJob(e.ctx, "cv-1")
	e.ok("DeidCoverageJob", err)
	if cj.After != 0 || cj.Skipped != 0 || cj.Instances != 0 || cj.Elements != 0 {
		e.Errorf("the job's part is %+v, want the start", cj)
	}
	if run, found, err := e.p.DeidCoverageInProgress(e.ctx); err != nil || !found || run.ID != "cv-1" {
		e.Errorf("in progress: %+v %v (%v), want cv-1", run, found, err)
	}
	_, err = e.p.QueueDeidCoverage(e.ctx, tenant.DeidCoverageQueue{ID: "cv-2", Now: at(501)}, e.audit(), nil)
	e.wantKind("a second job while the first has not ended", err, fault.Precondition)
	_, err = e.p.DeidCoverageJob(e.ctx, "cv-none")
	e.wantKind("a job that does not exist", err, fault.NotFound)
	if _, found, err := e.p.DeidCoverageReport(e.ctx); err != nil || found {
		e.Errorf("a job not ended is a report: found %v (%v)", found, err)
	}
}

// testDeidCoverageCounts guards the counting: the elements of the pages add
// up, the job's counts are of scans read, and, once the job ended, the report
// is its elements in order of tag, creator, withheld and VR, paged by the
// last element, with the counts of the job and no element lost.
func testDeidCoverageCounts(e *penv) {
	e.backfillTenant(true)
	j := e.queueCoverage("cv-1", 500)
	elements := []tenant.CoverageElement{coverageElement(0), coverageElement(1), coverageElement(2), coverageElement(4)}
	ended := e.runCoverage(j.ID, 501, elements)
	n := int64(len(searchFixture))
	if ended.State != tenant.JobDone || ended.Counts.InstancesSent != n || ended.Counts.ScansDone != n || ended.Counts.InstancesRefused != 0 {
		e.Fatalf("the ended job is %+v, want done with %d scans read", ended, n)
	}
	rep, found, err := e.p.DeidCoverageReport(e.ctx)
	e.ok("DeidCoverageReport", err)
	if !found || rep.JobID != j.ID || rep.Partial || rep.Scans != n || rep.ScansSkipped != 0 || rep.Instances != 2*n || rep.Recorded != n || rep.NotListed != n || rep.Elements != 4 {
		e.Fatalf("the report is %+v (found %v), want %d scans, %d instances, %d recorded, %d not listed, 4 elements", rep, found, n, 2*n, n, n)
	}
	var got []tenant.CoverageElement
	var after *tenant.CoverageKey
	for {
		page, err := e.p.DeidCoverageElements(e.ctx, j.ID, after, 3)
		e.ok("DeidCoverageElements", err)
		if len(page) == 0 {
			break
		}
		got = append(got, page...)
		k := page[len(page)-1].CoverageKey
		after = &k
	}
	if len(got) != 4 {
		e.Fatalf("the report lists %d elements %v, want 4", len(got), got)
	}
	for i := 1; i < len(got); i++ {
		a, b := got[i-1].CoverageKey, got[i].CoverageKey
		if a.Tag > b.Tag || (a.Tag == b.Tag && a.Creator > b.Creator) {
			e.Errorf("elements %v and %v are out of order", a, b)
		}
	}
	for _, want := range elements {
		if i := slices.IndexFunc(got, func(c tenant.CoverageElement) bool { return c.CoverageKey == want.CoverageKey }); i < 0 || got[i] != want {
			e.Errorf("the element %+v is not in the report as it was counted: %+v", want, got)
		}
	}
}

// testDeidCoverageResumes guards a restart in the middle of a job and a page
// written twice: the job's place is the database's; a page that begins
// elsewhere than the job stands is refused and writes nothing, its elements
// with it; an element given on two pages is added up; and an end with scans
// left is refused.
func testDeidCoverageResumes(e *penv) {
	e.backfillTenant(true)
	j := e.queueCoverage("cv-1", 500)
	first := e.coverageScans(0, 4)
	elem := coverageElement(0)
	cj, err := e.p.ApplyDeidCoverage(e.ctx, tenant.DeidCoverageApply{JobID: j.ID, After: 0, Scans: first, Elements: []tenant.CoverageElement{elem}, Now: at(501)})
	e.ok("ApplyDeidCoverage", err)
	if cj.Job.State != tenant.JobRunning || cj.After != first[len(first)-1].ID || cj.Elements != 1 || cj.Instances != 8 || cj.Recorded != 4 {
		e.Fatalf("after a page the job is %+v, want running at scan %d with 1 element, 8 instances, 4 recorded", cj, first[len(first)-1].ID)
	}
	got, err := e.p.DeidCoverageJob(e.ctx, j.ID)
	e.ok("DeidCoverageJob", err)
	if got.After != cj.After {
		e.Fatalf("the job stands at scan %d, want %d", got.After, cj.After)
	}
	// The same page again, as a runner that never saw its answer would write
	// it: refused, and nothing is added.
	_, err = e.p.ApplyDeidCoverage(e.ctx, tenant.DeidCoverageApply{JobID: j.ID, After: 0, Scans: first, Elements: []tenant.CoverageElement{elem}, Now: at(502)})
	e.wantKind("a page that begins before the job's place", err, fault.Precondition)
	rest := e.coverageScans(got.After, 100)
	_, err = e.p.ApplyDeidCoverage(e.ctx, tenant.DeidCoverageApply{JobID: j.ID, After: rest[0].ID, Scans: rest[1:], Now: at(502)})
	e.wantKind("a page that begins after the job's place", err, fault.Precondition)
	_, err = e.p.EndDeidCoverage(e.ctx, j.ID, at(503))
	e.wantKind("an end with scans left to read", err, fault.Precondition)
	again, err := e.p.ApplyDeidCoverage(e.ctx, tenant.DeidCoverageApply{JobID: j.ID, After: got.After, Scans: rest, Elements: []tenant.CoverageElement{elem}, Now: at(504)})
	e.ok("ApplyDeidCoverage the rest", err)
	if again.Elements != 1 || again.Instances != 2*int64(len(searchFixture)) {
		e.Errorf("after every page the job is %+v, want 1 element and %d instances", again, 2*len(searchFixture))
	}
	ended, err := e.p.EndDeidCoverage(e.ctx, j.ID, at(505))
	e.ok("EndDeidCoverage", err)
	if ended.State != tenant.JobDone {
		e.Fatalf("the ended job is %+v", ended)
	}
	els, err := e.p.DeidCoverageElements(e.ctx, j.ID, nil, 10)
	e.ok("DeidCoverageElements", err)
	if len(els) != 1 || els[0].Instances != 4 || els[0].Occurrences != 6 {
		e.Errorf("the element given on two pages is %+v, want 4 instances and 6 occurrences added up", els)
	}
	// An ended job takes no page, and ends again as it is.
	_, err = e.p.ApplyDeidCoverage(e.ctx, tenant.DeidCoverageApply{JobID: j.ID, After: again.After, Scans: []tenant.DeidCoverageScanRow{{ID: again.After + 1}}, Now: at(506)})
	e.wantKind("a page after the end", err, fault.Precondition)
	if again, err := e.p.EndDeidCoverage(e.ctx, j.ID, at(507)); err != nil || again.State != tenant.JobDone {
		e.Errorf("a second end: %+v (%v), want the job as it is", again, err)
	}
}

// testDeidCoverageReplacesTheReport guards the report's replacement: a
// second job's end makes its elements the report and drops the first's, and
// a job that skipped a scan ends partial, said by the report.
func testDeidCoverageReplacesTheReport(e *penv) {
	e.backfillTenant(true)
	e.runCoverage(e.queueCoverage("cv-1", 500).ID, 501, []tenant.CoverageElement{coverageElement(0), coverageElement(1)})
	j := e.queueCoverage("cv-2", 700)
	rows := e.coverageScans(0, 100)
	rows[0] = tenant.DeidCoverageScanRow{ID: rows[0].ID, Skipped: true}
	_, err := e.p.ApplyDeidCoverage(e.ctx, tenant.DeidCoverageApply{JobID: j.ID, After: 0, Scans: rows, Elements: []tenant.CoverageElement{coverageElement(2)}, Now: at(701)})
	e.ok("ApplyDeidCoverage", err)
	// While the second job runs the first's report stands.
	if rep, found, err := e.p.DeidCoverageReport(e.ctx); err != nil || !found || rep.JobID != "cv-1" || rep.Elements != 2 {
		e.Fatalf("while the second job runs the report is %+v (found %v, %v), want the first's", rep, found, err)
	}
	// The second job ends at the very instant the first did (a coarse clock):
	// the report is still the second's, which is the one that ended last.
	ended, err := e.p.EndDeidCoverage(e.ctx, j.ID, at(601))
	e.ok("EndDeidCoverage", err)
	n := int64(len(searchFixture))
	if ended.State != tenant.JobPartial || ended.Counts.InstancesRefused != 1 || ended.Counts.InstancesSent != n-1 {
		e.Fatalf("the ended job is %+v, want partial with 1 scan skipped", ended)
	}
	rep, found, err := e.p.DeidCoverageReport(e.ctx)
	e.ok("DeidCoverageReport", err)
	if !found || rep.JobID != "cv-2" || !rep.Partial || rep.ScansSkipped != 1 || rep.Scans != n-1 || rep.Elements != 1 {
		e.Fatalf("the report is %+v (found %v), want the second job's, partial, 1 scan skipped, 1 element", rep, found)
	}
	if old, err := e.p.DeidCoverageElements(e.ctx, "cv-1", nil, 10); err != nil || len(old) != 0 {
		e.Errorf("the first job's elements are %v (%v), want none once the second ended", old, err)
	}
	if now, err := e.p.DeidCoverageElements(e.ctx, "cv-2", nil, 10); err != nil || len(now) != 1 || now[0].CoverageKey != coverageElement(2).CoverageKey {
		e.Errorf("the second job's elements are %v (%v), want its one", now, err)
	}
}

// testDeidCoverageStops guards a job stopped: it ends failed with its reason,
// its elements are dropped, and the report before it stands.
func testDeidCoverageStops(e *penv) {
	e.backfillTenant(true)
	e.runCoverage(e.queueCoverage("cv-1", 500).ID, 501, []tenant.CoverageElement{coverageElement(0)})
	j := e.queueCoverage("cv-2", 700)
	rows := e.coverageScans(0, 2)
	_, err := e.p.ApplyDeidCoverage(e.ctx, tenant.DeidCoverageApply{JobID: j.ID, After: 0, Scans: rows, Elements: []tenant.CoverageElement{coverageElement(1)}, Now: at(701)})
	e.ok("ApplyDeidCoverage", err)
	_, err = e.p.StopDeidCoverage(e.ctx, j.ID, "", at(702))
	e.wantKind("a stop with no reason", err, fault.Invalid)
	stopped, err := e.p.StopDeidCoverage(e.ctx, j.ID, "storage down", at(702))
	e.ok("StopDeidCoverage", err)
	if stopped.State != tenant.JobFailed || stopped.Reason != "storage down" {
		e.Fatalf("the stopped job is %+v, want failed with its reason", stopped)
	}
	if els, err := e.p.DeidCoverageElements(e.ctx, j.ID, nil, 10); err != nil || len(els) != 0 {
		e.Errorf("the stopped job's elements are %v (%v), want none", els, err)
	}
	rep, found, err := e.p.DeidCoverageReport(e.ctx)
	e.ok("DeidCoverageReport", err)
	if !found || rep.JobID != "cv-1" {
		e.Errorf("the report is %+v (found %v), want the first job's still", rep, found)
	}
	if els, err := e.p.DeidCoverageElements(e.ctx, "cv-1", nil, 10); err != nil || len(els) != 1 {
		e.Errorf("the first job's elements are %v (%v), want its one", els, err)
	}
	if again, err := e.p.StopDeidCoverage(e.ctx, j.ID, "again", at(703)); err != nil || again.Reason != "storage down" {
		e.Errorf("a second stop: %+v (%v), want the job as it is", again, err)
	}
	// A job is queued again once the stopped one ended.
	e.queueCoverage("cv-3", 800)
}

// testDeidCoverageValidates guards what a page may hold: scans in order of
// id and past its start, counts that agree, elements listed once with a VR
// and a creator that can be written, and a page of at most a bound.
func testDeidCoverageValidates(e *penv) {
	e.backfillTenant(true)
	j := e.queueCoverage("cv-1", 500)
	rows := e.coverageScans(0, 2)
	ask := func(what string, a tenant.DeidCoverageApply) {
		e.Helper()
		a.JobID, a.Now = j.ID, at(501)
		if a.Scans == nil {
			a.Scans = rows
		}
		_, err := e.p.ApplyDeidCoverage(e.ctx, a)
		e.wantKind(what, err, fault.Invalid)
	}
	ask("an empty page", tenant.DeidCoverageApply{Scans: []tenant.DeidCoverageScanRow{}})
	ask("scans out of order", tenant.DeidCoverageApply{Scans: []tenant.DeidCoverageScanRow{rows[1], rows[0]}})
	ask("more recorded than read", tenant.DeidCoverageApply{Scans: []tenant.DeidCoverageScanRow{{ID: rows[0].ID, Instances: 1, Recorded: 2}}})
	ask("a skipped scan with counts", tenant.DeidCoverageApply{Scans: []tenant.DeidCoverageScanRow{{ID: rows[0].ID, Skipped: true, Instances: 1}}})
	good := coverageElement(1)
	ask("an element listed twice", tenant.DeidCoverageApply{Elements: []tenant.CoverageElement{good, good}})
	bad := good
	bad.VR = "lo"
	ask("a VR in lower case", tenant.DeidCoverageApply{Elements: []tenant.CoverageElement{bad}})
	bad = good
	bad.Creator = "a\x00b"
	ask("a creator that is not printable", tenant.DeidCoverageApply{Elements: []tenant.CoverageElement{bad}})
	bad = good
	bad.Withheld = true
	ask("a creator both given and withheld", tenant.DeidCoverageApply{Elements: []tenant.CoverageElement{bad}})
	bad = coverageElement(0)
	bad.Creator = "VENDOR"
	ask("a creator on a standard element", tenant.DeidCoverageApply{Elements: []tenant.CoverageElement{bad}})
	bad = good
	bad.Tag = 0x00290110
	ask("a private element with a block number", tenant.DeidCoverageApply{Elements: []tenant.CoverageElement{bad}})
	bad = good
	bad.Occurrences = 0
	ask("fewer occurrences than instances", tenant.DeidCoverageApply{Elements: []tenant.CoverageElement{bad}})
	if cj, err := e.p.DeidCoverageJob(e.ctx, j.ID); err != nil || cj.After != 0 || cj.Elements != 0 {
		e.Errorf("a refused page changed the job: %+v (%v)", cj, err)
	}
	_, err := e.p.DeidCoverageElements(e.ctx, j.ID, nil, 0)
	e.wantKind("a page of no elements", err, fault.Invalid)
	_, err = e.p.DeidCoverageScans(e.ctx, 0, tenant.MaxDeidCoveragePage+1)
	e.wantKind("a page of too many scans", err, fault.Invalid)
}
