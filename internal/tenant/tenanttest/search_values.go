package tenanttest

import (
	"github.com/amrshadid/nopacs/internal/query"
	"github.com/amrshadid/nopacs/internal/search"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// testSearchTextWithNUL guards the archive of a scan whose header holds a NUL
// inside a text value, which a Postgres text column refuses: the row is made
// without it, the archive commits, and a search finds the scan by the value
// without the NUL.
func testSearchTextWithNUL(e *penv) {
	e.addReceiver("recv-a", "store", 11112)
	js := []byte(`{"0008103E":{"vr":"LO","Value":["T1\u0000 axial"]},"00080060":{"vr":"CS","Value":["MR"]},"00080008":{"vr":"CS","Value":["OR\u0000IGINAL","PRIMARY"]}}`)
	e.archiveStudy(studySpec{receiver: "recv-a", subject: "sub-a", session: "ses-1", study: 1, at: 10, date: "20240115",
		scans: []scanSpec{{label: "scan-n", modality: "MR", description: "T1 axial", instances: 1, row: search.ExtractJSON(js, instanceOf(1, 0, 0))}}})
	for _, cond := range []string{`SeriesDescription == "T1 axial"`, `ImageType == "ORIGINAL"`} {
		_, paths := e.searchAll(tenant.SearchQuery{Level: query.LevelScan, Condition: e.searchCondition(cond), Now: T0}, 10)
		if len(paths) != 1 {
			e.Errorf("%s finds %v, want the one scan, whose NULs were left out", cond, paths)
		}
	}
}
