package servecmd

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/core/record"
	"github.com/amrshadid/nopacs/internal/correct"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// disposalRig archives the files named, adds the delete rule trial7 that
// disposes a day after archive, and gives the worker its corrections
// engines; the caller moves the worker's clock past the due time.
func disposalRig(t *testing.T, files ...string) (*workRig, *correct.Engines) {
	t.Helper()
	r := newWorkRig(t)
	p := r.pipeline()
	r.accept(p, files...)
	r.waitFor("archived", time.Minute, func() bool { return r.archived(1) })
	for name, value := range map[string]string{"retention.shorten-notice": "1d", "retention.max-clock-step": "30d"} {
		if err := r.db.SetSetting(r.ctx, name, value, r.audit(), nil); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := r.db.AddRetentionRule(r.ctx, tenant.RetentionRule{Name: "trial7", DisposeAfter: tenant.RetentionPeriod{Days: 1},
		Action: tenant.RetentionDelete}, tenant.RetentionChange{Now: time.Now(), Notice: 24 * time.Hour}, r.audit(), nil); err != nil {
		t.Fatal(err)
	}
	es := retentionEngines(t, r)
	r.w.setEngines(es)
	return r, es
}

// disposeJobs returns every dispose job of the rig's tenant, oldest first.
func disposeJobs(t *testing.T, r *workRig) []tenant.DisposeJob {
	t.Helper()
	jobs, _, err := r.db.ListJobs(r.ctx, tenant.JobFilter{}, master.Page{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	var out []tenant.DisposeJob
	for i := len(jobs) - 1; i >= 0; i-- {
		if jobs[i].Kind != tenant.JobDispose {
			continue
		}
		dj, err := r.db.DisposeJob(r.ctx, jobs[i].ID)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, dj)
	}
	return out
}

// cancelOnRead is the tenant's database with each dispose job canceled the
// first time the runner reads it, after the read: the cancel lands between
// the runner's read of the job and its delete of the first session.
type cancelOnRead struct {
	tenantDB
	t     *testing.T
	audit master.AuditEntry
	once  *sync.Once
}

// DisposeJob answers the job as it was read, then cancels it.
func (c cancelOnRead) DisposeJob(ctx context.Context, id string) (tenant.DisposeJob, error) {
	dj, err := c.tenantDB.DisposeJob(ctx, id)
	if err != nil {
		return dj, err
	}
	c.once.Do(func() {
		if _, err := c.CancelJob(ctx, id, time.Now(), c.audit, nil); err != nil {
			c.t.Errorf("CancelJob: %v", err)
		}
	})
	return dj, nil
}

// TestACanceledDisposalDeletesNothingMore guards a cancel of a dispose job
// that lands after the runner read the job: the delete of its session is
// refused in the delete's own transaction, nothing is deleted and no
// outcome recorded, and the job stays canceled.
func TestACanceledDisposalDeletesNothingMore(t *testing.T) {
	r, _ := disposalRig(t, "MR_small.dcm")
	r.w.now = func() time.Time { return time.Now().Add(72 * time.Hour) }
	ret := &tenantRetention{w: r.w, name: "lab", st: cancelOnRead{tenantDB: r.db, t: t, audit: r.audit(), once: &sync.Once{}}, base: r.db,
		log: r.w.log}
	if err := ret.once(r.ctx); err != nil {
		t.Fatalf("the pass: %v", err)
	}
	jobs := disposeJobs(t, r)
	if len(jobs) != 1 || len(jobs[0].Sessions) != 1 {
		t.Fatalf("the dispose jobs are %+v, want one listing the session", jobs)
	}
	dj := jobs[0]
	if dj.Job.State != tenant.JobCanceled {
		t.Fatalf("the dispose job is %s, want canceled", dj.Job.State)
	}
	if s := dj.Sessions[0]; s.Outcome != tenant.DisposePending || s.Correction != "" {
		t.Fatalf("the canceled job's session is %+v, want no outcome and no delete", s)
	}
	cs, _, err := r.db.ListCorrections(r.ctx, tenant.CorrectionFilter{}, master.Page{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) != 0 {
		t.Fatalf("a canceled dispose job made corrections %+v", cs)
	}
	if !r.archived(1) {
		t.Fatal("the session the canceled job listed is no longer archived")
	}
}

// TestAnOverlappedSessionIsSkippedAndTheJobEnds guards the daily pass
// against a session another correction holds (a person's instance delete,
// committed and in its grace): its delete is refused as overlapping, which
// is a skip with that reason, not a session left for ever without an
// outcome, so the dispose job ends and the rule's next pass queues
// another.
func TestAnOverlappedSessionIsSkippedAndTheJobEnds(t *testing.T) {
	r, es := disposalRig(t, workStudy...)
	scans, err := r.db.ListScans(r.ctx, tenant.ScanFilter{Order: tenant.OrderLabel}, master.Page{Limit: 1000})
	if err != nil || len(scans.Items) == 0 {
		t.Fatalf("scans %+v: %v", scans.Items, err)
	}
	s0 := scans.Items[0]
	insts, _, err := r.db.ListHeldInstances(r.ctx, pathOfScan(s0), master.Page{Limit: 10})
	if err != nil || len(insts) < 2 {
		t.Fatalf("instances %d: %v", len(insts), err)
	}
	e, release, err := es.Use(r.ctx, "lab", r.db)
	if err != nil {
		t.Fatal(err)
	}
	target := tenant.CorrectionTarget{Subject: s0.SubjectLabel, Session: s0.SessionLabel, Scan: s0.Label, Instance: insts[0].SOPInstanceUID}
	pv, err := e.Preview(r.ctx, target)
	if err != nil {
		release()
		t.Fatal(err)
	}
	c, err := e.Delete(r.ctx, correct.DeleteRequest{Target: target, Reason: "consent withdrawn", Confirm: pv.Confirm, Audit: r.audit()})
	release()
	if err != nil {
		t.Fatal(err)
	}
	ret := &tenantRetention{w: r.w, name: "lab", st: r.db, log: r.w.log}
	for _, h := range []time.Duration{72, 96} {
		r.w.now = func() time.Time { return time.Now().Add(h * time.Hour) }
		if err := ret.once(r.ctx); err != nil {
			t.Fatalf("the pass at %d hours: %v", h, err)
		}
	}
	jobs := disposeJobs(t, r)
	if len(jobs) != 2 {
		t.Fatalf("the two passes queued %d dispose jobs, want one each", len(jobs))
	}
	for _, dj := range jobs {
		if dj.Job.State != tenant.JobDone || len(dj.Sessions) != 1 {
			t.Fatalf("a dispose job is %s listing %+v, want done", dj.Job.State, dj.Sessions)
		}
		if s := dj.Sessions[0]; s.Outcome != tenant.DisposeSkippedOverlap || s.Correction != "" {
			t.Fatalf("the session another correction holds is %+v, want skipped as overlapping", s)
		}
	}
	if got, err := r.db.Correction(r.ctx, c.ID); err != nil || got.State != record.StateCommitted {
		t.Fatalf("the person's delete is %+v (%v), want it still in its grace", got, err)
	}
}
