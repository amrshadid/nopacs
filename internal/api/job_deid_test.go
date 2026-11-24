package api_test

import (
	"fmt"
	"testing"
	"time"

	connect "connectrpc.com/connect/v2"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/deid/profile"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// createDeid is a call of CreateJob of a deid job on lab1.
func createDeid(j *v1.DeidJob) func(clients) outcome {
	return func(c clients) outcome {
		return call(c.jobs.CreateJob, &v1.CreateJobRequest{Tenant: "lab1", Kind: &v1.CreateJobRequest_Deid{Deid: j}})
	}
}

// TestDeidJobs guards CreateJob and GetJob of the deid kind: operator on
// the tenant and up; a mode required; a check naming no profile; a dry run
// of a profile the tenant does not have refused as not found, and of
// version 0 fixed to the latest when made; a job of the whole tenant made
// with no labels; the entry naming the kind, mode, target and version; and
// GetJob answering the tallies the work recorded, most first, which no
// other answer carries.
func TestDeidJobs(t *testing.T) {
	d := newJobData(t)
	f := d.f
	p := f.pipelineOf("lab1")
	v, err := profile.New("research", 1, &v1.ProfileDocument{Bases: []string{profile.AnnexE("basic")}}, profile.Shipped())
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := p.(tenant.Deidentification).CreateProfileVersion(ctx, tenant.NewProfileVersion{Name: "research", Source: v.Source(),
		Expanded: v.ExpandedText(), Checksum: v.Checksum()}, f.now(), master.AuditEntry{ID: "a1x", Time: f.now(), Method: "Test",
		TenantName: "lab1", Outcome: master.OutcomeDone, RequestID: "r1", RemoteAddress: "192.0.2.1:1"}, nil); err != nil {
		t.Fatal(err)
	}
	check := &v1.DeidJob{Mode: v1.DeidJobMode_DEID_JOB_MODE_CHECK}
	var made *v1.Job
	f.run(t, []tcase{
		{name: "reader", as: f.lab1Reader, call: createDeid(check), code: connect.CodePermissionDenied},
		{name: "no mode", as: d.operator, call: createDeid(&v1.DeidJob{}), code: connect.CodeInvalidArgument, msg: "deid.mode"},
		{name: "a check naming a profile", as: d.operator, call: createDeid(&v1.DeidJob{Mode: v1.DeidJobMode_DEID_JOB_MODE_CHECK, Profile: "research"}),
			code: connect.CodeInvalidArgument, msg: "deid.profile"},
		{name: "a version without a profile", as: d.operator, call: createDeid(&v1.DeidJob{Mode: v1.DeidJobMode_DEID_JOB_MODE_DRY_RUN, ProfileVersion: 2}),
			code: connect.CodeInvalidArgument, msg: "deid.profile_version"},
		{name: "a profile not there", as: d.operator, call: createDeid(&v1.DeidJob{Mode: v1.DeidJobMode_DEID_JOB_MODE_DRY_RUN, Profile: "nosuch"}),
			code: connect.CodeNotFound},
		{name: "a version not there", as: d.operator, call: createDeid(&v1.DeidJob{Mode: v1.DeidJobMode_DEID_JOB_MODE_DRY_RUN, Profile: "research", ProfileVersion: 9}),
			code: connect.CodeNotFound},
		{name: "a bad label", as: d.operator, call: createDeid(&v1.DeidJob{Mode: v1.DeidJobMode_DEID_JOB_MODE_CHECK, SubjectLabel: "Smith^John"}),
			code: connect.CodeInvalidArgument, msg: "label"},
		{name: "a dry run of the latest version", as: d.operator,
			call:  createDeid(&v1.DeidJob{Mode: v1.DeidJobMode_DEID_JOB_MODE_DRY_RUN, Profile: "research", SubjectLabel: d.session.GetSubjectLabel()}),
			audit: &wantAudit{log: "lab1", outcome: master.OutcomeDone, details: map[string]string{"kind": "deid", "mode": "dry_run", "profile": "research@1", "target": d.session.GetSubjectLabel()}},
			check: func(t *testing.T, o outcome) {
				j := o.res.(*v1.CreateJobResponse).GetJob()
				if dj := j.GetDeid(); dj.GetMode() != v1.DeidJobMode_DEID_JOB_MODE_DRY_RUN || dj.GetProfile() != "research" || dj.GetProfileVersion() != 1 ||
					j.GetState() != v1.JobState_JOB_STATE_QUEUED || j.GetSend() != nil || j.GetReason() != "" {
					t.Errorf("the dry run made: %v", j)
				}
			}},
		{name: "a check of the whole tenant", as: d.operator, call: createDeid(check),
			audit: &wantAudit{log: "lab1", outcome: master.OutcomeDone, details: map[string]string{"kind": "deid", "mode": "check"}},
			check: func(t *testing.T, o outcome) {
				made = o.res.(*v1.CreateJobResponse).GetJob()
				if dj := made.GetDeid(); dj.GetMode() != v1.DeidJobMode_DEID_JOB_MODE_CHECK || dj.GetSubjectLabel() != "" || dj.GetProfile() != "" {
					t.Errorf("the check made: %v", made)
				}
			}},
	})
	if made == nil {
		t.Fatal("no check was made")
	}
	// The work of the server, through the store: the listing, then each
	// scan's instances passed and tallied.
	now := f.now()
	for range 100 {
		w, err := p.ClaimWork(ctx, tenant.DeidWorkKind, "test", now, time.Minute)
		if err != nil {
			break
		}
		u, err := p.StartJobUnit(ctx, w.Fence(), now)
		if err != nil {
			t.Fatal(err)
		}
		if u.Listing {
			for more := true; more; {
				if _, more, err = p.ListJobScans(ctx, w.Fence(), now); err != nil {
					t.Fatal(err)
				}
			}
			continue
		}
		ins, err := p.JobScanInstances(ctx, w.Fence(), "", tenant.MaxJobInstancePage)
		if err != nil {
			t.Fatal(err)
		}
		prog := tenant.JobProgress{Fence: w.Fence(), FailuresKept: tenant.DefaultFailuresKept, Now: now,
			Tallies: map[string]int64{"check|as_received": int64(len(ins)), "class|1.2.840.10008.5.1.4.1.1.4|passed": int64(len(ins))}}
		for _, in := range ins {
			prog.To, prog.Sent, prog.SentBytes = in.SOPInstanceUID, prog.Sent+1, prog.SentBytes+in.Size
		}
		if _, err := p.EndJobScan(ctx, prog); err != nil {
			t.Fatal(err)
		}
	}
	f.run(t, []tcase{
		{name: "the tallies of a check", as: f.lab1Reader, call: getJob(made.GetId()), check: func(t *testing.T, o outcome) {
			j := o.res.(*v1.GetJobResponse).GetJob()
			if got := fmt.Sprint(j.GetTallies()); j.GetState() != v1.JobState_JOB_STATE_DONE || len(j.GetTallies()) != 2 ||
				j.GetTallies()[0].GetKey() != "check|as_received" || j.GetTallies()[0].GetCount() != 2 {
				t.Errorf("GetJob of a check that ran: %v, tallies %s", j.GetState(), got)
			}
		}},
		{name: "a list carries no tallies", as: f.lab1Reader, call: func(c clients) outcome {
			return call(c.jobs.ListJobs, &v1.ListJobsRequest{Tenant: "lab1"})
		}, check: func(t *testing.T, o outcome) {
			for _, j := range o.res.(*v1.ListJobsResponse).GetJobs() {
				if len(j.GetTallies()) > 0 {
					t.Errorf("ListJobs answered tallies for %s", j.GetId())
				}
			}
		}},
	})
}
