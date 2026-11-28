package servecmd

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/clock"
	"github.com/amrshadid/nopacs/internal/correct"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/place"
	"github.com/amrshadid/nopacs/internal/recall"
	"github.com/amrshadid/nopacs/internal/registry"
	"github.com/amrshadid/nopacs/internal/replicate"
	"github.com/amrshadid/nopacs/internal/secret"
	"github.com/amrshadid/nopacs/internal/server"
	"github.com/amrshadid/nopacs/internal/settingvalue"
	"github.com/amrshadid/nopacs/internal/storageswitch"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// TenantWorkInterval is how often the tenants are read again to start the
// background work of a tenant that became ready and stop that of one that
// went: the registry's own check interval, so a tenant opened again is at
// work within the same half minute it is served again.
const TenantWorkInterval = TenantCheckInterval

// tenantWorkStop bounds the stop of one tenant's background work when the
// tenant goes or its database is opened anew: each worker stops at its next
// step (a copy, a recall, a repair's object), well within it.
const tenantWorkStop = 30 * time.Second

// tenantWork runs, for each ready tenant, the background work that keeps its
// copies whole: the replicator, the recall worker, the repair jobs, the
// automatic repairs after a scrub, retention's pass, and the switches of its
// storages, whose gate every write to a primary asks. A tenant's work starts
// once the registry answers its database, stops when the registry no longer
// does (the tenant removed, unreachable, moved), and starts again over a
// database opened anew; every time it records comes from clock.Now.
type tenantWork struct {
	// quarantine holds the running quarantine watches, for the metrics.
	quarantine   quarantineWatches
	d            Deps
	reg          *registry.Registry
	tenants      workTenants
	values       settingvalue.Reader
	opener       place.Opener
	installation string
	confirmKey   *secret.DerivedKey
	owners       *ownerLocks
	// owner names this process in the claims of the replicas' scans and of
	// the repair jobs: host:pid, so a claim another server left is told
	// from one of this server's.
	owner   string
	engines atomic.Pointer[correct.Engines]
	// transfers is what the tenants' transfer jobs share (setTransferDeps);
	// until it is set they wait.
	transfers atomic.Pointer[transferDeps]
	log       *slog.Logger
	interval  time.Duration
	// now is the clock every worker records its times from: clock.Now,
	// which a build for the proofs of retention moves.
	now func() time.Time
	// checkFence is the registry's fence (registry.Registry.CheckFence),
	// which every write of a tenant's background work to its database asks
	// first (fencedDB); nil asks nothing. It reads every storage's owner
	// record, a request per storage on S3.
	checkFence func(name string) error
	// checkState is the registry's state of the tenant as it holds it now
	// (registry.Registry.Store: no owner record read), which every write of
	// the background work to a storage asks first (fencedOpen); nil asks
	// nothing. Asking checkFence there would cost a request per storage for
	// every object put; the state is what the last fence check found, and
	// the database write that ends each step (an end of a copy, a repair
	// completed) asks checkFence itself, so a tenant another installation
	// took over records nothing more of what was put.
	checkState func(name string) error
	// replicaIdle is the replicators' wait after a claim that found
	// nothing; 0 is replicate.DefaultIdle.
	replicaIdle time.Duration
	// lagInterval is how often each tenant's replicas' lag is read; 0 is
	// replicate.DefaultLagInterval.
	lagInterval time.Duration
	// wakeFeed, when set, is called with a tenant's name after its
	// background work wrote events, so a watcher reads them at once.
	wakeFeed func(name string)
	// quota holds the running quota watches, for the metrics.
	quota quotaWatches

	create sync.Mutex

	mu        sync.Mutex
	switchers map[string]*tenantSwitcher
	sets      map[string]*tenantSet
	// epochs counts, per tenant, each time its work was taken away
	// (removed, fenced, quiesced): a start read before it does not start
	// (startSet), so a reading of the tenants that began before an
	// export's drain does not start the work the drain stopped.
	epochs  map[string]uint64
	started bool
	stopped bool
	ctx     context.Context
	cancel  context.CancelFunc
	loop    sync.WaitGroup
}

// workTenants lists the tenants and answers each one's database as the
// registry answers it to a writer (registryTenants): a moved, fenced or
// unready tenant is refused.
type workTenants interface {
	Names(ctx context.Context) ([]string, error)
	Store(name string) (tenant.Store, error)
}

// tenantDB is every part of a tenant's database the background work
// reaches; both drivers' tenant databases are one (dbOf).
type tenantDB interface {
	tenant.Store
	tenant.Pipeline
	tenant.CatalogReads
	tenant.Corrections
	tenant.Switching
	tenant.Restoring
	tenant.Replication
	tenant.Recalls
	tenant.Repairs
	tenant.Scrubbing
	tenant.Jobs
	tenant.JobWork
	tenant.Retention
	tenant.Disposal
	tenant.SearchBackfills
	tenant.Holds
	tenant.SearchBackfills
}

// dbOf returns st as every part of a tenant's database, or an Internal
// fault for a database that lacks one: a mistake of the code, since every
// driver implements them all.
func dbOf(st any) (tenantDB, error) {
	db, ok := st.(tenantDB)
	if !ok {
		return nil, fault.New(fault.Internal, "the tenant's database lacks a part the server's background work needs")
	}
	return db, nil
}

// tenantSwitcher is a tenant's switcher and the database it runs over.
type tenantSwitcher struct {
	store tenantDB
	sw    *storageswitch.Switcher
}

// tenantSet is one tenant's running background work.
type tenantSet struct {
	store   tenantDB
	cancel  context.CancelFunc
	wg      sync.WaitGroup
	repl    *replicate.Worker
	recalls *recall.Worker
	lag     *replicate.Lag
	repairs chan struct{}
}

// newTenantWork returns the background work of every tenant, not started.
func newTenantWork(d Deps, reg *registry.Registry, cleanups *place.Cleanups, installation string, owners *ownerLocks) (*tenantWork, error) {
	key, err := d.Keys.Derive(secret.ConfirmToken)
	if err != nil {
		return nil, err
	}
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "server"
	}
	log := d.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	values := settingvalue.Reader{Master: d.Master}
	ctx, cancel := context.WithCancel(context.Background())
	return &tenantWork{d: d, reg: reg, tenants: registryTenants{master: d.Master, registry: reg}, values: values,
		opener: writingOpener(values, d.Keys, cleanups), installation: installation, confirmKey: key, owners: owners,
		owner: workerName(host, os.Getpid()), log: log, interval: TenantWorkInterval, now: clock.Now, checkFence: reg.CheckFence,
		checkState: func(name string) error {
			_, err := reg.Store(name)
			return err
		},
		switchers: map[string]*tenantSwitcher{}, sets: map[string]*tenantSet{}, ctx: ctx, cancel: cancel}, nil
}

// workerName returns the name this process claims work under: the host's
// name and the process id, as host-pid, in the letters a worker's name
// takes (1 to 64 of A-Z, a-z, 0-9, _ and -): every other byte of the host's
// name becomes "-", and a long host name is cut so the pid always fits.
func workerName(host string, pid int) string {
	suffix := "-" + strconv.Itoa(pid)
	b := []byte(host)
	for i, c := range b {
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '_' && c != '-' {
			b[i] = '-'
		}
	}
	if len(b) == 0 {
		b = []byte("server")
	}
	if len(b)+len(suffix) > 64 {
		b = b[:64-len(suffix)]
	}
	return string(b) + suffix
}

// setEngines hands the corrections' engines to the work that writes
// through them (a switch's records owed, retention's deletes); until then
// that work waits.
func (w *tenantWork) setEngines(es *correct.Engines) { w.engines.Store(es) }

// Start reads the tenants and starts the work of each ready one, then again
// each interval until Stop.
func (w *tenantWork) Start(context.Context) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.started || w.stopped {
		return nil
	}
	w.started = true
	w.loop.Add(1)
	go func() {
		defer w.loop.Done()
		t := time.NewTicker(w.interval)
		defer t.Stop()
		for {
			w.reconcile(w.ctx)
			select {
			case <-w.ctx.Done():
				return
			case <-t.C:
			}
		}
	}()
	return nil
}

// Stop stops every tenant's work and switcher, within ctx: a switch before
// its swap is refused ("server restarted"), as the next start would refuse
// it.
func (w *tenantWork) Stop(ctx context.Context) error {
	w.mu.Lock()
	w.stopped = true
	sets, switchers := w.sets, w.switchers
	w.sets, w.switchers = map[string]*tenantSet{}, map[string]*tenantSwitcher{}
	w.mu.Unlock()
	w.cancel()
	w.loop.Wait()
	var errs []error
	for _, s := range sets {
		errs = append(errs, s.stop(ctx))
	}
	for _, s := range switchers {
		errs = append(errs, s.sw.Stop(ctx))
	}
	return errors.Join(errs...)
}

// TenantRemoved stops the work of the tenant named at once, not at the next
// reading of the tenants.
func (w *tenantWork) TenantRemoved(name string) {
	w.mu.Lock()
	s := w.takeLocked(name)
	sw := w.switchers[name]
	delete(w.switchers, name)
	w.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), tenantWorkStop)
	defer cancel()
	if s != nil {
		_ = s.stop(ctx)
	}
	if sw != nil {
		_ = sw.sw.Stop(ctx)
	}
}

// takeLocked takes the running work of the tenant named out of the sets,
// and counts the taking in its epoch, so a start read before now does not
// start it again; it returns the work taken, or nil. The caller holds mu.
func (w *tenantWork) takeLocked(name string) *tenantSet {
	if w.epochs == nil {
		w.epochs = map[string]uint64{}
	}
	w.epochs[name]++
	s := w.sets[name]
	delete(w.sets, name)
	return s
}

// epochOf returns the tenant's epoch now (takeLocked).
func (w *tenantWork) epochOf(name string) uint64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.epochs[name]
}

// Quiesce stops the background work of the tenant named and waits, within
// ctx, until every worker has returned, so nothing of it writes after: an
// export's drain and a rebuild's ask it before the snapshot or the build.
// The work is started again by a later reading of the tenants once the
// registry answers the tenant's store again (an export given back) and no
// rebuild of it runs.
func (w *tenantWork) Quiesce(ctx context.Context, name string) error {
	w.mu.Lock()
	s := w.takeLocked(name)
	w.mu.Unlock()
	if s == nil {
		return nil
	}
	if err := s.stop(ctx); err != nil {
		return fault.Wrap(fault.Unavailable, err, "the tenant's background work did not stop in time")
	}
	return nil
}

// Fenced stops the background work of the tenant named, which a check of
// the registry has just fenced: its workers are told to stop at once,
// without waiting for them (the registry calls it holding the tenant's
// entry locked); every write they still try is refused by the fence
// (fencedDB, fencedOpen).
func (w *tenantWork) Fenced(name string) {
	w.mu.Lock()
	s := w.takeLocked(name)
	w.mu.Unlock()
	if s != nil {
		s.cancel()
	}
}

// rebuilding reports whether a rebuild of the tenant whose database is st is
// queued or running (tenant.Rebuilding's RebuildRunning), or whether that
// cannot be read: either way its background work is not started, since a
// rebuild's drain stopped it (Quiesce) so nothing writes what the build
// reads. A database that keeps no rebuilds has none.
func rebuilding(ctx context.Context, st tenantDB) bool {
	rb, ok := st.(interface {
		RebuildRunning(ctx context.Context) (bool, error)
	})
	if !ok {
		return false
	}
	running, err := rb.RebuildRunning(ctx)
	return err != nil || running
}

// reconcile starts the work of each ready tenant that has none, or whose
// database was opened anew, and stops the work of each tenant gone or not
// ready.
func (w *tenantWork) reconcile(ctx context.Context) {
	rctx, cancel := context.WithTimeout(ctx, tenantNamesTimeout)
	names, err := w.tenants.Names(rctx)
	cancel()
	if err != nil {
		if ctx.Err() == nil {
			w.log.Warn("tenant work: the tenants could not be read; tried again shortly", "kind", fault.KindOf(err).String())
		}
		return
	}
	listed := make(map[string]bool, len(names))
	for _, name := range names {
		listed[name] = true
		// The epoch is read before the store: work taken away after this
		// (an export's drain) is not started again by this reading.
		epoch := w.epochOf(name)
		var st tenantDB
		raw, err := w.tenants.Store(name)
		if err == nil {
			st, err = dbOf(raw)
		}
		w.mu.Lock()
		cur := w.sets[name]
		w.mu.Unlock()
		switch {
		case err != nil && cur != nil:
			w.drop(name, cur)
		case err != nil, cur != nil && cur.store == st:
		default:
			if cur != nil {
				w.drop(name, cur)
			}
			if rebuilding(ctx, st) {
				continue
			}
			if err := w.startSet(ctx, name, st, epoch); err != nil && ctx.Err() == nil {
				w.log.Warn("tenant work: a tenant's background work could not start; tried again shortly", "tenant", name,
					"kind", fault.KindOf(err).String(), "reason", fault.Message(err))
			}
		}
	}
	w.mu.Lock()
	var gone []string
	for name := range w.sets {
		if !listed[name] {
			gone = append(gone, name)
		}
	}
	w.mu.Unlock()
	for _, name := range gone {
		w.TenantRemoved(name)
	}
}

// drop stops s, the work of the tenant named, when it is still its current
// work.
func (w *tenantWork) drop(name string, s *tenantSet) {
	w.mu.Lock()
	if w.sets[name] == s {
		delete(w.sets, name)
	}
	w.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), tenantWorkStop)
	defer cancel()
	_ = s.stop(ctx)
}

// stop ends the set's workers and waits for them within ctx.
func (s *tenantSet) stop(ctx context.Context) error {
	s.cancel()
	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// tenantID returns the id of the tenant named, which its sealed secrets and
// owner records are bound to.
func (w *tenantWork) tenantID(ctx context.Context, name string) (string, error) {
	row, err := w.d.Master.TenantByName(ctx, name)
	return row.ID, err
}

// openFor returns the opener of the tenant's storages to write, by name,
// whose stores its caller closes.
func (w *tenantWork) openFor(t place.Tenant, st tenant.Pipeline) func(ctx context.Context, storage string) (blob.Store, error) {
	return func(ctx context.Context, storage string) (blob.Store, error) {
		row, err := st.Storage(ctx, storage)
		if err != nil {
			return nil, err
		}
		return w.opener.Open(ctx, t, row)
	}
}

// fenceOf returns the fence every write of the tenant's background work to
// its database asks (checkFence), or nil when there is none.
func (w *tenantWork) fenceOf(name string) func() error {
	if w.checkFence == nil {
		return nil
	}
	return func() error { return w.checkFence(name) }
}

// stateOf returns the check every write of the tenant's background work to
// a storage asks (checkState), or nil when there is none.
func (w *tenantWork) stateOf(name string) func() error {
	if w.checkState == nil {
		return nil
	}
	return func() error { return w.checkState(name) }
}

// startSet starts the background work of the tenant named over st, unless
// the tenant's work was taken away since epoch was read (epochOf). Every
// write the work makes to st asks the registry's fence first (fencedDB),
// and every write to a storage the registry's state of the tenant
// (fencedOpen, stateOf).
func (w *tenantWork) startSet(ctx context.Context, name string, st tenantDB, epoch uint64) error {
	id, err := w.tenantID(ctx, name)
	if err != nil {
		return err
	}
	sw, err := w.switcherFor(ctx, name, st)
	if err != nil {
		return err
	}
	db := fenceDB(st, w.fenceOf(name))
	state := w.stateOf(name)
	t := place.TenantOf(id, name, st)
	open := fencedOpen(w.openFor(t, st), state)
	log := w.log.With("tenant", name)
	// replica.rate, recall.wait and replica.lag-alert are read through the
	// tenant's settings at each claim, pass and look, so a change applies
	// without a restart.
	values := w.values.OnTenant(st)
	// Each put of a copy enters the replica's write gate, so a switch waits
	// for the copies in flight before it swaps the replica's place.
	repl, err := replicate.New(replicate.Config{Queue: db, Catalog: db, Now: w.now, Owner: w.owner, Log: log, Idle: w.replicaIdle,
		Rate: values.ReplicaRate, Pause: sw.Gate(),
		Open: func(ctx context.Context, storage string) (blob.Store, error) { return open(ctx, storage) }})
	if err != nil {
		return err
	}
	var lag *replicate.Lag
	if lagDB, ok := lagDBOf(st, db, w.fenceOf(name)); ok {
		lag, err = replicate.NewLag(replicate.LagConfig{Replicas: lagDB, Alert: values.LagAlert,
			Now: w.now, NewID: server.NewID, Interval: w.lagInterval, Log: log, Wake: func() {
				if w.wakeFeed != nil {
					w.wakeFeed(name)
				}
			}})
		if err != nil {
			return err
		}
	} else {
		log.Warn("tenant work: the tenant's database keeps no events; the replicas' lag is not watched")
	}
	// A recall asks a store to restore an object, a write of the store's
	// state, never of bytes: the store is opened beside the others, removing
	// nothing a crash left.
	openBeside := fencedOpen(func(ctx context.Context, storage string) (blob.Store, error) {
		row, err := st.Storage(ctx, storage)
		if err != nil {
			return nil, err
		}
		return w.opener.OpenBeside(ctx, t, row)
	}, state)
	recalls, err := recall.New(recall.Config{Queue: db, Now: w.now, Log: log, Wait: values.RecallWait,
		Open: func(ctx context.Context, storage string) (blob.Store, error) { return openBeside(ctx, storage) }})
	if err != nil {
		return err
	}
	sctx, cancel := context.WithCancel(w.ctx)
	s := &tenantSet{store: st, cancel: cancel, repl: repl, recalls: recalls, lag: lag, repairs: make(chan struct{}, 1)}
	w.mu.Lock()
	if w.stopped || w.epochs[name] != epoch {
		w.mu.Unlock()
		cancel()
		return nil
	}
	w.sets[name] = s
	w.mu.Unlock()
	run := func(f func(ctx context.Context)) {
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			f(sctx)
		}()
	}
	run(func(ctx context.Context) { _ = repl.Run(ctx) })
	run(func(ctx context.Context) { _ = recalls.Run(ctx) })
	if lag != nil {
		run(func(ctx context.Context) { _ = lag.Run(ctx) })
	}
	r := &tenantRepairs{w: w, name: name, st: db, open: open, gate: sw.Gate(), wake: s.repairs, log: log}
	run(w.quarantine.watch(w, name, st, log))
	run(w.quota.watch(w, name, st, log))
	run(r.run)
	run(r.autoLoop)
	ret := &tenantRetention{w: w, name: name, st: db, base: st, log: log}
	run(ret.run)
	run((&tenantBatches{w: w, name: name, st: db, base: st, log: log}).run)
	run((&tenantSearchFills{w: w, name: name, st: db, log: log}).run)
	run((&tenantTransfers{w: w, name: name, st: db, base: st, log: log}).run)
	run((&tenantDeidCoverages{w: w, name: name, st: db, log: log}).run)
	run(w.holdRecords(id, name, db, open, sw.Gate(), log))
	return nil
}

// set returns the running work of the tenant named, or nil.
func (w *tenantWork) set(name string) *tenantSet {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.sets[name]
}

// WakeReplicas asks the tenant's replicator to claim at once: a commit that
// queued a scan for its replicas calls it.
func (w *tenantWork) WakeReplicas(name string) {
	if s := w.set(name); s != nil {
		s.repl.Wake()
	}
}

// WakeRecalls asks the tenant's recall worker to read the due recalls at
// once: a request that made a recall calls it.
func (w *tenantWork) WakeRecalls(name string) {
	if s := w.set(name); s != nil {
		s.recalls.Wake()
	}
}

// WakeRepairs asks the tenant's repair worker to claim at once: a request
// that made a repair job calls it.
func (w *tenantWork) WakeRepairs(name string) {
	if s := w.set(name); s != nil {
		select {
		case s.repairs <- struct{}{}:
		default:
		}
	}
}

// serverAudit returns the audit entry of a step the server takes by itself
// on the tenant named.
func serverAudit(tenantName, method, object string, now time.Time) master.AuditEntry {
	return master.AuditEntry{ID: server.NewID(), Time: now.UTC().Truncate(time.Microsecond), LocalUser: "server", Method: method,
		TenantName: tenantName, Object: object, Outcome: master.OutcomeDone}
}
