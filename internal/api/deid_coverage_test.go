package api_test

import (
	"strings"
	"testing"

	connect "connectrpc.com/connect/v2"
	"google.golang.org/protobuf/encoding/protojson"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// refreshCoverage is a call of RefreshCoverage for lab1.
func refreshCoverage() func(clients) outcome {
	return func(c clients) outcome {
		return call(c.deid.RefreshCoverage, &v1.RefreshCoverageRequest{Tenant: "lab1"})
	}
}

// finishCoverage counts every scan of lab1 into the coverage job id with the
// elements given and ends it, as the server's runner does, through the port.
func finishCoverage(t *testing.T, d *catalogData, id string, elements []tenant.CoverageElement) {
	t.Helper()
	p := d.f.pipelineOf("lab1")
	scans, err := p.DeidCoverageScans(ctx, 0, 100)
	if err != nil || len(scans) != len(d.scans) {
		t.Fatalf("the coverage job lists %d scans (%v), want %d", len(scans), err, len(d.scans))
	}
	rows := make([]tenant.DeidCoverageScanRow, len(scans))
	for i, s := range scans {
		rows[i] = tenant.DeidCoverageScanRow{ID: s.ID, Instances: 1, Recorded: 1}
	}
	if _, err := p.ApplyDeidCoverage(ctx, tenant.DeidCoverageApply{JobID: id, Scans: rows, Elements: elements, Now: d.f.now()}); err != nil {
		t.Fatal(err)
	}
	if _, err := p.EndDeidCoverage(ctx, id, d.f.now()); err != nil {
		t.Fatal(err)
	}
}

// testRefreshCoverage guards RefreshCoverage: the operator on the tenant
// makes a queued deid_coverage job over the tenant's scans, a masked key may
// too (the job writes the database only), a reader is refused as is a key of
// another tenant, a second request while the job has not ended is a
// precondition failure, GetJob answers the job, and every attempt is audited
// with the job's id and no value.
func testRefreshCoverage(t *testing.T) {
	d := newCatalogData(t)
	d.refusals(t, "RefreshCoverage", func(c clients, tenantName string) outcome {
		return call(c.deid.RefreshCoverage, &v1.RefreshCoverageRequest{Tenant: tenantName})
	})
	masked, _ := d.f.maskedKey(d.f.onTenant("lab1", master.RoleOperator))
	d.f.run(t, []tcase{
		{name: "a reader is refused", as: d.f.lab1Reader, call: refreshCoverage(), code: connect.CodePermissionDenied},
	})
	r := mustCall[*v1.RefreshCoverageResponse](t, "RefreshCoverage", refreshCoverage()(d.f.as(masked)))
	j := r.GetJob()
	if j.GetState() != v1.JobState_JOB_STATE_QUEUED || j.GetDeidCoverage() == nil || j.GetCounts().GetInstances() != int64(len(d.scans)) {
		t.Errorf("the job is %v, want queued, of kind deid_coverage, over %d scans", j, len(d.scans))
	}
	got := mustCall[*v1.GetJobResponse](t, "GetJob", call(d.f.as(d.f.lab1Reader).jobs.GetJob, &v1.GetJobRequest{Tenant: "lab1", Id: j.GetId()}))
	if got.GetJob().GetDeidCoverage() == nil || got.GetJob().GetId() != j.GetId() {
		t.Errorf("GetJob answers %v, want the coverage job", got.GetJob())
	}
	d.f.run(t, []tcase{
		{name: "a second job while the first has not ended", as: d.f.lab1Operator, call: refreshCoverage(), code: connect.CodeFailedPrecondition, msg: "one runs at a time",
			audit: inTenant("lab1", master.OutcomeFailed)},
	})
	// A job that a retry would run again is not retried.
	if o := call(d.f.as(d.f.lab1Admin).jobs.RetryJob, &v1.RetryJobRequest{Tenant: "lab1", Id: j.GetId()}); o.err == nil {
		t.Errorf("a coverage job that has not ended was retried")
	}
}

// testGetCoverage guards GetCoverage: no report before a job has ended, the
// job in progress shown beside it; then the elements of the latest job that
// ended in order of tag, creator and VR, paged by a token that holds only
// while that report stands, each with its VR and counts; a creator the job
// withheld is answered withheld, with no name; a masked key reads it as an
// operator does, and a reader may not; and no value of any file is in the
// answer, which holds tags, VRs, vendor names and counts.
func testGetCoverage(t *testing.T) {
	d := newCatalogData(t)
	get := func(token string, size int32) func(clients) outcome {
		return func(c clients) outcome {
			return call(c.deid.GetCoverage, &v1.GetCoverageRequest{Tenant: "lab1", PageSize: size, PageToken: token})
		}
	}
	d.refusals(t, "GetCoverage", func(c clients, tenantName string) outcome {
		return call(c.deid.GetCoverage, &v1.GetCoverageRequest{Tenant: tenantName})
	})
	masked, _ := d.f.maskedKey(d.f.onTenant("lab1", master.RoleOperator))
	d.f.run(t, []tcase{
		{name: "a reader is refused", as: d.f.lab1Reader, call: get("", 0), code: connect.CodePermissionDenied},
		{name: "a page size past the bound", as: d.f.lab1Operator, call: get("", 1001), code: connect.CodeInvalidArgument, msg: "page_size"},
		{name: "a token before any report", as: d.f.lab1Operator, call: get("c1.x", 0), code: connect.CodeInvalidArgument, msg: "page_token"},
		{name: "before any job", as: d.f.lab1Operator, call: get("", 0), check: func(t *testing.T, o outcome) {
			if r := o.res.(*v1.GetCoverageResponse); r.GetReport() != nil || len(r.GetElements()) != 0 || r.GetRefreshing() != nil {
				t.Errorf("before any job: %v", r)
			}
		}},
	})
	first := mustCall[*v1.RefreshCoverageResponse](t, "RefreshCoverage", refreshCoverage()(d.f.as(d.f.lab1Operator))).GetJob()
	if r := mustCall[*v1.GetCoverageResponse](t, "GetCoverage", get("", 0)(d.f.as(d.f.lab1Operator))); r.GetReport() != nil || r.GetRefreshing().GetId() != first.GetId() {
		t.Errorf("while the first job runs: report %v, refreshing %v", r.GetReport(), r.GetRefreshing())
	}
	acme := tenant.CoverageKey{Tag: 0x00290010, Creator: "ACME RESEARCH", VR: "LO"}
	finishCoverage(t, d, first.GetId(), []tenant.CoverageElement{
		{CoverageKey: tenant.CoverageKey{Tag: 0x00180050, VR: "DS"}, Instances: 4, Occurrences: 4},
		{CoverageKey: acme, Instances: 4, Occurrences: 8},
		{CoverageKey: tenant.CoverageKey{Tag: 0x00290010, Withheld: true, VR: "LO"}, Instances: 1, Occurrences: 1},
		{CoverageKey: tenant.CoverageKey{Tag: 0x00200013, VR: "IS"}, Instances: 4, Occurrences: 4},
	})
	for _, as := range []caller{d.f.lab1Operator, masked} {
		r := mustCall[*v1.GetCoverageResponse](t, "GetCoverage", get("", 0)(d.f.as(as)))
		if r.GetReport().GetJobId() != first.GetId() || r.GetReport().GetScansRead() != int64(len(d.scans)) || r.GetReport().GetInstances() != int64(len(d.scans)) ||
			r.GetReport().GetInstancesRecorded() != int64(len(d.scans)) || r.GetReport().GetDistinctElements() != 4 || r.GetReport().GetEndTime() == nil || r.GetRefreshing() != nil {
			t.Fatalf("the report is %v", r)
		}
		var tags []string
		for _, e := range r.GetElements() {
			tags = append(tags, e.GetTag()+" "+e.GetVr()+" "+e.GetPrivateCreator())
		}
		want := "0018,0050 DS |0020,0013 IS |0029,xx10 LO |0029,xx10 LO ACME RESEARCH"
		if strings.Join(tags, "|") != want {
			t.Errorf("the elements are %q, want %q", strings.Join(tags, "|"), want)
		}
		w := r.GetElements()[2]
		if !w.GetCreatorWithheld() || w.GetPrivateCreator() != "" || !w.GetPrivate() || w.GetInstances() != 1 {
			t.Errorf("the withheld creator's element is %v, want withheld with no name", w)
		}
		if a := r.GetElements()[3]; a.GetInstances() != 4 || a.GetOccurrences() != 8 || a.GetCreatorWithheld() {
			t.Errorf("the vendor's element is %v", a)
		}
	}
	// The largest page a list allows is answered, with no more to follow.
	if r := mustCall[*v1.GetCoverageResponse](t, "the largest page", get("", 1000)(d.f.as(d.f.lab1Operator))); len(r.GetElements()) != 4 || r.GetNextPageToken() != "" {
		t.Errorf("a page of 1000 holds %d elements with token %q, want 4 and none", len(r.GetElements()), r.GetNextPageToken())
	}
	// Paged by a token.
	var tags []string
	token := ""
	for pages := 0; ; pages++ {
		r := mustCall[*v1.GetCoverageResponse](t, "GetCoverage", get(token, 3)(d.f.as(d.f.lab1Operator)))
		for _, e := range r.GetElements() {
			tags = append(tags, e.GetTag())
		}
		if token = r.GetNextPageToken(); token == "" {
			if pages != 1 {
				t.Errorf("a report of 4 elements was %d pages of 3", pages+1)
			}
			break
		}
	}
	if len(tags) != 4 {
		t.Errorf("the pages hold %v, want the 4 elements once", tags)
	}
	page := mustCall[*v1.GetCoverageResponse](t, "GetCoverage", get("", 3)(d.f.as(d.f.lab1Operator)))
	// No value is in the answer: it is text of tags, VRs, a vendor's name and
	// counts alone.
	text := protojson.Format(page)
	for _, banned := range []string{"PatientName", "Doe", "ZZTEST"} {
		if strings.Contains(text, banned) {
			t.Errorf("the answer holds %q: %s", banned, text)
		}
	}
	// A token holds only while the report it was made over stands.
	second := mustCall[*v1.RefreshCoverageResponse](t, "RefreshCoverage again", refreshCoverage()(d.f.as(d.f.lab1Operator))).GetJob()
	finishCoverage(t, d, second.GetId(), []tenant.CoverageElement{{CoverageKey: tenant.CoverageKey{Tag: 0x00180050, VR: "DS"}, Instances: 1, Occurrences: 1}})
	d.f.run(t, []tcase{
		{name: "a token of the report before", as: d.f.lab1Operator, call: get(page.GetNextPageToken(), 3), code: connect.CodeInvalidArgument, msg: "page_token"},
		{name: "a token that is not one", as: d.f.lab1Operator, call: get("not-a-token", 3), code: connect.CodeInvalidArgument, msg: "page_token"},
		{name: "the new report", as: d.f.lab1Operator, call: get("", 0), check: func(t *testing.T, o outcome) {
			r := o.res.(*v1.GetCoverageResponse)
			if r.GetReport().GetJobId() != second.GetId() || len(r.GetElements()) != 1 {
				t.Errorf("after the second job the report is %v", r)
			}
		}},
	})
}
