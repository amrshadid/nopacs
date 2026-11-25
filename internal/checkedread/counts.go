package checkedread

import (
	"cmp"
	"errors"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/fault"
)

// Counts counts, per tenant and replica, the reads of archived objects a
// replica served because the primary could not, and the reads of a replica
// that found its bytes not to be the catalog's, for the server's metrics.
// Only a read that turned from its primary touches it, so the reads the
// primaries serve cost nothing here. Its zero value is ready to use, and a
// nil *Counts counts nothing. It is safe for concurrent use.
//
// With Log set it also warns of a read a replica served because its
// primary could not, at most once per WarnEvery for each tenant and
// primary storage: a primary that answers not found for everything (a
// directory not mounted) would otherwise fail over unseen, and one warning
// a minute says so without a line per read. The limit is kept here, in
// the one Counts the server shares, because a Stores lives for one request.
type Counts struct {
	// Log, when set, takes the warnings; nil logs nothing.
	Log *slog.Logger

	mu            sync.Mutex
	served, wrong map[countKey]uint64
	// warned is when each primary storage was last warned of.
	warned map[countKey]time.Time
	seams  countsSeams
}

// WarnEvery is the least time between two warnings of reads turned from
// the same primary storage.
const WarnEvery = time.Minute

// countsSeams are what a test changes in Counts.
type countsSeams struct {
	// now, when set, is the clock of the warnings' limit.
	now func() time.Time
}

// warnTurned warns, unless one was given within WarnEvery, that a read of
// the tenant's primary storage turned to a replica after the primary
// failed with err. It names the tenant, the storage and the kind of the
// failure, never a key or an identifier of the archive's.
func (c *Counts) warnTurned(tenantName, storage string, err error) {
	if c == nil || c.Log == nil {
		return
	}
	now := time.Now
	if c.seams.now != nil {
		now = c.seams.now
	}
	at := now()
	k := countKey{tenantName, storage}
	c.mu.Lock()
	last, seen := c.warned[k]
	if seen && at.Sub(last) < WarnEvery {
		c.mu.Unlock()
		return
	}
	if c.warned == nil {
		c.warned = map[countKey]time.Time{}
	}
	c.warned[k] = at
	c.mu.Unlock()
	kind := fault.KindOf(err).String()
	switch {
	case errors.Is(err, blob.ErrNotOnline):
		kind = "not online"
	case StorageFailed(err):
		kind = "storage cannot be opened: " + kind
	}
	c.Log.Warn("checkedread: reads of a primary storage are served by a replica; the primary cannot serve them",
		"tenant", tenantName, "storage", storage, "fault", kind)
}

// countKey is a tenant and one of its storages.
type countKey struct{ tenant, storage string }

// Count is one count of Counts: a tenant, one of its replicas, and how
// many.
type Count struct {
	Tenant, Storage string
	Value           uint64
}

// ReplicaReads returns the reads each replica served, by tenant then
// replica.
func (c *Counts) ReplicaReads() []Count {
	return c.list(func(c *Counts) map[countKey]uint64 { return c.served })
}

// ReplicaMismatches returns the reads of each replica whose bytes were not
// the catalog's, by tenant then replica.
func (c *Counts) ReplicaMismatches() []Count {
	return c.list(func(c *Counts) map[countKey]uint64 { return c.wrong })
}

// addServed counts a read the replica served.
func (c *Counts) addServed(tenantName, storage string) {
	c.add(func(c *Counts) *map[countKey]uint64 { return &c.served }, tenantName, storage)
}

// addWrong counts a read of the replica that found wrong bytes.
func (c *Counts) addWrong(tenantName, storage string) {
	c.add(func(c *Counts) *map[countKey]uint64 { return &c.wrong }, tenantName, storage)
}

// add counts one in the map pick names, made on first use.
func (c *Counts) add(pick func(*Counts) *map[countKey]uint64, tenantName, storage string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	m := pick(c)
	if *m == nil {
		*m = map[countKey]uint64{}
	}
	(*m)[countKey{tenantName, storage}]++
}

// list returns the counts of the map of picks, sorted.
func (c *Counts) list(pick func(*Counts) map[countKey]uint64) []Count {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	m := pick(c)
	out := make([]Count, 0, len(m))
	for k, v := range m {
		out = append(out, Count{Tenant: k.tenant, Storage: k.storage, Value: v})
	}
	slices.SortFunc(out, func(a, b Count) int {
		return cmp.Or(cmp.Compare(a.Tenant, b.Tenant), cmp.Compare(a.Storage, b.Storage))
	})
	return out
}
