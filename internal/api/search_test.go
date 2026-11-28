package api_test

import (
	"slices"
	"testing"

	connect "connectrpc.com/connect/v2"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/master"
)

// testSearch guards Search: reader on the tenant; the rows of a search over
// real archived files are those the catalog's own modality filter lists, at
// each level; a count, a facet and a page agree with them; a condition that
// does not parse, that no index serves or that names a field the table does
// not hold is invalid, and is audited without its values; and a masked key
// finds no row archived as received.
func testSearch(t *testing.T) {
	d := newCatalogData(t)
	req := func(mod func(*v1.SearchRequest)) func(clients) outcome {
		return func(c clients) outcome {
			r := &v1.SearchRequest{Tenant: "lab1"}
			mod(r)
			return call(c.search.Search, r)
		}
	}
	d.refusals(t, "Search", func(c clients, tenant string) outcome {
		return call(c.search.Search, &v1.SearchRequest{Tenant: tenant})
	})
	reader := d.f.as(d.f.lab1Reader)
	modalities := map[string]bool{}
	for _, s := range d.scans {
		modalities[s.GetModality()] = true
	}
	for m := range modalities {
		var want []string
		for _, s := range d.scans {
			if s.GetModality() == m {
				want = append(want, scanKey(s))
			}
		}
		slices.Sort(want)
		r := mustCall[*v1.SearchResponse](t, "Search "+m, req(func(r *v1.SearchRequest) {
			r.Query = `Modality == "` + m + `"`
			r.Count, r.Facet, r.PageSize = true, "Modality", 1
		})(reader))
		var got []string
		for {
			got = append(got, scanKey(r.GetScans()[0]))
			if r.GetNextPageToken() == "" {
				break
			}
			tok := r.GetNextPageToken()
			r = mustCall[*v1.SearchResponse](t, "Search next", req(func(r *v1.SearchRequest) {
				r.Query, r.PageSize, r.PageToken = `Modality == "`+m+`"`, 1, tok
			})(reader))
		}
		slices.Sort(got)
		if !slices.Equal(got, want) {
			t.Errorf("a search for %s finds %v, the catalog lists %v", m, got, want)
		}
	}
	all := mustCall[*v1.SearchResponse](t, "Search of every scan", req(func(r *v1.SearchRequest) { r.Count, r.Facet = true, "Modality" })(reader))
	if all.TotalCount == nil || int(all.GetTotalCount()) != len(d.scans) || all.GetTotalIsLowerBound() || len(all.GetScans()) != len(d.scans) {
		t.Errorf("a search of every scan answers %d scans with count %v, want %d", len(all.GetScans()), all.TotalCount, len(d.scans))
	}
	var counted int64
	for _, f := range all.GetFacet() {
		counted += f.GetCount()
	}
	if int(counted) != len(d.scans) || len(all.GetFacet()) != len(modalities) {
		t.Errorf("the facet of Modality is %v over %d scans", all.GetFacet(), len(d.scans))
	}
	for _, m := range []string{"MR", "CT"} {
		if !modalities[m] {
			continue
		}
		want := d.sessionWith(t, m)
		r := mustCall[*v1.SearchResponse](t, "Search sessions", req(func(r *v1.SearchRequest) {
			r.Level, r.Query = v1.SearchLevel_SEARCH_LEVEL_SESSION, `has scan (Modality == "`+m+`")`
		})(reader))
		if !slices.Contains(sessionKeys(r.GetSessions()), sessionKey(want)) {
			t.Errorf("the sessions holding a %s scan are %v, want %s among them", m, sessionKeys(r.GetSessions()), sessionKey(want))
		}
		break
	}
	subjects := mustCall[*v1.SearchResponse](t, "Search subjects", req(func(r *v1.SearchRequest) {
		r.Level, r.Query = v1.SearchLevel_SEARCH_LEVEL_SUBJECT, `has session (has scan (Modality present))`
	})(reader))
	if len(subjects.GetSubjects()) != len(d.subjects) {
		t.Errorf("the subjects holding a scan with a modality are %d, want %d", len(subjects.GetSubjects()), len(d.subjects))
	}
	bad := func(name string, mod func(*v1.SearchRequest), msg string) tcase {
		return tcase{name: name, as: d.f.lab1Reader, call: req(mod), code: connect.CodeInvalidArgument, msg: msg, audit: inTenant("lab1", master.OutcomeFailed)}
	}
	d.f.run(t, []tcase{
		{name: "audited", as: d.f.lab1Reader, call: req(func(r *v1.SearchRequest) { r.Query = `Modality == "Smith^John"` }),
			audit: &wantAudit{log: "lab1", outcome: master.OutcomeDone, object: "-", details: map[string]string{"level": "unspecified", "fields": "Modality"}}},
		bad("a condition that does not parse", func(r *v1.SearchRequest) { r.Query = `Modality ==` }, "query"),
		bad("a negation alone", func(r *v1.SearchRequest) { r.Query = `not Modality == "MR"` }, "negation"),
		bad("a contains of two characters", func(r *v1.SearchRequest) { r.Query = `SeriesDescription contains "mp"` }, "three characters"),
		bad("a field the table does not hold", func(r *v1.SearchRequest) { r.Query = `PatientBirthDate == 2000-01-01` }, "PatientBirthDate"),
		bad("a level the contract does not define", func(r *v1.SearchRequest) { r.Level = 77 }, "level"),
		bad("a facet of a field of another level", func(r *v1.SearchRequest) { r.Facet = "session.date" }, "facet"),
		bad("a page token that is not the search's", func(r *v1.SearchRequest) { r.PageToken = "garbage" }, "page"),
		bad("a page size above the bound", func(r *v1.SearchRequest) { r.PageSize = 100000 }, "page_size"),
	})
	// A masked key finds no scan archived as received, whatever it asks.
	masked, _ := d.f.maskedKey(d.f.onTenant("lab1", master.RoleReader))
	seen := mustCall[*v1.ListScansResponse](t, "ListScans masked", call(d.f.as(masked).catalog.ListScans, &v1.ListScansRequest{Tenant: "lab1", PageSize: 1000})).GetScans()
	var visible []string
	for _, s := range seen {
		if !s.GetValuesWithheld() {
			visible = append(visible, scanKey(s))
		}
	}
	for _, q := range []string{"", `Modality present`, `has session (session.date absent or session.date present)`} {
		r := mustCall[*v1.SearchResponse](t, "Search masked", req(func(r *v1.SearchRequest) { r.Query, r.Count = q, true })(d.f.as(masked)))
		var got []string
		for _, s := range r.GetScans() {
			got = append(got, scanKey(s))
		}
		slices.Sort(got)
		slices.Sort(visible)
		if !slices.Equal(got, visible) || int(r.GetTotalCount()) != len(visible) {
			t.Errorf("a masked key's search %q finds %v counting %d, want the scans not archived as received %v", q, got, r.GetTotalCount(), visible)
		}
	}
}

// sessionKeys returns the keys of sessions.
func sessionKeys(ss []*v1.Session) []string {
	var out []string
	for _, s := range ss {
		out = append(out, sessionKey(s))
	}
	return out
}

// testListSearchAttributes guards ListSearchAttributes: reader on the
// tenant; every attribute of the search tables is listed, ready.
func testListSearchAttributes(t *testing.T) {
	d := newCatalogData(t)
	d.refusals(t, "ListSearchAttributes", func(c clients, tenant string) outcome {
		return call(c.search.ListSearchAttributes, &v1.ListSearchAttributesRequest{Tenant: tenant})
	})
	r := mustCall[*v1.ListSearchAttributesResponse](t, "ListSearchAttributes", call(d.f.as(d.f.lab1Reader).search.ListSearchAttributes,
		&v1.ListSearchAttributesRequest{Tenant: "lab1"}))
	got := map[string]bool{}
	for _, a := range r.GetAttributes() {
		got[a.GetField()] = a.GetReady()
	}
	for _, k := range []string{"Modality", "SeriesDescription", "SliceThickness", "ImageType", "OperatorsName"} {
		if ready, ok := got[k]; !ok || !ready {
			t.Errorf("the attribute %s is listed %v ready %v, want listed and ready", k, ok, ready)
		}
	}
}
