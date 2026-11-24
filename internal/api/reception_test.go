package api_test

import (
	"slices"
	"strings"
	"testing"

	connect "connectrpc.com/connect/v2"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/access"
	"github.com/amrshadid/nopacs/internal/master"
)

// study is the Study Instance UID the tests' receptions are of: made up,
// under the 2.25 root (PS3.5 B.2).
const study = "2.25.1234567890123456789"

// testListReceptions guards ListReceptions: reader on the tenant, and
// operator to filter by Study Instance UID, which the entry records only as
// present; the filters checked; and a page token bound to its filters.
func testListReceptions(t *testing.T) {
	f := newPlaceFixture(t)
	f.receiving(t)
	first := f.failedReception(t, "scanner1", study)
	second := f.failedReception(t, "scanner1", study)
	list := func(req *v1.ListReceptionsRequest) func(clients) outcome {
		return func(c clients) outcome {
			if req.Tenant == "" {
				req.Tenant = "lab1"
			}
			return call(c.receptions.ListReceptions, req)
		}
	}
	ids := func(o outcome) string {
		var out []string
		for _, r := range o.res.(*v1.ListReceptionsResponse).GetReceptions() {
			out = append(out, r.GetId())
		}
		return strings.Join(out, ",")
	}
	var token string
	f.run(t, []tcase{
		{name: "no key", as: nobody, call: list(&v1.ListReceptionsRequest{}), code: connect.CodeUnauthenticated},
		{name: "no grant", as: f.noGrant, call: list(&v1.ListReceptionsRequest{}), code: connect.CodePermissionDenied},
		{name: "operator on the installation", as: f.instOperator, call: list(&v1.ListReceptionsRequest{}), code: connect.CodePermissionDenied},
		{name: "no such tenant, no right to know", as: f.lab1Reader, call: list(&v1.ListReceptionsRequest{Tenant: "nolab"}), code: connect.CodePermissionDenied, msg: access.MessageDenied},
		{name: "reader, newest first", as: f.lab1Reader, call: list(&v1.ListReceptionsRequest{}), audit: inTenant("lab1", master.OutcomeDone),
			check: func(t *testing.T, o outcome) {
				if got := ids(o); got != second+","+first {
					t.Errorf("%q", got)
				}
				r := o.res.(*v1.ListReceptionsResponse).GetReceptions()[0]
				if r.GetStep() != v1.ReceptionStep_RECEPTION_STEP_FAILED || r.GetFailure() != "storage unreachable" || r.GetArrival() != 2 || r.GetStudyInstanceUid() != study {
					t.Errorf("%v", r)
				}
			}},
		{name: "reader, by study", as: f.lab1Reader, call: list(&v1.ListReceptionsRequest{StudyInstanceUid: study}), code: connect.CodePermissionDenied},
		{name: "operator, by study", as: f.lab1Operator, call: list(&v1.ListReceptionsRequest{StudyInstanceUid: study}),
			audit: &wantAudit{log: "lab1", outcome: master.OutcomeDone, details: map[string]string{"study_instance_uid": "present"}},
			check: func(t *testing.T, o outcome) {
				if ids(o) != second+","+first {
					t.Errorf("%q", ids(o))
				}
			}},
		{name: "a study that is not a UID", as: f.lab1Operator, call: list(&v1.ListReceptionsRequest{StudyInstanceUid: "Smith"}), code: connect.CodeInvalidArgument, msg: "study_instance_uid"},
		{name: "a step that is not one", as: f.lab1Reader, call: list(&v1.ListReceptionsRequest{Steps: []v1.ReceptionStep{v1.ReceptionStep_RECEPTION_STEP_UNSPECIFIED}}), code: connect.CodeInvalidArgument, msg: "steps"},
		{name: "a step a number names", as: f.lab1Reader, call: list(&v1.ListReceptionsRequest{Steps: []v1.ReceptionStep{99}}), code: connect.CodeInvalidArgument, msg: "steps"},
		{name: "in progress", as: f.lab1Reader, call: list(&v1.ListReceptionsRequest{InProgress: true}), check: func(t *testing.T, o outcome) {
			if ids(o) != "" {
				t.Errorf("%q", ids(o))
			}
		}},
		{name: "an end before the start", as: f.lab1Reader, call: list(&v1.ListReceptionsRequest{StartTime: timestamppb.New(t0.Add(1)), EndTime: timestamppb.New(t0)}), code: connect.CodeInvalidArgument, msg: "end_time"},
		{name: "a receiver that is not a name", as: f.lab1Reader, call: list(&v1.ListReceptionsRequest{Receiver: "Scanner 1"}), code: connect.CodeInvalidArgument, msg: "receiver"},
		{name: "a page of one, failed", as: f.lab1Reader, call: list(&v1.ListReceptionsRequest{PageSize: 1, Steps: []v1.ReceptionStep{v1.ReceptionStep_RECEPTION_STEP_FAILED}}),
			audit: &wantAudit{log: "lab1", outcome: master.OutcomeDone, details: map[string]string{"steps": "RECEPTION_STEP_FAILED"}},
			check: func(t *testing.T, o outcome) {
				token = o.res.(*v1.ListReceptionsResponse).GetNextPageToken()
				if ids(o) != second || token == "" {
					t.Errorf("%q %q", ids(o), token)
				}
			}},
		{name: "the next page, the same filters", as: f.lab1Reader, call: func(c clients) outcome {
			return list(&v1.ListReceptionsRequest{PageSize: 1, PageToken: token, Steps: []v1.ReceptionStep{v1.ReceptionStep_RECEPTION_STEP_FAILED}})(c)
		}, check: func(t *testing.T, o outcome) {
			if ids(o) != first {
				t.Errorf("%q", ids(o))
			}
		}},
		{name: "the token with other filters", as: f.lab1Reader, call: func(c clients) outcome {
			return list(&v1.ListReceptionsRequest{PageSize: 1, PageToken: token})(c)
		}, code: connect.CodeInvalidArgument, msg: "page_token"},
	})
	// The UID is never written to a log.
	for _, e := range append(f.tenantLog("lab1"), f.masterLog()...) {
		for k, v := range e.Details {
			if strings.Contains(v, study) {
				t.Errorf("an entry holds the study's UID under %s", k)
			}
		}
	}
}

// testGetReception guards GetReception: reader and up, the arrivals of its
// study, no scans before it archived, and an id not there.
func testGetReception(t *testing.T) {
	f := newPlaceFixture(t)
	f.receiving(t)
	first := f.failedReception(t, "scanner1", study)
	second := f.failedReception(t, "scanner1", study)
	get := func(id string) func(clients) outcome {
		return func(c clients) outcome {
			return call(c.receptions.GetReception, &v1.GetReceptionRequest{Tenant: "lab1", Id: id})
		}
	}
	f.run(t, []tcase{
		{name: "no grant", as: f.noGrant, call: get(second), code: connect.CodePermissionDenied},
		{name: "reader", as: f.lab1Reader, call: get(second), audit: &wantAudit{log: "lab1", outcome: master.OutcomeDone, object: second},
			check: func(t *testing.T, o outcome) {
				res := o.res.(*v1.GetReceptionResponse)
				a := res.GetArrivals()
				if res.GetReception().GetId() != second || len(a) != 2 || a[0].GetId() != first || a[1].GetArrival() != 2 || len(res.GetScans()) != 0 || res.GetScansTruncated() {
					t.Errorf("%v", res)
				}
			}},
		{name: "no id", as: f.lab1Reader, call: get(""), code: connect.CodeInvalidArgument, msg: "id"},
		{name: "an id too long", as: f.lab1Reader, call: get(strings.Repeat("a", 65)), code: connect.CodeInvalidArgument, msg: "id"},
		{name: "not there", as: f.lab1Reader, call: get("nope"), code: connect.CodeNotFound, msg: "no reception with this id"},
	})
}

// testRetryReception guards RetryReception: operator on the tenant and
// above, a reception that has not failed refused as a precondition, a
// repeat answered as it stands with the same retry, a removed receiver
// with none named refused, the entry the store writes with the change,
// naming the new reception, and the retry handed to the reception pipeline
// whole.
func testRetryReception(t *testing.T) {
	f := newPlaceFixture(t)
	f.receiving(t)
	failed := f.failedReception(t, "scanner1", study)
	open, _, err := f.pipelineOf("lab1").OpenReception(ctx, "scanner1", "2.25.77", f.newID(), f.now())
	if err != nil {
		t.Fatal(err)
	}
	retry := func(id, receiver string) func(clients) outcome {
		return func(c clients) outcome {
			return call(c.receptions.RetryReception, &v1.RetryReceptionRequest{Tenant: "lab1", Id: id, Receiver: receiver})
		}
	}
	var made string
	f.run(t, []tcase{
		{name: "no grant", as: f.noGrant, call: retry(failed, ""), code: connect.CodePermissionDenied},
		{name: "reader", as: f.lab1Reader, call: retry(failed, ""), code: connect.CodePermissionDenied},
		{name: "operator on the installation", as: f.instOperator, call: retry(failed, ""), code: connect.CodePermissionDenied},
		{name: "no id", as: f.lab1Operator, call: retry("", ""), code: connect.CodeInvalidArgument},
		{name: "a receiver that is not a name", as: f.lab1Operator, call: retry(failed, "Scanner 2"), code: connect.CodeInvalidArgument, msg: "receiver"},
		{name: "not there", as: f.lab1Operator, call: retry("nope", ""), code: connect.CodeNotFound},
		{name: "not failed", as: f.lab1Operator, call: retry(open.ID, ""), code: connect.CodeFailedPrecondition, audit: inTenant("lab1", master.OutcomeFailed)},
		{name: "a receiver named that does not exist", as: f.lab1Operator, call: retry(failed, "scanner9"), code: connect.CodeFailedPrecondition},
		{name: "retried", as: f.lab1Operator, call: retry(failed, ""),
			check: func(t *testing.T, o outcome) {
				res := o.res.(*v1.RetryReceptionResponse)
				made = res.GetRetry().GetId()
				if res.GetReception().GetRetryReception() != made || res.GetRetry().GetRetryOf() != failed || res.GetRetry().GetStep() != v1.ReceptionStep_RECEPTION_STEP_COMPLETE {
					t.Errorf("%v", res)
				}
				f.checkAudit(t, "retried", o.requestID, wantAudit{log: "lab1", outcome: master.OutcomeDone, object: failed, details: map[string]string{"retry_reception": made}})
				// The retry ran through the pipeline, which queues the new
				// reception's first unit at once.
				if _, retried := f.parts.decisions(); !slices.Contains(retried, "lab1/"+failed) {
					t.Errorf("the retry was not handed to the pipeline: %v", retried)
				}
			}},
		{name: "retried again", as: f.lab1Admin, call: retry(failed, ""), check: func(t *testing.T, o outcome) {
			if got := o.res.(*v1.RetryReceptionResponse).GetRetry().GetId(); got != made {
				t.Errorf("a repeat made %q, the first %q", got, made)
			}
		}},
	})
	o := retry(failed, "")(f.as(f.lab1Reader))
	f.refusedNotInTenant(t, "a refusal", "lab1", o)
}

// testDiscardReception guards DiscardReception: operator and up, the reason
// required and bounded, a retried reception refused, a repeat answered with
// its first reason, the removal of its files handed to the pipeline after
// the record, and the entry with the change.
func testDiscardReception(t *testing.T) {
	f := newPlaceFixture(t)
	f.receiving(t)
	failed := f.failedReception(t, "scanner1", study)
	retried := f.failedReception(t, "scanner1", "2.25.88")
	mustCall[*v1.RetryReceptionResponse](t, "retry", call(f.as(local).receptions.RetryReception, &v1.RetryReceptionRequest{Tenant: "lab1", Id: retried}))
	discard := func(id, reason string) func(clients) outcome {
		return func(c clients) outcome {
			return call(c.receptions.DiscardReception, &v1.DiscardReceptionRequest{Tenant: "lab1", Id: id, Reason: reason})
		}
	}
	var first *v1.Reception
	f.run(t, []tcase{
		{name: "no grant", as: f.noGrant, call: discard(failed, "bad study"), code: connect.CodePermissionDenied},
		{name: "reader", as: f.lab1Reader, call: discard(failed, "bad study"), code: connect.CodePermissionDenied},
		{name: "no reason", as: f.lab1Operator, call: discard(failed, ""), code: connect.CodeInvalidArgument, msg: "reason"},
		{name: "only spaces", as: f.lab1Operator, call: discard(failed, "   "), code: connect.CodeInvalidArgument, msg: "reason"},
		{name: "a reason too long", as: f.lab1Operator, call: discard(failed, strings.Repeat("x", 1025)), code: connect.CodeInvalidArgument, msg: "reason"},
		{name: "retried", as: f.lab1Operator, call: discard(retried, "bad study"), code: connect.CodeFailedPrecondition},
		{name: "not there", as: f.lab1Operator, call: discard("nope", "bad study"), code: connect.CodeNotFound},
		{name: "discarded", as: f.lab1Operator, call: discard(failed, "sent to the wrong tenant"),
			audit: &wantAudit{log: "lab1", outcome: master.OutcomeDone, object: failed, details: map[string]string{"files": "0", "bytes": "0"}},
			check: func(t *testing.T, o outcome) {
				first = o.res.(*v1.DiscardReceptionResponse).GetReception()
				if first.GetDiscardReason() != "sent to the wrong tenant" || first.GetDiscardTime() == nil || first.GetStep() != v1.ReceptionStep_RECEPTION_STEP_FAILED {
					t.Errorf("%v", first)
				}
				if d, _ := f.parts.removals(); len(d) != 1 || d[0] != "lab1/"+failed {
					t.Errorf("the removal handed over: %v", d)
				}
			}},
		{name: "discarded again", as: f.lab1Operator, call: discard(failed, "another reason"), check: func(t *testing.T, o outcome) {
			if got := o.res.(*v1.DiscardReceptionResponse).GetReception(); !proto.Equal(got, first) {
				t.Errorf("a repeat answers %v, the first %v", got, first)
			}
		}},
	})
}
