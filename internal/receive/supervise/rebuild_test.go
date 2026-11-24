package supervise_test

import (
	"context"
	"testing"

	"github.com/amrshadid/nopacs/internal/tenant"
)

// createReplace makes a rebuild job that replaces the catalog on the rig's
// tenant, as an admin's request does.
func (r *rig) createReplace(id string) {
	r.t.Helper()
	if _, err := r.ts.pipe().CreateJob(context.Background(), tenant.NewJob{ID: id, Kind: tenant.JobRebuild,
		Rebuild: &tenant.RebuildJob{Mode: tenant.RebuildReplace}, Created: r.clock.Now()}, r.audit(), nil); err != nil {
		r.t.Fatal(err)
	}
}

// cancelJob cancels a job, as a request does.
func (r *rig) cancelJob(id string) {
	r.t.Helper()
	if _, err := r.ts.pipe().CancelJob(context.Background(), id, r.clock.Now(), r.audit(), nil); err != nil {
		r.t.Fatal(err)
	}
}

// pipelineCount returns how many pipelines were started.
func (r *rig) pipelineCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.pipelines)
}

// rebuilding reports whether the receiver is down for a rebuild and its
// listener refuses as a temporary failure.
func (r *rig) rebuilding(name string, l *listener) func() bool {
	return func() bool {
		h := r.health(name)
		return h.State == tenant.ReceiverDown && h.Reason == tenant.DownTenantNotReady && h.Detail == tenant.PhraseTenantRebuilding &&
			l.spec.Refusal() == tenant.FailureTenantNotReady
	}
}

// TestAReplacePausesThePipeline guards the pause a rebuild that replaces
// the catalog holds: once its job is queued the tenant's pipeline is
// stopped, the tenant reads as drained, and the receivers stay listening
// but refuse as a temporary failure, down as not ready with the rebuild's
// phrase; once the job has ended (here canceled) a pipeline is started
// again and the receivers are up.
func TestAReplacePausesThePipeline(t *testing.T) {
	r := newRig(t)
	r.add("ct", 11112, true)
	r.start()
	r.waitFor("ct up", r.isUp("ct"))
	l := r.running("ct")[0]
	r.mu.Lock()
	first := r.pipelines[0]
	r.mu.Unlock()
	r.createReplace("rb-1")
	r.waitFor("ct refusing for the rebuild", r.rebuilding("ct", l))
	r.waitFor("drained", func() bool { return r.sup.Drained("lab") })
	if !first.stopped.Load() {
		t.Fatal("the tenant is drained with its pipeline running")
	}
	if l.stopped.Load() {
		t.Fatal("the listener was stopped for a rebuild; it must stay to refuse as transient")
	}
	if _, ok := r.sup.Decisions("lab"); ok {
		t.Fatal("a pipeline is handed to decisions while a replace holds the tenant")
	}
	n := r.pipelineCount()
	r.cancelJob("rb-1")
	r.waitFor("ct up again", r.isUp("ct"))
	r.waitFor("a pipeline started again", func() bool { return r.pipelineCount() == n+1 })
	if r.sup.Drained("lab") {
		t.Fatal("the tenant reads as drained with its pipeline running again")
	}
}

// TestAPipelineStartAsksForAReplaceAgain guards a replace made after the
// worker's round read that none held the tenant and before its pipeline
// starts: the start asks again, with starting set, sees it, and starts
// nothing, so no pipeline runs that the replace's drain would not wait for.
func TestAPipelineStartAsksForAReplaceAgain(t *testing.T) {
	r := newRig(t)
	r.add("ct", 11112, true)
	p := r.ts.pipe()
	// The second ask for the store is restartPipeline's, after the round's
	// own ask and its read of the replaces: the replace is made then.
	r.ts.onCall = func(n int) {
		if n == 2 {
			if _, err := p.CreateJob(context.Background(), tenant.NewJob{ID: "rb-1", Kind: tenant.JobRebuild,
				Rebuild: &tenant.RebuildJob{Mode: tenant.RebuildReplace}, Created: t0}, r.audit(), nil); err != nil {
				t.Error(err)
			}
		}
	}
	r.start()
	r.waitFor("ct listening", func() bool { return len(r.running("ct")) == 1 })
	r.waitFor("ct refusing for the rebuild", r.rebuilding("ct", r.running("ct")[0]))
	if n := r.pipelineCount(); n != 0 {
		t.Fatalf("%d pipelines started though a replace was made before the start", n)
	}
}

// TestAReplaceHoldsAcrossARestart guards that the pause is the tenant's
// database, not the process: a supervisor started while a replace is
// queued, as after a restart, starts no pipeline at all, only listeners
// that refuse; and starts one once the job has ended.
func TestAReplaceHoldsAcrossARestart(t *testing.T) {
	r := newRig(t)
	r.add("ct", 11112, true)
	r.createReplace("rb-1")
	r.start()
	r.waitFor("ct listening", func() bool { return len(r.running("ct")) == 1 })
	l := r.running("ct")[0]
	r.waitFor("ct refusing for the rebuild", r.rebuilding("ct", l))
	if !r.sup.Drained("lab") {
		t.Fatal("the tenant is not drained while a replace holds it")
	}
	if n := r.pipelineCount(); n != 0 {
		t.Fatalf("%d pipelines started while a replace holds the tenant", n)
	}
	r.cancelJob("rb-1")
	r.waitFor("ct up", r.isUp("ct"))
	if n := r.pipelineCount(); n != 1 {
		t.Fatalf("%d pipelines started once the replace ended, want 1", n)
	}
}
