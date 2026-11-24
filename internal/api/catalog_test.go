package api_test

import (
	"strings"
	"testing"

	connect "connectrpc.com/connect/v2"
	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/access"
	"github.com/amrshadid/nopacs/internal/master"
)

// refusals runs a catalog method as callers that must be refused, and as
// callers who ask for a tenant that does not exist, and checks what the
// entries of the refusals say: a refused request is recorded refused in the
// installation's log and leaves nothing in the tenant's.
func (d *catalogData) refusals(t *testing.T, name string, do func(c clients, tenant string) outcome) {
	t.Helper()
	f := d.f
	for _, c := range []struct {
		name   string
		as     caller
		tenant string
		code   connect.Code
	}{
		{"no key", nobody, "lab1", connect.CodeUnauthenticated},
		{"no grant", f.noGrant, "lab1", connect.CodePermissionDenied},
		{"a grant on another tenant", f.lab2Admin, "lab1", connect.CodePermissionDenied},
		{"a tenant that does not exist, for a reader", f.lab1Reader, "nolab", connect.CodePermissionDenied},
		{"a tenant name that is not one", f.lab1Reader, "Lab 1", connect.CodePermissionDenied},
		{"a tenant that does not exist, for an installation admin", f.instAdmin, "nolab", connect.CodeNotFound},
	} {
		o := do(f.as(c.as), c.tenant)
		if o.code() != c.code {
			t.Errorf("%s, %s: %v, want %v", name, c.name, o.err, c.code)
			continue
		}
		if c.code == connect.CodePermissionDenied && c.as.key != "" && !strings.Contains(o.message(), access.MessageDenied) {
			t.Errorf("%s, %s: message %q", name, c.name, o.message())
		}
		if c.name == "no grant" || c.name == "a grant on another tenant" {
			f.refusedNotInTenant(t, name+", "+c.name, "lab1", o)
		}
	}
}

// testListSubjects guards what ListSubjects refuses and records: no key,
// no grant, a combination of filters and order no index serves, a value
// that is not valid, a page token another list made; and that an answer is
// audited in the tenant's log with its filters and its order, a label
// prefix that is not the start of a label recorded as "invalid".
func testListSubjects(t *testing.T) {
	d := newCatalogData(t)
	req := func(mod func(*v1.ListSubjectsRequest)) func(clients) outcome {
		return func(c clients) outcome {
			r := &v1.ListSubjectsRequest{Tenant: "lab1"}
			mod(r)
			return call(c.catalog.ListSubjects, r)
		}
	}
	d.refusals(t, "ListSubjects", func(c clients, tenant string) outcome {
		return call(c.catalog.ListSubjects, &v1.ListSubjectsRequest{Tenant: tenant})
	})
	const label, newest = v1.CatalogOrder_CATALOG_ORDER_LABEL, v1.CatalogOrder_CATALOG_ORDER_NEWEST
	first := mustCall[*v1.ListSubjectsResponse](t, "first", req(func(r *v1.ListSubjectsRequest) { r.PageSize = 1 })(d.f.as(d.f.lab1Reader)))
	bad := func(name string, mod func(*v1.ListSubjectsRequest), msg string) tcase {
		return tcase{name: name, as: d.f.lab1Reader, call: req(mod), code: connect.CodeInvalidArgument, msg: msg, audit: inTenant("lab1", master.OutcomeFailed)}
	}
	start := timestamppb.New(d.subjects[0].GetArchiveTime().AsTime())
	d.f.run(t, []tcase{
		{name: "audited with its order and prefix", as: d.f.lab1Reader,
			call:  req(func(r *v1.ListSubjectsRequest) { r.Order = label; r.LabelPrefix = "sub-" }),
			audit: &wantAudit{log: "lab1", outcome: master.OutcomeDone, tenant: "lab1", object: "-", details: map[string]string{"order": "CATALOG_ORDER_LABEL", "label_prefix": "sub-"}}},
		{name: "a prefix that is not a start of a label is recorded invalid", as: d.f.lab1Reader,
			call: req(func(r *v1.ListSubjectsRequest) { r.Order = label; r.LabelPrefix = "Smith John" }), code: connect.CodeInvalidArgument, msg: "label prefix",
			audit: &wantAudit{log: "lab1", outcome: master.OutcomeFailed, details: map[string]string{"label_prefix": "invalid"}}},
		{name: "audited with its time range", as: d.f.lab1Reader, call: req(func(r *v1.ListSubjectsRequest) { r.StartTime = start }),
			audit: &wantAudit{log: "lab1", outcome: master.OutcomeDone, details: map[string]string{"start_time": "*"}}},
		bad("newest with a label prefix", func(r *v1.ListSubjectsRequest) { r.Order = newest; r.LabelPrefix = "sub" }, "subjects are listed"),
		bad("label with a time range", func(r *v1.ListSubjectsRequest) { r.Order = label; r.StartTime = start }, "subjects are listed"),
		bad("study date order", func(r *v1.ListSubjectsRequest) { r.Order = v1.CatalogOrder_CATALOG_ORDER_STUDY_DATE }, "subjects are listed"),
		bad("series description order", func(r *v1.ListSubjectsRequest) { r.Order = v1.CatalogOrder_CATALOG_ORDER_SERIES_DESCRIPTION }, "subjects are listed"),
		bad("an order the contract does not define", func(r *v1.ListSubjectsRequest) { r.Order = 99 }, "order"),
		bad("an end that is not after the start", func(r *v1.ListSubjectsRequest) { r.StartTime = start; r.EndTime = start }, "end_time"),
		bad("a start outside the years the log holds", func(r *v1.ListSubjectsRequest) { r.StartTime = &timestamppb.Timestamp{Seconds: -1 << 40} }, "start_time"),
		bad("a prefix with a character no label has", func(r *v1.ListSubjectsRequest) { r.Order = label; r.LabelPrefix = "a^b" }, "label prefix"),
		bad("a prefix of 129 bytes", func(r *v1.ListSubjectsRequest) { r.Order = label; r.LabelPrefix = strings.Repeat("a", 129) }, "label prefix"),
		bad("a negative page size", func(r *v1.ListSubjectsRequest) { r.PageSize = -1 }, "page_size"),
		bad("a page size of 1001", func(r *v1.ListSubjectsRequest) { r.PageSize = 1001 }, "page_size"),
		bad("a token that is not one", func(r *v1.ListSubjectsRequest) { r.PageToken = "x" }, "page_token"),
		bad("a token over 1 KiB", func(r *v1.ListSubjectsRequest) { r.PageToken = strings.Repeat("x", 1025) }, "page_token"),
		bad("a token with another order", func(r *v1.ListSubjectsRequest) { r.Order = label; r.PageToken = first.GetNextPageToken() }, "page_token"),
		bad("a token with another filter", func(r *v1.ListSubjectsRequest) { r.StartTime = start; r.PageToken = first.GetNextPageToken() }, "page_token"),
		{name: "a page of 1000", as: d.f.lab1Reader, call: req(func(r *v1.ListSubjectsRequest) { r.PageSize = 1000 })},
	})
}

// testGetSubject guards GetSubject: a subject read by its label, one that
// is not there, a label in another case (not found, never another's), a
// value that is not a label, and the entry's object.
func testGetSubject(t *testing.T) {
	d := newCatalogData(t)
	s := d.subjects[0]
	get := func(label string) func(clients) outcome {
		return func(c clients) outcome {
			return call(c.catalog.GetSubject, &v1.GetSubjectRequest{Tenant: "lab1", SubjectLabel: label})
		}
	}
	d.refusals(t, "GetSubject", func(c clients, tenant string) outcome {
		return call(c.catalog.GetSubject, &v1.GetSubjectRequest{Tenant: tenant, SubjectLabel: s.GetLabel()})
	})
	d.f.run(t, []tcase{
		{name: "by label", as: d.f.lab1Reader, call: get(s.GetLabel()),
			audit: &wantAudit{log: "lab1", outcome: master.OutcomeDone, tenant: "lab1", object: s.GetLabel()},
			check: func(t *testing.T, o outcome) {
				got := o.res.(*v1.GetSubjectResponse).GetSubject()
				if got.GetLabel() != s.GetLabel() || got.GetSessionCount() != s.GetSessionCount() || got.GetInstanceCount() != s.GetInstanceCount() ||
					got.GetSizeBytes() != s.GetSizeBytes() || !got.GetArchiveTime().AsTime().Equal(s.GetArchiveTime().AsTime()) {
					t.Errorf("got %v, listed %v", got, s)
				}
			}},
		{name: "not there", as: d.f.lab1Reader, call: get("nothere"), code: connect.CodeNotFound, msg: "no subject", audit: inTenant("lab1", master.OutcomeFailed)},
		{name: "in another case", as: d.f.lab1Reader, call: get(strings.ToUpper(s.GetLabel())), code: connect.CodeNotFound, msg: "no subject"},
		{name: "not a label", as: d.f.lab1Reader, call: get("Smith^John"), code: connect.CodeInvalidArgument, msg: "subject_label",
			audit: &wantAudit{log: "lab1", outcome: master.OutcomeFailed, object: "invalid"}},
		{name: "no label", as: d.f.lab1Reader, call: get(""), code: connect.CodeInvalidArgument, msg: "subject_label"},
		{name: "a label of 129 bytes", as: d.f.lab1Reader, call: get(strings.Repeat("a", 129)), code: connect.CodeInvalidArgument, msg: "subject_label"},
	})
}

// testListSessions guards what ListSessions refuses and records: a subject
// that is not there, filters no index serves, values that are not valid,
// and the entry's object and details.
func testListSessions(t *testing.T) {
	d := newCatalogData(t)
	s := d.sessions[0]
	req := func(mod func(*v1.ListSessionsRequest)) func(clients) outcome {
		return func(c clients) outcome {
			r := &v1.ListSessionsRequest{Tenant: "lab1"}
			mod(r)
			return call(c.catalog.ListSessions, r)
		}
	}
	d.refusals(t, "ListSessions", func(c clients, tenant string) outcome {
		return call(c.catalog.ListSessions, &v1.ListSessionsRequest{Tenant: tenant})
	})
	const label, newest, date = v1.CatalogOrder_CATALOG_ORDER_LABEL, v1.CatalogOrder_CATALOG_ORDER_NEWEST, v1.CatalogOrder_CATALOG_ORDER_STUDY_DATE
	first := mustCall[*v1.ListSessionsResponse](t, "first", req(func(r *v1.ListSessionsRequest) { r.PageSize = 1 })(d.f.as(d.f.lab1Reader)))
	bad := func(name string, mod func(*v1.ListSessionsRequest), msg string) tcase {
		return tcase{name: name, as: d.f.lab1Reader, call: req(mod), code: connect.CodeInvalidArgument, msg: msg, audit: inTenant("lab1", master.OutcomeFailed)}
	}
	when := timestamppb.New(s.GetArchiveTime().AsTime())
	d.f.run(t, []tcase{
		{name: "audited with its subject and filters", as: d.f.lab1Reader,
			call: req(func(r *v1.ListSessionsRequest) {
				r.SubjectLabel = s.GetSubjectLabel()
				r.Order = date
				r.StudyDateStart = "19000101"
			}),
			audit: &wantAudit{log: "lab1", outcome: master.OutcomeDone, object: s.GetSubjectLabel(), details: map[string]string{"order": "CATALOG_ORDER_STUDY_DATE", "study_date_start": "19000101"}}},
		{name: "audited with a modality and no subject", as: d.f.lab1Reader, call: req(func(r *v1.ListSessionsRequest) { r.Modality = "MR" }),
			audit: &wantAudit{log: "lab1", outcome: master.OutcomeDone, object: "-", details: map[string]string{"modality": "MR"}}},
		{name: "a subject that is not there", as: d.f.lab1Reader, call: req(func(r *v1.ListSessionsRequest) { r.SubjectLabel = "nothere" }),
			code: connect.CodeNotFound, msg: "no subject", audit: inTenant("lab1", master.OutcomeFailed)},
		{name: "a subject in another case", as: d.f.lab1Reader, call: req(func(r *v1.ListSessionsRequest) { r.SubjectLabel = strings.ToUpper(s.GetSubjectLabel()) }), code: connect.CodeNotFound},
		{name: "a receiver no receiver holds is not invalid", as: d.f.lab1Reader, call: req(func(r *v1.ListSessionsRequest) { r.Receiver = "removed" })},
		bad("a subject that is not a label", func(r *v1.ListSessionsRequest) { r.SubjectLabel = "Smith^John" }, "label"),
		bad("a subject and a modality", func(r *v1.ListSessionsRequest) { r.SubjectLabel = s.GetSubjectLabel(); r.Modality = "MR" }, "sessions are listed"),
		bad("a modality and a receiver", func(r *v1.ListSessionsRequest) { r.Modality = "MR"; r.Receiver = "scanner1" }, "sessions are listed"),
		bad("a subject and a receiver", func(r *v1.ListSessionsRequest) { r.SubjectLabel = s.GetSubjectLabel(); r.Receiver = "scanner1" }, "sessions are listed"),
		bad("label with a modality", func(r *v1.ListSessionsRequest) { r.Order = label; r.Modality = "MR" }, "sessions are listed"),
		bad("label with a prefix and no subject", func(r *v1.ListSessionsRequest) { r.Order = label; r.LabelPrefix = "ses" }, "sessions are listed"),
		bad("newest with a prefix", func(r *v1.ListSessionsRequest) {
			r.Order = newest
			r.LabelPrefix = "ses"
			r.SubjectLabel = s.GetSubjectLabel()
		}, "sessions are listed"),
		bad("newest with a study date", func(r *v1.ListSessionsRequest) { r.StudyDateStart = "20000101" }, "sessions are listed"),
		bad("label with a study date", func(r *v1.ListSessionsRequest) { r.Order = label; r.StudyDateEnd = "20000101" }, "sessions are listed"),
		bad("study date with a time range", func(r *v1.ListSessionsRequest) { r.Order = date; r.StartTime = when }, "sessions are listed"),
		bad("study date with a modality", func(r *v1.ListSessionsRequest) { r.Order = date; r.Modality = "MR" }, "sessions are listed"),
		bad("label with a time range", func(r *v1.ListSessionsRequest) { r.Order = label; r.EndTime = when }, "sessions are listed"),
		bad("a modality in lower case", func(r *v1.ListSessionsRequest) { r.Modality = "mr" }, "modality"),
		bad("a modality of 17 characters", func(r *v1.ListSessionsRequest) { r.Modality = strings.Repeat("M", 17) }, "modality"),
		bad("a receiver that is not a name", func(r *v1.ListSessionsRequest) { r.Receiver = "Smith John" }, "receiver"),
		bad("a date that is not one", func(r *v1.ListSessionsRequest) { r.Order = date; r.StudyDateStart = "2003-04-17" }, "study date"),
		bad("a date that does not exist", func(r *v1.ListSessionsRequest) { r.Order = date; r.StudyDateEnd = "20030230" }, "study date"),
		bad("a date range that ends before it starts", func(r *v1.ListSessionsRequest) {
			r.Order = date
			r.StudyDateStart, r.StudyDateEnd = "20030418", "20030417"
		}, "range"),
		bad("an end that is not after the start", func(r *v1.ListSessionsRequest) { r.StartTime = when; r.EndTime = when }, "end_time"),
		bad("an order the contract does not define", func(r *v1.ListSessionsRequest) { r.Order = 99 }, "order"),
		bad("series description order", func(r *v1.ListSessionsRequest) { r.Order = v1.CatalogOrder_CATALOG_ORDER_SERIES_DESCRIPTION }, "sessions are listed"),
		bad("a page size of 1001", func(r *v1.ListSessionsRequest) { r.PageSize = 1001 }, "page_size"),
		bad("a token that is not one", func(r *v1.ListSessionsRequest) { r.PageToken = "x" }, "page_token"),
		bad("a token with another order", func(r *v1.ListSessionsRequest) { r.Order = label; r.PageToken = first.GetNextPageToken() }, "page_token"),
		bad("a token with another filter", func(r *v1.ListSessionsRequest) { r.Modality = "MR"; r.PageToken = first.GetNextPageToken() }, "page_token"),
	})
}

// testGetSession guards GetSession: a session read by its two labels, a
// missing subject or session, labels in another case, values that are not
// labels, and the entry's object, "<subject>/<session>".
func testGetSession(t *testing.T) {
	d := newCatalogData(t)
	s := d.sessions[0]
	get := func(subject, session string) func(clients) outcome {
		return func(c clients) outcome {
			return call(c.catalog.GetSession, &v1.GetSessionRequest{Tenant: "lab1", SubjectLabel: subject, SessionLabel: session})
		}
	}
	d.refusals(t, "GetSession", func(c clients, tenant string) outcome {
		return call(c.catalog.GetSession, &v1.GetSessionRequest{Tenant: tenant, SubjectLabel: s.GetSubjectLabel(), SessionLabel: s.GetLabel()})
	})
	other := d.sessions[1].GetSubjectLabel()
	d.f.run(t, []tcase{
		{name: "by labels", as: d.f.lab1Reader, call: get(s.GetSubjectLabel(), s.GetLabel()),
			audit: &wantAudit{log: "lab1", outcome: master.OutcomeDone, tenant: "lab1", object: sessionKey(s)},
			check: func(t *testing.T, o outcome) {
				got := o.res.(*v1.GetSessionResponse).GetSession()
				if got.GetStudyInstanceUid() == "" || got.GetStorage() != "main" || got.GetReceiver() != "scanner1" || got.GetScanCount() < 1 || len(got.GetModalities()) == 0 {
					t.Errorf("got %v", got)
				}
			}},
		{name: "a subject that is not there", as: d.f.lab1Reader, call: get("nothere", s.GetLabel()), code: connect.CodeNotFound, msg: "no session", audit: inTenant("lab1", master.OutcomeFailed)},
		{name: "a session that is not there", as: d.f.lab1Reader, call: get(s.GetSubjectLabel(), "nothere"), code: connect.CodeNotFound, msg: "no session"},
		{name: "a session of another subject", as: d.f.lab1Reader, call: get(other, s.GetLabel()), code: connect.CodeNotFound, msg: "no session"},
		{name: "in another case", as: d.f.lab1Reader, call: get(s.GetSubjectLabel(), strings.ToUpper(s.GetLabel())), code: connect.CodeNotFound},
		{name: "a subject that is not a label", as: d.f.lab1Reader, call: get("Smith^John", s.GetLabel()), code: connect.CodeInvalidArgument, msg: "subject_label",
			audit: &wantAudit{log: "lab1", outcome: master.OutcomeFailed, object: "invalid/" + s.GetLabel()}},
		{name: "a session that is not a label", as: d.f.lab1Reader, call: get(s.GetSubjectLabel(), "../x"), code: connect.CodeInvalidArgument, msg: "session_label"},
		{name: "no subject", as: d.f.lab1Reader, call: get("", s.GetLabel()), code: connect.CodeInvalidArgument, msg: "subject_label"},
		{name: "no session", as: d.f.lab1Reader, call: get(s.GetSubjectLabel(), ""), code: connect.CodeInvalidArgument, msg: "session_label"},
	})
}

// testListScans guards what ListScans refuses and records: a subject or a
// session that is not there, a session with no subject, filters no index
// serves, values that are not valid, and the entry's object and details,
// a series description prefix recorded only as present.
func testListScans(t *testing.T) {
	d := newCatalogData(t)
	sc := d.scans[0]
	req := func(mod func(*v1.ListScansRequest)) func(clients) outcome {
		return func(c clients) outcome {
			r := &v1.ListScansRequest{Tenant: "lab1"}
			mod(r)
			return call(c.catalog.ListScans, r)
		}
	}
	d.refusals(t, "ListScans", func(c clients, tenant string) outcome {
		return call(c.catalog.ListScans, &v1.ListScansRequest{Tenant: tenant})
	})
	const label, newest, series = v1.CatalogOrder_CATALOG_ORDER_LABEL, v1.CatalogOrder_CATALOG_ORDER_NEWEST, v1.CatalogOrder_CATALOG_ORDER_SERIES_DESCRIPTION
	first := mustCall[*v1.ListScansResponse](t, "first", req(func(r *v1.ListScansRequest) { r.PageSize = 1 })(d.f.as(d.f.lab1Reader)))
	bad := func(name string, mod func(*v1.ListScansRequest), msg string) tcase {
		return tcase{name: name, as: d.f.lab1Reader, call: req(mod), code: connect.CodeInvalidArgument, msg: msg, audit: inTenant("lab1", master.OutcomeFailed)}
	}
	session := func(r *v1.ListScansRequest) {
		r.SubjectLabel, r.SessionLabel = sc.GetSubjectLabel(), sc.GetSessionLabel()
	}
	when := timestamppb.New(sc.GetArchiveTime().AsTime())
	d.f.run(t, []tcase{
		{name: "audited with its session and filters", as: d.f.lab1Reader, call: req(func(r *v1.ListScansRequest) { session(r); r.Order = label; r.LabelPrefix = "scan" }),
			audit: &wantAudit{log: "lab1", outcome: master.OutcomeDone, object: sc.GetSubjectLabel() + "/" + sc.GetSessionLabel(),
				details: map[string]string{"order": "CATALOG_ORDER_LABEL", "label_prefix": "scan"}}},
		{name: "a series description prefix is recorded only as present", as: d.f.lab1Reader,
			call:  req(func(r *v1.ListScansRequest) { r.Order = series; r.SeriesDescriptionPrefix = "Smith^John" }),
			audit: &wantAudit{log: "lab1", outcome: master.OutcomeDone, object: "-", details: map[string]string{"series_description_prefix": "present"}},
			check: func(t *testing.T, o outcome) {
				for _, en := range d.f.tenantLog("lab1") {
					for k, v := range en.Details {
						if strings.Contains(v, "Smith") {
							t.Errorf("the detail %s holds the prefix: %q", k, v)
						}
					}
				}
			}},
		{name: "a subject that is not there", as: d.f.lab1Reader, call: req(func(r *v1.ListScansRequest) { r.Order = label; r.SubjectLabel = "nothere" }),
			code: connect.CodeNotFound, msg: "no subject", audit: inTenant("lab1", master.OutcomeFailed)},
		{name: "a session that is not there", as: d.f.lab1Reader, call: req(func(r *v1.ListScansRequest) { session(r); r.SessionLabel = "nothere" }), code: connect.CodeNotFound, msg: "no session"},
		{name: "a subject in another case", as: d.f.lab1Reader, call: req(func(r *v1.ListScansRequest) { r.Order = label; r.SubjectLabel = strings.ToUpper(sc.GetSubjectLabel()) }), code: connect.CodeNotFound},
		bad("a session with no subject", func(r *v1.ListScansRequest) { r.SessionLabel = sc.GetSessionLabel() }, "subject"),
		bad("a subject that is not a label", func(r *v1.ListScansRequest) { r.Order = label; r.SubjectLabel = "Smith^John" }, "label"),
		bad("newest of a subject alone", func(r *v1.ListScansRequest) { r.Order = newest; r.SubjectLabel = sc.GetSubjectLabel() }, "scans are listed"),
		bad("a session and a modality", func(r *v1.ListScansRequest) { session(r); r.Modality = "MR" }, "scans are listed"),
		bad("a modality and a receiver", func(r *v1.ListScansRequest) { r.Modality = "MR"; r.Receiver = "scanner1" }, "scans are listed"),
		bad("newest with a label prefix", func(r *v1.ListScansRequest) { session(r); r.LabelPrefix = "scan" }, "scans are listed"),
		bad("newest with a description prefix", func(r *v1.ListScansRequest) { r.SeriesDescriptionPrefix = "s" }, "scans are listed"),
		bad("label with a prefix and no session", func(r *v1.ListScansRequest) {
			r.Order = label
			r.LabelPrefix = "scan"
			r.SubjectLabel = sc.GetSubjectLabel()
		}, "scans are listed"),
		bad("label with a modality", func(r *v1.ListScansRequest) { r.Order = label; r.Modality = "MR" }, "scans are listed"),
		bad("label with a time range", func(r *v1.ListScansRequest) { r.Order = label; r.StartTime = when }, "scans are listed"),
		bad("series description with a subject", func(r *v1.ListScansRequest) { r.Order = series; r.SubjectLabel = sc.GetSubjectLabel() }, "scans are listed"),
		bad("series description with a modality", func(r *v1.ListScansRequest) { r.Order = series; r.Modality = "CT" }, "scans are listed"),
		bad("series description with a label prefix", func(r *v1.ListScansRequest) { r.Order = series; r.LabelPrefix = "scan" }, "scans are listed"),
		bad("series description with a time range", func(r *v1.ListScansRequest) { r.Order = series; r.EndTime = when }, "scans are listed"),
		bad("a series description prefix of 65 characters", func(r *v1.ListScansRequest) { r.Order = series; r.SeriesDescriptionPrefix = strings.Repeat("a", 65) }, "series description prefix"),
		bad("a modality in lower case", func(r *v1.ListScansRequest) { r.Modality = "ct" }, "modality"),
		bad("a modality with a space at its end", func(r *v1.ListScansRequest) { r.Modality = "CT " }, "modality"),
		bad("a receiver that is not a name", func(r *v1.ListScansRequest) { r.Receiver = "../x" }, "receiver"),
		bad("an end that is not after the start", func(r *v1.ListScansRequest) { r.StartTime = when; r.EndTime = when }, "end_time"),
		bad("an order the contract does not define", func(r *v1.ListScansRequest) { r.Order = 99 }, "order"),
		bad("study date order", func(r *v1.ListScansRequest) { r.Order = v1.CatalogOrder_CATALOG_ORDER_STUDY_DATE }, "scans are listed"),
		bad("a page size of 1001", func(r *v1.ListScansRequest) { r.PageSize = 1001 }, "page_size"),
		bad("a token that is not one", func(r *v1.ListScansRequest) { r.PageToken = "x" }, "page_token"),
		bad("a token with another order", func(r *v1.ListScansRequest) { r.Order = label; r.PageToken = first.GetNextPageToken() }, "page_token"),
		bad("a token with another filter", func(r *v1.ListScansRequest) { r.Modality = "MR"; r.PageToken = first.GetNextPageToken() }, "page_token"),
		{name: "a series description prefix of 64 characters", as: d.f.lab1Reader, call: req(func(r *v1.ListScansRequest) { r.Order = series; r.SeriesDescriptionPrefix = strings.Repeat("a", 64) })},
	})
}

// testGetScan guards GetScan: a scan read by its three labels with the
// values of its files, a missing level, labels in another case, values that
// are not labels, and the entry's object, "<subject>/<session>/<scan>".
func testGetScan(t *testing.T) {
	d := newCatalogData(t)
	sc := d.scans[0]
	get := func(subject, session, scan string) func(clients) outcome {
		return func(c clients) outcome {
			return call(c.catalog.GetScan, &v1.GetScanRequest{Tenant: "lab1", SubjectLabel: subject, SessionLabel: session, ScanLabel: scan})
		}
	}
	d.refusals(t, "GetScan", func(c clients, tenant string) outcome {
		return call(c.catalog.GetScan, &v1.GetScanRequest{Tenant: tenant, SubjectLabel: sc.GetSubjectLabel(), SessionLabel: sc.GetSessionLabel(), ScanLabel: sc.GetLabel()})
	})
	d.f.run(t, []tcase{
		{name: "by labels", as: d.f.lab1Reader, call: get(sc.GetSubjectLabel(), sc.GetSessionLabel(), sc.GetLabel()),
			audit: &wantAudit{log: "lab1", outcome: master.OutcomeDone, tenant: "lab1", object: scanKey(sc)},
			check: func(t *testing.T, o outcome) {
				got := o.res.(*v1.GetScanResponse).GetScan()
				if got.GetSeriesInstanceUid() == "" || got.GetStorage() != "main" || got.GetReceiver() != "scanner1" || len(got.GetManifestSha256()) != 64 || got.GetInstanceCount() < 1 {
					t.Errorf("got %v", got)
				}
			}},
		{name: "every scan by its labels", as: d.f.lab1Reader, call: func(c clients) outcome {
			for _, s := range d.scans {
				o := get(s.GetSubjectLabel(), s.GetSessionLabel(), s.GetLabel())(c)
				if o.err != nil || scanKey(o.res.(*v1.GetScanResponse).GetScan()) != scanKey(s) {
					t.Errorf("%s: %v", scanKey(s), o.err)
				}
			}
			return get(sc.GetSubjectLabel(), sc.GetSessionLabel(), sc.GetLabel())(c)
		}},
		{name: "a subject that is not there", as: d.f.lab1Reader, call: get("nothere", sc.GetSessionLabel(), sc.GetLabel()), code: connect.CodeNotFound, msg: "no scan", audit: inTenant("lab1", master.OutcomeFailed)},
		{name: "a session that is not there", as: d.f.lab1Reader, call: get(sc.GetSubjectLabel(), "nothere", sc.GetLabel()), code: connect.CodeNotFound, msg: "no scan"},
		{name: "a scan that is not there", as: d.f.lab1Reader, call: get(sc.GetSubjectLabel(), sc.GetSessionLabel(), "nothere"), code: connect.CodeNotFound, msg: "no scan"},
		{name: "in another case", as: d.f.lab1Reader, call: get(sc.GetSubjectLabel(), sc.GetSessionLabel(), strings.ToUpper(sc.GetLabel())), code: connect.CodeNotFound},
		{name: "values that are not labels", as: d.f.lab1Reader, call: get("Smith^John", "../x", strings.Repeat("a", 129)), code: connect.CodeInvalidArgument, msg: "label",
			audit: &wantAudit{log: "lab1", outcome: master.OutcomeFailed, object: "invalid/invalid/invalid"}},
		{name: "no scan label", as: d.f.lab1Reader, call: get(sc.GetSubjectLabel(), sc.GetSessionLabel(), ""), code: connect.CodeInvalidArgument, msg: "label"},
		{name: "no subject label", as: d.f.lab1Reader, call: get("", sc.GetSessionLabel(), sc.GetLabel()), code: connect.CodeInvalidArgument, msg: "label"},
	})
}
