package tenanttest

import (
	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// testCatalogLargest guards LargestInstance: 0 for a catalog that holds
// nothing, then the most bytes any instance the catalog holds takes, the
// same as the largest of every scan's instances listed one by one.
func testCatalogLargest(e *penv) {
	n, err := e.p.LargestInstance(e.ctx)
	e.ok("LargestInstance of an empty catalog", err)
	if n != 0 {
		e.Fatalf("an empty catalog's largest instance is %d bytes, want 0", n)
	}
	e.catalogTenant()
	var want int64
	scans, err := e.p.ListScans(e.ctx, tenant.ScanFilter{}, master.Page{Limit: master.MaxPageLimit})
	e.ok("ListScans", err)
	for _, s := range scans.Items {
		ins, _, err := e.p.ListHeldInstances(e.ctx, label.Path{Subject: s.SubjectLabel, Session: s.SessionLabel, Scan: s.Label},
			master.Page{Limit: master.MaxPageLimit})
		e.ok("ListHeldInstances", err)
		for _, in := range ins {
			want = max(want, in.Size, in.Offset+in.Length)
		}
	}
	n, err = e.p.LargestInstance(e.ctx)
	e.ok("LargestInstance", err)
	if want == 0 || n != want {
		e.Fatalf("the largest instance is %d bytes, want %d", n, want)
	}
}
