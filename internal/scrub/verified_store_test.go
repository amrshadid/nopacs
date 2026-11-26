package scrub

import (
	"math"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/tenant"
)

// TestCoverageFromTheTenantsDatabase guards a storage's coverage computed
// from what the tenant's database records of a real scrub: every object of
// the primary read whole is verified; a replica holds its primary's scans,
// so its objects count all of them, and a scan still pending on it, not
// read there, is not verified; nothing is overdue within the interval; and
// a page of the database's answer is cut at the bound asked.
func TestCoverageFromTheTenantsDatabase(t *testing.T) {
	r := newReplicaRig(t)
	r.archiveTwo("s1", "1.2.826.0.1.3680043.10.531")
	r.addReplica("copy")
	r.copyAll()
	r.archiveTwo("s2", "1.2.826.0.1.3680043.10.532") // pending on the replica
	cfg := r.config()
	cfg.Verifications = func(tenant.Store) tenant.VerificationRecorder { return r.p }
	r.scrubAll(cfg)

	now := time.Now()
	day := 24 * time.Hour
	main, err := Coverage(r.ctx, r.p, "main", 30*day, now)
	if err != nil {
		t.Fatal(err)
	}
	if main.Objects != 4 || main.Verified != 4 || main.Share != 1 || main.Overdue != 0 || main.OldestUnverified != 0 {
		t.Errorf("the primary's coverage is %+v; want 4 objects, all verified", main)
	}
	copyCov, err := Coverage(r.ctx, r.p, "copy", 30*day, now)
	if err != nil {
		t.Fatal(err)
	}
	if copyCov.Objects != 4 || copyCov.Verified != 2 || math.Abs(copyCov.Share-0.5) > 1e-9 || copyCov.Overdue != 0 {
		t.Errorf("the replica's coverage is %+v; want 4 objects, 2 verified, none overdue", copyCov)
	}
	page, next, err := r.p.ScanVerifications(r.ctx, "copy", "", 1)
	if err != nil || len(page) != 1 || next == "" {
		t.Fatalf("a page of one = %+v, %q, %v; want one scan and a cursor", page, next, err)
	}
	rest, end, err := r.p.ScanVerifications(r.ctx, "copy", next, 10)
	if err != nil || len(rest) != 1 || end != "" {
		t.Errorf("the page after = %+v, %q, %v; want the other scan and no cursor", rest, end, err)
	}
	if _, _, err := r.p.ScanVerifications(r.ctx, "nowhere", "", 10); err == nil {
		t.Error("the verifications of a storage the tenant does not have answered no error")
	}
}
