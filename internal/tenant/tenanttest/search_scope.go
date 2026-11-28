package tenanttest

import (
	"slices"
	"strings"

	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/query"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// testSearchStudyScope guards a search of scans kept to one study
// (SearchQuery.StudyUID): it finds the scans of that study's session that
// the condition matches and no other, with the count and the facet over
// them; a study the tenant does not hold finds nothing; a level other than
// scans refuses it; and a page token of a scoped search is not a token of
// the same search over the whole tenant.
func testSearchStudyScope(e *penv) {
	e.searchTenant(e.deidProfile())
	study := uidOf(1, 1)
	all := tenant.SearchQuery{Level: query.LevelScan, Page: master.Page{Limit: 100}, Now: T0, Count: true, CountRows: 1000}
	whole, wholePaths := e.searchAll(all, 3)
	scoped := all
	scoped.StudyUID = study
	r, paths := e.searchAll(scoped, 2)
	if len(paths) == 0 || len(paths) >= len(wholePaths) {
		e.Fatalf("a search kept to one study finds %v of the tenant's %v", paths, wholePaths)
	}
	session := strings.Join(strings.Split(paths[0], "/")[:2], "/")
	for _, p := range paths {
		if got := strings.Join(strings.Split(p, "/")[:2], "/"); got != session {
			e.Errorf("a search kept to one study finds %s, of session %s and %s", p, got, session)
		}
	}
	if r.Total != int64(len(paths)) || whole.Total != int64(len(wholePaths)) {
		e.Errorf("the counts are %d and %d, want %d and %d", r.Total, whole.Total, len(paths), len(wholePaths))
	}
	// A condition and the scope are both kept.
	both := scoped
	both.Condition = e.searchCondition(`Modality == "MR"`)
	_, mr := e.searchAll(both, 100)
	_, wholeMR := e.searchAll(tenant.SearchQuery{Level: query.LevelScan, Condition: both.Condition, Now: T0}, 100)
	for _, p := range mr {
		if !slices.Contains(paths, p) || !slices.Contains(wholeMR, p) {
			e.Errorf("%s is found by the scoped MR search and not by both the scope and the condition", p)
		}
	}
	for _, p := range wholeMR {
		if slices.Contains(paths, p) && !slices.Contains(mr, p) {
			e.Errorf("%s is of the study and of modality MR and the scoped search lacks it", p)
		}
	}
	none := scoped
	none.StudyUID = "2.25.99.99"
	if r, paths := e.searchAll(none, 10); len(paths) != 0 || r.Total != 0 {
		e.Errorf("a study the tenant does not hold finds %v counting %d", paths, r.Total)
	}
	sessions := scoped
	sessions.Level = query.LevelSession
	_, err := e.p.Search(e.ctx, sessions)
	e.wantKind("a search of sessions kept to one study", err, fault.Invalid)
	// A token of the scoped search is not the unscoped search's.
	a, err := e.p.Search(e.ctx, tenant.SearchQuery{Level: query.LevelScan, StudyUID: study, Page: master.Page{Limit: 1}, Now: T0})
	e.ok("Search", err)
	if a.Next == "" {
		e.Fatal("a scoped search of several scans answers one page")
	}
	_, err = e.p.Search(e.ctx, tenant.SearchQuery{Level: query.LevelScan, Page: master.Page{Limit: 1, Cursor: a.Next}, Now: T0})
	e.wantKind("a page token of a scoped search used unscoped", err, fault.Invalid)
}
