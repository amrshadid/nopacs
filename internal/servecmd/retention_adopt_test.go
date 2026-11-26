package servecmd

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/holds"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// activatedDB is a tenant's database whose catalog a test activates again
// (its event feed's history id moved, as a rebuild's replace or a restore
// moves it) and whose storages' listing a test makes fail, as storages
// that do not answer fail the reading of their hold records.
type activatedDB struct {
	tenantDB
	history atomic.Value
	fail    atomic.Bool
}

// EventHistory answers the history id the test set, or the database's.
func (a *activatedDB) EventHistory(ctx context.Context) (string, error) {
	if h, ok := a.history.Load().(string); ok {
		return h, nil
	}
	return holds.ActivationOf(ctx, a.tenantDB)
}

// ListStorages fails while fail is set.
func (a *activatedDB) ListStorages(ctx context.Context, page master.Page) ([]tenant.Storage, string, error) {
	if a.fail.Load() {
		return nil, "", fault.New(fault.Unavailable, "the storages do not answer")
	}
	return a.tenantDB.ListStorages(ctx, page)
}

// TestRetentionWaitsForAnAdoptionAfterEachActivation guards the holds
// adopted before a disposal after the catalog is activated again at
// runtime: retention adopted the holds at its start; then a replace makes
// the catalog again (its history id moves) whose database lacks a hold
// that a record on the storage holds, and reading the storages' records
// fails. While it fails, no check disposes of anything, though both
// sessions are due; once the records are read, the hold is adopted and
// only the unheld session is deleted.
func TestRetentionWaitsForAnAdoptionAfterEachActivation(t *testing.T) {
	r := newWorkRig(t)
	p := r.pipeline()
	r.accept(p, workStudy...)
	r.accept(p, "MR_small.dcm")
	r.waitFor("both receptions to be archived", time.Minute, func() bool { return r.archived(2) })
	for name, value := range map[string]string{"retention.shorten-notice": "1d", "retention.max-clock-step": "30d"} {
		if err := r.db.SetSetting(r.ctx, name, value, r.audit(), nil); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := r.db.AddRetentionRule(r.ctx, tenant.RetentionRule{Name: "trial7", DisposeAfter: tenant.RetentionPeriod{Days: 1},
		Action: tenant.RetentionDelete}, tenant.RetentionChange{Now: time.Now(), Notice: 24 * time.Hour}, r.audit(), nil); err != nil {
		t.Fatal(err)
	}
	r.w.setEngines(retentionEngines(t, r))
	db := &activatedDB{tenantDB: r.db}
	ret := &tenantRetention{w: r.w, name: "lab", st: db, base: r.db, log: r.w.log}
	r.w.now = time.Now
	if err := ret.once(r.ctx); err != nil {
		t.Fatalf("the check at the start: %v", err)
	}
	if fenced, err := holds.ActivationOf(r.ctx, fenceDB(r.db, func() error { return nil })); err != nil || fenced == "" {
		t.Fatalf("the fenced database answers the history id %q (%v), want the database's", fenced, err)
	}
	scans, err := r.db.ListScans(r.ctx, tenant.ScanFilter{Order: tenant.OrderLabel}, master.Page{Limit: 1000})
	if err != nil {
		t.Fatal(err)
	}
	held := scans.Items[0].SubjectLabel
	lost := putLostHoldRecord(t, r, "main", held)
	db.history.Store("a-history-made-by-the-replace")
	db.fail.Store(true)
	r.w.now = func() time.Time { return time.Now().Add(72 * time.Hour) }
	if err := ret.once(r.ctx); err == nil {
		t.Fatal("a check whose adoption after the activation failed answered no error")
	}
	if jobs := disposeJobs(t, r); len(jobs) != 0 {
		t.Fatalf("a check whose adoption after the activation failed queued %+v", jobs)
	}
	db.fail.Store(false)
	if err := ret.once(r.ctx); err != nil {
		t.Fatalf("the check once the records are read: %v", err)
	}
	if h, err := r.db.Hold(r.ctx, lost); err != nil || h.State != tenant.HoldActive || !h.FoundOnStorage {
		t.Fatalf("the hold found on storage after the activation is %+v (%v)", h, err)
	}
	deleted := 0
	for _, dj := range disposeJobs(t, r) {
		for _, s := range dj.Sessions {
			if s.SubjectLabel == held && s.Outcome == tenant.DisposeDeleted {
				t.Fatalf("the held subject's session was deleted: %+v", s)
			}
			if s.Outcome == tenant.DisposeDeleted {
				deleted++
			}
		}
	}
	if deleted != 1 {
		t.Fatalf("%d sessions were deleted, want the unheld one", deleted)
	}
}
