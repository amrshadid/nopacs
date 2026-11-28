package supervise

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/receive"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// maxReceiverPages bounds the pages of one reading of a tenant's receivers,
// so a store that never ends its list cannot hold the worker.
const maxReceiverPages = 1000

// tenantRun is the worker of one tenant: its pipeline, its listeners, its
// gates and its grouping of refused callers. Only its own goroutine changes
// its fields, but for the gates, which listeners read (atomic), and the
// sink's pipeline, which stores read (under the sink's lock).
type tenantRun struct {
	sup  *Supervisor
	name string
	log  *slog.Logger

	poke    chan struct{}
	retired chan struct{}
	once    sync.Once

	// store is the database the pipeline runs over; a different one from
	// Tenants.Store is a reopened database, and the pipeline is restarted
	// on it.
	store    tenant.Store
	pipeline Pipeline
	sink     *switchSink

	receivers map[string]*receiverRun
	gates     atomic.Pointer[gateState]
	nextSpace time.Time
	// rows are the receivers as last read, and nextRead when they are read
	// again unless a poke comes first: a wake for the space check or a
	// retry acts on these without reading the database.
	rows     []tenant.Receiver
	nextRead time.Time
	space    spaceState
	// view is what the worker last published for View.
	view atomic.Pointer[viewState]

	failures *failureGroups
	// stopping is set while stopPipeline waits for a pipeline to stop, and
	// starting while restartPipeline starts one, for Drained, which reads
	// them from another goroutine.
	stopping, starting atomic.Bool
}

// newTenantRun returns the worker of the tenant named name.
func newTenantRun(s *Supervisor, name string) *tenantRun {
	t := &tenantRun{
		sup: s, name: name, log: s.log.With("tenant", name),
		poke: make(chan struct{}, 1), retired: make(chan struct{}),
		sink: &switchSink{}, receivers: map[string]*receiverRun{},
	}
	t.gates.Store(&gateState{})
	t.failures = newFailureGroups(t)
	return t
}

// wake makes the worker read its tenant at once. It never blocks.
func (t *tenantRun) wake() {
	select {
	case t.poke <- struct{}{}:
	default:
	}
}

// retire ends the worker: it stops its listeners and its pipeline and
// returns.
func (t *tenantRun) retire() { t.once.Do(func() { close(t.retired) }) }

// run reconciles the tenant until retired, then stops what it runs.
func (t *tenantRun) run() {
	defer t.sup.wg.Done()
	defer t.shutdown()
	poked := true
	for {
		t.reconcile(poked)
		t.failures.flush(t.sup.cfg.Now())
		wait := t.sup.cfg.Interval
		// Space, limits and certificates are looked at every
		// SpaceInterval, however long the receivers' periodic read is.
		if d := time.Until(t.nextRead); d < wait {
			wait = max(d, 10*time.Millisecond)
		}
		if d := time.Until(t.nextSpace); d < wait {
			wait = max(d, 10*time.Millisecond)
		}
		if d, ok := t.nextRetry(); ok && d < wait {
			wait = max(d, 10*time.Millisecond)
		}
		if d := t.failures.nextFlush(t.sup.cfg.Now()); d > 0 && d < wait {
			wait = d
		}
		timer := time.NewTimer(wait)
		poked = false
		select {
		case <-t.retired:
			timer.Stop()
			return
		case <-t.poke:
			poked = true
		case <-timer.C:
		}
		timer.Stop()
	}
}

// reconcile makes the tenant's pipeline and listeners match its database.
// It reads the receivers when read is set (a poke, or the first round) or
// Interval has passed since it last did; otherwise it acts on the rows last
// read, for the space check, the listeners' checks and the retries.
func (t *tenantRun) reconcile(read bool) {
	now := t.sup.cfg.Now()
	st, err := t.sup.cfg.Tenants.Store(t.name)
	if err != nil {
		// The listeners hold their ports and reject new associations as
		// transient until the database is back; the pipeline is left as it
		// is, since every write it tries fails and is tried again.
		t.setGates(func(g *gateState) { g.notReady = true })
		if fault.KindOf(err) == fault.Precondition {
			// A tenant held read only (moved by an export, or fenced by
			// another installation's owner record) answers its database,
			// so its pipeline's writes would succeed: it is stopped, the
			// files it holds finished or left in incoming with their
			// place kept, so nothing more is written for the tenant here.
			t.stopPipeline()
		}
		t.refreshHealth(now)
		return
	}
	p, ok := st.(tenant.Pipeline)
	if !ok {
		return
	}
	switch paused, known := t.replacing(p); {
	case known && paused:
		// A rebuild replaces the catalog: no pipeline runs until it ends,
		// so nothing is archived that its shadow would miss, while the
		// listeners go on refusing as a temporary failure.
		t.setGates(func(g *gateState) { g.rebuilding = true })
		t.stopPipeline()
	case !known:
		// Not known whether a rebuild holds the tenant: a pipeline that
		// runs is left running (its writes fail as the read did, and a
		// replace waits for it to stop before building), and none is
		// started until the next round knows.
	default:
		t.setGates(func(g *gateState) { g.rebuilding = false })
		if !sameStore(st, t.store) || t.pipeline == nil {
			t.restartPipeline(st, p)
		}
	}
	t.setGates(func(g *gateState) { g.notReady = t.pipeline == nil })
	if read || t.rows == nil || !time.Now().Before(t.nextRead) {
		rows, err := t.readReceivers(p)
		if err != nil {
			t.log.Warn("receivers: the tenant's receivers could not be read; nothing changes", "reason", fault.Message(err))
			t.setGates(func(g *gateState) { g.notReady = true })
			t.refreshHealth(now)
			return
		}
		t.rows = rows
		t.nextRead = time.Now().Add(t.sup.cfg.Interval)
	}
	rows := t.rows
	// Due times are read from the machine's monotonic clock, not from Now,
	// which dates what is recorded: a wait must pass whatever Now says.
	if mono := time.Now(); !mono.Before(t.nextSpace) {
		t.checkSpace(st, p, rows)
		t.nextSpace = mono.Add(t.sup.cfg.SpaceInterval)
	}
	t.apply(rows)
	t.publish()
}

// replacing reports whether a rebuild that replaces the tenant's catalog is
// queued or running, read from the tenant's database at every round so a
// restart, a job made by a request, and a job ended in any way are all
// seen; known is false when the database did not answer.
func (t *tenantRun) replacing(p tenant.Pipeline) (paused, known bool) {
	ctx, cancel := context.WithTimeout(t.sup.ctx, opTimeout)
	defer cancel()
	paused, err := p.RebuildReplacing(ctx)
	if err != nil {
		t.log.Warn("receivers: whether a rebuild replaces the tenant's catalog could not be read", "reason", fault.Message(err))
		return false, false
	}
	return paused, true
}

// readReceivers reads every receiver of the tenant, page after page.
func (t *tenantRun) readReceivers(p tenant.Pipeline) ([]tenant.Receiver, error) {
	var out []tenant.Receiver
	page := master.Page{Limit: master.MaxPageLimit}
	for range maxReceiverPages {
		ctx, cancel := context.WithTimeout(t.sup.ctx, opTimeout)
		batch, next, err := p.ListReceivers(ctx, page)
		cancel()
		if err != nil {
			return nil, err
		}
		out = append(out, batch...)
		if next == "" {
			return out, nil
		}
		page.Cursor = next
	}
	return nil, fault.New(fault.Internal, "the tenant's list of receivers did not end")
}

// restartPipeline stops the pipeline over the old database, if any, and
// starts one over st, within StartTimeout. One pipeline runs per tenant at a time, since each
// owns the tenant's incoming area. Stores that arrive meanwhile are refused
// as a temporary failure by the sink.
//
// starting is set for the whole restart, before the tenant is asked again,
// and cleared once the new pipeline is published: a tenant made read only
// (an export) after the reconcile got its store would otherwise have its
// pipeline started after Drained, seeing neither a pipeline nor a stop,
// said it was drained, and that pipeline would write after the export's
// snapshot. Asked again with starting set, the tenant either refuses (and
// nothing starts) or was still writable when Drained found starting set.
func (t *tenantRun) restartPipeline(st tenant.Store, p tenant.Pipeline) {
	t.starting.Store(true)
	defer t.starting.Store(false)
	t.stopPipeline()
	if again, err := t.sup.cfg.Tenants.Store(t.name); err != nil || !sameStore(again, st) {
		// Read only, unreachable or reopened since: the next round decides.
		return
	}
	// Asked again with starting set, as the tenant is: a replace made after
	// the round's first look either is seen here, and nothing starts, or
	// its unit's drain finds starting set (or the pipeline published) and
	// waits for the stop the next round makes.
	if paused, known := t.replacing(p); paused || !known {
		return
	}
	t.store = st
	ctx, cancel := context.WithTimeout(t.sup.ctx, t.startLimit(p))
	defer cancel()
	go func() {
		// A long start holds this worker; its retirement (the supervisor's
		// stop, the tenant's removal) ends it rather than wait for its bound.
		select {
		case <-t.retired:
		case <-ctx.Done():
		}
		cancel()
	}()
	g := Gates{
		LowDisk:   func(context.Context) (bool, error) { return t.gates.Load().incomingFull, nil },
		OverLimit: func(context.Context) (bool, error) { return t.gates.Load().overLimit, nil },
	}
	pl, err := t.sup.cfg.StartPipeline(ctx, t.name, p, g)
	if err != nil {
		t.log.Warn("receivers: the tenant's reception pipeline could not start; it is tried again", "reason", fault.Message(err))
		return
	}
	t.pipeline = pl
	t.sink.set(pl)
	// Decisions reach the new pipeline at once, not after the rest of the
	// reconcile. The removals owed while none ran are units of work in the
	// tenant's database, which its workers run now that they run.
	t.publish()
}

// startLimit returns how long a start of the tenant's pipeline may take:
// the tenant's receiver.start-limit, or StartTimeout when it cannot be read
// (the start is then bounded as it was before the setting, never left
// unbounded).
func (t *tenantRun) startLimit(p tenant.Pipeline) time.Duration {
	if t.sup.cfg.StartLimit == nil {
		return t.sup.cfg.StartTimeout
	}
	ctx, cancel := context.WithTimeout(t.sup.ctx, opTimeout)
	defer cancel()
	d, err := t.sup.cfg.StartLimit(ctx, t.name, p)
	if err != nil || d <= 0 {
		t.log.Warn("receivers: the tenant's start limit could not be read; the default applies", "reason", fault.Message(err))
		return t.sup.cfg.StartTimeout
	}
	return d
}

// stopPipeline stops the running pipeline, if any, within StopTimeout.
func (t *tenantRun) stopPipeline() {
	if t.pipeline == nil {
		return
	}
	t.stopping.Store(true)
	defer t.stopping.Store(false)
	t.sink.set(nil)
	// No decision is handed to a pipeline that is stopping: from here
	// Decisions finds none, and a release or a retry is refused until the
	// next pipeline is published.
	stopping := t.pipeline
	t.pipeline = nil
	t.publish()
	ctx, cancel := context.WithTimeout(context.WithoutCancel(t.sup.ctx), t.sup.cfg.StopTimeout)
	defer cancel()
	if err := stopping.Stop(ctx); err != nil {
		t.log.Warn("receivers: the tenant's reception pipeline did not stop cleanly", "reason", fault.Message(err))
	}
}

// shutdown stops every listener at once, each finishing its stores in
// flight, marks each receiver stopping, then stops the pipeline.
func (t *tenantRun) shutdown() {
	now := t.sup.cfg.Now()
	var wg sync.WaitGroup
	for _, run := range t.receivers {
		t.setHealth(run.row, down(tenant.DownStopping, tenant.PhraseStopping, now))
		if run.listener == nil {
			continue
		}
		wg.Add(1)
		go func(run *receiverRun) {
			defer wg.Done()
			run.stop(t.sup.ctx)
		}(run)
	}
	wg.Wait()
	t.failures.flushAll(t.sup.cfg.Now())
	t.stopPipeline()
}

// switchSink is the Sink every listener of a tenant is given: it hands a
// file to whichever pipeline runs now, so a pipeline restarted on a
// reopened database does not restart the listeners, and refuses as a
// temporary failure while none runs.
type switchSink struct {
	mu sync.RWMutex
	p  Pipeline
}

// set makes p the pipeline files go to; nil refuses them.
func (s *switchSink) set(p Pipeline) {
	s.mu.Lock()
	s.p = p
	s.mu.Unlock()
}

// running reports whether a pipeline takes files now.
func (s *switchSink) running() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.p != nil
}

// reasonNoPipeline is the refusal of a file that arrives while no pipeline
// runs.
const reasonNoPipeline = "the tenant's reception pipeline is not running"

// Accept hands a to the running pipeline, or refuses it as a temporary
// failure. The pipeline's Stop waits for every Accept it began, so a
// pipeline being replaced finishes the file it took.
func (s *switchSink) Accept(ctx context.Context, a receive.Arrival) receive.Outcome {
	s.mu.RLock()
	p := s.p
	s.mu.RUnlock()
	if p == nil {
		return receive.Outcome{Status: receive.Refused, Reason: reasonNoPipeline}
	}
	return p.Accept(ctx, a)
}
