package dirimport

import (
	"context"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/receive"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// overQuotaSink is a pipeline whose tenant is at its storage quota: it
// refuses every file for it.
type overQuotaSink struct{}

// Accept refuses the file for the quota.
func (overQuotaSink) Accept(context.Context, receive.Arrival) receive.Outcome {
	return receive.Outcome{Status: receive.Refused, Reason: receive.ReasonOverQuota}
}

// TestAUnitAtTheQuotaWaits guards an import while its tenant is at its
// storage quota: the unit gives its folder back with the quota's reason,
// its place kept and no attempt spent, so the job waits rather than fails,
// and goes on once the tenant has room.
func TestAUnitAtTheQuotaWaits(t *testing.T) {
	e := newEnv(t, "")
	e.writeFiles(map[string]string{"proj1/sessA/ct2/ct2n-6293.dcm": "ct2n-6293.dcm"})
	j := e.createJob()
	cfg := e.config()
	cfg.Sink = func(string) receive.Sink { return overQuotaSink{} }
	got := make(chan end, 1)
	cfg.seams.ended = func(folder string, how end, reason string) {
		if folder == "proj1/sessA" && reason == receive.ReasonOverQuota {
			select {
			case got <- how:
			default:
			}
		}
	}
	start := time.Now()
	e.start(cfg)
	select {
	case how := <-got:
		if how != endRelease {
			t.Errorf("the unit at the quota ended %d, want endRelease (%d): given back, no attempt spent", how, endRelease)
		}
	case <-time.After(importBound):
		t.Fatal("the unit whose file was refused for the quota did not end")
	}
	job, err := e.store.Job(e.ctx, j.ID)
	if err != nil {
		t.Fatal(err)
	}
	if job.State.Ended() {
		t.Errorf("the job at the quota is %s (%s), want it waiting", job.State, job.Reason)
	}
	// It looks again as a unit whose receiver is disabled does, not at the
	// pace of a refusal for a moment: a person must purge or raise the
	// bound first.
	units, _, err := e.store.ListImportUnits(e.ctx, j.ID, tenant.ImportUnitFilter{}, master.Page{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	for _, u := range units {
		if u.Folder != "proj1/sessA" {
			continue
		}
		w, err := e.store.Work(e.ctx, tenant.ImportUnitWorkID(j.ID, u.ID))
		if err != nil {
			t.Fatal(err)
		}
		if w.LastError != receive.ReasonOverQuota || w.Due.Before(start.Add(WaitDelay)) {
			t.Errorf("the unit at the quota is due %v with %q, want at least %v after it started, with the quota's reason",
				w.Due.Sub(start), w.LastError, WaitDelay)
		}
		return
	}
	t.Errorf("the job's units are %+v, want proj1/sessA among them", units)
}
