package postgres

import (
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/tenant"
)

// TestCommitScanUpdatesScanInPlace guards that a commit adding instances to
// a scan that exists changes the scan's row without writing again an entry
// in each of its indexes: Postgres can only do that (a heap-only update)
// when no indexed column changes, and a scan's instance count changes with
// every commit. An index that held it (the one that tells whether a storage
// holds a scan with instances) made every one of 200 single-instance
// commits write all nine indexes of scans again.
func TestCommitScanUpdatesScanInPlace(t *testing.T) {
	e := newCenv(t, 1, 4)
	e.storage("store")
	e.receiver("a", "store", 11112)
	const commits = 200
	r, fs := e.identified("a", cUID(1, 0), "store", "sub-01", "ses-1", func(r tenant.Reception) []tenant.ReceivedFile {
		out := make([]tenant.ReceivedFile, commits)
		for i := range out {
			out[i] = cFile(r, 0, i)
		}
		return out
	})
	claims := make([]tenant.InstanceClaim, len(fs))
	for i, f := range fs {
		claims[i] = claimOfFile(f)
	}
	_, err := e.p().ClaimInstances(e.ctx, r.ID, claims, cT0, nil)
	e.ok("claim", err)
	for i := range commits {
		_, err := e.p().CommitScan(e.ctx, scanCommit(r, "sub-01", "ses-1", "scan-1", "store", fs[i:i+1]))
		e.ok("commit", err)
	}
	// The statistics views are filled by each backend when it goes idle, so
	// the counts are read until they hold every update or time runs out.
	var upd, hot int64
	for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline); time.Sleep(250 * time.Millisecond) {
		if err := e.db.QueryRow(e.ctx, `SELECT n_tup_upd, n_tup_hot_upd FROM pg_stat_user_tables WHERE relid = 'scans'::regclass`).Scan(&upd, &hot); err != nil {
			t.Fatal(err)
		}
		if upd >= commits-1 {
			break
		}
	}
	t.Logf("%d updates of scans, %d of them heap-only", upd, hot)
	if upd < commits-1 {
		t.Fatalf("only %d updates of scans were counted for %d commits", upd, commits)
	}
	if hot*10 < upd*9 {
		t.Errorf("%d of %d updates of scans were heap-only, want at least nine in ten", hot, upd)
	}
}
