package checkedread

import (
	"time"

	"github.com/amrshadid/nopacs/internal/blob"
)

// WrapStores makes s wrap each storage it opens with wrap, so a test can
// make one misbehave.
func WrapStores(s *Stores, wrap func(name string, st blob.Store) blob.Store) { s.seams.wrap = wrap }

// UseReplicas makes s ask r, rather than its pipeline, of the replicas.
func UseReplicas(s *Stores, r Replicas) { s.replicas = r }

// CountsClock makes c read the time of its warnings' limit from now.
func CountsClock(c *Counts, now func() time.Time) { c.seams.now = now }
