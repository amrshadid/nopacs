package tenanttest

import (
	"slices"

	"github.com/amrshadid/nopacs/internal/query"
	"github.com/amrshadid/nopacs/internal/search"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// testRebuildRemakesSearch guards the search table through a rebuild that
// replaces the catalog: every row is made again from what the build read
// of each scan's metadata (a scan the catalog held with other values has
// the shadow's, one the shadow lacks is gone with its row), and the rows
// an archive made and a rebuild made are the same for the same header.
func testRebuildRemakesSearch(e *renv) {
	bases := map[int]int{1: 0, 2: 500}
	rowOf := func(vals map[string]string, source string) search.Row {
		return search.Extract(dataSetOf(e.penv, vals), source)
	}
	e.rows = func(study, scan int) search.Row {
		return rowOf(map[string]string{"Manufacturer": "BEFORE", "Modality": "MR"}, uidOf(3, bases[study]+1000*scan))
	}
	e.rebuildSetup()
	found := func(cond string) []string {
		e.Helper()
		_, got := e.searchAll(tenant.SearchQuery{Level: query.LevelScan, Condition: e.searchCondition(cond), Now: T0}, 100)
		slices.Sort(got)
		return got
	}
	s1, s3 := "sub-01/ses-1/scan-"+uidOf(2, 100), "sub-01/ses-2/scan-"+uidOf(2, 200)
	if got := found(`Manufacturer == "BEFORE"`); len(got) != 3 {
		e.Fatalf("before the rebuild %v hold the first values, want three scans", got)
	}
	var shadow []liveScan
	for _, s := range e.liveScans() {
		switch s.scan.SeriesUID {
		case uidOf(2, 101):
			continue
		case uidOf(2, 100):
			s.scan.Search = rowOf(map[string]string{"Manufacturer": "REBUILT", "Modality": "MR", "SliceThickness": "2.5", "ImageType": `A\B`},
				s.instances[0].SOPInstanceUID)
		default:
			s.scan.Search = rowOf(map[string]string{"Manufacturer": "BEFORE", "Modality": "CT"}, s.instances[0].SOPInstanceUID)
		}
		shadow = append(shadow, s)
	}
	e.createRebuild("rb-1", tenant.RebuildReplace)
	w := e.claimRebuild(at(10))
	e.build(w, shadow, 1000)
	_, err := e.p.ActivateCatalog(e.ctx, w.Fence(), at(30))
	e.ok("ActivateCatalog", err)
	if got := found(`Manufacturer == "REBUILT"`); !slices.Equal(got, []string{s1}) {
		e.Errorf("after the rebuild the scans of the new values are %v, want %s", got, s1)
	}
	if got := found(`Manufacturer == "BEFORE"`); !slices.Equal(got, []string{s3}) {
		e.Errorf("after the rebuild the scans of the old values are %v, want %s alone: the scan the rebuild removed has no row", got, s3)
	}
	if got := found(`SliceThickness = 2.5 and ImageType == "B"`); !slices.Equal(got, []string{s1}) {
		e.Errorf("after the rebuild a number and a value of a multi-valued attribute find %v, want %s", got, s1)
	}
	if got := found(`Modality == "MR"`); !slices.Equal(got, []string{s1}) {
		e.Errorf("after the rebuild the MR scans are %v, want %s", got, s1)
	}
}
