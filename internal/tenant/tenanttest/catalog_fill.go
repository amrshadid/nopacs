package tenanttest

import (
	"slices"

	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// testCatalogValuesFilled guards FillScanValues, which gives a scan
// archived before the catalog kept its header values the values its files
// say: a scan and session archived with none are found by the lists that
// filter by them once filled (study date, modality, through the session's
// modalities too, and series description); a value held is never changed,
// by the fill of another scan of the session or by a fill again; no total
// and no update time moves; and a series with no scan is NotFound, values
// not of their kind Invalid.
func testCatalogValuesFilled(e *penv) {
	e.catalogTenant()
	e.archiveStudy(studySpec{receiver: "recv-a", subject: "sub-f", session: "ses-f", study: 7, at: 200, scans: []scanSpec{
		{label: "scan-old", instances: 2},
		{label: "scan-held", modality: "CT", description: "Held", instances: 1}}})
	old := label.Path{Subject: "sub-f", Session: "ses-f", Scan: "scan-old"}
	held := label.Path{Subject: "sub-f", Session: "ses-f", Scan: "scan-held"}
	read := func() (tenant.CatalogSubject, tenant.CatalogSession, tenant.CatalogScan, tenant.CatalogScan, tenant.CatalogCounts) {
		e.Helper()
		sub, err := e.p.CatalogSubject(e.ctx, "sub-f")
		e.ok("CatalogSubject", err)
		ses, err := e.p.CatalogSession(e.ctx, "sub-f", "ses-f")
		e.ok("CatalogSession", err)
		a, err := e.p.CatalogScan(e.ctx, old)
		e.ok("CatalogScan", err)
		b, err := e.p.CatalogScan(e.ctx, held)
		e.ok("CatalogScan", err)
		totals, err := e.p.CatalogTotals(e.ctx)
		e.ok("CatalogTotals", err)
		return sub, ses, a, b, totals
	}
	sub0, ses0, old0, held0, totals0 := read()
	if old0.Values != (tenant.ScanValues{}) || ses0.StudyDate != "" {
		e.Fatalf("a scan archived without values: %+v, session study date %q", old0.Values, ses0.StudyDate)
	}
	mrSessions := func() []string {
		got, _ := catalogPages(e, 10, func(p master.Page) (tenant.CatalogPage[tenant.CatalogSession], error) {
			return e.p.ListSessions(e.ctx, tenant.SessionFilter{Modality: "OT"}, p)
		})
		return sessionPaths(got)
	}
	e.wantList("the sessions of a modality before the fill", mrSessions(), []string{})

	e.ok("FillScanValues", e.p.FillScanValues(e.ctx, seriesOf(7, 0), tenant.ScanValuesOf("20190607", "OT", "Old Series", "12")))
	e.ok("FillScanValues", e.p.FillScanValues(e.ctx, seriesOf(7, 1), tenant.ScanValuesOf("20010101", "MR", "other", "4")))

	sub1, ses1, old1, held1, totals1 := read()
	if old1.Values != (tenant.ScanValues{Modality: "OT", SeriesDescription: "Old Series", SeriesNumber: 12, HasSeriesNumber: true}) {
		e.Errorf("a scan's values after the fill: %+v", old1.Values)
	}
	if held1.Values != (tenant.ScanValues{Modality: "CT", SeriesDescription: "Held", SeriesNumber: 4, HasSeriesNumber: true}) {
		e.Errorf("a scan's held values after a fill with others: %+v, want them kept and the number filled", held1.Values)
	}
	if ses1.StudyDate != "20190607" || !slices.Equal(ses1.Modalities, []string{"CT", "OT"}) {
		e.Errorf("the session after the fills: study date %q, modalities %q; want the first date given and both modalities",
			ses1.StudyDate, ses1.Modalities)
	}
	if sub1 != sub0 || totals1 != totals0 || old1.Instances != old0.Instances || old1.Bytes != old0.Bytes || !old1.Updated.Equal(old0.Updated) ||
		held1.Instances != held0.Instances || !held1.Updated.Equal(held0.Updated) || ses1.Instances != ses0.Instances ||
		ses1.Scans != ses0.Scans || ses1.Bytes != ses0.Bytes || !ses1.Updated.Equal(ses0.Updated) {
		e.Errorf("a fill moved a total or an update time:\nsubject %+v -> %+v\nsession %+v -> %+v\ntotals %+v -> %+v",
			sub0, sub1, ses0, ses1, totals0, totals1)
	}
	e.wantList("the sessions of a modality after the fill", mrSessions(), []string{"sub-f/ses-f"})
	dated, _ := catalogPages(e, 10, func(p master.Page) (tenant.CatalogPage[tenant.CatalogSession], error) {
		return e.p.ListSessions(e.ctx, tenant.SessionFilter{Order: tenant.OrderStudyDate, StudyDateStart: "20190101", StudyDateEnd: "20191231"}, p)
	})
	e.wantList("a study date filled", sessionPaths(dated), []string{"sub-f/ses-f"})
	scans, _ := catalogPages(e, 10, func(p master.Page) (tenant.CatalogPage[tenant.CatalogScan], error) {
		return e.p.ListScans(e.ctx, tenant.ScanFilter{Modality: "OT"}, p)
	})
	e.wantList("the scans of a modality filled", scanPaths(scans), []string{"sub-f/ses-f/scan-old"})
	described, _ := catalogPages(e, 10, func(p master.Page) (tenant.CatalogPage[tenant.CatalogScan], error) {
		return e.p.ListScans(e.ctx, tenant.ScanFilter{Order: tenant.OrderSeriesDescription, DescriptionPrefix: "old s"}, p)
	})
	e.wantList("a description filled", scanPaths(described), []string{"sub-f/ses-f/scan-old"})

	e.ok("FillScanValues", e.p.FillScanValues(e.ctx, seriesOf(7, 0), tenant.ScanValuesOf("20240101", "US", "again", "1")))
	e.ok("FillScanValues", e.p.FillScanValues(e.ctx, seriesOf(7, 0), tenant.ScanValues{}))
	if _, ses2, old2, _, totals2 := read(); old2.Values != old1.Values || ses2.StudyDate != ses1.StudyDate ||
		!slices.Equal(ses2.Modalities, ses1.Modalities) || totals2 != totals1 {
		e.Errorf("a fill again changed what was filled: %+v, session %+v", old2.Values, ses2)
	}
	e.wantTotalsRecounted("after the fills")

	e.wantKind("a series with no scan", e.p.FillScanValues(e.ctx, seriesOf(7, 9), tenant.ScanValuesOf("", "MR", "", "")), fault.NotFound)
	e.wantKind("a series UID not well formed", e.p.FillScanValues(e.ctx, "1.2..3", tenant.ScanValues{}), fault.Invalid)
	e.wantKind("a value not of its kind", e.p.FillScanValues(e.ctx, seriesOf(7, 0), tenant.ScanValues{Modality: "mr"}), fault.Invalid)
}
