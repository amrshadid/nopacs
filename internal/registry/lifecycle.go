package registry

import (
	"context"
	"strconv"
	"sync"
	"time"

	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
)

// Start reads every tenant from the master, opens each in parallel (at most
// MaxOpening at once, each under its own deadline), waits until each is
// ready, unreachable or failed, then checks every tenant each Interval until
// Close. It fails only when the master cannot list the tenants, or when it
// was called before.
func (r *Registry) Start(ctx context.Context) error {
	if err := r.enter(); err != nil {
		return err
	}
	defer r.wg.Done()
	r.mu.Lock()
	if r.started {
		r.mu.Unlock()
		return fault.New(fault.Internal, "registry: started twice")
	}
	r.started = true
	r.mu.Unlock()

	entries, err := r.registerAll(ctx)
	if err != nil {
		return err
	}
	r.warnFileLimit(entries)
	var wg sync.WaitGroup
	for _, e := range entries {
		wg.Add(1)
		r.goOwned("start", func() {
			defer wg.Done()
			e.op.Lock()
			defer e.op.Unlock()
			r.refresh(e, true)
		})
	}
	wg.Wait()

	r.goOwned("watch", r.watch)
	return nil
}

// listAll reads every tenant from the master, a page at a time.
func (r *Registry) listAll(ctx context.Context) ([]master.Tenant, error) {
	var rows []master.Tenant
	page := master.Page{Limit: master.MaxPageLimit}
	for {
		batch, next, err := r.cfg.Master.ListTenants(ctx, page)
		if err != nil {
			return nil, err
		}
		rows = append(rows, batch...)
		if next == "" {
			return rows, nil
		}
		page.Cursor = next
	}
}

// registerAll reads every tenant from the master and registers it, with no
// Create or Remove running between the two.
func (r *Registry) registerAll(ctx context.Context) ([]*entry, error) {
	r.listing.Lock()
	defer r.listing.Unlock()
	rows, err := r.listAll(ctx)
	if err != nil {
		return nil, err
	}
	entries := make([]*entry, 0, len(rows))
	for _, row := range rows {
		if e := r.register(row); e != nil {
			entries = append(entries, e)
		}
	}
	return entries, nil
}

// watch checks every tenant, and reconciles the registry with the master,
// each Interval until the registry closes.
func (r *Registry) watch() {
	t := time.NewTicker(r.cfg.Interval)
	defer t.Stop()
	for {
		select {
		case <-r.ctx.Done():
			return
		case <-t.C:
			r.checkAll()
			r.goOwned("reconcile", r.reconcile)
		}
	}
}

// checkAll starts one check of every registered tenant and does not wait
// for them: a tenant whose open hangs until its deadline delays no other
// tenant's check, this round or the next. It returns a group a test may wait
// on; the timer does not.
func (r *Registry) checkAll() *sync.WaitGroup {
	var wg sync.WaitGroup
	for _, e := range *r.entries.Load() {
		wg.Add(1)
		r.goOwned("check", func() {
			defer wg.Done()
			r.check(e)
		})
	}
	return &wg
}

// check is one timer check of e: a ping when its store is open, an open
// when it has none, nothing when it is failed. An entry whose lock is held
// (an open, a check, a creation or a removal still running) is skipped.
func (r *Registry) check(e *entry) {
	if !e.op.TryLock() {
		return
	}
	defer e.op.Unlock()
	r.refresh(e, false)
}

// reconcile brings the registry to the master's list of tenants: an entry
// whose row is gone, or belongs to another id, is unregistered and closed;
// a row the registry does not know is registered and opened. One runs at a
// time; one that finds another running does nothing.
func (r *Registry) reconcile() {
	if !r.reconciling.CompareAndSwap(false, true) {
		return
	}
	defer r.reconciling.Store(false)
	ctx, cancel := context.WithTimeout(r.ctx, r.cfg.OpenTimeout)
	defer cancel()

	r.listing.Lock()
	rows, err := r.listAll(ctx)
	if err != nil {
		r.listing.Unlock()
		r.log.Warn("the tenants cannot be reconciled with the master", "kind", fault.KindOf(err).String(), "message", fault.Message(err))
		return
	}
	byName := make(map[string]master.Tenant, len(rows))
	for _, row := range rows {
		byName[row.Name] = row
	}
	var gone, fresh []*entry
	for name, e := range *r.entries.Load() {
		if row, ok := byName[name]; !ok || row.ID != e.id {
			r.unregister(e)
			gone = append(gone, e)
		}
	}
	known := *r.entries.Load()
	for _, row := range rows {
		if _, ok := known[row.Name]; !ok {
			if e := r.register(row); e != nil {
				fresh = append(fresh, e)
			}
		}
	}
	r.listing.Unlock()

	for _, e := range gone {
		r.log.Info("tenant unregistered: the master no longer has it", "tenant", e.name)
		r.retire(e)
	}
	for _, e := range fresh {
		r.log.Info("tenant registered: the master has it", "tenant", e.name)
		r.goOwned("open", func() {
			e.op.Lock()
			defer e.op.Unlock()
			r.refresh(e, true)
		})
	}
}

// register adds an entry for row unless one with its name exists, and
// returns the entry for row's id, or nil when the name belongs to another
// id or the registry is closed.
func (r *Registry) register(row master.Tenant) *entry {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed.Load() {
		return nil
	}
	old := *r.entries.Load()
	if e, ok := old[row.Name]; ok {
		if e.id == row.ID {
			return e
		}
		return nil
	}
	e := &entry{id: row.ID, name: row.Name, driver: row.Driver, sealed: row.SealedAddress,
		written: row.State, writtenReason: row.StateReason}
	if row.State.ReadOnly() {
		e.readOnly, e.readOnlyReason = row.State, row.StateReason
	}
	e.ctx, e.cancel = context.WithCancel(r.ctx)
	// Until its first open the tenant is shown as being opened, with no
	// store, so it is refused rather than served from nothing.
	e.view.Store(&view{state: master.StateUpgrading})
	next := make(map[string]*entry, len(old)+1)
	for k, v := range old {
		next[k] = v
	}
	next[row.Name] = e
	r.entries.Store(&next)
	return e
}

// unregister removes the entry named name when it is e, so a removal never
// takes away an entry a later creation of the same name made.
func (r *Registry) unregister(e *entry) {
	if r.drop(e) && r.cfg.Unregistered != nil {
		r.cfg.Unregistered(e.name)
	}
}

// drop takes e out of the registry's entries, and reports whether it was
// there.
func (r *Registry) drop(e *entry) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	old := *r.entries.Load()
	if old[e.name] != e {
		return false
	}
	next := make(map[string]*entry, len(old))
	for k, v := range old {
		if k != e.name {
			next[k] = v
		}
	}
	r.entries.Store(&next)
	return true
}

// retire ends an entry already unregistered: it ends the entry's open in
// progress, waits for its lock, and closes its store.
func (r *Registry) retire(e *entry) {
	e.cancel()
	e.op.Lock()
	defer e.op.Unlock()
	r.retireLocked(e)
}

// retireLocked is retire for a caller that holds e.op.
func (r *Registry) retireLocked(e *entry) {
	e.removed = true
	e.view.Store(&view{state: master.StateUnreachable, reason: "the tenant was removed", since: r.cfg.Now()})
	if err := e.closeStore(); err != nil {
		r.log.Warn("a removed tenant's database did not close cleanly", "tenant", e.name, "message", fault.Message(err))
	}
}

// retireName unregisters and closes the entry named name, whatever its id:
// the master has no tenant of that name.
func (r *Registry) retireName(name string) {
	e := (*r.entries.Load())[name]
	if e == nil {
		return
	}
	r.unregister(e)
	r.retire(e)
}

// Close stops the timer, ends every open in progress and waits for it, and
// for Start, Create and Remove calls in progress, then closes every store.
// It waits for driver calls no longer than ctx allows: a driver that ignores
// its context is abandoned, and Close logs and returns an error saying how
// many calls it left running. Calls made after Close fail as Unavailable. A
// second Close does nothing.
func (r *Registry) Close(ctx context.Context) error {
	r.mu.Lock()
	if r.closed.Swap(true) {
		r.mu.Unlock()
		return nil
	}
	m := *r.entries.Load()
	empty := map[string]*entry{}
	r.entries.Store(&empty)
	r.mu.Unlock()

	r.cancel()
	// The waiting goroutine is the one left running when Close gives up;
	// it ends as soon as the abandoned driver calls return.
	waited := make(chan struct{})
	go func() {
		r.wg.Wait()
		close(waited)
	}()
	var abandoned error
	select {
	case <-waited:
	case <-ctx.Done():
		n := r.inflight.Load()
		r.log.Error("the registry closed without waiting for driver calls that ignore their deadline", "abandoned", n)
		abandoned = fault.New(fault.Internal, "the registry closed with "+strconv.FormatInt(n, 10)+" driver calls still running; they were abandoned")
	}
	err := r.closeAll(m)
	if abandoned != nil {
		return abandoned
	}
	return err
}

// closeAll closes the store of every entry of m, MaxOpening at a time.
// Closing a SQLite store checkpoints its log and syncs it, which is most of
// a millisecond to tens of them; 1,000 of them one by one took 12 seconds on
// a laptop (TestScaleThousandTenants), so they are closed as they were opened.
func (r *Registry) closeAll(m map[string]*entry) error {
	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		first error
	)
	work := make(chan *entry)
	for range min(r.cfg.MaxOpening, len(m)) {
		wg.Go(func() {
			defer r.recoverLog("close")
			for e := range work {
				e.op.Lock()
				e.removed = true
				err := e.closeStore()
				e.op.Unlock()
				if err != nil {
					mu.Lock()
					if first == nil {
						first = err
					}
					mu.Unlock()
				}
			}
		})
	}
	for _, e := range m {
		work <- e
	}
	close(work)
	wg.Wait()
	return first
}

// closeStore closes the entry's store and forgets it. The caller holds op.
func (e *entry) closeStore() error {
	s := e.store
	e.store = nil
	if s == nil {
		return nil
	}
	return s.Close()
}
