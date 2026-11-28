package api_test

import (
	"database/sql"
	"path/filepath"
	"testing"

	connect "connectrpc.com/connect/v2"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/master"
)

// testBackfillAttribute guards BackfillAttribute: admin on the tenant (a
// reader and an operator are refused, and so is a key of another tenant); an
// attribute the search tables do not hold is invalid; one that is ready has
// nothing to fill; one being filled makes a queued search_backfill job that
// names the attributes it covers, which GetJob answers too; a second request
// while the job has not ended is a precondition failure; and every attempt
// is audited, with the attribute and never a value.
func testBackfillAttribute(t *testing.T) {
	d := newCatalogData(t)
	req := func(attribute string) func(clients) outcome {
		return func(c clients) outcome {
			return call(c.search.BackfillAttribute, &v1.BackfillAttributeRequest{Tenant: "lab1", Attribute: attribute})
		}
	}
	d.refusals(t, "BackfillAttribute", func(c clients, tenant string) outcome {
		return call(c.search.BackfillAttribute, &v1.BackfillAttributeRequest{Tenant: tenant, Attribute: "Modality"})
	})
	failed := func(name string, as caller, attribute string, code connect.Code, msg string) tcase {
		return tcase{name: name, as: as, call: req(attribute), code: code, msg: msg, audit: inTenant("lab1", master.OutcomeFailed)}
	}
	d.f.run(t, []tcase{
		{name: "a reader is refused", as: d.f.lab1Reader, call: req("Modality"), code: connect.CodePermissionDenied},
		{name: "an operator is refused", as: d.f.lab1Operator, call: req("Modality"), code: connect.CodePermissionDenied},
		failed("an attribute the search tables do not hold", d.f.lab1Admin, "PatientBirthDate", connect.CodeInvalidArgument, "PatientBirthDate"),
		failed("an attribute that is ready", d.f.lab1Admin, "Modality", connect.CodeFailedPrecondition, "nothing to backfill"),
	})

	// An attribute added after scans were archived is being filled: put in
	// that state by the statement a migration that adds a column would make.
	db, err := sql.Open("sqlite", filepath.Join(d.f.data, "tenants", "lab1.db")+"?_pragma=busy_timeout(10000)")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.ExecContext(ctx, `UPDATE search_attributes SET state = 'building' WHERE field IN ('EchoTime', 'FlipAngle')`); err != nil {
		t.Fatal(err)
	}
	r := mustCall[*v1.BackfillAttributeResponse](t, "BackfillAttribute", req("EchoTime")(d.f.as(d.f.lab1Admin)))
	j := r.GetJob()
	if j.GetState() != v1.JobState_JOB_STATE_QUEUED || j.GetSearchBackfill() == nil ||
		len(j.GetSearchBackfill().GetFields()) != 2 || j.GetSearchBackfill().GetFields()[0] != "EchoTime" || j.GetSearchBackfill().GetFields()[1] != "FlipAngle" ||
		j.GetCounts().GetInstances() != int64(len(d.scans)) {
		t.Errorf("the job is %v, want queued, covering EchoTime and FlipAngle over %d scans", j, len(d.scans))
	}
	got := mustCall[*v1.GetJobResponse](t, "GetJob", call(d.f.as(d.f.lab1Reader).jobs.GetJob, &v1.GetJobRequest{Tenant: "lab1", Id: j.GetId()}))
	if got.GetJob().GetSearchBackfill() == nil || got.GetJob().GetId() != j.GetId() {
		t.Errorf("GetJob answers %v, want the search backfill job", got.GetJob())
	}
	d.f.run(t, []tcase{
		failed("a second job while the first has not ended", d.f.lab1Admin, "FlipAngle", connect.CodeFailedPrecondition, "one runs at a time"),
	})
	// The attributes stay being filled: a search that names one is refused.
	if o := call(d.f.as(d.f.lab1Reader).search.Search, &v1.SearchRequest{Tenant: "lab1", Query: "EchoTime > 1"}); o.code() != connect.CodeFailedPrecondition {
		t.Errorf("a search on EchoTime while it is being filled: %v, want a precondition failure", o.err)
	}
}
