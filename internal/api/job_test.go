package api_test

import (
	"slices"
	"strings"
	"testing"
	"time"

	connect "connectrpc.com/connect/v2"
	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/access"
	"github.com/amrshadid/nopacs/internal/api"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// testCreateJob guards CreateJob: operator on the tenant and up; the
// request's rules (a kind, a sender's name, labels, a level named without
// the one above, a reception that only the server sets); a sender the
// tenant does not have as a precondition and a target it does not hold as
// not found; a job queued with an id the server made, its key recorded;
// and the entry with the job's id as its object and the kind, sender and
// target path in details.
func testCreateJob(t *testing.T) {
	d := newJobData(t)
	f := d.f
	s0 := d.scan
	create := func(j *v1.SendJob) func(clients) outcome { return createJob(j) }
	f.run(t, []tcase{
		{name: "no key", as: nobody, call: create(target("pacs", "x")), code: connect.CodeUnauthenticated},
		{name: "no grant", as: f.noGrant, call: create(target("pacs", "x")), code: connect.CodePermissionDenied},
		{name: "reader", as: f.lab1Reader, call: create(target("pacs", "x")), code: connect.CodePermissionDenied},
		{name: "admin on the installation", as: f.instAdmin, call: create(target("pacs", "x")), code: connect.CodePermissionDenied},
		{name: "no such tenant, no right to know", as: d.operator, call: func(c clients) outcome {
			return call(c.jobs.CreateJob, &v1.CreateJobRequest{Tenant: "nolab", Kind: &v1.CreateJobRequest_Send{Send: target("pacs", "x")}})
		}, code: connect.CodePermissionDenied, msg: access.MessageDenied},
		{name: "no kind", as: d.operator, call: func(c clients) outcome {
			return call(c.jobs.CreateJob, &v1.CreateJobRequest{Tenant: "lab1"})
		}, code: connect.CodeInvalidArgument, msg: "send", audit: &wantAudit{log: "lab1", outcome: master.OutcomeFailed, details: map[string]string{"kind": "none"}}},
		{name: "a bad sender name", as: d.operator, call: create(target("Pacs", "x")), code: connect.CodeInvalidArgument, msg: "send.sender"},
		{name: "no sender", as: d.operator, call: create(target("", "x")), code: connect.CodeInvalidArgument, msg: "send.sender"},
		{name: "no subject", as: d.operator, call: create(&v1.SendJob{Sender: "pacs"}), code: connect.CodeInvalidArgument, msg: "label"},
		{name: "a bad label", as: d.operator, call: create(target("pacs", "Smith^John")), code: connect.CodeInvalidArgument, msg: "label",
			audit: &wantAudit{log: "lab1", outcome: master.OutcomeFailed, details: map[string]string{"kind": "send", "sender": "pacs", "target": "invalid"}}},
		{name: "a scan without its session", as: d.operator, call: create(&v1.SendJob{Sender: "pacs", SubjectLabel: s0.GetSubjectLabel(), ScanLabel: s0.GetLabel()}), code: connect.CodeInvalidArgument, msg: "session"},
		{name: "a reception given", as: d.operator, call: create(&v1.SendJob{Sender: "pacs", SubjectLabel: s0.GetSubjectLabel(), Reception: "r1"}), code: connect.CodeInvalidArgument, msg: "reception"},
		{name: "a receiver given", as: d.operator, call: create(&v1.SendJob{Sender: "pacs", SubjectLabel: s0.GetSubjectLabel(), Receiver: "scanner1"}), code: connect.CodeInvalidArgument, msg: "receiver"},
		{name: "a sender the tenant does not have", as: d.operator, call: create(target("none", s0.GetSubjectLabel())), code: connect.CodeFailedPrecondition},
		{name: "a subject the tenant does not hold", as: d.operator, call: create(target("pacs", "nosuchsubject")), code: connect.CodeNotFound},
		{name: "a session the tenant does not hold", as: d.operator, call: create(target("pacs", s0.GetSubjectLabel(), "nosuchsession")), code: connect.CodeNotFound},
		{name: "a subject", as: d.operator, call: create(target("pacs", s0.GetSubjectLabel())),
			audit: &wantAudit{log: "lab1", outcome: master.OutcomeDone, tenant: "lab1", details: map[string]string{"kind": "send", "sender": "pacs", "target": s0.GetSubjectLabel()}},
			check: func(t *testing.T, o outcome) {
				j := o.res.(*v1.CreateJobResponse).GetJob()
				if j.GetId() == "" || j.GetState() != v1.JobState_JOB_STATE_QUEUED || j.GetSend().GetSender() != "pacs" || j.GetSend().GetSubjectLabel() != s0.GetSubjectLabel() ||
					j.GetCreatedByKeyId() != d.operatorID || j.GetCreateTime() == nil || j.GetCounts() == nil || j.GetCounts().GetListed() || j.GetReason() != "" {
					t.Errorf("the job made: %v", j)
				}
				entries := entriesOf(f.tenantLog("lab1"), o.requestID)
				if len(entries) != 1 || entries[0].Object != j.GetId() {
					t.Errorf("the entry's object is not the job's id %q: %+v", j.GetId(), entries)
				}
			}},
		{name: "a session", as: f.lab1Admin, call: create(target("pacs", s0.GetSubjectLabel(), s0.GetSessionLabel())),
			audit: &wantAudit{log: "lab1", outcome: master.OutcomeDone, details: map[string]string{"target": s0.GetSubjectLabel() + "/" + s0.GetSessionLabel()}}},
		{name: "a scan, to a disabled sender", as: d.operator, call: create(target("spare", s0.GetSubjectLabel(), s0.GetSessionLabel(), s0.GetLabel())),
			audit: &wantAudit{log: "lab1", outcome: master.OutcomeDone, details: map[string]string{"target": s0.GetSubjectLabel() + "/" + s0.GetSessionLabel() + "/" + s0.GetLabel()}},
			check: func(t *testing.T, o outcome) {
				j := o.res.(*v1.CreateJobResponse).GetJob()
				if j.GetState() != v1.JobState_JOB_STATE_QUEUED || j.GetReason() != "sender disabled" {
					t.Errorf("a job to a disabled sender waits and says why: %v", j)
				}
			}},
		{name: "the local operator leaves no key id", as: local, call: create(target("pacs", s0.GetSubjectLabel())),
			check: func(t *testing.T, o outcome) {
				if id := o.res.(*v1.CreateJobResponse).GetJob().GetCreatedByKeyId(); id != "" {
					t.Errorf("key id %q", id)
				}
			}},
	})
}

// testGetJob guards GetJob: reader on the tenant and up, a job not there, a
// bad id, the counts of a job that ran (each instance in exactly one count,
// the parts summing to the whole), and the reason a job in progress is held
// back, from the server's view of its sender.
func testGetJob(t *testing.T) {
	d := newJobData(t)
	f := d.f
	sending := f.sending()
	subject := d.session.GetSubjectLabel()
	ran := d.makeJob(t, "pacs", subject)
	d.work(t, "pacs", true, true)
	held := d.makeJob(t, "pacs", subject)
	disabled := d.makeJob(t, "spare", subject)
	f.run(t, []tcase{
		{name: "no key", as: nobody, call: getJob(ran.GetId()), code: connect.CodeUnauthenticated},
		{name: "no grant", as: f.noGrant, call: getJob(ran.GetId()), code: connect.CodePermissionDenied},
		{name: "a bad id", as: f.lab1Reader, call: getJob("Smith^John"), code: connect.CodeInvalidArgument, msg: "id"},
		{name: "no such job", as: f.lab1Reader, call: getJob("nosuchjob"), code: connect.CodeNotFound, msg: "no job with this id"},
		{name: "a job that ran", as: f.lab1Reader, call: getJob(ran.GetId()), audit: &wantAudit{log: "lab1", outcome: master.OutcomeDone, object: ran.GetId()},
			check: func(t *testing.T, o outcome) {
				j := o.res.(*v1.GetJobResponse).GetJob()
				c := j.GetCounts()
				if j.GetState() != v1.JobState_JOB_STATE_PARTIAL || j.GetStartTime() == nil || j.GetEndTime() == nil || !c.GetListed() ||
					c.GetInstances() == 0 || c.GetInstancesWarned() != 1 || c.GetInstancesRefused() != c.GetInstances()-1 || c.GetInstancesSent() != 0 ||
					c.GetInstancesRemaining() != 0 || c.GetScansDone() != c.GetScans() {
					t.Errorf("the counts of a job refused by its peer: %v", j)
				}
				if c.GetInstances() != c.GetInstancesSent()+c.GetInstancesWarned()+c.GetInstancesRefused()+c.GetInstancesRemaining() ||
					c.GetBytes() != c.GetBytesSent()+c.GetBytesWarned()+c.GetBytesRefused()+c.GetBytesRemaining() || c.GetBytes() == 0 {
					t.Errorf("an instance is in exactly one count: %v", c)
				}
				if j.GetReason() != "" {
					t.Errorf("a job that ended partial has no reason: %q", j.GetReason())
				}
			}},
		{name: "a queued job, its sender unknown", as: f.lab1Reader, call: getJob(held.GetId()), check: func(t *testing.T, o outcome) {
			if j := o.res.(*v1.GetJobResponse).GetJob(); j.GetState() != v1.JobState_JOB_STATE_QUEUED || j.GetReason() != "" {
				t.Errorf("%v", j)
			}
		}},
		{name: "a job whose sender is disabled", as: f.lab1Reader, call: getJob(disabled.GetId()), check: func(t *testing.T, o outcome) {
			if j := o.res.(*v1.GetJobResponse).GetJob(); j.GetReason() != "sender disabled" {
				t.Errorf("%v", j)
			}
		}},
	})
	sending.setHealth("lab1", "pacs", api.SenderHealth{State: tenant.SenderDown, DownReason: tenant.SenderDownPeerFailing, Since: t0})
	f.run(t, []tcase{
		{name: "a job whose sender is down", as: f.lab1Reader, call: getJob(held.GetId()), check: func(t *testing.T, o outcome) {
			if j := o.res.(*v1.GetJobResponse).GetJob(); j.GetReason() != "sender down" {
				t.Errorf("%v", j)
			}
		}},
		{name: "a job that ended is not held", as: f.lab1Reader, call: getJob(ran.GetId()), check: func(t *testing.T, o outcome) {
			if j := o.res.(*v1.GetJobResponse).GetJob(); j.GetReason() != "" {
				t.Errorf("%v", j)
			}
		}},
	})
}

// testListJobs guards ListJobs: reader on the tenant and up; newest first;
// each filter (states, in progress, sender, time) alone, and the rules of
// the filters; paging; and the entry with the filters in details.
func testListJobs(t *testing.T) {
	d := newJobData(t)
	f := d.f
	subject := d.session.GetSubjectLabel()
	first := d.makeJob(t, "pacs", subject)
	mid := first.GetCreateTime().AsTime().Add(30 * time.Minute)
	d.work(t, "pacs", false, true)
	f.advance(time.Hour)
	second := d.makeJob(t, "pacs", subject)
	third := d.makeJob(t, "spare", subject)
	list := func(r *v1.ListJobsRequest) func(clients) outcome {
		return func(c clients) outcome {
			r.Tenant = "lab1"
			return call(c.jobs.ListJobs, r)
		}
	}
	ids := func(o outcome) []string {
		var out []string
		for _, j := range o.res.(*v1.ListJobsResponse).GetJobs() {
			out = append(out, j.GetId())
		}
		return out
	}
	want := func(want ...string) func(*testing.T, outcome) {
		return func(t *testing.T, o outcome) {
			if got := ids(o); !slices.Equal(got, want) {
				t.Errorf("jobs %v, want %v", got, want)
			}
		}
	}
	var token string
	f.run(t, []tcase{
		{name: "no key", as: nobody, call: list(&v1.ListJobsRequest{}), code: connect.CodeUnauthenticated},
		{name: "no grant", as: f.noGrant, call: list(&v1.ListJobsRequest{}), code: connect.CodePermissionDenied},
		{name: "an unspecified state", as: f.lab1Reader, call: list(&v1.ListJobsRequest{States: []v1.JobState{v1.JobState_JOB_STATE_UNSPECIFIED}}), code: connect.CodeInvalidArgument, msg: "states"},
		{name: "a bad sender", as: f.lab1Reader, call: list(&v1.ListJobsRequest{Sender: "Pacs"}), code: connect.CodeInvalidArgument, msg: "sender"},
		{name: "an end before its start", as: f.lab1Reader, call: list(&v1.ListJobsRequest{StartTime: timestamppb.New(mid.Add(time.Hour)), EndTime: timestamppb.New(mid)}), code: connect.CodeInvalidArgument, msg: "end_time"},
		{name: "a negative page size", as: f.lab1Reader, call: list(&v1.ListJobsRequest{PageSize: -1}), code: connect.CodeInvalidArgument, msg: "page_size"},
		{name: "every job, newest first", as: f.lab1Reader, call: list(&v1.ListJobsRequest{}), check: want(third.GetId(), second.GetId(), first.GetId()),
			audit: &wantAudit{log: "lab1", outcome: master.OutcomeDone, object: "-"}},
		{name: "by state", as: f.lab1Reader, call: list(&v1.ListJobsRequest{States: []v1.JobState{v1.JobState_JOB_STATE_PARTIAL}}), check: want(first.GetId()),
			audit: &wantAudit{log: "lab1", outcome: master.OutcomeDone, details: map[string]string{"states": "JOB_STATE_PARTIAL"}}},
		{name: "in progress", as: f.lab1Reader, call: list(&v1.ListJobsRequest{InProgress: true}), check: want(third.GetId(), second.GetId()),
			audit: &wantAudit{log: "lab1", outcome: master.OutcomeDone, details: map[string]string{"in_progress": "true"}}},
		{name: "in progress and ended is nothing", as: f.lab1Reader, call: list(&v1.ListJobsRequest{InProgress: true, States: []v1.JobState{v1.JobState_JOB_STATE_DONE}}), check: want()},
		{name: "by sender", as: f.lab1Reader, call: list(&v1.ListJobsRequest{Sender: "spare"}), check: want(third.GetId()),
			audit: &wantAudit{log: "lab1", outcome: master.OutcomeDone, details: map[string]string{"sender": "spare"}}},
		{name: "by time", as: f.lab1Reader, call: list(&v1.ListJobsRequest{StartTime: timestamppb.New(mid)}), check: want(third.GetId(), second.GetId())},
		{name: "before a time", as: f.lab1Reader, call: list(&v1.ListJobsRequest{EndTime: timestamppb.New(mid)}), check: want(first.GetId())},
		{name: "a first page", as: f.lab1Reader, call: list(&v1.ListJobsRequest{PageSize: 2}), check: func(t *testing.T, o outcome) {
			r := o.res.(*v1.ListJobsResponse)
			if len(r.GetJobs()) != 2 || r.GetNextPageToken() == "" {
				t.Fatalf("%v", r)
			}
			token = r.GetNextPageToken()
		}},
		{name: "the last page", as: f.lab1Reader, call: func(c clients) outcome { return list(&v1.ListJobsRequest{PageSize: 2, PageToken: token})(c) }, check: want(first.GetId())},
		{name: "a token that is not one", as: f.lab1Reader, call: list(&v1.ListJobsRequest{PageToken: "x"}), code: connect.CodeInvalidArgument, msg: "page_token"},
		{name: "the reason of a job held back", as: f.lab1Reader, call: list(&v1.ListJobsRequest{Sender: "spare"}), check: func(t *testing.T, o outcome) {
			if j := o.res.(*v1.ListJobsResponse).GetJobs()[0]; j.GetReason() != "sender disabled" {
				t.Errorf("%v", j)
			}
		}},
	})
}

// testCancelJob guards CancelJob: operator on the tenant and up; a queued
// job canceled with its units, answered as it stands when repeated; a job
// that ended done, partial or failed a precondition; and the entry with the
// job's id.
func testCancelJob(t *testing.T) {
	d := newJobData(t)
	f := d.f
	subject := d.session.GetSubjectLabel()
	ended := d.makeJob(t, "pacs", subject)
	d.work(t, "pacs", false, true)
	queued := d.makeJob(t, "pacs", subject)
	f.run(t, []tcase{
		{name: "no key", as: nobody, call: cancelJob(queued.GetId()), code: connect.CodeUnauthenticated},
		{name: "no grant", as: f.noGrant, call: cancelJob(queued.GetId()), code: connect.CodePermissionDenied},
		{name: "reader", as: f.lab1Reader, call: cancelJob(queued.GetId()), code: connect.CodePermissionDenied},
		{name: "a bad id", as: d.operator, call: cancelJob("Smith^John"), code: connect.CodeInvalidArgument, msg: "id"},
		{name: "no such job", as: d.operator, call: cancelJob("nosuchjob"), code: connect.CodeNotFound, msg: "no job with this id"},
		{name: "a job that ended partial", as: d.operator, call: cancelJob(ended.GetId()), code: connect.CodeFailedPrecondition,
			audit: &wantAudit{log: "lab1", outcome: master.OutcomeFailed, object: ended.GetId()}},
		{name: "canceled", as: d.operator, call: cancelJob(queued.GetId()), audit: &wantAudit{log: "lab1", outcome: master.OutcomeDone, object: queued.GetId(), tenant: "lab1"},
			check: func(t *testing.T, o outcome) {
				j := o.res.(*v1.CancelJobResponse).GetJob()
				if j.GetState() != v1.JobState_JOB_STATE_CANCELED || j.GetEndTime() == nil || j.GetReason() != "canceled" {
					t.Errorf("%v", j)
				}
			}},
		{name: "again, as it stands", as: d.operator, call: cancelJob(queued.GetId()), check: func(t *testing.T, o outcome) {
			if j := o.res.(*v1.CancelJobResponse).GetJob(); j.GetState() != v1.JobState_JOB_STATE_CANCELED {
				t.Errorf("%v", j)
			}
		}},
	})
	// Nothing of the canceled job is left to send: its units ended with it.
	p := f.pipelineOf("lab1")
	if w, err := p.ClaimWork(ctx, tenant.SendKind("pacs"), "test", f.now().Add(time.Hour), time.Minute); err == nil {
		t.Errorf("a unit of the canceled job is still queued: %+v", w)
	}
}

// testRetryJob guards RetryJob: operator on the tenant and up; a new job
// made of what an ended job did not send, with retry_of and retry_job
// naming each other; a repeat answered as it stands with the job made
// then; a job in progress, a job done with nothing to send again, and a job
// whose sender was removed as preconditions; and the entry with the new
// job's id.
func testRetryJob(t *testing.T) {
	d := newJobData(t)
	f := d.f
	subject := d.session.GetSubjectLabel()
	partial := d.makeJob(t, "pacs", subject)
	d.work(t, "pacs", false, true)
	done := d.makeJob(t, "pacs", subject)
	d.work(t, "pacs", false, false)
	running := d.makeJob(t, "pacs", subject)
	var retryID string
	f.run(t, []tcase{
		{name: "no key", as: nobody, call: retryJob(partial.GetId()), code: connect.CodeUnauthenticated},
		{name: "no grant", as: f.noGrant, call: retryJob(partial.GetId()), code: connect.CodePermissionDenied},
		{name: "reader", as: f.lab1Reader, call: retryJob(partial.GetId()), code: connect.CodePermissionDenied},
		{name: "a bad id", as: d.operator, call: retryJob("Smith^John"), code: connect.CodeInvalidArgument, msg: "id"},
		{name: "no such job", as: d.operator, call: retryJob("nosuchjob"), code: connect.CodeNotFound, msg: "no job with this id"},
		{name: "a job in progress", as: d.operator, call: retryJob(running.GetId()), code: connect.CodeFailedPrecondition},
		{name: "a job done with nothing to send again", as: d.operator, call: retryJob(done.GetId()), code: connect.CodeFailedPrecondition},
		{name: "retried", as: d.operator, call: retryJob(partial.GetId()),
			audit: &wantAudit{log: "lab1", outcome: master.OutcomeDone, object: partial.GetId(), details: map[string]string{"retry_job": "*"}},
			check: func(t *testing.T, o outcome) {
				r := o.res.(*v1.RetryJobResponse)
				if r.GetJob().GetId() != partial.GetId() || r.GetJob().GetRetryJob() != r.GetRetry().GetId() || r.GetRetry().GetRetryOf() != partial.GetId() ||
					r.GetRetry().GetState() != v1.JobState_JOB_STATE_QUEUED || r.GetJob().GetState() != v1.JobState_JOB_STATE_PARTIAL ||
					r.GetRetry().GetSend().GetSender() != "pacs" || r.GetRetry().GetSend().GetSubjectLabel() != subject {
					t.Errorf("%v", r)
				}
				retryID = r.GetRetry().GetId()
				entries := entriesOf(f.tenantLog("lab1"), o.requestID)
				if len(entries) != 1 || entries[0].Details["retry_job"] != retryID {
					t.Errorf("the entry names the new job %q: %+v", retryID, entries)
				}
			}},
		{name: "again, as it stands", as: d.operator, call: retryJob(partial.GetId()), check: func(t *testing.T, o outcome) {
			if r := o.res.(*v1.RetryJobResponse); r.GetRetry().GetId() != retryID {
				t.Errorf("a repeat made %q, not %q", r.GetRetry().GetId(), retryID)
			}
		}},
	})
	// The sender removed (its jobs ended or canceled with it): a job of it
	// cannot be retried.
	mustCall[*v1.RemoveSenderResponse](t, "remove", removeSender("pacs", true)(f.as(local)))
	f.run(t, []tcase{
		{name: "a job whose sender was removed", as: d.operator, call: retryJob(done.GetId()), code: connect.CodeFailedPrecondition},
	})
}

// testListJobFailures guards ListJobFailures: reader on the tenant and up;
// the refused instances, and with include_warnings those sent with a
// warning, in the order of labels then SOP Instance UID; paging; a job not
// there; and the entry with the job's id.
func testListJobFailures(t *testing.T) {
	d := newJobData(t)
	f := d.f
	sj := d.session.GetSubjectLabel()
	job := d.makeJob(t, "pacs", sj)
	d.work(t, "pacs", true, true)
	list := func(id string, warnings bool, size int32, token string) func(clients) outcome {
		return func(c clients) outcome {
			return call(c.jobs.ListJobFailures, &v1.ListJobFailuresRequest{Tenant: "lab1", Id: id, IncludeWarnings: warnings, PageSize: size, PageToken: token})
		}
	}
	j := mustCall[*v1.GetJobResponse](t, "the job", getJob(job.GetId())(f.as(f.lab1Reader))).GetJob().GetCounts()
	var all []*v1.JobFailure
	f.run(t, []tcase{
		{name: "no key", as: nobody, call: list(job.GetId(), false, 0, ""), code: connect.CodeUnauthenticated},
		{name: "no grant", as: f.noGrant, call: list(job.GetId(), false, 0, ""), code: connect.CodePermissionDenied},
		{name: "a bad id", as: f.lab1Reader, call: list("Smith^John", false, 0, ""), code: connect.CodeInvalidArgument, msg: "id"},
		{name: "no such job", as: f.lab1Reader, call: list("nosuchjob", false, 0, ""), code: connect.CodeNotFound, msg: "no job with this id"},
		{name: "a negative page size", as: f.lab1Reader, call: list(job.GetId(), false, -1, ""), code: connect.CodeInvalidArgument, msg: "page_size"},
		{name: "the refused", as: f.lab1Reader, call: list(job.GetId(), false, 0, ""), audit: &wantAudit{log: "lab1", outcome: master.OutcomeDone, object: job.GetId()},
			check: func(t *testing.T, o outcome) {
				fs := o.res.(*v1.ListJobFailuresResponse).GetFailures()
				if int64(len(fs)) != j.GetInstancesRefused() || len(fs) == 0 {
					t.Fatalf("%d failures, the job refused %d", len(fs), j.GetInstancesRefused())
				}
				for _, x := range fs {
					if x.GetCode() != v1.JobFailureCode_JOB_FAILURE_CODE_PEER_REFUSED || x.GetPeerStatus() != 0xC000 || x.GetInstanceId() == "" ||
						x.GetSubjectLabel() != sj || x.GetSessionLabel() == "" || x.GetScanLabel() == "" || x.GetTime() == nil || x.GetDetail() == "" {
						t.Errorf("a failure: %v", x)
					}
				}
			}},
		{name: "with the warnings", as: f.lab1Reader, call: list(job.GetId(), true, 0, ""), check: func(t *testing.T, o outcome) {
			all = o.res.(*v1.ListJobFailuresResponse).GetFailures()
			if int64(len(all)) != j.GetInstancesRefused()+1 {
				t.Fatalf("%d failures, want %d", len(all), j.GetInstancesRefused()+1)
			}
			warned := 0
			for i, x := range all {
				if x.GetCode() == v1.JobFailureCode_JOB_FAILURE_CODE_PEER_WARNING {
					warned++
				}
				if i > 0 {
					a, b := all[i-1], x
					ka, kb := a.GetSubjectLabel()+"\x00"+a.GetSessionLabel()+"\x00"+a.GetScanLabel()+"\x00"+a.GetInstanceId(), b.GetSubjectLabel()+"\x00"+b.GetSessionLabel()+"\x00"+b.GetScanLabel()+"\x00"+b.GetInstanceId()
					if strings.Compare(ka, kb) >= 0 {
						t.Errorf("not in order of labels then instance: %q then %q", ka, kb)
					}
				}
			}
			if warned != 1 {
				t.Errorf("%d warnings", warned)
			}
		}},
		{name: "a page of one", as: f.lab1Reader, call: list(job.GetId(), true, 1, ""), check: func(t *testing.T, o outcome) {
			r := o.res.(*v1.ListJobFailuresResponse)
			if len(r.GetFailures()) != 1 || r.GetFailures()[0].GetInstanceId() != all[0].GetInstanceId() || (len(all) > 1 && r.GetNextPageToken() == "") {
				t.Errorf("%v", r)
			}
		}},
		{name: "a token that is not one", as: f.lab1Reader, call: list(job.GetId(), true, 1, "x"), code: connect.CodeInvalidArgument, msg: "page_token"},
	})
}

// TestJobChangesWakeTheSenders guards the notice to the part of the server
// that sends: a job made, canceled and retried each tells it the tenant's
// jobs changed, so a worker takes the work at once and not at its next
// look.
func TestJobChangesWakeTheSenders(t *testing.T) {
	d := newJobData(t)
	sending := d.f.sending()
	j := d.makeJob(t, "pacs", d.session.GetSubjectLabel())
	if n := sending.changes(); n != 1 {
		t.Fatalf("%d notices after CreateJob, want 1", n)
	}
	mustCall[*v1.CancelJobResponse](t, "cancel", cancelJob(j.GetId())(d.f.as(local)))
	if n := sending.changes(); n != 2 {
		t.Fatalf("%d notices after CancelJob, want 2", n)
	}
	mustCall[*v1.RetryJobResponse](t, "retry", retryJob(j.GetId())(d.f.as(local)))
	if n := sending.changes(); n != 3 {
		t.Errorf("%d notices after RetryJob, want 3", n)
	}
}
