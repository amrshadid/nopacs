package servecmd

import (
	"context"
	"sync"
	"time"

	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/clock"
	"github.com/amrshadid/nopacs/internal/correct"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/place"
	"github.com/amrshadid/nopacs/internal/receive"
	"github.com/amrshadid/nopacs/internal/storageswitch"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// switchClock is the switches' clock (storageswitch.Clock): the server's
// clock for every time written, the machine's timers for every wait.
type switchClock struct{}

// Now returns the server's clock.
func (switchClock) Now() time.Time { return clock.Now() }

// After returns time.After(d).
func (switchClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

// ownerLocks holds, per tenant, the one lock under which the owner records
// of its storages are advanced: by an export or a reclaim
// (backup.OwnerCheck.Advance, Reclaim) and by a switch taking the record on
// its replica's place. Each reads the generation the database holds, then
// writes the database and the records one past it; two at once would read
// the same generation, and the one that writes second leaves a record the
// other's database row does not name, which the next check of the owner
// records fences the tenant for.
type ownerLocks struct {
	mu    sync.Mutex
	locks map[string]chan struct{}
}

// of returns the tenant's lock, a channel of one slot: taken by a send,
// let go by a receive, and waited on with a context.
func (o *ownerLocks) of(name string) chan struct{} {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.locks == nil {
		o.locks = map[string]chan struct{}{}
	}
	l, ok := o.locks[name]
	if !ok {
		l = make(chan struct{}, 1)
		o.locks[name] = l
	}
	return l
}

// lock takes the tenant's lock, waiting until it is free or ctx ends, and
// returns its release, which lets it go once however often it is called.
func (o *ownerLocks) lock(ctx context.Context, name string) (func(), error) {
	l := o.of(name)
	select {
	case l <- struct{}{}:
		return sync.OnceFunc(func() { <-l }), nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// ownerSerialDB is the switch's view of the tenant's database whose reading
// of the owner generations takes the tenant's owner lock: a switch reads
// them (StorageOwners) at the start of its owner step, then opens the
// replica's place (ownerSerialPlace), writes the database and the record,
// and closes the place, which lets the lock go. A failure before the place
// is open lets it go at once. The owner step is the only reader of
// StorageOwners and the only caller of OpenPlace in a switch.
type ownerSerialDB struct {
	storageswitch.DB
	held *heldOwnerLock
}

// heldOwnerLock is the owner lock a switch's owner step holds between its
// read of the generations and the close of the replica's place.
type heldOwnerLock struct {
	locks *ownerLocks
	name  string

	mu      sync.Mutex
	release func()
}

// take takes the tenant's owner lock for one owner step. A step that took
// it and ended without opening the place (a failed open) has let it go.
func (h *heldOwnerLock) take(ctx context.Context) error {
	release, err := h.locks.lock(ctx, h.name)
	if err != nil {
		return err
	}
	h.mu.Lock()
	h.release = release
	h.mu.Unlock()
	return nil
}

// let lets the lock go, when it is held.
func (h *heldOwnerLock) let() {
	h.mu.Lock()
	r := h.release
	h.release = nil
	h.mu.Unlock()
	if r != nil {
		r()
	}
}

// StorageOwners takes the owner lock, then reads the generations; a read
// that fails lets it go.
func (d ownerSerialDB) StorageOwners(ctx context.Context) ([]tenant.StorageOwner, error) {
	if err := d.held.take(ctx); err != nil {
		return nil, err
	}
	out, err := d.DB.StorageOwners(ctx)
	if err != nil {
		d.held.let()
	}
	return out, err
}

// ownerSerialPlace returns the switch's opener of the replica's place: open
// failing lets the owner lock go; the store it opens lets it go when
// closed.
func ownerSerialPlace(open storageswitch.OpenPlace, held *heldOwnerLock) storageswitch.OpenPlace {
	return func(ctx context.Context, row tenant.Storage) (blob.Store, error) {
		bs, err := open(ctx, row)
		if err != nil {
			held.let()
			return nil, err
		}
		return &lockedPlace{Store: bs, held: held}, nil
	}
}

// lockedPlace is the replica's place a switch writes its owner record to,
// which lets the owner lock go when it is closed. The owner step reads and
// puts one object on it, so it needs no more of the store than Store.
type lockedPlace struct {
	blob.Store
	held *heldOwnerLock
}

// Close closes the store, then lets the lock go.
func (p *lockedPlace) Close() error {
	err := p.Store.Close()
	p.held.let()
	return err
}

// Switcher returns the switcher of the tenant named, over its database as
// the registry answers it now: what a switch request is run through.
func (w *tenantWork) Switcher(ctx context.Context, name string) (*storageswitch.Switcher, error) {
	st, err := w.reg.Store(name)
	if err != nil {
		return nil, err
	}
	db, err := dbOf(st)
	if err != nil {
		return nil, err
	}
	return w.switcherFor(ctx, name, db)
}

// switcherFor returns the switcher of the tenant named over st, making it
// when the tenant has none over st, and ending, before it answers, the
// switches the last server left unfinished (storageswitch.Switcher.Recover).
// It is asked before the tenant's reception pipeline starts, whose writes
// ask its gate, so no receiver takes a file before a switch left accepted
// is refused; the switcher over a database replaced is stopped.
//
// The switcher replaced is stopped with the create lock let go: its run may
// be inside WriteRecordsOwed, which makes the tenant's corrections engine,
// whose configuration asks switcherFor (correctionWrites); stopped under
// the lock, the stop would wait on its own run until its deadline, and the
// switch would be refused for it.
func (w *tenantWork) switcherFor(ctx context.Context, name string, st tenantDB) (*storageswitch.Switcher, error) {
	w.create.Lock()
	unlocked := false
	defer func() {
		if !unlocked {
			w.create.Unlock()
		}
	}()
	for {
		w.mu.Lock()
		cur := w.switchers[name]
		stopped := w.stopped
		if cur != nil && cur.store != st {
			delete(w.switchers, name)
		}
		w.mu.Unlock()
		if stopped {
			return nil, fault.New(fault.Unavailable, "the server is stopping; try again once it has started")
		}
		if cur == nil {
			break
		}
		if cur.store == st {
			return cur.sw, nil
		}
		w.create.Unlock()
		unlocked = true
		sctx, cancel := context.WithTimeout(ctx, tenantWorkStop)
		_ = cur.sw.Stop(sctx)
		cancel()
		w.create.Lock()
		unlocked = false
	}
	id, err := w.tenantID(ctx, name)
	if err != nil {
		return nil, err
	}
	t := place.TenantOf(id, name, st)
	held := &heldOwnerLock{locks: w.owners, name: name}
	sw, err := storageswitch.New(storageswitch.Config{
		DB:           ownerSerialDB{DB: st, held: held},
		Tenant:       t,
		Installation: w.installation,
		OwnerKeys:    w.d.Keys,
		OpenPlace: ownerSerialPlace(func(ctx context.Context, row tenant.Storage) (blob.Store, error) {
			return w.opener.OpenBeside(ctx, t, row)
		}, held),
		Reseal: func(ctx context.Context, primary, replica tenant.Storage) ([]byte, []byte, error) {
			return place.SwapSecrets(ctx, w.d.Keys, t, primary, replica)
		},
		ConfirmKey:       w.confirmKey,
		Wake:             func() { w.WakeReplicas(name) },
		WriteRecordsOwed: func(ctx context.Context, _ string) error { return w.writeRecordsOwed(ctx, name, st) },
		Clock:            switchClock{},
		Log:              w.log,
	})
	if err != nil {
		return nil, err
	}
	if err := sw.Recover(ctx); err != nil {
		_ = sw.Stop(ctx)
		return nil, err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.stopped {
		_ = sw.Stop(ctx)
		return nil, fault.New(fault.Unavailable, "the server is stopping; try again once it has started")
	}
	w.switchers[name] = &tenantSwitcher{store: st, sw: sw}
	return sw, nil
}

// receptionWrites returns what the reception pipeline of the tenant named is
// given of its background work: the gate of its switcher, which its every
// write to a primary asks first (receive.Config.Pause), and the call after
// each commit (receive.Config.Wake), which wakes the tenant's event streams
// (wake) and its replicator, since a commit queues its scan for the
// replicas. The switcher is made, ending the switches a stopped server
// left, before it answers, so no file is taken before then.
func (w *tenantWork) receptionWrites(ctx context.Context, name string, st tenantDB, wake func(string)) (receive.WritePause, func(), error) {
	sw, err := w.switcherFor(ctx, name, st)
	if err != nil {
		return nil, nil, err
	}
	return sw.Gate(), func() {
		wake(name)
		w.WakeReplicas(name)
	}, nil
}

// correctionWrites returns what the corrections engine of the tenant named
// is given of its switcher: its fence, which refuses a step that writes to a
// storage while the registry's fence (fence) does or while a switch of any
// of the tenant's storages runs (switchingFence), and the gate a purge
// already running asks before each removal (correct.Config.Pause).
func (w *tenantWork) correctionWrites(ctx context.Context, name string, st tenantDB, fence func() error) (func(context.Context) error,
	correct.WritePause, error) {
	sw, err := w.switcherFor(ctx, name, st)
	if err != nil {
		return nil, nil, err
	}
	return func(ctx context.Context) error {
		if err := fence(); err != nil {
			return err
		}
		return switchingFence(ctx, st)
	}, sw.Gate(), nil
}

// writeRecordsOwed writes the correction records the tenant owes its
// replicas through its corrections engine (correct.Engine.WriteOwedRecords),
// which a switch asks at each turn of its drain: the swap is refused while
// one is owed to the replica it makes the primary.
func (w *tenantWork) writeRecordsOwed(ctx context.Context, name string, st tenantDB) error {
	es := w.engines.Load()
	if es == nil {
		return nil
	}
	e, release, err := es.Use(ctx, name, st)
	if err != nil {
		return err
	}
	defer release()
	_, err = e.WriteOwedRecords(ctx)
	return err
}

// switchingFence answers the refusal of a correction step while a switch of
// any of the tenant's storages runs (accepted and not yet swapped or
// refused), as the gate refuses a write: the step is refused before it
// writes and its client is told to try again shortly. A purge already
// running is held by the gate itself, before each removal
// (correct.Config.Pause).
func switchingFence(ctx context.Context, st tenant.Switching) error {
	left, err := st.UnfinishedSwitches(ctx)
	if err != nil {
		return err
	}
	for _, sw := range left {
		if sw.State == tenant.SwitchAccepted {
			return storageswitch.Refusal(sw.Primary)
		}
	}
	return nil
}
