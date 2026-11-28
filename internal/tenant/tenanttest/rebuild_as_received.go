package tenanttest

import (
	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// testRebuildAsReceived guards that a rebuilt catalog holds the as-received
// flag of its scans and sessions as the instances it was built from say,
// whatever the catalog it replaces held: the flags are worked out again
// when the shadow is finished, for every scan and session in it. The catalog
// in use holds a session of two scans archived as received and a session
// archived de-identified; the shadow is built from storages that hold one
// of the first session's scans de-identified and the second session's
// instance as received, and after the activation the flags are those of
// the shadow's instances.
func testRebuildAsReceived(e *renv) {
	e.addReceiver("recv", "store", 11112)
	v1, _, err := e.p.CreateProfileVersion(e.ctx, newVersion("research", "one"), at(0), e.audit(), nil)
	e.ok("CreateProfileVersion", err)
	e.finish(e.archive("recv", 1, 2, 1, 0, at(-7200), "ses-1"))
	e.profile = v1.Ref()
	e.finish(e.archive("recv", 2, 1, 1, 500, at(-3600), "ses-2"))
	flag := func(session, scan string) (bool, bool) {
		e.Helper()
		sc, err := e.p.CatalogScan(e.ctx, label.Path{Subject: "sub-01", Session: session, Scan: scan})
		e.ok("CatalogScan", err)
		se, err := e.p.CatalogSession(e.ctx, "sub-01", session)
		e.ok("CatalogSession", err)
		return sc.AsReceived, se.AsReceived
	}
	first, second, third := "scan-"+uidOf(2, 100), "scan-"+uidOf(2, 101), "scan-"+uidOf(2, 200)
	for _, c := range []struct {
		session, scan string
		scanWant      bool
		sessionWant   bool
	}{{"ses-1", first, true, true}, {"ses-1", second, true, true}, {"ses-2", third, false, false}} {
		if sc, se := flag(c.session, c.scan); sc != c.scanWant || se != c.sessionWant {
			e.Fatalf("before the rebuild %s of %s is as received %v and its session %v, want %v and %v", c.scan, c.session, sc, se,
				c.scanWant, c.sessionWant)
		}
	}

	e.createRebuild("rb-1", tenant.RebuildReplace)
	w := e.claimRebuild(at(10))
	shadow := e.liveScans()
	for i := range shadow {
		for j := range shadow[i].instances {
			switch shadow[i].scan.SeriesUID {
			case uidOf(2, 100):
				shadow[i].instances[j].Profile = v1.Ref()
			case uidOf(2, 200):
				shadow[i].instances[j].Profile = tenant.ProfileRef{}
			}
		}
	}
	e.build(w, shadow, 1000)
	_, err = e.p.ActivateCatalog(e.ctx, w.Fence(), at(30))
	e.ok("ActivateCatalog", err)
	for _, c := range []struct {
		session, scan string
		scanWant      bool
		sessionWant   bool
	}{{"ses-1", first, false, true}, {"ses-1", second, true, true}, {"ses-2", third, true, true}} {
		if sc, se := flag(c.session, c.scan); sc != c.scanWant || se != c.sessionWant {
			e.Errorf("after the rebuild %s of %s is as received %v and its session %v, want %v and %v", c.scan, c.session, sc, se,
				c.scanWant, c.sessionWant)
		}
	}
}
