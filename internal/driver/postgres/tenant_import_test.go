package postgres

import (
	"fmt"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// TestCancelImportMeetsItsEnd guards a cancel that meets an import job's
// end at once, at read committed: the cancel reads the job running, then
// waits for the import job's row, which the last unit's EndImportWalk took
// first and which ends the job done. Once the cancel holds the row it
// judges the job again and refuses as a precondition; the job stays done,
// with no canceled event. A second transaction holds the import job's row
// so both calls are known to wait there before either goes on.
func TestCancelImportMeetsItsEnd(t *testing.T) {
	app := randomName(t, "imp_")
	address := withParams(t, newSchema(t), map[string]string{"pool_max_conns": "4", "application_name": app})
	e := &cenv{t: t, ctx: t.Context(), db: admin(t, address), app: app}
	e.stores = append(e.stores, openTenantAt(t, address, "imp"))
	holder := admin(t, address)
	e.storage("store")
	e.receiver("imp", "store", 11112)
	_, err := e.p().CreateImportJob(e.ctx, tenant.NewImportJob{ID: "j", Receiver: "imp", Path: "p",
		FailuresKept: tenant.MinFailuresKept, Created: cT0}, e.audit(), nil)
	e.ok("CreateImportJob", err)
	claim := func() tenant.Work {
		w, err := e.p().ClaimWork(e.ctx, tenant.ImportWorkKind, "w", cT0.Add(time.Second), time.Minute)
		e.ok("ClaimWork", err)
		_, _, err = e.p().StartImportUnit(e.ctx, w.Fence(), cT0.Add(time.Second))
		e.ok("StartImportUnit", err)
		return w
	}
	listing := claim()
	_, err = e.p().AddImportUnits(e.ctx, listing.Fence(), []string{"p/s1"}, cT0.Add(time.Second))
	e.ok("AddImportUnits", err)
	_, err = e.p().EndImportWalk(e.ctx, listing.Fence(), cT0.Add(time.Second))
	e.ok("EndImportWalk of the listing", err)
	last := claim()

	tx, err := holder.Begin(e.ctx)
	e.ok("Begin", err)
	defer func() { _ = tx.Rollback(e.ctx) }()
	if _, err := tx.Exec(e.ctx, `SELECT 1 FROM import_jobs WHERE job_id = 'j' FOR UPDATE`); err != nil {
		t.Fatal(err)
	}
	ended := make(chan error, 1)
	go func() {
		_, err := e.p().EndImportWalk(e.ctx, last.Fence(), cT0.Add(2*time.Second))
		ended <- err
	}()
	e.waitBlocked(1)
	canceled := make(chan error, 1)
	go func() {
		_, err := e.p().CancelJob(e.ctx, "j", cT0.Add(3*time.Second), e.audit(), nil)
		canceled <- err
	}()
	e.waitBlocked(2)
	if err := tx.Rollback(e.ctx); err != nil {
		t.Fatal(err)
	}
	e.ok("EndImportWalk of the last unit", <-ended)
	if err := <-canceled; fault.KindOf(err) != fault.Precondition {
		t.Errorf("the cancel that met the job's end answered %v, want a precondition failure", err)
	}
	j, err := e.p().Job(e.ctx, "j")
	e.ok("Job", err)
	if j.State != tenant.JobDone || j.Reason != "" {
		t.Errorf("the job is %s %q, want done: a cancel never turns a job ended meanwhile canceled", j.State, j.Reason)
	}
	page, err := e.p().ReadEvents(e.ctx, "", []tenant.EventType{tenant.EventJobStep}, 100)
	e.ok("ReadEvents", err)
	var steps []string
	for _, ev := range page.Events {
		if p, ok := ev.Payload.(*tenant.JobStepReached); ok && p.Job == "j" {
			steps = append(steps, string(p.State))
		}
	}
	if got := fmt.Sprint(steps); got != "[queued running done]" {
		t.Errorf("the job's events reached %s, want [queued running done]", got)
	}
}
