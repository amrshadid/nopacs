package servecmd

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/core/record"
	"github.com/amrshadid/nopacs/internal/correct"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// retentionEngines returns the corrections engines over the rig as serve
// configures them for what retention needs: the tenant's storages opened by
// their rows, the rig's clock, a day's grace and the confirm key.
func retentionEngines(t *testing.T, r *workRig) *correct.Engines {
	t.Helper()
	storages := newTenantStorages(r.w.opener, oneTenant{}, "lab", r.db)
	t.Cleanup(func() { _ = storages.Close() })
	es, err := correct.NewEngines(correct.EnginesConfig{
		Tenants: oneTenantStore{st: r.db},
		Configure: func(context.Context, string, tenant.Corrections) (correct.Config, func() error, error) {
			return correct.Config{TenantID: "T0000000000001", Keys: r.keys, OpenStorage: storages.Open, Now: r.w.now,
				Grace:      func(context.Context) (time.Duration, error) { return 24 * time.Hour, nil },
				ConfirmKey: r.w.confirmKey}, nil, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = es.Stop(context.Background()) })
	return es
}

// oneTenantStore lists the one tenant lab and answers its database.
type oneTenantStore struct{ st tenantDB }

// Names answers lab.
func (oneTenantStore) Names(context.Context) ([]string, error) { return []string{"lab"}, nil }

// Store answers lab's database.
func (o oneTenantStore) Store(string) (tenant.Store, error) { return o.st, nil }

// TestRetentionsPassDisposesThroughTheOrdinaryDelete guards retention's
// daily pass as the server runs it per tenant: two sessions archived under
// a delete rule of one day, one of them held, and the pass run three days
// later by the server's clock. The due, unheld session becomes a delete
// correction made through the corrections engine, with origin retention,
// the rule's name and the server's reason, its records on the storage as
// every delete's are; the held one is skipped as held, its data untouched;
// the dispose job lists both with what became of each.
func TestRetentionsPassDisposesThroughTheOrdinaryDelete(t *testing.T) {
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
	scans, err := r.db.ListScans(r.ctx, tenant.ScanFilter{Order: tenant.OrderLabel}, master.Page{Limit: 1000})
	if err != nil {
		t.Fatal(err)
	}
	subjects := map[string]bool{}
	for _, s := range scans.Items {
		subjects[s.SubjectLabel] = true
	}
	if len(subjects) != 2 {
		t.Fatalf("the two studies made subjects %v, want two", subjects)
	}
	held := scans.Items[0].SubjectLabel
	target := tenant.HoldTarget{Subject: held}
	hs, err := r.db.HoldStorages(r.ctx, target)
	if err != nil {
		t.Fatal(err)
	}
	var written []string
	for _, h := range hs {
		written = append(written, h.Name)
	}
	if _, _, err := r.db.PlaceHold(r.ctx, tenant.HoldPlace{ID: mustCorrectionID(t), Target: target, Reason: "litigation", Written: written,
		Now: time.Now()}, r.audit(), nil); err != nil {
		t.Fatal(err)
	}
	r.w.setEngines(retentionEngines(t, r))
	r.w.now = func() time.Time { return time.Now().Add(72 * time.Hour) }
	ret := &tenantRetention{w: r.w, name: "lab", st: r.db, log: r.w.log}
	if err := ret.once(r.ctx); err != nil {
		t.Fatalf("the pass: %v", err)
	}
	jobs, _, err := r.db.ListJobs(r.ctx, tenant.JobFilter{}, master.Page{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	var dj tenant.DisposeJob
	for _, j := range jobs {
		if j.Kind == tenant.JobDispose {
			if dj, err = r.db.DisposeJob(r.ctx, j.ID); err != nil {
				t.Fatal(err)
			}
		}
	}
	if !dj.Scheduled || dj.Rule != "trial7" || len(dj.Sessions) != 1 {
		t.Fatalf("the pass's dispose job: %+v", dj)
	}
	s := dj.Sessions[0]
	if s.SubjectLabel == held || s.Outcome != tenant.DisposeDeleted || s.Correction == "" {
		t.Fatalf("the due session's outcome: %+v (held subject %s)", s, held)
	}
	c, err := r.db.Correction(r.ctx, s.Correction)
	if err != nil {
		t.Fatal(err)
	}
	if c.Kind != record.KindDelete || c.Origin != tenant.OriginRetention || c.RetentionRule != "trial7" ||
		c.Reason != "retention rule trial7, due "+s.Due.UTC().Format(time.DateOnly) || c.State != record.StateCommitted {
		t.Fatalf("the disposal's delete: %+v", c)
	}
	if f := r.files("main", "meta/.corrections/"+s.Correction); len(f) != 1 {
		t.Fatalf("the delete's record is not on the storage: %v", f)
	}
	// The held subject is not due-and-unheld, so the preview leaves it out;
	// it is counted held and its data is all still served.
	if got, _, err := r.db.ListHeldInstances(r.ctx, pathOfScan(scans.Items[0]), master.Page{Limit: 1000}); err != nil || len(got) == 0 {
		t.Fatalf("the held subject's instances: %d (%v)", len(got), err)
	}
}

// flakyRules is a tenant's database whose first listing of the retention
// rules fails, as a database that did not answer; it counts the listings,
// one per daily pass.
type flakyRules struct {
	tenantDB
	listed atomic.Int64
}

// ListRetentionRules fails the first time, then lists.
func (f *flakyRules) ListRetentionRules(ctx context.Context, now time.Time, page master.Page) ([]tenant.RetentionRule, string, error) {
	if f.listed.Add(1) == 1 {
		return nil, "", fault.New(fault.Unavailable, "the database did not answer")
	}
	return f.tenantDB.ListRetentionRules(ctx, now, page)
}

// TestAFailedDailyPassRunsAgainAtTheNextCheck guards retention's pass owed:
// the check that makes a pass records its time before the pass runs, so a
// pass that failed is run again at the next hourly check, not a day later;
// once one succeeds the next check makes no pass.
func TestAFailedDailyPassRunsAgainAtTheNextCheck(t *testing.T) {
	r := newWorkRig(t)
	r.w.setEngines(retentionEngines(t, r))
	db := &flakyRules{tenantDB: r.db}
	ret := &tenantRetention{w: r.w, name: "lab", st: db, base: r.db, log: r.w.log}
	if err := ret.once(r.ctx); err == nil {
		t.Fatal("the first pass did not fail")
	}
	if err := ret.once(r.ctx); err != nil {
		t.Fatalf("the next check: %v", err)
	}
	if n := db.listed.Load(); n != 2 {
		t.Fatalf("the rules were listed %d times by a failed pass and the next check, want 2 (the pass run again)", n)
	}
	if err := ret.once(r.ctx); err != nil {
		t.Fatal(err)
	}
	if n := db.listed.Load(); n != 2 {
		t.Fatalf("a check after a pass that succeeded listed the rules again (%d listings)", n)
	}
}

// mustCorrectionID returns a new operation id, the form a hold's id takes.
func mustCorrectionID(t *testing.T) string {
	t.Helper()
	id, err := tenant.NewCorrectionID(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// pathOfScan returns a catalog scan's labels.
func pathOfScan(s tenant.CatalogScan) label.Path {
	return label.Path{Subject: s.SubjectLabel, Session: s.SessionLabel, Scan: s.Label}
}
