package tenanttest

import (
	"context"
	"testing"

	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// sendingEnv returns a sending test's helpers on a store a driver's own
// test opened, its ids made from seq on.
func sendingEnv(t *testing.T, p tenant.Pipeline, seq int) *senv {
	return &senv{eenv: &eenv{penv: &penv{env: &env{T: t, s: p, ctx: context.Background(), seq: seq}, p: p}}}
}

// FillBeforeSending writes, through the port, a tenant's data at the schema
// before senders and jobs: a storage "store", a receiver "recv", a
// reception done that archived two scans of three instances into session
// sub-01/ses-1, and a second reception into the same session left archived,
// not done. Nothing of it reads the tables senders and jobs add.
func FillBeforeSending(t *testing.T, p tenant.Pipeline) {
	t.Helper()
	e := sendingEnv(t, p, 0)
	e.addReceiver("recv", "store", 11112)
	e.finish(e.archive("recv", 1, 2, 3, 0, at(0), "ses-1"))
	e.archive("recv", 1, 1, 2, 500, at(600), "ses-1")
}

// CheckAfterSending checks, after the migration that adds senders and
// jobs, that what FillBeforeSending wrote reads as it was, that the
// status counts no job, and that a sender and a job over the data work:
// the job lists the session's scans with what the catalog held and sends
// every instance.
func CheckAfterSending(t *testing.T, p tenant.Pipeline) {
	t.Helper()
	e := sendingEnv(t, p, 100000)
	totals, err := p.CatalogTotals(e.ctx)
	e.ok("CatalogTotals", err)
	if totals.Sessions != 1 || totals.Scans != 2 || totals.Instances != 8 {
		t.Errorf("the catalog after the migration: %+v, want 1 session, 2 scans, 8 instances", totals)
	}
	receptions, _, err := p.ListReceptions(e.ctx, tenant.ReceptionFilter{}, master.Page{Limit: 10})
	e.ok("ListReceptions", err)
	if len(receptions) != 2 {
		t.Errorf("the receptions after the migration: %d, want 2", len(receptions))
	}
	if r, _ := p.Receiver(e.ctx, "recv"); len(r.ForwardTo) != 0 {
		t.Errorf("a receiver from before forwards to %v", r.ForwardTo)
	}
	jobs, _, err := p.ListJobs(e.ctx, tenant.JobFilter{}, master.Page{Limit: 10})
	if err != nil || len(jobs) != 0 {
		t.Errorf("jobs after the migration: %v, %v; want none", jobs, err)
	}
	e.addSender("orthanc")
	e.createJob("j1", "orthanc", tenant.JobTarget{SubjectLabel: "sub-01", SessionLabel: "ses-1"}, at(3600))
	j := e.runJob("orthanc", allSent, at(3700))
	e.wantCounts("a job over data from before the migration", j, 8, 8, 0, 0, 2, 2, true)
	if j.State != tenant.JobDone {
		t.Errorf("the job ended %s, want done", j.State)
	}
}
