package api_test

import (
	"errors"
	"testing"

	connect "connectrpc.com/connect/v2"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/master"
)

// TestCreateRebuildJob guards JobService.CreateJob for a rebuild: an
// operator makes a compare; a replace needs admin (an operator is answered
// permission denied); a mode is required, a rebuild from one storage is
// not served, a SQLite tenant takes no scratch schema, and the job is
// answered and read back with its mode.
func TestCreateRebuildJob(t *testing.T) {
	f := newFixture(t)
	rebuild := func(mode v1.RebuildJobMode, scratch, from string) *v1.CreateJobRequest {
		return &v1.CreateJobRequest{Tenant: "lab1", Kind: &v1.CreateJobRequest_Rebuild{Rebuild: &v1.RebuildJob{Mode: mode,
			ScratchSchema: scratch, FromStorage: from}}}
	}
	code := func(err error) connect.Code {
		var ce *connect.Error
		if errors.As(err, &ce) {
			return ce.Code()
		}
		return 0
	}
	for _, c := range []struct {
		name string
		as   caller
		req  *v1.CreateJobRequest
		want connect.Code
	}{
		{"no mode", f.lab1Operator, rebuild(v1.RebuildJobMode_REBUILD_JOB_MODE_UNSPECIFIED, "", ""), connect.CodeInvalidArgument},
		{"a scratch schema on SQLite", f.lab1Operator, rebuild(v1.RebuildJobMode_REBUILD_JOB_MODE_COMPARE, "scratch", ""), connect.CodeInvalidArgument},
		{"a scratch schema not an identifier", f.lab1Operator, rebuild(v1.RebuildJobMode_REBUILD_JOB_MODE_COMPARE, "No-Way", ""), connect.CodeInvalidArgument},
		{"from one storage", f.lab1Operator, rebuild(v1.RebuildJobMode_REBUILD_JOB_MODE_COMPARE, "", "main"), connect.CodeUnimplemented},
		{"a replace by an operator", f.lab1Operator, rebuild(v1.RebuildJobMode_REBUILD_JOB_MODE_REPLACE, "", ""), connect.CodePermissionDenied},
		{"a compare by a reader", f.lab1Reader, rebuild(v1.RebuildJobMode_REBUILD_JOB_MODE_COMPARE, "", ""), connect.CodePermissionDenied},
	} {
		if _, err := f.as(c.as).jobs.CreateJob(ctx, c.req); code(err) != c.want {
			t.Errorf("%s: %v, want %v", c.name, err, c.want)
		}
	}
	made, err := f.as(f.lab1Operator).jobs.CreateJob(ctx, rebuild(v1.RebuildJobMode_REBUILD_JOB_MODE_COMPARE, "", ""))
	if err != nil {
		t.Fatalf("a compare by an operator: %v", err)
	}
	if got := made.GetJob().GetRebuild(); got.GetMode() != v1.RebuildJobMode_REBUILD_JOB_MODE_COMPARE || got.GetScratchSchema() != "" {
		t.Errorf("the rebuild made reads %+v", made.GetJob())
	}
	if _, err := f.as(f.lab1Operator).jobs.CreateJob(ctx, rebuild(v1.RebuildJobMode_REBUILD_JOB_MODE_COMPARE, "", "")); code(err) != connect.CodeFailedPrecondition {
		t.Errorf("a second rebuild while one is queued: %v, want a failed precondition", err)
	}
	got, err := f.as(f.lab1Reader).jobs.GetJob(ctx, &v1.GetJobRequest{Tenant: "lab1", Id: made.GetJob().GetId()})
	if err != nil {
		t.Fatal(err)
	}
	if got.GetJob().GetRebuild().GetMode() != v1.RebuildJobMode_REBUILD_JOB_MODE_COMPARE || got.GetJob().GetState() != v1.JobState_JOB_STATE_QUEUED {
		t.Errorf("the rebuild read back is %+v", got.GetJob())
	}
}

// TestReplaceTellsTheReceivers guards that the receivers' supervisor is
// told at once of a replace made and of one canceled, so the tenant's
// receivers stop, and start again, without waiting for its next round.
func TestReplaceTellsTheReceivers(t *testing.T) {
	f := newFixture(t)
	before := f.parts.receiversPoked("lab1")
	made, err := f.as(f.lab1Admin).jobs.CreateJob(ctx, &v1.CreateJobRequest{Tenant: "lab1", Kind: &v1.CreateJobRequest_Rebuild{
		Rebuild: &v1.RebuildJob{Mode: v1.RebuildJobMode_REBUILD_JOB_MODE_REPLACE}}})
	if err != nil {
		t.Fatal(err)
	}
	if got := f.parts.receiversPoked("lab1"); got != before+1 {
		t.Fatalf("after a replace was made the receivers were told %d times, want %d", got, before+1)
	}
	if _, err := f.as(f.lab1Operator).jobs.CancelJob(ctx, &v1.CancelJobRequest{Tenant: "lab1", Id: made.GetJob().GetId()}); err != nil {
		t.Fatal(err)
	}
	if got := f.parts.receiversPoked("lab1"); got != before+2 {
		t.Fatalf("after the replace was canceled the receivers were told %d times, want %d", got, before+2)
	}
}

// TestCreateReplaceRebuild guards a rebuild that replaces the catalog: an
// admin makes it, queued, its audit entry naming its mode; an operator is
// answered permission denied, and is told so even while a rebuild is
// queued, before what the store would say of a second one; an admin's
// second rebuild is a failed precondition.
func TestCreateReplaceRebuild(t *testing.T) {
	f := newFixture(t)
	replace := func(c clients) outcome {
		return call(c.jobs.CreateJob, &v1.CreateJobRequest{Tenant: "lab1", Kind: &v1.CreateJobRequest_Rebuild{Rebuild: &v1.RebuildJob{
			Mode: v1.RebuildJobMode_REBUILD_JOB_MODE_REPLACE}}})
	}
	f.run(t, []tcase{
		{name: "an operator", as: f.lab1Operator, call: replace, code: connect.CodePermissionDenied},
		{name: "an admin", as: f.lab1Admin, call: replace,
			audit: &wantAudit{log: "lab1", outcome: master.OutcomeDone, details: map[string]string{"kind": "rebuild", "mode": "replace"}},
			check: func(t *testing.T, o outcome) {
				j := o.res.(*v1.CreateJobResponse).GetJob()
				if j.GetRebuild().GetMode() != v1.RebuildJobMode_REBUILD_JOB_MODE_REPLACE || j.GetState() != v1.JobState_JOB_STATE_QUEUED {
					t.Errorf("the replace made reads %+v", j)
				}
			}},
		{name: "an operator while a replace is queued", as: f.lab1Operator, call: replace, code: connect.CodePermissionDenied},
		{name: "a second replace by an admin", as: f.lab1Admin, call: replace, code: connect.CodeFailedPrecondition},
	})
}
