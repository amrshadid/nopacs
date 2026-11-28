package tenanttest

import (
	"slices"
	"strconv"
	"testing"

	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/query"
	"github.com/amrshadid/nopacs/internal/search"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// backfillField is the attribute the backfill tests add late: a number with
// an exact index, and no folded column.
const backfillField = "EchoTime"

// SearchHooks are what the search tests need of a store beyond its port,
// each given by the driver: the port cannot make an attribute late, write a
// row as an archive step does, or read every table of the database.
type SearchHooks struct {
	// Building puts an attribute of the store's search tables in state
	// building, as a migration that added its column to a tenant holding
	// scans does.
	Building func(tb testing.TB, p tenant.Pipeline, field string)
	// Exec runs a statement written with "?" placeholders.
	Exec func(tb testing.TB, p tenant.Pipeline, query string, args ...any)
	// CountText counts the rows of every table of the database, the
	// derived indexes included, that hold needle in any column.
	CountText func(tb testing.TB, p tenant.Pipeline, needle string) int
}

// RunSearchBackfill runs the contract of the tenant port's search backfill
// (tenant.SearchBackfills), and of the search rows through the corrections
// that touch them, against the stores open makes, with the hooks the driver
// gives.
func RunSearchBackfill(t *testing.T, open OpenPipeline, h SearchHooks) {
	old := func(f func(*penv, func(testing.TB, tenant.Pipeline, string))) func(*penv, SearchHooks) {
		return func(e *penv, h SearchHooks) { f(e, h.Building) }
	}
	tests := []struct {
		name string
		fn   func(e *penv, h SearchHooks)
	}{
		{"SearchBackfillQueue", old(testSearchBackfillQueue)},
		{"SearchBackfillFills", old(testSearchBackfillFills)},
		{"SearchBackfillResumes", old(testSearchBackfillResumes)},
		{"SearchBackfillKeepsNewerRows", old(testSearchBackfillKeepsNewerRows)},
		{"SearchBackfillEnds", old(testSearchBackfillEnds)},
		{"SearchBackfillHiddenScans", old(testSearchBackfillHiddenScans)},
		{"SearchBackfillRacesADelete", testSearchBackfillRacesADelete},
		{"SearchPurgeLeavesNoOldValue", testSearchPurgeLeavesNoOldValue},
		{"SearchUndoAfterLowerArrival", testSearchUndoAfterLowerArrival},
		{"SearchEditUndoAfterLowerArrival", testSearchEditUndoAfterLowerArrival},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			p := open(t, SuiteTenant)
			tt.fn(&penv{env: &env{T: t, s: p, ctx: t.Context()}, p: p, open: open}, h)
		})
	}
}

// echoOf returns the Echo Time of the i-th scan of the search fixture: ten
// times its place from one, none for the last two.
func echoOf(i int) (string, bool) {
	if i >= len(searchFixture)-2 {
		return "", false
	}
	return strconv.Itoa((i + 1) * 10), true
}

// backfillTenant archives the search fixture, each scan with the row of its
// header, which holds the Echo Time as the header does when withEcho, and
// lacks it otherwise, as a row written before the attribute was added does.
// It returns the fixture.
func (e *penv) backfillTenant(withEcho bool) []fixtureScan {
	e.Helper()
	e.addReceiver("recv-a", "store", 11112)
	fx := make([]fixtureScan, len(searchFixture))
	copy(fx, searchFixture)
	type key struct{ subject, session string }
	var order []key
	scans := map[key][]int{}
	for i := range fx {
		fx[i].vals = maps(fx[i].vals)
		if v, ok := echoOf(i); ok {
			fx[i].vals[backfillField] = v
		}
		fx[i].ds = dataSetOf(e, fx[i].vals)
		k := key{fx[i].subject, fx[i].session}
		if _, ok := scans[k]; !ok {
			order = append(order, k)
		}
		scans[k] = append(scans[k], i)
	}
	for n, k := range order {
		spec := studySpec{receiver: "recv-a", subject: k.subject, session: k.session, study: n + 1, at: 10 + 10*n, date: fx[scans[k][0]].date}
		for _, i := range scans[k] {
			vals := maps(fx[i].vals)
			if !withEcho {
				delete(vals, backfillField)
			}
			spec.scans = append(spec.scans, scanSpec{label: fx[i].label, modality: fx[i].vals["Modality"], description: fx[i].vals["SeriesDescription"],
				instances: 1, row: search.Extract(dataSetOf(e, vals), instanceOf(n+1, len(spec.scans), 0))})
		}
		e.archiveStudy(spec)
	}
	return fx
}

// maps returns a copy of m.
func maps(m map[string]string) map[string]string {
	out := make(map[string]string, len(m)+1)
	for k, v := range m {
		out[k] = v
	}
	return out
}

// backfillRows returns the rows a runner would read for the next limit
// scans after the place given: each scan's complete row, read from its
// header as the archive step reads a file, with the SOP Instance UID of the
// scan's one instance as its source.
func (e *penv) backfillRows(fx []fixtureScan, after int64, limit int) []tenant.SearchBackfillRow {
	e.Helper()
	scans, err := e.p.SearchBackfillScans(e.ctx, after, limit)
	e.ok("SearchBackfillScans", err)
	var rows []tenant.SearchBackfillRow
	for _, s := range scans {
		where := s.Path.Subject + "/" + s.Path.Session + "/" + s.Path.Scan
		i := slices.IndexFunc(fx, func(f fixtureScan) bool { return f.path() == where })
		if i < 0 {
			e.Fatalf("the archive holds scan %s the fixture does not", where)
		}
		rows = append(rows, tenant.SearchBackfillRow{ID: s.ID, Manifest: s.Manifest, Row: search.Extract(fx[i].ds, e.sourceOf(s))})
	}
	return rows
}

// sourceOf returns the SOP Instance UID of the one instance of the scan, the
// source its row was written from.
func (e *penv) sourceOf(s tenant.SearchBackfillScan) string {
	e.Helper()
	held, _, err := e.p.ListHeldInstances(e.ctx, s.Path, master.Page{Limit: 5})
	e.ok("ListHeldInstances", err)
	if len(held) != 1 {
		e.Fatalf("scan %v holds %d instances, want 1", s.Path, len(held))
	}
	return held[0].SOPInstanceUID
}

// queueBackfill queues a search backfill job of the late attribute.
func (e *penv) queueBackfill(id string) tenant.Job {
	e.Helper()
	j, err := e.p.QueueSearchBackfill(e.ctx, tenant.SearchBackfillQueue{ID: id, Field: backfillField, KeyID: "K1234567890a", Now: at(500)}, e.audit(), nil)
	e.ok("QueueSearchBackfill", err)
	return j
}

// scanQuery is a search of scans at the level, in pages of one hundred.
func (e *penv) scanQuery(cond string, level query.Level) (tenant.SearchResult, []string, error) {
	e.Helper()
	q := tenant.SearchQuery{Level: level, Condition: e.searchCondition(cond), Page: master.Page{Limit: 100}, Now: T0, Count: true, CountRows: 1000}
	r, err := e.p.Search(e.ctx, q)
	if err != nil {
		return r, nil, err
	}
	_, paths := e.searchAll(q, 3)
	return r, paths, nil
}

// backfillConditions are the searches the backfill tests ask of the late
// attribute, at the level each is for.
var backfillConditions = []struct {
	cond  string
	level query.Level
}{
	{`EchoTime == 30`, query.LevelScan},
	{`EchoTime between 25 and 55`, query.LevelScan},
	{`EchoTime > 70`, query.LevelScan},
	{`Modality == "MR" and EchoTime >= 40`, query.LevelScan},
	{`EchoTime present`, query.LevelScan},
	{`EchoTime absent`, query.LevelScan},
	{`has scan (EchoTime >= 60)`, query.LevelSession},
	{`has scan (EchoTime < 20)`, query.LevelSubject},
}

// testSearchBackfillQueue guards the making of a search backfill job: an
// attribute the search tables do not hold is invalid, one already ready has
// nothing to fill, one being filled makes a queued job that covers it and
// counts the scans, a second job is refused while the first has not ended,
// and a job id taken is a conflict.
func testSearchBackfillQueue(e *penv, building func(testing.TB, tenant.Pipeline, string)) {
	e.backfillTenant(false)
	ask := func(id, field string) (tenant.Job, error) {
		return e.p.QueueSearchBackfill(e.ctx, tenant.SearchBackfillQueue{ID: id, Field: field, KeyID: "K1234567890a", Now: at(500)}, e.audit(), nil)
	}
	_, err := ask("sb-bad", "NotAnAttribute")
	e.wantKind("an attribute the search tables lack", err, fault.Invalid)
	_, err = ask("sb-ready", backfillField)
	e.wantKind("an attribute that is ready", err, fault.Precondition)
	building(e.T, e.p, backfillField)
	j, err := ask("sb-1", backfillField)
	e.ok("QueueSearchBackfill", err)
	if j.Kind != tenant.JobSearchBackfill || j.State != tenant.JobQueued || j.Counts.Scans != int64(len(searchFixture)) {
		e.Fatalf("the job is %+v, want a queued search_backfill job over %d scans", j, len(searchFixture))
	}
	bj, err := e.p.SearchBackfillJob(e.ctx, "sb-1")
	e.ok("SearchBackfillJob", err)
	if !slices.Equal(bj.Fields, []string{backfillField}) || bj.After != 0 || bj.Skipped != 0 {
		e.Errorf("the job's part is %+v, want fields [%s] at the start", bj, backfillField)
	}
	_, err = ask("sb-2", backfillField)
	e.wantKind("a second job while the first has not ended", err, fault.Precondition)
	_, err = e.p.SearchBackfillJob(e.ctx, "sb-none")
	e.wantKind("a job that does not exist", err, fault.NotFound)
	if _, err := e.p.Job(e.ctx, "sb-1"); err != nil {
		e.Errorf("the job is not one of the tenant's jobs: %v", err)
	}
}

// testSearchBackfillFills guards the fill: a tenant whose attribute was
// added after its scans were archived refuses a search on it while it is
// building, and, once a job has read every scan and ended, answers every
// condition as a tenant archived with the attribute from the start does,
// in rows, order and counts, at the three levels; a search that does not
// name the attribute is answered throughout.
func testSearchBackfillFills(e *penv, building func(testing.TB, tenant.Pipeline, string)) {
	fx := e.backfillTenant(false)
	building(e.T, e.p, backfillField)
	fresh := e.fresh()
	fresh.backfillTenant(true)
	_, _, err := e.scanQuery(`EchoTime == 30`, query.LevelScan)
	e.wantKind("a search on an attribute being filled", err, fault.Precondition)
	if _, _, err := e.scanQuery(`Modality == "MR"`, query.LevelScan); err != nil {
		e.Errorf("a search that does not name the attribute: %v", err)
	}
	states, err := e.p.SearchAttributes(e.ctx)
	e.ok("SearchAttributes", err)
	for _, s := range states {
		if s.Ready == (s.Field == backfillField) {
			e.Errorf("attribute %s is ready %v while %s is being filled", s.Field, s.Ready, backfillField)
		}
	}
	j := e.queueBackfill("sb-1")
	after := int64(0)
	for pages := 0; ; pages++ {
		rows := e.backfillRows(fx, after, 3)
		if len(rows) == 0 {
			break
		}
		bj, err := e.p.ApplySearchBackfill(e.ctx, tenant.SearchBackfillApply{JobID: j.ID, After: after, Rows: rows, Now: at(501 + pages)})
		e.ok("ApplySearchBackfill", err)
		if bj.After != rows[len(rows)-1].ID || bj.Job.State != tenant.JobRunning {
			e.Fatalf("after a page the job is %+v, want running at scan %d", bj, rows[len(rows)-1].ID)
		}
		after = bj.After
	}
	ended, err := e.p.EndSearchBackfill(e.ctx, j.ID, at(600))
	e.ok("EndSearchBackfill", err)
	if ended.State != tenant.JobDone || ended.Counts.InstancesSent != int64(len(searchFixture)) || ended.Counts.InstancesRefused != 0 ||
		ended.Counts.ScansDone != int64(len(searchFixture)) {
		e.Fatalf("the ended job is %+v, want done with %d scans written", ended, len(searchFixture))
	}
	states, err = e.p.SearchAttributes(e.ctx)
	e.ok("SearchAttributes", err)
	for _, s := range states {
		if !s.Ready {
			e.Errorf("attribute %s is not ready after the backfill ended", s.Field)
		}
	}
	nonEmpty := 0
	for _, c := range backfillConditions {
		got, gotPaths, err := e.scanQuery(c.cond, c.level)
		e.ok("Search "+c.cond, err)
		want, wantPaths, err := fresh.scanQuery(c.cond, c.level)
		fresh.ok("fresh Search "+c.cond, err)
		if !slices.Equal(gotPaths, wantPaths) || got.Total != want.Total || got.AtLeast != want.AtLeast {
			e.Errorf("%s: backfilled %v (%d), fresh %v (%d)", c.cond, gotPaths, got.Total, wantPaths, want.Total)
		}
		if len(wantPaths) > 0 {
			nonEmpty++
		}
	}
	if nonEmpty < len(backfillConditions)-1 {
		e.Errorf("only %d of %d conditions matched a row: the comparison proves little", nonEmpty, len(backfillConditions))
	}
	// A row's other values are as the archive wrote them.
	a, apaths := e.searchAll(tenant.SearchQuery{Level: query.LevelScan, Condition: e.searchCondition(`SeriesDescription contains "mprage" and Manufacturer == "SIEMENS"`), Now: T0}, 50)
	b, bpaths := fresh.searchAll(tenant.SearchQuery{Level: query.LevelScan, Condition: fresh.searchCondition(`SeriesDescription contains "mprage" and Manufacturer == "SIEMENS"`), Now: T0}, 50)
	if !slices.Equal(apaths, bpaths) || len(a.Scans) != len(b.Scans) {
		e.Errorf("a search on other attributes answers %v after the backfill, %v fresh", apaths, bpaths)
	}
}

// fresh opens a second, empty store, as a tenant archived with the
// attribute from the start.
func (e *penv) fresh() *penv {
	e.Helper()
	p := e.open(e.T, SuiteTenant)
	return &penv{env: &env{T: e.T, s: p, ctx: e.ctx}, p: p, open: e.open}
}

// testSearchBackfillResumes guards a restart in the middle of a job and a
// page written twice: the job's place is the database's, so a runner that
// begins again from it finds the next page; a page that begins elsewhere
// than the job stands (written already, or by another runner) is refused
// and writes nothing; a page that is not in order of id, or empty, is
// invalid.
func testSearchBackfillResumes(e *penv, building func(testing.TB, tenant.Pipeline, string)) {
	fx := e.backfillTenant(false)
	building(e.T, e.p, backfillField)
	j := e.queueBackfill("sb-1")
	first := e.backfillRows(fx, 0, 4)
	bj, err := e.p.ApplySearchBackfill(e.ctx, tenant.SearchBackfillApply{JobID: j.ID, After: 0, Rows: first, Now: at(501)})
	e.ok("ApplySearchBackfill", err)
	// A runner that read the job's place after a restart finds it.
	got, err := e.p.SearchBackfillJob(e.ctx, j.ID)
	e.ok("SearchBackfillJob", err)
	if got.After != bj.After || got.After != first[len(first)-1].ID {
		e.Fatalf("the job stands at scan %d, want %d", got.After, first[len(first)-1].ID)
	}
	// The same page again, as a runner that never saw its answer would write
	// it, carrying values that would be wrong: refused, and nothing changes.
	poison := make([]tenant.SearchBackfillRow, len(first))
	for i, r := range first {
		poison[i] = tenant.SearchBackfillRow{ID: r.ID, Row: search.Empty()}
	}
	_, err = e.p.ApplySearchBackfill(e.ctx, tenant.SearchBackfillApply{JobID: j.ID, After: 0, Rows: poison, Now: at(502)})
	e.wantKind("a page that begins before the job's place", err, fault.Precondition)
	rest := e.backfillRows(fx, got.After, 100)
	_, err = e.p.ApplySearchBackfill(e.ctx, tenant.SearchBackfillApply{JobID: j.ID, After: rest[0].ID, Rows: rest[1:], Now: at(502)})
	e.wantKind("a page that begins after the job's place", err, fault.Precondition)
	_, err = e.p.ApplySearchBackfill(e.ctx, tenant.SearchBackfillApply{JobID: j.ID, After: got.After, Rows: nil, Now: at(502)})
	e.wantKind("an empty page", err, fault.Invalid)
	_, err = e.p.ApplySearchBackfill(e.ctx, tenant.SearchBackfillApply{JobID: j.ID, After: 0, Rows: []tenant.SearchBackfillRow{first[1], first[0]}, Now: at(502)})
	e.wantKind("a page out of order", err, fault.Invalid)
	// A scan whose manifest is no longer the one its row was read under was
	// changed meanwhile: the page is refused whole, writing nothing.
	changed := slices.Clone(rest)
	changed[len(changed)-1].Manifest = sum(4242)
	_, err = e.p.ApplySearchBackfill(e.ctx, tenant.SearchBackfillApply{JobID: j.ID, After: got.After, Rows: changed, Now: at(502)})
	e.wantIs("a page with a scan changed after it was read", err, tenant.ErrSearchBackfillStale, fault.Precondition)
	if again, err := e.p.SearchBackfillJob(e.ctx, j.ID); err != nil || again.After != got.After || again.Job.Counts.InstancesSent != int64(len(first)) {
		e.Errorf("a refused page moved the job to %+v (%v)", again, err)
	}
	_, err = e.p.EndSearchBackfill(e.ctx, j.ID, at(503))
	e.wantKind("an end with scans left to read", err, fault.Precondition)
	if st, _ := e.p.SearchAttributes(e.ctx); !slices.ContainsFunc(st, func(s tenant.SearchAttributeState) bool { return s.Field == backfillField && !s.Ready }) {
		e.Errorf("%s is ready while its backfill has scans left", backfillField)
	}
	_, err = e.p.ApplySearchBackfill(e.ctx, tenant.SearchBackfillApply{JobID: j.ID, After: got.After, Rows: rest, Now: at(504)})
	e.ok("the rest of the scans", err)
	ended, err := e.p.EndSearchBackfill(e.ctx, j.ID, at(505))
	e.ok("EndSearchBackfill", err)
	if ended.State != tenant.JobDone || ended.Counts.InstancesSent != int64(len(searchFixture)) {
		e.Errorf("the resumed job is %+v, want done with every scan written once", ended)
	}
	r, _, err := e.scanQuery(`EchoTime present`, query.LevelScan)
	e.ok("Search", err)
	if r.Total != int64(len(searchFixture)-2) {
		e.Errorf("%d scans have an Echo Time after the resumed backfill, want %d", r.Total, len(searchFixture)-2)
	}
	again, err := e.p.EndSearchBackfill(e.ctx, j.ID, at(506))
	e.ok("EndSearchBackfill again", err)
	if again.State != tenant.JobDone {
		e.Errorf("a job ended and ended again is %s", again.State)
	}
	_, err = e.p.ApplySearchBackfill(e.ctx, tenant.SearchBackfillApply{JobID: j.ID, After: got.After, Rows: rest, Now: at(507)})
	e.wantKind("a page for a job that ended", err, fault.Precondition)
}

// testSearchBackfillKeepsNewerRows guards a row an archive step wrote while
// the job ran: a row written from a file with a lower SOP Instance UID than
// the one the backfill read is kept, and one written from the same file, or
// from a higher one, takes the backfill's values.
func testSearchBackfillKeepsNewerRows(e *penv, building func(testing.TB, tenant.Pipeline, string)) {
	fx := e.backfillTenant(false)
	building(e.T, e.p, backfillField)
	j := e.queueBackfill("sb-1")
	rows := e.backfillRows(fx, 0, 100)
	with := func(echo, source string) search.Row {
		return search.Extract(dataSetOf(e, map[string]string{backfillField: echo, "Modality": "MR"}), source)
	}
	// The archived rows' sources are the scans' own instances, "2.25.3.…".
	// The first scan's page row was read from a file higher than the row's
	// (the archive wrote a lower one meanwhile): kept as it is. The second's
	// was read from the row's own file: replaced. The third's from a lower
	// file than the row's: replaced.
	rows[0].Row = with("777", "2.25.9")
	rows[1].Row = with("555", rows[1].Row.Source)
	rows[2].Row = with("333", "2.25.0")
	_, err := e.p.ApplySearchBackfill(e.ctx, tenant.SearchBackfillApply{JobID: j.ID, After: 0, Rows: rows, Now: at(501)})
	e.ok("ApplySearchBackfill", err)
	_, err = e.p.EndSearchBackfill(e.ctx, j.ID, at(502))
	e.ok("EndSearchBackfill", err)
	for echo, want := range map[string]int{"777": 0, "555": 1, "333": 1} {
		_, paths, err := e.scanQuery("EchoTime == "+echo, query.LevelScan)
		e.ok("Search", err)
		if len(paths) != want {
			e.Errorf("EchoTime == %s finds %v, want %d scans", echo, paths, want)
		}
	}
}

// testSearchBackfillEnds guards the ends of a job: a scan whose metadata
// could not be read is skipped, counted refused, keeps no value, and the job
// ends partial with the attribute ready; a job canceled writes nothing more
// and leaves the attribute being filled; a job stopped ends failed, with its
// reason, and leaves it too; and a scan deleted since the page was listed
// is not written.
func testSearchBackfillEnds(e *penv, building func(testing.TB, tenant.Pipeline, string)) {
	fx := e.backfillTenant(false)
	building(e.T, e.p, backfillField)
	j := e.queueBackfill("sb-1")
	rows := e.backfillRows(fx, 0, 100)
	rows[2].Skipped = true
	if _, err := e.p.ApplySearchBackfill(e.ctx, tenant.SearchBackfillApply{JobID: j.ID, After: 0, Rows: rows, Now: at(501)}); err != nil {
		e.Fatalf("ApplySearchBackfill: %v", err)
	}
	ended, err := e.p.EndSearchBackfill(e.ctx, j.ID, at(502))
	e.ok("EndSearchBackfill", err)
	if ended.State != tenant.JobPartial || ended.Counts.InstancesRefused != 1 || ended.Counts.InstancesSent != int64(len(searchFixture)-1) {
		e.Fatalf("the job is %+v, want partial with one scan refused", ended)
	}
	states, err := e.p.SearchAttributes(e.ctx)
	e.ok("SearchAttributes", err)
	for _, st := range states {
		if want := map[bool]int64{true: 1}[st.Field == backfillField]; st.ScansSkipped != want {
			e.Errorf("attribute %s says %d scans skipped, want %d", st.Field, st.ScansSkipped, want)
		}
	}
	bj, err := e.p.SearchBackfillJob(e.ctx, j.ID)
	e.ok("SearchBackfillJob", err)
	if bj.Skipped != 1 {
		e.Errorf("the job skipped %d scans, want 1", bj.Skipped)
	}
	r, _, err := e.scanQuery(`EchoTime present`, query.LevelScan)
	e.ok("Search", err)
	if r.Total != int64(len(searchFixture)-3) {
		e.Errorf("%d scans have an Echo Time, want %d: the skipped scan has none", r.Total, len(searchFixture)-3)
	}

	// A canceled job, and one stopped.
	e2 := e.fresh()
	fx2 := e2.backfillTenant(false)
	building(e.T, e2.p, backfillField)
	j2 := e2.queueBackfill("sb-2")
	first := e2.backfillRows(fx2, 0, 2)
	_, err = e2.p.ApplySearchBackfill(e2.ctx, tenant.SearchBackfillApply{JobID: j2.ID, After: 0, Rows: first, Now: at(501)})
	e2.ok("ApplySearchBackfill", err)
	canceled, err := e2.p.CancelJob(e2.ctx, j2.ID, at(502), e2.audit(), nil)
	e2.ok("CancelJob", err)
	if canceled.State != tenant.JobCanceled {
		e2.Fatalf("the job is %s, want canceled", canceled.State)
	}
	rest := e2.backfillRows(fx2, first[len(first)-1].ID, 100)
	_, err = e2.p.ApplySearchBackfill(e2.ctx, tenant.SearchBackfillApply{JobID: j2.ID, After: first[len(first)-1].ID, Rows: rest, Now: at(503)})
	e2.wantKind("a page for a canceled job", err, fault.Precondition)
	_, _, err = e2.scanQuery(`EchoTime == 30`, query.LevelScan)
	e2.wantKind("a search on an attribute whose backfill was canceled", err, fault.Precondition)
	// A new job takes the attribute over from the canceled one.
	j3 := e2.queueBackfill("sb-3")
	stopped, err := e2.p.StopSearchBackfill(e2.ctx, j3.ID, "the storage did not answer", at(504))
	e2.ok("StopSearchBackfill", err)
	if stopped.State != tenant.JobFailed || stopped.Reason != "the storage did not answer" {
		e2.Errorf("the job is %s (%q), want failed with the reason", stopped.State, stopped.Reason)
	}
	_, _, err = e2.scanQuery(`EchoTime == 30`, query.LevelScan)
	e2.wantKind("a search on an attribute whose backfill failed", err, fault.Precondition)
	_, err = e2.p.StopSearchBackfill(e2.ctx, j3.ID, "", at(505))
	e2.wantKind("a stop with no reason", err, fault.Invalid)
}

// testSearchBackfillHiddenScans guards the scans a delete hid and has not
// purged: the job lists them with the delete that hid them, writes their
// hidden rows (never a live row, which they have none of), and the undo of
// the delete then brings back a scan whose new column is filled, found by a
// search on it.
func testSearchBackfillHiddenScans(e *penv, building func(testing.TB, tenant.Pipeline, string)) {
	ev, ok := e.p.(EventStore)
	if !ok {
		e.Fatal("the store is not an event store")
	}
	c := &cenv{senv: &senv{eenv: &eenv{penv: e, ev: ev}}}
	c.setSearchRows()
	c.setupTwo()
	s1, s2, s3 := "sub-01/ses-1/scan-"+uidOf(2, 100), "sub-01/ses-1/scan-"+uidOf(2, 101), "sub-01/ses-2/scan-"+uidOf(2, 200)
	building(e.T, e.p, backfillField)
	del := c.prepareDelete(tenant.CorrectionTarget{Subject: "sub-01", Session: "ses-1", Scan: "scan-" + uidOf(2, 100)}, 500)
	del = c.commit(del.ID, 600, nil)
	j := e.queueBackfill("sb-1")
	if j.Counts.Scans != 3 {
		e.Errorf("the job covers %d scans, want the 2 held and the 1 hidden", j.Counts.Scans)
	}
	scans, err := e.p.SearchBackfillScans(e.ctx, 0, 100)
	e.ok("SearchBackfillScans", err)
	if len(scans) != 3 {
		e.Fatalf("the job lists %d scans, want 3", len(scans))
	}
	var rows []tenant.SearchBackfillRow
	hidden := 0
	for _, s := range scans {
		echo := "10"
		switch s.Path.Subject + "/" + s.Path.Session + "/" + s.Path.Scan {
		case s1:
			echo = "11"
		case s2:
			echo = "12"
		case s3:
			echo = "13"
		}
		if s.Correction != "" {
			hidden++
			if s.Correction != del.ID || s.Path.Scan != "scan-"+uidOf(2, 100) {
				e.Errorf("the hidden scan is %+v, want the scan the delete %s hid", s, del.ID)
			}
		}
		rows = append(rows, tenant.SearchBackfillRow{ID: s.ID, Correction: s.Correction, Manifest: s.Manifest,
			Row: search.Extract(dataSetOf(e, map[string]string{backfillField: echo, "Modality": "MR"}), "2.25.0")})
	}
	if hidden != 1 {
		e.Fatalf("%d of the job's scans are hidden, want 1", hidden)
	}
	_, err = e.p.ApplySearchBackfill(e.ctx, tenant.SearchBackfillApply{JobID: j.ID, After: 0, Rows: rows, Now: at(650)})
	e.ok("ApplySearchBackfill", err)
	ended, err := e.p.EndSearchBackfill(e.ctx, j.ID, at(660))
	e.ok("EndSearchBackfill", err)
	if ended.State != tenant.JobDone || ended.Counts.InstancesSent != 3 {
		e.Fatalf("the job is %+v, want done with 3 scans written", ended)
	}
	found := func(cond string) []string {
		_, paths := e.searchAll(tenant.SearchQuery{Level: query.LevelScan, Condition: e.searchCondition(cond), Now: T0}, 100)
		slices.Sort(paths)
		return paths
	}
	if got := found("EchoTime == 11"); len(got) != 0 {
		e.Errorf("the hidden scan is found while it is deleted: %v", got)
	}
	if got := found("EchoTime == 12"); !slices.Equal(got, []string{s2}) {
		e.Errorf("EchoTime == 12 finds %v, want %s", got, s2)
	}
	_, err = e.p.UndoCorrection(e.ctx, del.ID, "the wrong scan", at(700), e.audit(), nil)
	e.ok("UndoCorrection", err)
	if got := found("EchoTime == 11"); !slices.Equal(got, []string{s1}) {
		e.Errorf("after the undo EchoTime == 11 finds %v, want the scan brought back, %s", got, s1)
	}
}
