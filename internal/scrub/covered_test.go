package scrub

import (
	"math"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/tenant"
)

// TestEndedScrubKeepsItsStoragesCoverage guards the coverage the server's
// metrics read: none is kept before a scrub ends; once a scrub of the whole
// tenant ends, the coverage of each storage it read is kept by tenant and
// storage (the primary wholly verified, the replica half, its other scan
// pending there); and a tenant removed drops what was kept of it.
func TestEndedScrubKeepsItsStoragesCoverage(t *testing.T) {
	r := newReplicaRig(t)
	r.archiveTwo("s1", "1.2.826.0.1.3680043.10.541")
	r.addReplica("copy")
	r.copyAll()
	r.archiveTwo("s2", "1.2.826.0.1.3680043.10.542") // pending on the replica
	cfg := r.config()
	cfg.Verifications = func(tenant.Store) tenant.VerificationRecorder { return r.p }
	s := r.start(cfg)
	if got := s.Coverages(); len(got) != 0 {
		t.Fatalf("before any scrub ended the coverages are %+v; want none", got)
	}
	r.waitEnded(s, r.createScrub("").ID)
	// The job reads ended as its last unit's write commits; the unit keeps
	// the coverage just after.
	got := s.Coverages()
	for deadline := time.Now().Add(10 * time.Second); len(got) < 2 && time.Now().Before(deadline); got = s.Coverages() {
		time.Sleep(10 * time.Millisecond)
	}
	if len(got) != 2 || got[0].Tenant != "lab" || got[0].Storage != "copy" || got[1].Storage != "main" {
		t.Fatalf("after the scrub the coverages are %+v; want lab's copy and main", got)
	}
	if c := got[0]; c.Objects != 4 || c.Verified != 2 || math.Abs(c.Share-0.5) > 1e-9 || c.Overdue != 0 || c.At.IsZero() {
		t.Errorf("the replica's kept coverage is %+v; want 4 objects, 2 verified", c)
	}
	if c := got[1]; c.Objects != 4 || c.Verified != 4 || c.Share != 1 || c.OldestUnverified != 0 {
		t.Errorf("the primary's kept coverage is %+v; want 4 objects, all verified", c)
	}
	s.TenantRemoved("lab")
	if got := s.Coverages(); len(got) != 0 {
		t.Errorf("after the tenant was removed the coverages are %+v; want none", got)
	}
}
