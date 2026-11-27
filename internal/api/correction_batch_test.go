package api_test

import (
	"context"
	"strings"
	"testing"
	"time"

	connect "connectrpc.com/connect/v2"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/access"
	"github.com/amrshadid/nopacs/internal/correct"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// batchCall is a call of DeleteBatch on lab1.
func batchCall(targets []*v1.CorrectionTarget, reason, confirm string, validateOnly bool) func(clients) outcome {
	return func(c clients) outcome {
		return call(c.corrections.DeleteBatch, &v1.DeleteBatchRequest{Tenant: "lab1", Targets: targets, Reason: reason, Confirm: confirm,
			ValidateOnly: validateOnly})
	}
}

// batchTargetsCall is a call of ListDeleteBatchTargets on lab1.
func batchTargetsCall(job string, size int32, token string) func(clients) outcome {
	return func(c clients) outcome {
		return call(c.corrections.ListDeleteBatchTargets, &v1.ListDeleteBatchTargetsRequest{Tenant: "lab1", JobId: job, PageSize: size, PageToken: token})
	}
}

// runBatch runs one pass of lab1's batch job on its corrections engine, as
// the server's background work does.
func (c *correctionArchive) runBatch(t *testing.T, job string) correct.BatchResult {
	t.Helper()
	ctx := context.Background()
	st, err := c.registry.Store("lab1")
	if err != nil {
		t.Fatal(err)
	}
	e, release, err := c.engines.Use(ctx, "lab1", st)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	res, err := e.RunBatch(ctx, correct.BatchRun{Store: st.(tenant.DeleteBatches), JobID: job,
		Audit: func(tenant.CorrectionTarget, time.Time) master.AuditEntry {
			return master.AuditEntry{ID: c.newID(), Time: c.now(), LocalUser: "server", Method: "test", TenantName: "lab1", Outcome: master.OutcomeDone}
		}})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// testDeleteBatch guards DeleteBatch: admin on the tenant and never a
// masked key; every invalid list, the tenant's bound included; a target the
// tenant does not hold named by its place; the preview lists every target
// with its hold, the totals and a token of at most 128 bytes, and deletes
// nothing; a token is refused for other targets, another reason, or when
// this server did not make it; the token makes one job of kind
// delete_batch, answered again for the same token; a second batch is
// refused while it has not ended; its run deletes the target not held and
// skips the held one; a delete it made is undone as any delete.
func testDeleteBatch(t *testing.T) {
	c := newCorrectionArchive(t)
	masked, _ := c.maskedKey(c.onTenant("lab1", master.RoleAdmin))
	both := []*v1.CorrectionTarget{subjectTarget(c.subject), subjectTarget(c.other)}
	hold := mustCall[*v1.PlaceHoldResponse](t, "PlaceHold", placeCall(&v1.HoldTarget{SubjectLabel: c.other}, "litigation")(c.as(local)))
	var confirm, job string
	c.run(t, []tcase{
		{name: "no grant", as: c.noGrant, call: batchCall(both, "x", "", true), code: connect.CodePermissionDenied},
		{name: "operator", as: c.lab1Operator, call: batchCall(both, "x", "", true), code: connect.CodePermissionDenied},
		{name: "a masked admin", as: masked, call: batchCall(both, "x", "", true), code: connect.CodePermissionDenied, msg: access.MessageMaskedBulk,
			audit: &wantAudit{outcome: master.OutcomeRefused, reason: "masked key", details: map[string]string{access.DetailMasked: "true"}}},
		{name: "no target", as: c.lab1Admin, call: batchCall(nil, "x", "", true), code: connect.CodeInvalidArgument, msg: "targets",
			audit: &wantAudit{log: "lab1", outcome: master.OutcomeFailed, details: map[string]string{"targets": "0", "validate_only": "true"}}},
		{name: "no reason", as: c.lab1Admin, call: batchCall(both, "", "", true), code: connect.CodeInvalidArgument, msg: "reason"},
		{name: "an instance", as: c.lab1Admin, call: batchCall([]*v1.CorrectionTarget{{SubjectLabel: c.subject, SessionLabel: "s", ScanLabel: "x",
			InstanceId: "1.2.3"}}, "x", "", true), code: connect.CodeInvalidArgument, msg: "targets"},
		{name: "a target twice", as: c.lab1Admin, call: batchCall([]*v1.CorrectionTarget{subjectTarget(c.subject), subjectTarget(strings.ToUpper(c.subject))},
			"x", "", true), code: connect.CodeInvalidArgument, msg: "targets"},
		{name: "a token with a preview", as: c.lab1Admin, call: batchCall(both, "x", "k2.x", true), code: connect.CodeInvalidArgument, msg: "confirm"},
		{name: "no token", as: c.lab1Admin, call: batchCall(both, "x", "", false), code: connect.CodeInvalidArgument, msg: "confirm"},
		{name: "a bound of one", as: local, call: setSetting("correction.batch-max", tenantScope("lab1"), "1")},
		{name: "over the tenant's bound", as: c.lab1Admin, call: batchCall(both, "x", "", true), code: connect.CodeInvalidArgument,
			msg: "correction.batch-max"},
		{name: "the bound lifted", as: local, call: unsetSetting("correction.batch-max", tenantScope("lab1"))},
		{name: "a subject the tenant does not hold", as: c.lab1Admin, call: batchCall([]*v1.CorrectionTarget{subjectTarget(c.subject), subjectTarget("nobody")},
			"x", "", true), code: connect.CodeNotFound, msg: "target 2"},
		{name: "preview", as: c.lab1Admin, call: batchCall(both, "consent withdrawn", "", true),
			audit: &wantAudit{log: "lab1", outcome: master.OutcomeDone, details: map[string]string{"targets": "2", "validate_only": "true",
				"reason": "consent withdrawn"}},
			check: func(t *testing.T, o outcome) {
				r := o.res.(*v1.DeleteBatchResponse)
				p := r.GetPreview()
				confirm = p.GetConfirm()
				if r.GetJob() != nil || confirm == "" || len(confirm) > 128 || len(p.GetItems()) != 2 || p.GetTargetsHeld() != 1 ||
					p.GetCounts().GetInstances() != 3 || p.GetPurgeTime() == nil {
					t.Fatalf("the preview is %v", r)
				}
				first, second := p.GetItems()[0], p.GetItems()[1]
				if first.GetTarget().GetSubjectLabel() != c.subject || len(first.GetHolds()) != 0 || first.GetCounts().GetInstances() != 1 ||
					len(first.GetStorages()) != 1 || second.GetTarget().GetSubjectLabel() != c.other || len(second.GetHolds()) != 1 ||
					second.GetHolds()[0] != hold.GetHold().GetId() {
					t.Errorf("the preview's items are %v", p.GetItems())
				}
				if !c.subjectAnswers(t, c.subject) {
					t.Error("a preview deleted the subject")
				}
			}},
		{name: "the token with another reason", as: c.lab1Admin, call: func(cl clients) outcome { return batchCall(both, "another", confirm, false)(cl) },
			code: connect.CodeFailedPrecondition},
		{name: "the token with other targets", as: c.lab1Admin, call: func(cl clients) outcome {
			return batchCall(both[:1], "consent withdrawn", confirm, false)(cl)
		}, code: connect.CodeFailedPrecondition},
		{name: "a token this server did not make", as: c.lab1Admin, call: batchCall(both, "consent withdrawn", "k2.b1.1700000000000000."+
			strings.Repeat("0", 64)+".AAAAAAAAAAAAAAAAAAAAAA", false), code: connect.CodeFailedPrecondition},
		{name: "confirm", as: c.lab1Admin, call: func(cl clients) outcome { return batchCall(both, "consent withdrawn", confirm, false)(cl) },
			audit: &wantAudit{log: "lab1", outcome: master.OutcomeDone, details: map[string]string{"targets": "2", "validate_only": "false", "job": "*"}},
			check: func(t *testing.T, o outcome) {
				j := o.res.(*v1.DeleteBatchResponse).GetJob()
				job = j.GetId()
				if j.GetDeleteBatch().GetReason() != "consent withdrawn" || j.GetDeleteBatch().GetTargets() != 2 || j.GetState() != v1.JobState_JOB_STATE_QUEUED ||
					j.GetCounts().GetScans() != 2 || j.GetPaused() {
					t.Errorf("the job is %v", j)
				}
			}},
		{name: "the token again", as: c.lab1Admin, call: func(cl clients) outcome { return batchCall(both, "consent withdrawn", confirm, false)(cl) },
			check: func(t *testing.T, o outcome) {
				if got := o.res.(*v1.DeleteBatchResponse).GetJob().GetId(); got != job {
					t.Errorf("the token again answers job %s, want %s", got, job)
				}
			}},
	})
	p := mustCall[*v1.DeleteBatchResponse](t, "preview", batchCall(both[:1], "again", "", true)(c.as(c.lab1Admin)))
	o := batchCall(both[:1], "again", p.GetPreview().GetConfirm(), false)(c.as(c.lab1Admin))
	if o.code() != connect.CodeFailedPrecondition || !strings.Contains(o.message(), "one runs at a time") {
		t.Fatalf("a second batch while the first runs answers %v", o.err)
	}
	if res := c.runBatch(t, job); !res.Ended || res.Met[tenant.BatchDeleted] != 1 || res.Met[tenant.BatchSkippedHeld] != 1 {
		t.Fatalf("the run answers %+v", res)
	}
	if c.subjectAnswers(t, c.subject) || !c.subjectAnswers(t, c.other) {
		t.Fatal("after the run the catalog does not show the held subject alone")
	}
	ts := mustCall[*v1.ListDeleteBatchTargetsResponse](t, "targets", batchTargetsCall(job, 0, "")(c.as(c.lab1Operator))).GetTargets()
	if len(ts) != 2 || ts[0].GetOutcome() != v1.DeleteBatchOutcome_DELETE_BATCH_OUTCOME_DELETED || ts[0].GetCorrectionId() == "" ||
		ts[1].GetOutcome() != v1.DeleteBatchOutcome_DELETE_BATCH_OUTCOME_SKIPPED_HELD || ts[1].GetCorrectionId() != "" {
		t.Fatalf("the targets are %v", ts)
	}
	j := mustCall[*v1.GetJobResponse](t, "GetJob", call(c.as(c.lab1Operator).jobs.GetJob, &v1.GetJobRequest{Tenant: "lab1", Id: job}))
	if j.GetJob().GetState() != v1.JobState_JOB_STATE_PARTIAL || len(j.GetJob().GetTallies()) != 2 {
		t.Fatalf("the ended batch is %v", j.GetJob())
	}
	cor := mustCall[*v1.GetCorrectionResponse](t, "GetCorrection", call(c.as(c.lab1Operator).corrections.GetCorrection,
		&v1.GetCorrectionRequest{Tenant: "lab1", Id: ts[0].GetCorrectionId()})).GetCorrection()
	if cor.GetReason() != "consent withdrawn" || cor.GetKind() != v1.CorrectionKind_CORRECTION_KIND_DELETE || cor.GetCreatedByKeyId() == "" {
		t.Fatalf("the batch's delete is %v", cor)
	}
	u := mustCall[*v1.UndoCorrectionResponse](t, "undo", call(c.as(c.lab1Admin).corrections.UndoCorrection,
		&v1.UndoCorrectionRequest{Tenant: "lab1", Id: ts[0].GetCorrectionId(), Reason: "deleted by mistake"}))
	if u.GetCorrection().GetState() != v1.CorrectionState_CORRECTION_STATE_UNDONE || !c.subjectAnswers(t, c.subject) {
		t.Fatalf("the undo of the batch's delete answers %v", u.GetCorrection())
	}
	_ = mustCall[*v1.ReleaseHoldResponse](t, "ReleaseHold", releaseCall(hold.GetHold().GetId(), "done")(c.as(local)))
}

// testListDeleteBatchTargets guards ListDeleteBatchTargets: operator on the
// tenant; the targets in order, in pages whose token is bound to the job;
// a job the tenant does not have is not found.
func testListDeleteBatchTargets(t *testing.T) {
	c := newCorrectionArchive(t)
	both := []*v1.CorrectionTarget{subjectTarget(c.subject), subjectTarget(c.other)}
	p := mustCall[*v1.DeleteBatchResponse](t, "preview", batchCall(both, "consent withdrawn", "", true)(c.as(c.lab1Admin)))
	job := mustCall[*v1.DeleteBatchResponse](t, "confirm", batchCall(both, "consent withdrawn", p.GetPreview().GetConfirm(), false)(c.as(c.lab1Admin))).
		GetJob().GetId()
	var token string
	c.run(t, []tcase{
		{name: "reader", as: c.lab1Reader, call: batchTargetsCall(job, 0, ""), code: connect.CodePermissionDenied},
		{name: "a page of one", as: c.lab1Operator, call: batchTargetsCall(job, 1, ""),
			audit: &wantAudit{log: "lab1", outcome: master.OutcomeDone, object: job},
			check: func(t *testing.T, o outcome) {
				r := o.res.(*v1.ListDeleteBatchTargetsResponse)
				token = r.GetNextPageToken()
				if len(r.GetTargets()) != 1 || r.GetTargets()[0].GetPosition() != 1 || r.GetTargets()[0].GetTarget().GetSubjectLabel() != c.subject ||
					r.GetTargets()[0].GetOutcome() != v1.DeleteBatchOutcome_DELETE_BATCH_OUTCOME_PENDING || token == "" {
					t.Errorf("the first page is %v", r)
				}
			}},
		{name: "the next page", as: c.lab1Operator, call: func(cl clients) outcome { return batchTargetsCall(job, 1, token)(cl) },
			check: func(t *testing.T, o outcome) {
				r := o.res.(*v1.ListDeleteBatchTargetsResponse)
				if len(r.GetTargets()) != 1 || r.GetTargets()[0].GetTarget().GetSubjectLabel() != c.other || r.GetNextPageToken() != "" {
					t.Errorf("the last page is %v", r)
				}
			}},
		{name: "the token for another job", as: c.lab1Operator, call: func(cl clients) outcome { return batchTargetsCall("other", 1, token)(cl) },
			code: connect.CodeInvalidArgument, msg: "page_token"},
		{name: "a job the tenant does not have", as: c.lab1Operator, call: batchTargetsCall("nope", 0, ""), code: connect.CodeNotFound},
	})
}

// pauseCall and resumeCall are calls of PauseJob and ResumeJob on lab1.
func pauseCall(id string) func(clients) outcome {
	return func(c clients) outcome { return call(c.jobs.PauseJob, &v1.PauseJobRequest{Tenant: "lab1", Id: id}) }
}

// resumeCall is a call of ResumeJob on lab1.
func resumeCall(id string) func(clients) outcome {
	return func(c clients) outcome { return call(c.jobs.ResumeJob, &v1.ResumeJobRequest{Tenant: "lab1", Id: id}) }
}

// pausedBatch makes a batch of lab1's two subjects and answers its job.
func (c *correctionArchive) pausedBatch(t *testing.T) string {
	t.Helper()
	both := []*v1.CorrectionTarget{subjectTarget(c.subject), subjectTarget(c.other)}
	p := mustCall[*v1.DeleteBatchResponse](t, "preview", batchCall(both, "consent withdrawn", "", true)(c.as(c.lab1Admin)))
	return mustCall[*v1.DeleteBatchResponse](t, "confirm", batchCall(both, "consent withdrawn", p.GetPreview().GetConfirm(), false)(c.as(c.lab1Admin))).
		GetJob().GetId()
}

// testPauseJob guards PauseJob: operator on the tenant; the job answered
// paused and its run deleting nothing; a pause again answers it as it
// stands; a job of another kind and a job that ended are refused.
func testPauseJob(t *testing.T) {
	c := newCorrectionArchive(t)
	job := c.pausedBatch(t)
	c.run(t, []tcase{
		{name: "reader", as: c.lab1Reader, call: pauseCall(job), code: connect.CodePermissionDenied},
		{name: "operator", as: c.lab1Operator, call: pauseCall(job), audit: &wantAudit{log: "lab1", outcome: master.OutcomeDone, object: job},
			check: func(t *testing.T, o outcome) {
				if j := o.res.(*v1.PauseJobResponse).GetJob(); !j.GetPaused() || j.GetState() != v1.JobState_JOB_STATE_QUEUED {
					t.Errorf("the paused job is %v", j)
				}
			}},
		{name: "again", as: c.lab1Operator, call: pauseCall(job)},
		{name: "a job the tenant does not have", as: c.lab1Operator, call: pauseCall("nope"), code: connect.CodeNotFound},
	})
	if res := c.runBatch(t, job); !res.Stopped || !c.subjectAnswers(t, c.subject) {
		t.Fatalf("a paused batch's run answers %+v", res)
	}
	c.run(t, []tcase{
		{name: "canceled", as: c.lab1Operator, call: func(cl clients) outcome {
			return call(cl.jobs.CancelJob, &v1.CancelJobRequest{Tenant: "lab1", Id: job})
		}},
		{name: "a job that ended", as: c.lab1Operator, call: pauseCall(job), code: connect.CodeFailedPrecondition},
	})
	scrub := mustCall[*v1.CreateJobResponse](t, "CreateJob", call(c.as(c.lab1Operator).jobs.CreateJob, &v1.CreateJobRequest{Tenant: "lab1",
		Kind: &v1.CreateJobRequest_Scrub{Scrub: &v1.ScrubJob{}}})).GetJob().GetId()
	c.run(t, []tcase{
		{name: "a job of another kind", as: c.lab1Operator, call: pauseCall(scrub), code: connect.CodeFailedPrecondition, msg: "only a batch delete job"},
	})
}

// testResumeJob guards ResumeJob: admin on the tenant and never a masked
// key; the job answered no longer paused, and its run then deletes.
func testResumeJob(t *testing.T) {
	c := newCorrectionArchive(t)
	masked, _ := c.maskedKey(c.onTenant("lab1", master.RoleAdmin))
	job := c.pausedBatch(t)
	_ = mustCall[*v1.PauseJobResponse](t, "PauseJob", pauseCall(job)(c.as(c.lab1Operator)))
	c.run(t, []tcase{
		{name: "operator", as: c.lab1Operator, call: resumeCall(job), code: connect.CodePermissionDenied},
		{name: "a masked admin", as: masked, call: resumeCall(job), code: connect.CodePermissionDenied, msg: access.MessageMaskedBulk},
		{name: "admin", as: c.lab1Admin, call: resumeCall(job), audit: &wantAudit{log: "lab1", outcome: master.OutcomeDone, object: job},
			check: func(t *testing.T, o outcome) {
				if j := o.res.(*v1.ResumeJobResponse).GetJob(); j.GetPaused() {
					t.Errorf("the resumed job is %v", j)
				}
			}},
	})
	if res := c.runBatch(t, job); !res.Ended || c.subjectAnswers(t, c.subject) || c.subjectAnswers(t, c.other) {
		t.Fatalf("the resumed batch's run answers %+v", res)
	}
}
