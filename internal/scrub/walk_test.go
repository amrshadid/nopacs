package scrub

import (
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/core/layout"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// TestOrphanOfAScanGoneIsCleared guards that an open orphan finding that
// names a scan the catalog no longer holds is cleared by a walk that finds
// its object gone, on a primary and on a replica alike: the scan's unit
// that would clear it is never made once the scan has left the catalog.
func TestOrphanOfAScanGoneIsCleared(t *testing.T) {
	r := newReplicaRig(t)
	r.archiveTwo("s1", "1.2.826.0.1.3680043.10.519")
	r.addReplica("copy")
	r.copyAll()
	gone := label.Path{Subject: "sub-01", Session: "ses-gone", Scan: "gone"}
	key, err := layout.InstanceKey(gone, "1.2.826.0.1.3680043.10.519.77")
	if err != nil {
		t.Fatal(err)
	}
	old := r.createScrub("")
	w, err := r.p.ClaimWork(r.ctx, tenant.ScrubWorkKind, "test", time.Now(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	for _, storage := range []string{"main", "copy"} {
		f := tenant.ScrubFinding{Kind: tenant.FindingOrphan, Storage: storage, Key: key, Path: gone}
		f.ID = tenant.ScrubFindingID(old.ID, storage, key, f.Kind)
		if _, _, err := r.p.RecordScrubFinding(r.ctx, tenant.ScrubFindingRecord{Fence: w.Fence(), Finding: f, Now: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := r.p.CancelJob(r.ctx, old.ID, time.Now(), r.audit(), nil); err != nil {
		t.Fatal(err)
	}
	if got := r.findings(false); len(got) != 2 {
		t.Fatalf("before the scrub the findings are %v; want the two orphans of the scan gone", got)
	}
	r.scrubAll(r.config())
	if got := r.findings(false); len(got) != 0 {
		t.Errorf("after a walk found their objects gone the findings are %v; want none", got)
	}
}
