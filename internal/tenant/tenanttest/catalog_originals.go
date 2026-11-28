package tenanttest

import (
	"slices"
	"strconv"

	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// testCatalogOriginals guards how the catalog's lists treat the rows
// archived as received for a caller that may not learn their values
// (tenant.OriginalsHidden): a filter on a value read from the files (a
// modality, a range of study dates, a series description prefix) never
// keeps such a session or scan, though it keeps the de-identified ones; an
// order by such a value lists them after the rest, in label order, across
// pages; a cursor of one treatment is refused by the other; and the same
// lists with OriginalsAll are as they were.
func testCatalogOriginals(e *penv) {
	e.addStorage("store")
	e.addReceiver("recv-a", "store", 11112)
	v, _ := e.create(newVersion("research", "one"), 1)
	deid := v.Ref()
	for _, s := range []studySpec{
		{receiver: "recv-a", subject: "sub-1", session: "ses-1", study: 1, at: 10, date: "20200105", profile: deid, scans: []scanSpec{
			{label: "scan-a", modality: "MR", description: "Alpha", instances: 1}, {label: "scan-b", modality: "CT", description: "Beta", instances: 1}}},
		{receiver: "recv-a", subject: "sub-2", session: "ses-1", study: 2, at: 20, date: "20210301", scans: []scanSpec{
			{label: "scan-c", modality: "MR", description: "Gamma", instances: 1}, {label: "scan-d", modality: "CT", description: "Delta", instances: 1}}},
		{receiver: "recv-a", subject: "sub-3", session: "ses-1", study: 3, at: 30, date: "20220101", profile: deid, scans: []scanSpec{
			{label: "scan-e", modality: "CT", description: "Epsilon", instances: 1}}},
	} {
		e.archiveStudy(s)
	}
	sessions := func(f tenant.SessionFilter, limit int) (labels []string, pages int) {
		e.Helper()
		page := master.Page{Limit: limit}
		for {
			got, err := e.p.ListSessions(e.ctx, f, page)
			e.ok("ListSessions", err)
			for _, s := range got.Items {
				labels = append(labels, s.SubjectLabel)
			}
			pages++
			if got.Next == "" || pages > 10 {
				return labels, pages
			}
			page.Cursor = got.Next
		}
	}
	scans := func(f tenant.ScanFilter, limit int) (labels []string) {
		e.Helper()
		page := master.Page{Limit: limit}
		for n := 0; n < 10; n++ {
			got, err := e.p.ListScans(e.ctx, f, page)
			e.ok("ListScans", err)
			for _, s := range got.Items {
				labels = append(labels, s.Label)
			}
			if got.Next == "" {
				break
			}
			page.Cursor = got.Next
		}
		return labels
	}
	want := func(what string, got []string, w ...string) {
		e.Helper()
		if !slices.Equal(got, w) {
			e.Errorf("%s: %v, want %v", what, got, w)
		}
	}
	hide := tenant.OriginalsHidden

	// A modality.
	got, _ := sessions(tenant.SessionFilter{Modality: "CT"}, 10)
	want("sessions with a CT, all", got, "sub-3", "sub-2", "sub-1")
	got, _ = sessions(tenant.SessionFilter{Modality: "CT", Originals: hide}, 10)
	want("sessions with a CT, hidden", got, "sub-3", "sub-1")
	want("scans of CT, all", scans(tenant.ScanFilter{Modality: "CT"}, 10), "scan-e", "scan-d", "scan-b")
	want("scans of CT, hidden", scans(tenant.ScanFilter{Modality: "CT", Originals: hide}, 10), "scan-e", "scan-b")
	want("scans of CT, hidden, a page of one", scans(tenant.ScanFilter{Modality: "CT", Originals: hide}, 1), "scan-e", "scan-b")

	// A range of study dates, then the order by study date alone.
	got, _ = sessions(tenant.SessionFilter{Order: tenant.OrderStudyDate, StudyDateStart: "20200101", StudyDateEnd: "20211231"}, 10)
	want("sessions of 2020-2021, all", got, "sub-2", "sub-1")
	got, _ = sessions(tenant.SessionFilter{Order: tenant.OrderStudyDate, StudyDateStart: "20200101", StudyDateEnd: "20211231", Originals: hide}, 10)
	want("sessions of 2020-2021, hidden", got, "sub-1")
	got, _ = sessions(tenant.SessionFilter{Order: tenant.OrderStudyDate}, 10)
	want("sessions by study date, all", got, "sub-3", "sub-2", "sub-1")
	for _, limit := range []int{10, 2, 1} {
		got, _ = sessions(tenant.SessionFilter{Order: tenant.OrderStudyDate, Originals: hide}, limit)
		want("sessions by study date, hidden, pages of "+strconv.Itoa(limit), got, "sub-3", "sub-1", "sub-2")
	}

	// A series description prefix, then the order by description alone.
	want("scans described g, all", scans(tenant.ScanFilter{Order: tenant.OrderSeriesDescription, DescriptionPrefix: "g"}, 10), "scan-c")
	want("scans described g, hidden", scans(tenant.ScanFilter{Order: tenant.OrderSeriesDescription, DescriptionPrefix: "g", Originals: hide}, 10))
	want("scans by description, all", scans(tenant.ScanFilter{Order: tenant.OrderSeriesDescription}, 10), "scan-a", "scan-b", "scan-d", "scan-e", "scan-c")
	for _, limit := range []int{10, 3, 1} {
		want("scans by description, hidden, pages of "+strconv.Itoa(limit),
			scans(tenant.ScanFilter{Order: tenant.OrderSeriesDescription, Originals: hide}, limit), "scan-a", "scan-b", "scan-e", "scan-c", "scan-d")
	}

	// A cursor made under one treatment is no page token of the other.
	first, err := e.p.ListSessions(e.ctx, tenant.SessionFilter{Order: tenant.OrderStudyDate, Originals: hide}, master.Page{Limit: 1})
	e.ok("ListSessions", err)
	_, err = e.p.ListSessions(e.ctx, tenant.SessionFilter{Order: tenant.OrderStudyDate}, master.Page{Limit: 1, Cursor: first.Next})
	e.wantKind("a hidden cursor given to a list of all", err, fault.Invalid)
}
