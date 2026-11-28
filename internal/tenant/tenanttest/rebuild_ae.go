package tenanttest

import "github.com/amrshadid/nopacs/internal/tenant"

// testRebuildKeepsAETitles guards that a rebuilt session keeps the
// calling and called AE titles the metadata of its scans carry, so a dry run
// after a rebuild still reads them.
func testRebuildKeepsAETitles(e *renv) {
	e.rebuildSetup()
	var shadow []liveScan
	for _, s := range e.liveScans() {
		s.scan.Route.CallingAE, s.scan.Route.CalledAE = "RB_CALLING", "RB_CALLED"
		shadow = append(shadow, s)
	}
	e.createRebuild("rb-1", tenant.RebuildReplace)
	w := e.claimRebuild(at(10))
	e.build(w, shadow, 1000)
	_, err := e.p.ActivateCatalog(e.ctx, w.Fence(), at(30))
	e.ok("ActivateCatalog", err)
	se, err := e.p.CatalogSession(e.ctx, "sub-01", "ses-1")
	e.ok("CatalogSession", err)
	labels, err := e.p.CatalogLabels(e.ctx, se.StudyUID, nil)
	e.ok("CatalogLabels", err)
	if r := labels.Session.Route; r.CallingAE != "RB_CALLING" || r.CalledAE != "RB_CALLED" {
		e.Errorf("the rebuilt session's AE titles are %q and %q, want those of its scans", r.CallingAE, r.CalledAE)
	}
	got, err := e.p.RecentSessions(e.ctx, "recv", 10)
	if err == nil {
		for _, s := range got {
			if s.SessionLabel == "ses-1" && (s.Route.CallingAE != "RB_CALLING" || s.Route.CalledAE != "RB_CALLED") {
				e.Errorf("RecentSessions after the rebuild: %q and %q", s.Route.CallingAE, s.Route.CalledAE)
			}
		}
	}
}
