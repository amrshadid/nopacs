package tenanttest

import (
	"fmt"
	"testing"

	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// FillBeforeDeidJobs writes, through the port, jobs at the schema before
// deid jobs: a receiver, a sender "orthanc", a session of two scans of
// three instances, and three send jobs: "j-done" run to its end with the
// first instance of each scan refused (partial, both failures listed), "j-canceled" canceled
// after its listing, and "j-queued" left queued. It returns them as read.
func FillBeforeDeidJobs(t *testing.T, p tenant.Pipeline) []tenant.Job {
	t.Helper()
	e := sendingEnv(t, p, 0)
	e.addReceiver("recv", "store", 11112)
	e.finish(e.archive("recv", 1, 2, 3, 0, at(0), "ses-1"))
	e.addSender("orthanc")
	target := tenant.JobTarget{SubjectLabel: "sub-01", SessionLabel: "ses-1"}
	e.createJob("j-done", "orthanc", target, at(3600))
	e.runJob("orthanc", func(i int, _ tenant.ScanInstance) tenant.JobFailureCode {
		if i == 0 {
			return tenant.FailurePeerRefused
		}
		return ""
	}, at(3700))
	e.createJob("j-canceled", "orthanc", target, at(3800))
	e.list("orthanc", at(3800))
	_, err := p.CancelJob(e.ctx, "j-canceled", at(3810), e.audit(), nil)
	e.ok("CancelJob", err)
	e.createJob("j-queued", "orthanc", target, at(3900))
	var out []tenant.Job
	for _, id := range []string{"j-done", "j-canceled", "j-queued"} {
		j, err := p.Job(e.ctx, id)
		e.ok("Job", err)
		out = append(out, j)
	}
	return out
}

// CheckAfterDeidJobs checks, after the migration that adds deid jobs,
// that the jobs FillBeforeDeidJobs wrote read exactly as they did, as
// send jobs with no mode or profile, their failures still listed, the
// queued one still claimable under its sender's kind and run to its end;
// that an id taken still refuses a second job; and that a deid job of the
// whole tenant can be made and listed beside them.
func CheckAfterDeidJobs(t *testing.T, p tenant.Pipeline, before []tenant.Job) {
	t.Helper()
	e := sendingEnv(t, p, 100000)
	for _, want := range before {
		got, err := p.Job(e.ctx, want.ID)
		e.ok("Job", err)
		if fmt.Sprintf("%+v", got) != fmt.Sprintf("%+v", want) || got.Kind != tenant.JobSend || got.Mode != "" || !got.Profile.IsZero() {
			t.Errorf("job %s after the migration:\n%+v\nwant\n%+v", want.ID, got, want)
		}
	}
	failures, _, err := p.ListJobFailures(e.ctx, "j-done", true, master.Page{Limit: 10})
	if err != nil || len(failures) != 2 {
		t.Errorf("the failures of a job from before: %v, %v", failures, err)
	}
	j := e.runJob("orthanc", allSent, at(4000))
	if j.ID != "j-queued" || j.State != tenant.JobDone {
		t.Errorf("the queued job run after the migration: %+v", j)
	}
	d, err := p.CreateJob(e.ctx, tenant.NewJob{ID: "d1", Kind: tenant.JobDeid, Mode: tenant.DeidCheck, Created: at(5000)}, e.audit(), nil)
	if err != nil || d.Kind != tenant.JobDeid {
		t.Fatalf("a deid job after the migration: %+v, %v", d, err)
	}
	jobs, _, err := p.ListJobs(e.ctx, tenant.JobFilter{}, master.Page{Limit: 10})
	if err != nil || len(jobs) != 4 || jobs[0].ID != "d1" {
		t.Errorf("the jobs listed after the migration: %d, %v", len(jobs), err)
	}
	_, err = p.CreateJob(e.ctx, tenant.NewJob{ID: "j-queued", Sender: "orthanc", Target: tenant.JobTarget{SubjectLabel: "sub-01"},
		Created: at(5001)}, e.audit(), nil)
	e.wantKind("a job of an id from before the migration", err, fault.Conflict)
}
