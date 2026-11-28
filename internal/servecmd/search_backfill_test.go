package servecmd

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"path/filepath"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/checkedread"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/query"
	"github.com/amrshadid/nopacs/internal/searchfill"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// backfillColumns are the attributes the search backfill tests give back to
// the state of a column added late, by the columns each holds.
var backfillColumns = map[string][]string{
	"Modality":       {"modality", "modality_folded"},
	"Manufacturer":   {"manufacturer", "manufacturer_folded"},
	"SliceThickness": {"slice_thickness"},
}

// searchAnswers asks the searches the backfill tests compare and returns,
// for each, the scans it finds and the count, as the tenant answers now.
func (r *workRig) searchAnswers() map[string]string {
	r.t.Helper()
	out := map[string]string{}
	for _, cond := range []string{`Modality == "CT"`, `Modality == "MR"`, `Manufacturer present`, `SliceThickness present`,
		`Modality == "CT" and SliceThickness present`, `Manufacturer prefix "G"`} {
		node, err := query.Parse(cond)
		if err != nil {
			r.t.Fatal(err)
		}
		var paths []string
		q := tenant.SearchQuery{Level: query.LevelScan, Condition: node, Page: master.Page{Limit: 100}, Count: true, CountRows: 1000, Now: time.Now()}
		res, err := r.db.Search(r.ctx, q)
		if err != nil {
			r.t.Fatalf("Search %s: %v", cond, err)
		}
		for _, s := range res.Scans {
			paths = append(paths, s.SubjectLabel+"/"+s.SessionLabel+"/"+s.Label)
		}
		slices.Sort(paths)
		out[cond] = (&answer{paths, res.Total}).String()
	}
	return out
}

// answer is what a search found, for comparison and for a message.
type answer struct {
	paths []string
	total int64
}

// String writes the answer.
func (a *answer) String() string {
	s := ""
	for _, p := range a.paths {
		s += p + ";"
	}
	return s + "#" + strconv.FormatInt(a.total, 10)
}

// raw opens a connection of the test's own to the tenant's database, closed
// at the test's end.
func (r *workRig) raw() *sql.DB {
	r.t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(r.data, "tenants", "lab.db")+"?_pragma=busy_timeout(10000)")
	if err != nil {
		r.t.Fatal(err)
	}
	r.t.Cleanup(func() { _ = db.Close() })
	return db
}

// unfill puts the attributes in the state of columns added after the scans
// were archived: no value in any row, state building. The columns are set to
// NULL by a connection of the test's own, since the port keeps no way to
// make an attribute late.
func (r *workRig) unfill() {
	r.t.Helper()
	db := r.raw()
	for field, cols := range backfillColumns {
		for _, c := range cols {
			if _, err := db.ExecContext(r.ctx, `UPDATE scan_search SET `+c+` = NULL`); err != nil { // #nosec G202 -- the column names are this file's own.
				r.t.Fatal(err)
			}
		}
		if _, err := db.ExecContext(r.ctx, `UPDATE search_attributes SET state = 'building' WHERE field = ?`, field); err != nil {
			r.t.Fatal(err)
		}
	}
}

// stopAfter is a storage whose read of the n-th object cancels the context
// it is given, as a restart does between two pages of a backfill.
type stopAfter struct {
	blob.Store
	reads  *int
	n      int
	cancel context.CancelFunc
}

// Read counts the read and, at the n-th, cancels.
func (s stopAfter) Read(ctx context.Context, key string, offset, length int64) (io.ReadCloser, error) {
	*s.reads++
	if s.n > 0 && *s.reads == s.n {
		s.cancel()
	}
	return s.Store.Read(ctx, key, offset, length)
}

// stopStores wraps the storages a pass reads.
type stopStores struct {
	searchfill.Storages
	reads  *int
	n      int
	cancel context.CancelFunc
}

// Get returns the storage wrapped.
func (s stopStores) Get(ctx context.Context, name string) (blob.Store, error) {
	st, err := s.Storages.Get(ctx, name)
	if err != nil {
		return nil, err
	}
	return stopAfter{Store: st, reads: s.reads, n: s.n, cancel: s.cancel}, nil
}

// runner makes a backfill runner of one-scan pages over the rig's storages,
// counting the metadata reads in reads and canceling cancel at the n-th
// (never when n is 0).
func (r *workRig) runner(reads *int, n int, cancel context.CancelFunc) *searchfill.Runner {
	r.t.Helper()
	opener := r.w.opener
	opener.StaleUploadAge = -1
	tenantID := func(context.Context, string) (string, error) { return "T0000000000001", nil }
	run, err := searchfill.New(searchfill.Config{Store: r.db, Page: 1, Now: time.Now, Log: slog.New(slog.DiscardHandler),
		Storages: func(context.Context) (searchfill.Storages, error) {
			return stopStores{Storages: checkedread.NewStores(opener, tenantID, r.db, "lab"), reads: reads, n: n, cancel: cancel}, nil
		}})
	if err != nil {
		r.t.Fatal(err)
	}
	return run
}

// TestSearchBackfillFillsAnArchiveAndResumesAfterARestart guards the search
// backfill as the server runs it over a real reception: the attributes of a
// tenant archived by the pipeline are put back in the state of columns added
// late (empty, building), a search naming them is refused, and a job that is
// stopped after its first scan (as a restart does) goes on from the scan
// after it, with a runner made anew, reads each metadata file once, writes
// each scan once, and ends done with the attributes ready, after which
// every search answers as it did over the archive made with them.
func TestSearchBackfillFillsAnArchiveAndResumesAfterARestart(t *testing.T) {
	r := newWorkRig(t)
	p := r.pipeline()
	r.accept(p, workStudy...)
	r.accept(p, "MR_small.dcm")
	r.waitFor("the receptions to be archived", time.Minute, func() bool { return r.archived(2) })
	want := r.searchAnswers()
	scans, err := r.db.SearchBackfillScans(r.ctx, 0, 100)
	if err != nil || len(scans) < 3 {
		t.Fatalf("the archive holds %d scans (%v), want at least 3", len(scans), err)
	}
	if want[`Modality == "CT"`] == "#0" || want[`Manufacturer present`] == "#0" || want[`SliceThickness present`] == "#0" {
		t.Fatalf("the archive answers nothing to a search the backfill must restore: %v", want)
	}

	r.unfill()
	node, _ := query.Parse(`Modality == "CT"`)
	_, err = r.db.Search(r.ctx, tenant.SearchQuery{Level: query.LevelScan, Condition: node, Page: master.Page{Limit: 10}, Now: time.Now()})
	if fault.KindOf(err) != fault.Precondition {
		t.Fatalf("a search on an attribute being filled: %v, want a precondition failure", err)
	}
	job, err := r.db.QueueSearchBackfill(r.ctx, tenant.SearchBackfillQueue{ID: "sb-1", Field: "Modality", KeyID: "K1234567890a", Now: time.Now()}, r.audit(), nil)
	if err != nil {
		t.Fatal(err)
	}

	// The first pass stops after the first scan: its second read cancels.
	ctx, cancel := context.WithCancel(r.ctx)
	reads := 0
	res, err := r.runner(&reads, 2, cancel).Run(ctx, job.ID)
	cancel()
	if err == nil || res.Ended || res.Written != 1 {
		t.Fatalf("the stopped pass wrote %d scans, ended %v, error %v; want one scan, not ended, an error", res.Written, res.Ended, err)
	}
	mid, err := r.db.SearchBackfillJob(r.ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if mid.After != scans[0].ID || mid.Job.State != tenant.JobRunning || mid.Job.Counts.InstancesSent != 1 {
		t.Fatalf("the stopped job stands at scan %d, %s, %d written; want scan %d, running, one", mid.After, mid.Job.State, mid.Job.Counts.InstancesSent, scans[0].ID)
	}
	if _, err := r.db.Search(r.ctx, tenant.SearchQuery{Level: query.LevelScan, Condition: node, Page: master.Page{Limit: 10}, Now: time.Now()}); fault.KindOf(err) != fault.Precondition {
		t.Fatalf("a search while the backfill is half done: %v, want a precondition failure", err)
	}

	// A runner made anew goes on from the database's place.
	reads = 0
	res, err = r.runner(&reads, 0, func() {}).Run(r.ctx, job.ID)
	if err != nil || !res.Ended || res.Written != len(scans)-1 || reads != len(scans)-1 {
		t.Fatalf("the resumed pass wrote %d scans with %d reads, ended %v, error %v; want %d of each, ended", res.Written, reads, res.Ended, err, len(scans)-1)
	}
	ended, err := r.db.SearchBackfillJob(r.ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if ended.Job.State != tenant.JobDone || ended.Job.Counts.InstancesSent != int64(len(scans)) || ended.Skipped != 0 {
		t.Fatalf("the job is %s with %d scans written and %d skipped, want done with %d", ended.Job.State, ended.Job.Counts.InstancesSent, ended.Skipped, len(scans))
	}
	got := r.searchAnswers()
	for cond, w := range want {
		if got[cond] != w {
			t.Errorf("%s: after the backfill %q, as archived %q", cond, got[cond], w)
		}
	}
	// A pass over the ended job changes nothing and reads nothing.
	reads = 0
	if res, err := r.runner(&reads, 0, func() {}).Run(r.ctx, job.ID); err != nil || !res.Ended || reads != 0 {
		t.Errorf("a pass over an ended job: ended %v, %d reads, error %v", res.Ended, reads, err)
	}

	// The server's own loop takes a job it finds, as it does every few seconds.
	r.unfill()
	if _, err := r.db.QueueSearchBackfill(r.ctx, tenant.SearchBackfillQueue{ID: "sb-2", Field: "Manufacturer", KeyID: "K1234567890a", Now: time.Now()}, r.audit(), nil); err != nil {
		t.Fatal(err)
	}
	loop := &tenantSearchFills{w: r.w, name: "lab", st: r.db, log: slog.New(slog.DiscardHandler)}
	if err := loop.once(r.ctx); err != nil {
		t.Fatal(err)
	}
	second, err := r.db.SearchBackfillJob(r.ctx, "sb-2")
	if err != nil || second.Job.State != tenant.JobDone {
		t.Fatalf("the server's loop left the job %s (%v), want done", second.Job.State, err)
	}
	for cond, w := range want {
		if got := r.searchAnswers()[cond]; got != w {
			t.Errorf("%s: after the loop's backfill %q, as archived %q", cond, got, w)
		}
	}
}

// TestSearchBackfillReadsAPageAgainWhenAScanChanges guards a scan that
// changes between the read of its metadata and the write of its row (an
// edit or a new file changes its manifest, and wrote the row itself): the
// page is refused whole and read again, so the row written is the one the
// scan's metadata holds now, never the one read before the change.
func TestSearchBackfillReadsAPageAgainWhenAScanChanges(t *testing.T) {
	r := newWorkRig(t)
	p := r.pipeline()
	r.accept(p, workStudy...)
	r.accept(p, "MR_small.dcm")
	r.waitFor("the receptions to be archived", time.Minute, func() bool { return r.archived(2) })
	scans, err := r.db.SearchBackfillScans(r.ctx, 0, 100)
	if err != nil || len(scans) < 3 {
		t.Fatalf("the archive holds %d scans (%v), want at least 3", len(scans), err)
	}
	r.unfill()
	job, err := r.db.QueueSearchBackfill(r.ctx, tenant.SearchBackfillQueue{ID: "sb-1", Field: "Modality", KeyID: "K1234567890a", Now: time.Now()}, r.audit(), nil)
	if err != nil {
		t.Fatal(err)
	}
	// The first scan's manifest changes as its metadata is being read.
	change := func() {
		other := make([]byte, 32)
		other[0] = 7
		if _, err := r.raw().ExecContext(r.ctx, `UPDATE scans SET manifest_sha256 = ? WHERE id = ?`, other, scans[0].ID); err != nil {
			t.Error(err)
		}
	}
	reads := 0
	res, err := r.runner(&reads, 1, change).Run(r.ctx, job.ID)
	if err != nil || !res.Ended || reads != len(scans)+1 {
		t.Fatalf("the pass made %d reads for %d scans, ended %v, error %v; want one read more than the scans, ended", reads, len(scans), res.Ended, err)
	}
	done, err := r.db.SearchBackfillJob(r.ctx, job.ID)
	if err != nil || done.Job.State != tenant.JobDone || done.Job.Counts.InstancesSent != int64(len(scans)) {
		t.Fatalf("the job is %+v (%v), want done with %d scans written once each", done.Job, err, len(scans))
	}
}
