package servecmd

import (
	"github.com/amrshadid/nopacs/internal/checkedread"
	"github.com/amrshadid/nopacs/internal/obs"
)

// The metrics of the reads replicas serve. Each sample is labeled with the
// tenant and the replica: their names are the installation's own, bounded
// by the storages its admins made (at most two replicas a primary).
const (
	metricReplicaReads      = "nopacs_reads_served_by_replica_total"
	metricReplicaMismatches = "nopacs_replica_read_mismatches_total"
)

// readCounters returns the reads replicas served because a primary could
// not (its object marked damaged, missing or not online, or its storage not
// answering), and the reads of a replica whose bytes were not the
// catalog's, as metrics, one sample a tenant and replica. A metric with no
// sample yet is left out until its first.
func readCounters(c *checkedread.Counts) []obs.Counter {
	var out []obs.Counter
	add := func(name, help string, counts []checkedread.Count) {
		for _, n := range counts {
			out = append(out, obs.Counter{Name: name, Help: help, Value: n.Value, Labels: []string{"tenant", n.Tenant, "storage", n.Storage}})
		}
	}
	add(metricReplicaReads, "Reads of archived instances a replica served because the primary's object was marked damaged, missing or not online, or its storage did not answer.", c.ReplicaReads())
	add(metricReplicaMismatches, "Reads of a replica whose bytes were not the catalog's; each is recorded as the replica's damage and copied again.", c.ReplicaMismatches())
	return out
}
