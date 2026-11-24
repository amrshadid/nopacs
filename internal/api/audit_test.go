package api_test

import (
	"strings"
	"testing"
	"time"

	connect "connectrpc.com/connect/v2"
	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/master"
)

// testListAuditEntries guards ListAuditEntries: admin on the scope asked
// for; the installation's log or the tenant's, newest first; each filter;
// paging with its limits; each invalid filter; the listing recorded in the
// log it read, with its filters.
func testListAuditEntries(t *testing.T) {
	f := settingsFixture(t)
	// A hundred and fifty entries in the installation's log, so the default
	// page of 100 shows.
	var many []master.AuditEntry
	for range 150 {
		many = append(many, master.AuditEntry{ID: f.newID(), Time: f.now(), Method: "test.fill", Outcome: master.OutcomeRefused, KeyID: f.noGrantID})
	}
	if err := f.master.AppendAudit(ctx, many...); err != nil {
		t.Fatal(err)
	}
	call(f.as(f.lab1Admin).settings.SetSetting, &v1.SetSettingRequest{Setting: &v1.Setting{Name: testRetention, Scope: tenantScope("lab1"), Value: "30d"}})
	list := func(r *v1.ListAuditEntriesRequest) func(clients) outcome {
		return func(c clients) outcome { return call(c.audit.ListAuditEntries, r) }
	}
	count := func(want int) func(t *testing.T, o outcome) {
		return func(t *testing.T, o outcome) {
			if got := len(o.res.(*v1.ListAuditEntriesResponse).GetEntries()); got != want {
				t.Errorf("%d entries, want %d", got, want)
			}
		}
	}
	at := func(d time.Duration) *timestamppb.Timestamp { return timestamppb.New(t0.Add(d)) }
	var token string
	f.run(t, []tcase{
		{name: "no key", as: nobody, call: list(&v1.ListAuditEntriesRequest{Scope: installation()}), code: connect.CodeUnauthenticated},
		{name: "installation, just below the role", as: f.instOperator, call: list(&v1.ListAuditEntriesRequest{Scope: installation()}), code: connect.CodePermissionDenied},
		{name: "installation, at the role, the default page", as: f.instAdmin, call: list(&v1.ListAuditEntriesRequest{Scope: installation()}),
			audit: &wantAudit{outcome: master.OutcomeDone, details: map[string]string{"scope": "installation"}},
			check: func(t *testing.T, o outcome) {
				r := o.res.(*v1.ListAuditEntriesResponse)
				token = r.GetNextPageToken()
				es := r.GetEntries()
				if len(es) != 100 || token == "" {
					t.Fatalf("%d entries, token %q", len(es), token)
				}
				for i := 1; i < len(es); i++ {
					if es[i].GetTime().AsTime().After(es[i-1].GetTime().AsTime()) {
						t.Errorf("not newest first at %d", i)
					}
				}
			}},
		{name: "the next page", as: f.instAdmin, call: func(c clients) outcome {
			return list(&v1.ListAuditEntriesRequest{Scope: installation(), PageToken: token})(c)
		}},
		{name: "the token with another filter", as: f.instAdmin, call: func(c clients) outcome {
			return list(&v1.ListAuditEntriesRequest{Scope: installation(), Outcome: v1.AuditOutcome_AUDIT_OUTCOME_DONE, PageToken: token})(c)
		}, code: connect.CodeInvalidArgument, msg: "page_token"},
		{name: "by key and outcome", as: f.instAdmin,
			call:  list(&v1.ListAuditEntriesRequest{Scope: installation(), KeyId: f.noGrantID, Outcome: v1.AuditOutcome_AUDIT_OUTCOME_REFUSED, PageSize: 1000}),
			audit: &wantAudit{outcome: master.OutcomeDone, details: map[string]string{"key_id": f.noGrantID, "outcome": "refused"}},
			check: count(150)},
		{name: "by time", as: f.instAdmin,
			call:  list(&v1.ListAuditEntriesRequest{Scope: installation(), StartTime: at(0), EndTime: at(time.Hour), PageSize: 1000}),
			audit: &wantAudit{outcome: master.OutcomeDone, details: map[string]string{"start_time": "*", "end_time": "*"}}},
		{name: "a time before every entry", as: f.instAdmin, call: list(&v1.ListAuditEntriesRequest{Scope: installation(), EndTime: at(-time.Hour)}), check: count(0)},
		{name: "end not after start", as: f.instAdmin, call: list(&v1.ListAuditEntriesRequest{Scope: installation(), StartTime: at(time.Hour), EndTime: at(time.Hour)}),
			code: connect.CodeInvalidArgument, msg: "end_time", audit: failed("end_time")},
		{name: "a time before 1970", as: f.instAdmin, call: list(&v1.ListAuditEntriesRequest{Scope: installation(), StartTime: timestamppb.New(time.Date(1969, 1, 1, 0, 0, 0, 0, time.UTC))}),
			code: connect.CodeInvalidArgument, msg: "start_time"},
		{name: "a timestamp that is not valid", as: f.instAdmin, call: list(&v1.ListAuditEntriesRequest{Scope: installation(), EndTime: &timestamppb.Timestamp{Nanos: -1}}),
			code: connect.CodeInvalidArgument, msg: "end_time"},
		{name: "an unknown outcome", as: f.instAdmin, call: list(&v1.ListAuditEntriesRequest{Scope: installation(), Outcome: v1.AuditOutcome(9)}),
			code: connect.CodeInvalidArgument, msg: "outcome"},
		{name: "a malformed key id", as: f.instAdmin, call: list(&v1.ListAuditEntriesRequest{Scope: installation(), KeyId: "a b"}),
			code: connect.CodeInvalidArgument, msg: "key_id"},
		{name: "page_size at 1000", as: f.instAdmin, call: list(&v1.ListAuditEntriesRequest{Scope: installation(), PageSize: 1000})},
		{name: "page_size at 1001", as: f.instAdmin, call: list(&v1.ListAuditEntriesRequest{Scope: installation(), PageSize: 1001}), code: connect.CodeInvalidArgument, msg: "page_size"},
		{name: "page_token at 1025 bytes", as: f.instAdmin, call: list(&v1.ListAuditEntriesRequest{Scope: installation(), PageToken: strings.Repeat("t", 1025)}), code: connect.CodeInvalidArgument, msg: "page_token"},
		{name: "no scope", as: f.instAdmin, call: list(&v1.ListAuditEntriesRequest{}), code: connect.CodeInvalidArgument, msg: "scope"},
		{name: "tenant, an installation admin", as: f.instAdmin, call: list(&v1.ListAuditEntriesRequest{Scope: tenantScope("lab1")}), code: connect.CodePermissionDenied},
		{name: "tenant, just below the role", as: f.lab1Operator, call: list(&v1.ListAuditEntriesRequest{Scope: tenantScope("lab1")}), code: connect.CodePermissionDenied},
		{name: "tenant, at the role", as: f.lab1Admin, call: list(&v1.ListAuditEntriesRequest{Scope: tenantScope("lab1")}),
			audit: &wantAudit{log: "lab1", outcome: master.OutcomeDone, tenant: "lab1", details: map[string]string{"scope": "tenant/lab1"}},
			check: func(t *testing.T, o outcome) {
				es := o.res.(*v1.ListAuditEntriesResponse).GetEntries()
				if len(es) == 0 || es[0].GetMethod() != "/nopacs.v1.SettingsService/SetSetting" || es[0].GetTenant() != "lab1" || es[0].GetObject() != testRetention {
					t.Errorf("the tenant's log starts with %v", es)
				}
			}},
		{name: "tenant, the listing is in its log", as: f.lab1Admin, call: list(&v1.ListAuditEntriesRequest{Scope: tenantScope("lab1"), Outcome: v1.AuditOutcome_AUDIT_OUTCOME_DONE}),
			check: func(t *testing.T, o outcome) {
				es := o.res.(*v1.ListAuditEntriesResponse).GetEntries()
				if len(es) == 0 || es[0].GetMethod() != "/nopacs.v1.AuditService/ListAuditEntries" {
					t.Errorf("the newest entry is not the last listing: %v", es)
				}
			}},
		{name: "tenant, page_size at 1001", as: f.lab1Admin, call: list(&v1.ListAuditEntriesRequest{Scope: tenantScope("lab1"), PageSize: 1001}),
			code: connect.CodeInvalidArgument, msg: "page_size", audit: inTenant("lab1", master.OutcomeFailed)},
		{name: "no such tenant, may list", as: local, call: list(&v1.ListAuditEntriesRequest{Scope: tenantScope("nolab")}), code: connect.CodeNotFound},
		{name: "no such tenant, no right to know", as: f.lab1Admin, call: list(&v1.ListAuditEntriesRequest{Scope: tenantScope("nolab")}), code: connect.CodePermissionDenied},
		{name: "a tenant that is not ready", as: local, call: list(&v1.ListAuditEntriesRequest{Scope: tenantScope("down")}), code: connect.CodeUnavailable, audit: inMasterRerouted("down")},
	})
}
