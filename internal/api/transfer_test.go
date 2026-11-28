package api_test

import (
	"context"
	"testing"
	"time"

	connect "connectrpc.com/connect/v2"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/access"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// transferCall is a call of CreateTransfer from lab1 to lab2.
func transferCall(subject, session, target, receiver, reason string, move, validateOnly bool) func(clients) outcome {
	return func(c clients) outcome {
		return call(c.transfers.CreateTransfer, &v1.CreateTransferRequest{Tenant: "lab1", SubjectLabel: subject, SessionLabel: session,
			TargetTenant: target, Receiver: receiver, Move: move, Reason: reason, ValidateOnly: validateOnly})
	}
}

// addJobReceiver gives lab2 a storage and an enabled receiver of type job
// named name, de-identifying with no profile (a profile version needs a
// stored profile, which this test does not need).
func (c *correctionArchive) addJobReceiver(t *testing.T, name string) {
	t.Helper()
	if _, err := c.pipelineOf("lab2").Storage(context.Background(), "main"); err != nil {
		mustCall[*v1.AddStorageResponse](t, "add lab2's storage", addStorage("lab2", dirStorage("main", "fast", "lab2"))(c.as(local)))
	}
	entry := master.AuditEntry{ID: "a-" + name + "-job-receiver", Time: t0, KeyID: "K1234567890a", RemoteAddress: "192.0.2.10:50000", Method: "Test",
		TenantName: "lab2", Outcome: master.OutcomeDone, RequestID: "r-" + name}
	if _, err := c.pipelineOf("lab2").AddReceiver(context.Background(), tenant.Receiver{Name: name, Type: tenant.ReceiverJob, Enabled: true, Storage: "main",
		QuietPeriod: 5 * time.Second, MaxOpenTime: time.Minute, MaxFileBytes: 1 << 30}, t0, entry, nil); err != nil {
		t.Fatalf("AddReceiver: %v", err)
	}
}

// testCreateTransfer guards CreateTransfer: admin on the source and writer
// on the target (a target the caller holds no grant on is answered as no
// grant, never as missing), never a masked key; each invalid line; a subject
// the source does not hold; a target with no receiver of type job, or several
// with none named; the preview, which makes nothing and counts what the
// receiver would refuse; the job made, queued at its first step, with an
// entry in each tenant's log; a second transfer of the subject while one has
// not ended.
func testCreateTransfer(t *testing.T) {
	c := newCorrectionArchive(t)
	masked, _ := c.maskedKey(c.onTenant("lab1", master.RoleAdmin), c.onTenant("lab2", master.RoleWriter))
	both, _ := c.key(c.onTenant("lab1", master.RoleAdmin), c.onTenant("lab2", master.RoleWriter))
	lab2Reader, _ := c.key(c.onTenant("lab1", master.RoleAdmin), c.onTenant("lab2", master.RoleReader))
	var job string
	c.run(t, []tcase{
		{name: "no grant", as: c.noGrant, call: transferCall(c.subject, "", "lab2", "", "x", false, true), code: connect.CodePermissionDenied},
		{name: "operator on the source", as: c.lab1Operator, call: transferCall(c.subject, "", "lab2", "", "x", false, true), code: connect.CodePermissionDenied},
		{name: "admin on the source and no grant on the target", as: c.lab1Admin, call: transferCall(c.subject, "", "lab2", "", "x", false, true),
			code: connect.CodePermissionDenied, msg: access.MessageDenied},
		{name: "reader on the target", as: lab2Reader, call: transferCall(c.subject, "", "lab2", "", "x", false, true), code: connect.CodePermissionDenied},
		{name: "a target that does not exist, for a caller with no grant on it", as: c.lab1Admin, call: transferCall(c.subject, "", "lab9", "", "x", false, true),
			code: connect.CodePermissionDenied, msg: access.MessageDenied},
		{name: "a target that does not exist, for the local operator", as: local, call: transferCall(c.subject, "", "lab9", "", "x", false, true),
			code: connect.CodeNotFound},
		{name: "a masked key", as: masked, call: transferCall(c.subject, "", "lab2", "", "x", false, true), code: connect.CodePermissionDenied,
			msg: access.MessageMaskedDestination, audit: &wantAudit{outcome: master.OutcomeRefused, reason: "masked key", details: map[string]string{access.DetailMasked: "true"}}},
		{name: "no subject", as: both, call: transferCall("", "", "lab2", "", "x", false, true), code: connect.CodeInvalidArgument, msg: "subject_label"},
		{name: "a subject label that is not one", as: both, call: transferCall("a b", "", "lab2", "", "x", false, true), code: connect.CodeInvalidArgument, msg: "subject_label"},
		{name: "a session label that is not one", as: both, call: transferCall(c.subject, "a b", "lab2", "", "x", false, true), code: connect.CodeInvalidArgument, msg: "session_label"},
		{name: "the source as the target", as: both, call: transferCall(c.subject, "", "lab1", "", "x", false, true), code: connect.CodeInvalidArgument, msg: "target_tenant"},
		{name: "no reason", as: both, call: transferCall(c.subject, "", "lab2", "", "", false, true), code: connect.CodeInvalidArgument, msg: "reason"},
		{name: "a subject the source does not hold", as: both, call: transferCall("nobody", "", "lab2", "", "x", false, true), code: connect.CodeFailedPrecondition,
			msg: "enabled receiver of type job"},
	})
	// The target has no receiver of type job yet: the receiver is checked
	// before the subject, which a caller with a grant on both may know.
	c.addJobReceiver(t, "xfer")
	c.run(t, []tcase{
		{name: "a subject the source does not hold", as: both, call: transferCall("nobody", "", "lab2", "", "x", false, true), code: connect.CodeNotFound},
		{name: "a receiver that does not exist", as: both, call: transferCall(c.subject, "", "lab2", "nothing", "x", false, true), code: connect.CodeFailedPrecondition,
			msg: "enabled receiver of type job named nothing"},
		{name: "the preview", as: both, call: transferCall(c.subject, "", "lab2", "", "moving a study", false, true),
			audit: &wantAudit{log: "lab1", outcome: master.OutcomeDone, details: map[string]string{"target_tenant": "lab2", "move": "false", "validate_only": "true",
				"reason": "moving a study"}},
			check: func(t *testing.T, o outcome) {
				r := o.res.(*v1.CreateTransferResponse)
				p := r.GetPreview()
				if r.GetTransfer() != nil || p.GetInstances() != int64(c.instances) || p.GetSessions() != 1 || p.GetReceiver() != "xfer" || p.GetReceiverDeidentifies() ||
					!p.GetRefusalsCounted() || p.GetRefusedInstances() != int64(c.instances) || p.GetBytes() == 0 {
					t.Fatalf("the preview is %v, want the subject's instances all refused (archived as received, a receiver that does not de-identify)", r)
				}
				c.checkAudit(t, "the preview in the target's log", o.requestID, wantAudit{log: "lab2", outcome: master.OutcomeDone,
					details: map[string]string{"target_tenant": "lab2", "validate_only": "true"}})
				if jobs := mustCall[*v1.ListJobsResponse](t, "ListJobs", call(c.as(local).jobs.ListJobs, &v1.ListJobsRequest{Tenant: "lab1"})); len(jobs.GetJobs()) != 0 {
					t.Fatalf("a preview made a job: %v", jobs)
				}
			}},
		{name: "the transfer", as: both, call: transferCall(c.subject, "", "lab2", "", "moving a study", true, false),
			audit: &wantAudit{log: "lab1", outcome: master.OutcomeDone, details: map[string]string{"target_tenant": "lab2", "move": "true", "validate_only": "false",
				"reason": "moving a study", "job": "*"}},
			check: func(t *testing.T, o outcome) {
				r := o.res.(*v1.CreateTransferResponse)
				tr := r.GetTransfer()
				job = tr.GetJob().GetId()
				if job == "" || tr.GetSubjectLabel() != c.subject || tr.GetSessionLabel() != "" || tr.GetTargetTenant() != "lab2" || tr.GetReceiver() != "xfer" || !tr.GetMove() ||
					tr.GetReason() != "moving a study" || tr.GetStep() != v1.TransferStep_TRANSFER_STEP_LISTING || tr.GetJob().GetState() != v1.JobState_JOB_STATE_QUEUED ||
					tr.GetJob().GetKind() != nil || tr.GetItems().GetListed() != 0 {
					t.Fatalf("the transfer is %v", r)
				}
				c.checkAudit(t, "the transfer in the target's log", o.requestID, wantAudit{log: "lab2", outcome: master.OutcomeDone,
					details: map[string]string{"target_tenant": "lab2", "move": "true", "job": job}})
			}},
		{name: "a second transfer of the subject", as: both, call: transferCall(c.subject, "", "lab2", "", "again", false, false), code: connect.CodeFailedPrecondition,
			msg: "has not ended"},
	})
	// JobService shows the job, of a kind a client that does not know it shows
	// by state and counts alone.
	g := mustCall[*v1.GetJobResponse](t, "GetJob", call(c.as(local).jobs.GetJob, &v1.GetJobRequest{Tenant: "lab1", Id: job}))
	if g.GetJob().GetId() != job || g.GetJob().GetKind() != nil || g.GetJob().GetState() != v1.JobState_JOB_STATE_QUEUED {
		t.Fatalf("GetJob of the transfer is %v", g)
	}
	// A second receiver of type job makes the choice the caller's.
	c.addJobReceiver(t, "xfer2")
	c.run(t, []tcase{
		{name: "several receivers, none named", as: both, call: transferCall(c.subject, "", "lab2", "", "x", false, true), code: connect.CodeFailedPrecondition,
			msg: "name one"},
		{name: "several receivers, one named", as: both, call: transferCall(c.subject, "", "lab2", "xfer2", "x", false, true)},
	})
}

// testGetTransfer guards GetTransfer: operator on the tenant, a reader
// refused; an id the tenant does not hold is not found; the transfer made is
// answered with its step and its items.
func testGetTransfer(t *testing.T) {
	c := newCorrectionArchive(t)
	c.addJobReceiver(t, "xfer")
	both, _ := c.key(c.onTenant("lab1", master.RoleAdmin), c.onTenant("lab2", master.RoleWriter))
	made := mustCall[*v1.CreateTransferResponse](t, "CreateTransfer", transferCall(c.subject, "", "lab2", "", "moving", false, false)(c.as(both)))
	id := made.GetTransfer().GetJob().GetId()
	get := func(tenantName, id string) func(clients) outcome {
		return func(cl clients) outcome {
			return call(cl.transfers.GetTransfer, &v1.GetTransferRequest{Tenant: tenantName, Id: id})
		}
	}
	c.run(t, []tcase{
		{name: "no grant", as: c.noGrant, call: get("lab1", id), code: connect.CodePermissionDenied},
		{name: "reader", as: c.lab1Reader, call: get("lab1", id), code: connect.CodePermissionDenied},
		{name: "no id", as: c.lab1Operator, call: get("lab1", ""), code: connect.CodeInvalidArgument, msg: "id"},
		{name: "an id the tenant does not hold", as: c.lab1Operator, call: get("lab1", "nojob"), code: connect.CodeNotFound},
		{name: "the other tenant has none", as: local, call: get("lab2", id), code: connect.CodeNotFound},
		{name: "the transfer", as: c.lab1Operator, call: get("lab1", id),
			audit: &wantAudit{log: "lab1", outcome: master.OutcomeDone, object: id},
			check: func(t *testing.T, o outcome) {
				tr := o.res.(*v1.GetTransferResponse).GetTransfer()
				if tr.GetJob().GetId() != id || tr.GetTargetTenant() != "lab2" || tr.GetMove() || tr.GetStep() != v1.TransferStep_TRANSFER_STEP_LISTING {
					t.Fatalf("the transfer is %v", tr)
				}
			}},
	})
}

// testListTransfers guards ListTransfers: operator on the tenant; the
// transfers made, newest first, in pages.
func testListTransfers(t *testing.T) {
	c := newCorrectionArchive(t)
	c.addJobReceiver(t, "xfer")
	both, _ := c.key(c.onTenant("lab1", master.RoleAdmin), c.onTenant("lab2", master.RoleWriter))
	first := mustCall[*v1.CreateTransferResponse](t, "first", transferCall(c.subject, "", "lab2", "", "one", false, false)(c.as(both)))
	// One transfer of a subject runs at a time: end the first, so a second of
	// another subject and a third of the same follow.
	second := mustCall[*v1.CreateTransferResponse](t, "second", transferCall(c.other, "", "lab2", "", "two", false, false)(c.as(both)))
	list := func(size int32, token string) func(clients) outcome {
		return func(cl clients) outcome {
			return call(cl.transfers.ListTransfers, &v1.ListTransfersRequest{Tenant: "lab1", PageSize: size, PageToken: token})
		}
	}
	var token string
	c.run(t, []tcase{
		{name: "no grant", as: c.noGrant, call: list(0, ""), code: connect.CodePermissionDenied},
		{name: "reader", as: c.lab1Reader, call: list(0, ""), code: connect.CodePermissionDenied},
		{name: "a token this list did not return", as: c.lab1Operator, call: list(1, "x"), code: connect.CodeInvalidArgument, msg: "page_token"},
		{name: "the first page", as: c.lab1Operator, call: list(1, ""), check: func(t *testing.T, o outcome) {
			r := o.res.(*v1.ListTransfersResponse)
			token = r.GetNextPageToken()
			if len(r.GetTransfers()) != 1 || token == "" || r.GetTransfers()[0].GetJob().GetId() != second.GetTransfer().GetJob().GetId() {
				t.Fatalf("the first page is %v, want the newer transfer and a token", r)
			}
		}},
		{name: "the second page", as: c.lab1Operator, call: func(cl clients) outcome { return list(1, token)(cl) }, check: func(t *testing.T, o outcome) {
			r := o.res.(*v1.ListTransfersResponse)
			if len(r.GetTransfers()) != 1 || r.GetNextPageToken() != "" || r.GetTransfers()[0].GetJob().GetId() != first.GetTransfer().GetJob().GetId() {
				t.Fatalf("the second page is %v, want the older transfer and no token", r)
			}
		}},
	})
}
