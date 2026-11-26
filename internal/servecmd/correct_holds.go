package servecmd

import (
	"context"
	"sync"

	"github.com/amrshadid/nopacs/internal/holds"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// purgeAdoptions keeps, per tenant, whether the holds found on its
// storages were adopted for the corrections' purge pass
// (correct.EnginesConfig.HoldsAdopted) since this start and since the
// catalog's last activation, as retention keeps it for its disposals: a
// database lost and made again, or restored from before a hold was placed,
// has no row of that hold until its record is adopted, and a committed
// delete past its grace purged before then would remove what the hold
// keeps. The hold-record worker adopts the same records at the tenant's
// start; whichever reads them first adopts, and adopting a hold adopted
// already changes nothing.
type purgeAdoptions struct {
	work *tenantWork

	mu sync.Mutex
	by map[string]*purgeAdoption
}

// purgeAdoption is one tenant's adoption mark, read and set under mu, so
// two passes never adopt at once.
type purgeAdoption struct {
	mu      sync.Mutex
	adopted holds.Adopted
}

// of returns the tenant's adoption mark.
func (p *purgeAdoptions) of(name string) *purgeAdoption {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.by == nil {
		p.by = map[string]*purgeAdoption{}
	}
	a := p.by[name]
	if a == nil {
		a = &purgeAdoption{}
		p.by[name] = a
	}
	return a
}

// check answers nil once the holds found on the storages of the tenant
// named are adopted for the catalog st is at now, adopting them first when
// that is owed (holds.Writer.AdoptFound). A storage that cannot be read is
// the error, and the pass purges nothing of the tenant until every
// storage was read.
func (p *purgeAdoptions) check(ctx context.Context, name string, st tenant.Store) error {
	db, err := dbOf(st)
	if err != nil {
		return err
	}
	a := p.of(name)
	a.mu.Lock()
	defer a.mu.Unlock()
	owed, mark, err := a.adopted.Owed(ctx, db)
	if err != nil || !owed {
		return err
	}
	hw, err := p.work.holdWriterOver(ctx, name, db, db)
	if err != nil {
		return err
	}
	found, err := hw.AdoptFound(ctx)
	for _, id := range found.Adopted {
		p.work.log.Warn("holds: a hold record was found active with no row before the next purge; the hold holds again", "tenant", name,
			"hold", id)
	}
	if err != nil {
		return err
	}
	a.adopted.Done(mark)
	return nil
}
