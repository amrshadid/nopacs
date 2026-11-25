package rebuild

import (
	"sync/atomic"
	"testing"

	"github.com/amrshadid/nopacs/internal/tenant"
)

// TestRebuildReadsNoReplica guards that a rebuild makes the catalog from the
// primaries alone: a replica holds its primary's keys, so read too it would
// make every scan one series on two storages, and read first (its name
// sorts before its primary's) it would name the replica as where each
// scan's files are. Here the replica answers what its primary holds, as an
// in-step replica does, and the compare still finds the catalog equal.
func TestRebuildReadsNoReplica(t *testing.T) {
	drivers(t, func(t *testing.T, postgres bool) {
		r := newRig(t, postgres)
		r.archive(studyFiles...)
		if _, err := r.p.AddStorage(r.ctx, tenant.Storage{Name: "a-copy", Type: tenant.StorageDirectory,
			Directory: tenant.DirectoryStorage{Root: "replicas"}, ReplicaOf: "main"}, r.clock.Now(), r.audit(), nil); err != nil {
			t.Fatal(err)
		}
		before := r.catalogText()
		totals, err := r.p.CatalogTotals(r.ctx)
		if err != nil {
			t.Fatal(err)
		}
		var writes atomic.Int64
		j, tallies := r.run(r.config(&writes), tenant.RebuildCompare)
		if j.State != tenant.JobDone || j.Counts.InstancesSent != totals.Instances || j.Counts.Scans != totals.Scans {
			t.Errorf("the compare with a replica ended %+v, the catalog holds %+v", j, totals)
		}
		if len(tallies) != 0 {
			t.Errorf("the compare with a replica tallied %v, want nothing", tallies)
		}
		if got := r.catalogText(); got != before {
			t.Error("a compare changed the catalog in use")
		}
	})
}
