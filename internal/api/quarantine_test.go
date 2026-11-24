package api_test

import (
	"slices"
	"strings"
	"testing"

	connect "connectrpc.com/connect/v2"
	"google.golang.org/protobuf/proto"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/access"
	"github.com/amrshadid/nopacs/internal/api"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// testListQuarantinedFiles guards ListQuarantinedFiles: reader on the
// tenant, and operator for a UID filter, recorded only as present; the
// filters checked; and a token bound to its filters.
func testListQuarantinedFiles(t *testing.T) {
	f := newPlaceFixture(t)
	f.receiving(t)
	held := f.heldFile(t, "scanner1", false)
	large := f.heldFile(t, "scanner1", true)
	list := func(req *v1.ListQuarantinedFilesRequest) func(clients) outcome {
		return func(c clients) outcome {
			if req.Tenant == "" {
				req.Tenant = "lab1"
			}
			return call(c.quarantine.ListQuarantinedFiles, req)
		}
	}
	ids := func(o outcome) string {
		var out []string
		for _, q := range o.res.(*v1.ListQuarantinedFilesResponse).GetFiles() {
			out = append(out, q.GetId())
		}
		return strings.Join(out, ",")
	}
	var token string
	f.run(t, []tcase{
		{name: "no key", as: nobody, call: list(&v1.ListQuarantinedFilesRequest{}), code: connect.CodeUnauthenticated},
		{name: "no grant", as: f.noGrant, call: list(&v1.ListQuarantinedFilesRequest{}), code: connect.CodePermissionDenied},
		{name: "no such tenant, no right to know", as: f.lab1Reader, call: list(&v1.ListQuarantinedFilesRequest{Tenant: "nolab"}), code: connect.CodePermissionDenied, msg: access.MessageDenied},
		{name: "reader, newest first", as: f.lab1Reader, call: list(&v1.ListQuarantinedFilesRequest{}), audit: inTenant("lab1", master.OutcomeDone),
			check: func(t *testing.T, o outcome) {
				if ids(o) != large+","+held {
					t.Errorf("%q", ids(o))
				}
			}},
		{name: "reader, held only", as: f.lab1Reader, call: list(&v1.ListQuarantinedFilesRequest{State: v1.QuarantineState_QUARANTINE_STATE_HELD, Reason: v1.QuarantineReason_QUARANTINE_REASON_TOO_LARGE}),
			audit: &wantAudit{log: "lab1", outcome: master.OutcomeDone, details: map[string]string{"state": "QUARANTINE_STATE_HELD", "reason": "QUARANTINE_REASON_TOO_LARGE"}},
			check: func(t *testing.T, o outcome) {
				if ids(o) != large {
					t.Errorf("%q", ids(o))
				}
			}},
		{name: "reader, by study", as: f.lab1Reader, call: list(&v1.ListQuarantinedFilesRequest{StudyInstanceUid: "2.25.1234567890"}), code: connect.CodePermissionDenied},
		{name: "reader, by instance", as: f.lab1Reader, call: list(&v1.ListQuarantinedFilesRequest{SopInstanceUid: "2.25.1"}), code: connect.CodePermissionDenied},
		{name: "operator, by instance", as: f.lab1Operator, call: list(&v1.ListQuarantinedFilesRequest{SopInstanceUid: "2.25.9" + strings.TrimPrefix(held, "id-")}),
			audit: &wantAudit{log: "lab1", outcome: master.OutcomeDone, details: map[string]string{"sop_instance_uid": "present"}},
			check: func(t *testing.T, o outcome) {
				if ids(o) != held {
					t.Errorf("%q", ids(o))
				}
			}},
		{name: "operator, by study", as: f.lab1Operator, call: list(&v1.ListQuarantinedFilesRequest{StudyInstanceUid: "2.25.1234567890"}),
			audit: &wantAudit{log: "lab1", outcome: master.OutcomeDone, details: map[string]string{"study_instance_uid": "present"}},
			check: func(t *testing.T, o outcome) {
				if ids(o) != large+","+held {
					t.Errorf("%q", ids(o))
				}
			}},
		{name: "a state no value names", as: f.lab1Reader, call: list(&v1.ListQuarantinedFilesRequest{State: 9}), code: connect.CodeInvalidArgument, msg: "state"},
		{name: "a reason no value names", as: f.lab1Reader, call: list(&v1.ListQuarantinedFilesRequest{Reason: 99}), code: connect.CodeInvalidArgument, msg: "reason"},
		{name: "a UID that is not one", as: f.lab1Operator, call: list(&v1.ListQuarantinedFilesRequest{SopInstanceUid: "x"}), code: connect.CodeInvalidArgument},
		{name: "a page of one", as: f.lab1Reader, call: list(&v1.ListQuarantinedFilesRequest{PageSize: 1, Receiver: "scanner1"}), check: func(t *testing.T, o outcome) {
			token = o.res.(*v1.ListQuarantinedFilesResponse).GetNextPageToken()
			if ids(o) != large || token == "" {
				t.Errorf("%q %q", ids(o), token)
			}
		}},
		{name: "the token with other filters", as: f.lab1Reader, call: func(c clients) outcome {
			return list(&v1.ListQuarantinedFilesRequest{PageSize: 1, PageToken: token})(c)
		}, code: connect.CodeInvalidArgument, msg: "page_token"},
	})
}

// testGetQuarantinedFile guards GetQuarantinedFile: reader for the record,
// operator for its header; a header that cannot be given left out without
// the too-large flag; one longer than the bound left out with it; and the
// entry saying whether the header was asked for.
func testGetQuarantinedFile(t *testing.T) {
	f := newPlaceFixture(t)
	f.receiving(t)
	held := f.heldFile(t, "scanner1", false)
	get := func(id string, header bool) func(clients) outcome {
		return func(c clients) outcome {
			return call(c.quarantine.GetQuarantinedFile, &v1.GetQuarantinedFileRequest{Tenant: "lab1", Id: id, IncludeHeader: header})
		}
	}
	f.run(t, []tcase{
		{name: "no grant", as: f.noGrant, call: get(held, false), code: connect.CodePermissionDenied},
		{name: "reader", as: f.lab1Reader, call: get(held, false),
			audit: &wantAudit{log: "lab1", outcome: master.OutcomeDone, object: held, details: map[string]string{"include_header": "false"}},
			check: func(t *testing.T, o outcome) {
				q := o.res.(*v1.GetQuarantinedFileResponse).GetFile()
				if q.GetReason() != v1.QuarantineReason_QUARANTINE_REASON_BAD_LABEL || q.GetState() != v1.QuarantineState_QUARANTINE_STATE_HELD || !q.GetContentKept() ||
					q.GetCaller().GetCallingAeTitle() != "PRISMA1" || len(q.GetSha256()) != 64 || q.GetDetails()["fault"] != "missing" {
					t.Errorf("%v", q)
				}
			}},
		{name: "reader, with the header", as: f.lab1Reader, call: get(held, true), code: connect.CodePermissionDenied},
		{name: "operator, with a header that cannot be read", as: f.lab1Operator, call: get(held, true),
			audit: &wantAudit{log: "lab1", outcome: master.OutcomeDone, details: map[string]string{"include_header": "true"}},
			check: func(t *testing.T, o outcome) {
				res := o.res.(*v1.GetQuarantinedFileResponse)
				if res.GetHeaderJson() != "" || res.GetHeaderTooLarge() {
					t.Errorf("%v", res)
				}
			}},
		{name: "not there", as: f.lab1Reader, call: get("nope", false), code: connect.CodeNotFound, msg: "no quarantined file with this id"},
		{name: "no id", as: f.lab1Reader, call: get("", false), code: connect.CodeInvalidArgument},
	})
	headers := func(text string, tooLarge bool) {
		h := func(tenant.QuarantinedFile, int) (string, bool, error) { return text, tooLarge, nil }
		f.parts.header.Store(&h)
	}
	headers(`{"00100010":{"vr":"PN"}}`, false)
	if res := mustCall[*v1.GetQuarantinedFileResponse](t, "get", get(held, true)(f.as(f.lab1Operator))); res.GetHeaderJson() != `{"00100010":{"vr":"PN"}}` {
		t.Errorf("a header: %v", res)
	}
	headers("", true)
	if res := mustCall[*v1.GetQuarantinedFileResponse](t, "get", get(held, true)(f.as(f.lab1Operator))); res.GetHeaderJson() != "" || !res.GetHeaderTooLarge() {
		t.Errorf("a header too large: %v", res)
	}
	headers(strings.Repeat("x", api.MaxHeaderBytes+1), false)
	if res := mustCall[*v1.GetQuarantinedFileResponse](t, "get", get(held, true)(f.as(f.lab1Operator))); res.GetHeaderJson() != "" || !res.GetHeaderTooLarge() {
		t.Error("a header over the bound was answered, or cut")
	}
}

// testReleaseQuarantinedFile guards ReleaseQuarantinedFile: operator and
// up; the one mode; a file whose bytes were never kept, and one rejected,
// refused as preconditions; a reason, optional, bounded as a rejection's
// and kept on the record; a release that joins a reception; a repeat
// answered as it stands with its first reason; and the entry with the mode,
// the receiver and the reception the file joined; the release handed to the
// reception pipeline whole.
func testReleaseQuarantinedFile(t *testing.T) {
	f := newPlaceFixture(t)
	f.receiving(t)
	held := f.heldFile(t, "scanner1", false)
	large := f.heldFile(t, "scanner1", true)
	rejected := f.heldFile(t, "scanner1", false)
	mustCall[*v1.RejectQuarantinedFileResponse](t, "reject", call(f.as(local).quarantine.RejectQuarantinedFile, &v1.RejectQuarantinedFileRequest{Tenant: "lab1", Id: rejected, Reason: "sent twice"}))
	withReason := func(id, reason string) func(clients) outcome {
		return func(c clients) outcome {
			return call(c.quarantine.ReleaseQuarantinedFile, &v1.ReleaseQuarantinedFileRequest{Tenant: "lab1", Id: id,
				Mode: v1.ReleaseMode_RELEASE_MODE_AS_IS, Reason: reason})
		}
	}
	release := func(id string, mode v1.ReleaseMode, receiver string) func(clients) outcome {
		return func(c clients) outcome {
			return call(c.quarantine.ReleaseQuarantinedFile, &v1.ReleaseQuarantinedFileRequest{Tenant: "lab1", Id: id, Mode: mode, Receiver: receiver})
		}
	}
	asIs := v1.ReleaseMode_RELEASE_MODE_AS_IS
	var first *v1.QuarantinedFile
	f.run(t, []tcase{
		{name: "no grant", as: f.noGrant, call: release(held, asIs, ""), code: connect.CodePermissionDenied},
		{name: "reader", as: f.lab1Reader, call: release(held, asIs, ""), code: connect.CodePermissionDenied},
		{name: "no mode", as: f.lab1Operator, call: release(held, v1.ReleaseMode_RELEASE_MODE_UNSPECIFIED, ""), code: connect.CodeInvalidArgument, msg: "mode"},
		{name: "the removed mode", as: f.lab1Operator, call: release(held, 2, ""), code: connect.CodeInvalidArgument, msg: "mode"},
		{name: "not there", as: f.lab1Operator, call: release("nope", asIs, ""), code: connect.CodeNotFound},
		{name: "bytes never kept", as: f.lab1Operator, call: release(large, asIs, ""), code: connect.CodeFailedPrecondition},
		{name: "rejected", as: f.lab1Operator, call: release(rejected, asIs, ""), code: connect.CodeFailedPrecondition},
		{name: "a receiver named that does not exist", as: f.lab1Operator, call: release(held, asIs, "scanner9"), code: connect.CodeFailedPrecondition},
		{name: "a reason of spaces", as: f.lab1Operator, call: withReason(held, "   "), code: connect.CodeInvalidArgument, msg: "reason"},
		{name: "a reason too long", as: f.lab1Operator, call: withReason(held, strings.Repeat("x", 1025)), code: connect.CodeInvalidArgument, msg: "reason"},
		{name: "released", as: f.lab1Operator, call: withReason(held, "the sender confirmed it"),
			audit: &wantAudit{log: "lab1", outcome: master.OutcomeDone, object: held, details: map[string]string{"mode": "RELEASE_MODE_AS_IS",
				"receiver": "scanner1", "release_reception": "*"}},
			check: func(t *testing.T, o outcome) {
				first = o.res.(*v1.ReleaseQuarantinedFileResponse).GetFile()
				if first.GetState() != v1.QuarantineState_QUARANTINE_STATE_RELEASED || first.GetReleaseReception() == "" || first.GetReleaseMode() != asIs ||
					first.GetReleaseReceiver() != "scanner1" || first.GetReleaseReason() != "the sender confirmed it" {
					t.Errorf("%v", first)
				}
				// The release ran through the pipeline, which gives the
				// bytes their landed name first, not the store alone.
				if released, _ := f.parts.decisions(); !slices.Contains(released, "lab1/"+held) {
					t.Errorf("the release was not handed to the pipeline: %v", released)
				}
			}},
		{name: "released again", as: f.lab1Admin, call: withReason(held, "another reason"), check: func(t *testing.T, o outcome) {
			if got := o.res.(*v1.ReleaseQuarantinedFileResponse).GetFile(); !proto.Equal(got, first) {
				t.Errorf("a repeat answers %v, the first %v", got, first)
			}
		}},
	})
}

// testRejectQuarantinedFile guards RejectQuarantinedFile: operator and up,
// the reason required, a released file refused, a repeat answered with its
// first reason, the removal of the bytes handed to the pipeline after the
// record, and the entry the store writes with the reason, size and checksum.
func testRejectQuarantinedFile(t *testing.T) {
	f := newPlaceFixture(t)
	f.receiving(t)
	held := f.heldFile(t, "scanner1", false)
	released := f.heldFile(t, "scanner1", false)
	mustCall[*v1.ReleaseQuarantinedFileResponse](t, "release", call(f.as(local).quarantine.ReleaseQuarantinedFile, &v1.ReleaseQuarantinedFileRequest{Tenant: "lab1", Id: released, Mode: v1.ReleaseMode_RELEASE_MODE_AS_IS}))
	reject := func(id, reason string) func(clients) outcome {
		return func(c clients) outcome {
			return call(c.quarantine.RejectQuarantinedFile, &v1.RejectQuarantinedFileRequest{Tenant: "lab1", Id: id, Reason: reason})
		}
	}
	var first *v1.QuarantinedFile
	f.run(t, []tcase{
		{name: "no grant", as: f.noGrant, call: reject(held, "x"), code: connect.CodePermissionDenied},
		{name: "reader", as: f.lab1Reader, call: reject(held, "x"), code: connect.CodePermissionDenied},
		{name: "no reason", as: f.lab1Operator, call: reject(held, ""), code: connect.CodeInvalidArgument, msg: "reason"},
		{name: "only spaces", as: f.lab1Operator, call: reject(held, "  "), code: connect.CodeInvalidArgument, msg: "reason"},
		{name: "released", as: f.lab1Operator, call: reject(released, "x"), code: connect.CodeFailedPrecondition},
		{name: "not there", as: f.lab1Operator, call: reject("nope", "x"), code: connect.CodeNotFound},
		{name: "rejected", as: f.lab1Operator, call: reject(held, "re-sent by mistake"),
			audit: &wantAudit{log: "lab1", outcome: master.OutcomeDone, object: held, details: map[string]string{"reason": "bad_label", "size": "528650", "sha256": "*"}},
			check: func(t *testing.T, o outcome) {
				first = o.res.(*v1.RejectQuarantinedFileResponse).GetFile()
				if first.GetState() != v1.QuarantineState_QUARANTINE_STATE_REJECTED || first.GetRejectReason() != "re-sent by mistake" || first.GetContentKept() || first.GetDecideTime() == nil {
					t.Errorf("%v", first)
				}
				if _, r := f.parts.removals(); len(r) != 1 || r[0] != "lab1/q/"+held {
					t.Errorf("the removal handed over: %v", r)
				}
			}},
		{name: "rejected again", as: f.lab1Operator, call: reject(held, "another reason"), check: func(t *testing.T, o outcome) {
			if got := o.res.(*v1.RejectQuarantinedFileResponse).GetFile(); !proto.Equal(got, first) {
				t.Errorf("a repeat answers %v, the first %v", got, first)
			}
			if _, r := f.parts.removals(); len(r) != 1 {
				t.Errorf("a repeat handed the bytes over again: %v", r)
			}
		}},
	})
}
