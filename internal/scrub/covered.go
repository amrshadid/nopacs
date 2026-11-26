package scrub

import (
	"cmp"
	"context"
	"slices"
	"sync"
	"time"

	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// TenantCoverage is one storage's coverage (Coverage) as the last scrub
// of it to end left it, with the tenant it belongs to.
type TenantCoverage struct {
	// Tenant names the tenant.
	Tenant string
	// StorageCoverage is the storage's coverage then.
	StorageCoverage
	// At is when it was computed.
	At time.Time
}

// coverageTimeout bounds the reading of one scrub's coverage: a page per
// thousand scans of each storage it read.
const coverageTimeout = 2 * time.Minute

// coverages holds the coverage of each storage a scrub of this process
// ended on, for the server's metrics: a scrape reads it without touching a
// database. A restart forgets it until the next scrub of each storage
// ends.
type coverages struct {
	mu sync.Mutex
	by map[[2]string]TenantCoverage
}

// set keeps c as the tenant's storage's coverage.
func (c *coverages) set(v TenantCoverage) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.by == nil {
		c.by = map[[2]string]TenantCoverage{}
	}
	c.by[[2]string{v.Tenant, v.Storage}] = v
}

// forget drops what is kept of the tenant named name.
func (c *coverages) forget(name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for k := range c.by {
		if k[0] == name {
			delete(c.by, k)
		}
	}
}

// Coverages returns the coverage of each storage as the last scrub of it
// that ended in this process left it, by tenant then storage: what the
// server's metrics show of how much of each storage a scrub verified within
// scrub.interval.
func (s *Supervisor) Coverages() []TenantCoverage {
	s.covered.mu.Lock()
	out := make([]TenantCoverage, 0, len(s.covered.by))
	for _, v := range s.covered.by {
		out = append(out, v)
	}
	s.covered.mu.Unlock()
	slices.SortFunc(out, func(a, b TenantCoverage) int {
		return cmp.Or(cmp.Compare(a.Tenant, b.Tenant), cmp.Compare(a.Storage, b.Storage))
	})
	return out
}

// recordCoverage computes and keeps the coverage of the storages the ended
// job read (job.Storage, or every storage of the tenant for a scrub of the
// whole tenant), over scrub.interval as it applies now. A tenant database
// that keeps no verifications has no coverage to tell; a read that fails is
// logged and leaves what was kept, the next scrub's end trying again.
func (u *unitRun) recordCoverage(st tenant.Store, job tenant.Job) {
	if u.sup.cfg.Verifications == nil || st == nil {
		return
	}
	src, ok := u.sup.cfg.Verifications(st).(tenant.VerificationSource)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(u.ctx), coverageTimeout)
	defer cancel()
	interval, err := u.sup.cfg.TenantSettings(st).ScrubInterval(ctx)
	if err != nil {
		u.log.Warn("scrubs: scrub.interval could not be read; the storage's coverage is not computed now", "reason", fault.Message(err))
		return
	}
	names := []string{job.Storage}
	if job.Storage == "" {
		lister := &unitRun{t: u.t, sup: u.sup, p: u.p, ctx: ctx}
		if names, err = lister.storageNames(); err != nil {
			u.log.Warn("scrubs: the tenant's storages could not be listed; their coverage is not computed now", "reason", fault.Message(err))
			return
		}
	}
	for _, name := range names {
		now := u.sup.cfg.Now()
		cv, err := Coverage(ctx, src, name, interval, now)
		if err != nil {
			u.log.Warn("scrubs: a storage's coverage could not be computed", "storage", name, "reason", fault.Message(err))
			continue
		}
		u.sup.covered.set(TenantCoverage{Tenant: u.t.name, StorageCoverage: cv, At: now})
	}
}
