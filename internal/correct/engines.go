package correct

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// Tenants is where Engines learns which tenants exist and reaches each
// one's database.
type Tenants interface {
	// Names returns the names of every tenant of the installation.
	Names(ctx context.Context) ([]string, error)
	// Store returns the open database of the tenant named name. A tenant
	// that is not ready is an error.
	Store(name string) (tenant.Store, error)
}

// EnginesConfig is what Engines needs.
type EnginesConfig struct {
	// Tenants lists the tenants the pass goes over.
	Tenants Tenants
	// Configure returns the Config of the engine of the tenant named name
	// over its database st (Tenant and Store set by Engines), and what to
	// close once that engine is no longer used (its storages), or nil.
	Configure func(ctx context.Context, name string, st tenant.Corrections) (Config, func() error, error)
	// Interval is how often the pass runs: each tenant's Recover until it
	// succeeds once, then, each time, the deletes and replaces a request
	// left prepared and the tenant's PurgeDue. Zero is DefaultInterval.
	Interval time.Duration
	// RecoverWait is how long a request waits for its tenant's first
	// Recover before it is answered unavailable. Zero is
	// DefaultRecoverWait.
	RecoverWait time.Duration
	// Log takes what the pass reports; nil for none.
	Log *slog.Logger
}

// DefaultInterval is how often the pass runs by default: a minute, so a
// grace period ends at most a minute late, at the cost of a few reads of
// each tenant's database a minute.
const DefaultInterval = time.Minute

// DefaultRecoverWait is how long a request waits by default for its
// tenant's first Recover: shorter than the half minute a client usually
// waits for an answer, so a tenant whose Recover is slow or failing (a
// storage that does not answer) is answered unavailable, with the reason,
// rather than cut by the client's own deadline.
const DefaultRecoverWait = 20 * time.Second

// Engines holds one Engine per tenant, made when first asked for, so the
// API's requests and the server's background work act through the same
// engine (a relabel's stop is seen by its copy), and runs the work a
// request leaves to the background and the pass on a timer: Recover at
// start, then the deletes and replaces a request left prepared and the
// purges whose grace has ended (PurgeDue). One tenant's background work
// runs one at a time, also across an engine made anew for the tenant, so a
// purge, a relabel's copy and Recover never act on the same correction at
// once.
type Engines struct {
	cfg EnginesConfig

	mu sync.Mutex
	// runs holds each tenant's current engine.
	runs map[string]*engineRun
	// works holds each tenant's lock of background work, by name, kept for
	// the life of Engines: an engine made anew for a tenant (its database
	// opened again) takes the same lock as the one it replaces, whose work
	// may still run.
	works   map[string]*sync.Mutex
	started bool
	stopped bool

	ctx    context.Context
	cancel context.CancelFunc
	stop   chan struct{}
	wg     sync.WaitGroup
}

// engineRun is one tenant's engine, the database it was made over, the
// lock its background work holds, the gate its requests and its Recover
// pass through (Use), and the count of what holds it.
type engineRun struct {
	store  tenant.Store
	engine *Engine
	close  func() error
	work   *sync.Mutex
	gate   sync.RWMutex
	// holds counts the requests, background work, passes and Recover that
	// act through the engine: what it opened is closed only once none
	// does, when it is replaced or at Stop.
	holds sync.WaitGroup
	// recovered is set, under work, once Recover has succeeded; ready is
	// closed then.
	recovered bool
	ready     chan struct{}
	// recovering is set, under Engines.mu, while a Recover a request asked
	// for runs.
	recovering bool
}

// NewEngines returns the engines over cfg; Start runs the pass.
func NewEngines(cfg EnginesConfig) (*Engines, error) {
	if cfg.Tenants == nil || cfg.Configure == nil {
		return nil, fault.New(fault.Internal, "the corrections' engines need the tenants and a way to configure each engine")
	}
	if cfg.Interval <= 0 {
		cfg.Interval = DefaultInterval
	}
	if cfg.RecoverWait <= 0 {
		cfg.RecoverWait = DefaultRecoverWait
	}
	if cfg.Log == nil {
		cfg.Log = slog.New(slog.DiscardHandler)
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Engines{cfg: cfg, runs: map[string]*engineRun{}, works: map[string]*sync.Mutex{}, ctx: ctx, cancel: cancel,
		stop: make(chan struct{})}, nil
}

// errEnginesStopped answers a request for an engine once Stop has begun.
var errEnginesStopped = fault.New(fault.Unavailable, "the server is stopping; try again once it has started")

// Use returns the engine of the tenant named name, whose database is st,
// for one request, making it when there is none or when st is not the
// database it was made over (the tenant was removed and made again, or
// opened anew), and the release the request calls once its engine call has
// returned. A request never runs before the tenant's first Recover has
// succeeded, nor beside a Recover or the pass's abandon of what a request
// left: those abandon a delete or a replace they find prepared, taking it
// for one no request holds, and a request's own is prepared while it runs.
// So a request waits for the first Recover, asking for it when the pass has
// not run yet, up to RecoverWait (then it is answered unavailable), and
// each waits for the requests in flight.
func (es *Engines) Use(ctx context.Context, name string, st tenant.Store) (*Engine, func(), error) {
	r, err := es.acquire(ctx, name, st)
	if err != nil {
		return nil, nil, err
	}
	if err := es.waitRecovered(ctx, name, r); err != nil {
		r.holds.Done()
		return nil, nil, err
	}
	r.gate.RLock()
	return r.engine, sync.OnceFunc(func() {
		r.gate.RUnlock()
		r.holds.Done()
	}), nil
}

// acquire returns the tenant's engineRun, making it as Use says, held once
// for the caller, who calls its holds.Done once done with it.
func (es *Engines) acquire(ctx context.Context, name string, st tenant.Store) (*engineRun, error) {
	es.mu.Lock()
	defer es.mu.Unlock()
	if es.stopped {
		return nil, errEnginesStopped
	}
	if r, ok := es.runs[name]; ok && r.store == st {
		r.holds.Add(1)
		return r, nil
	}
	cs, ok := st.(tenant.Corrections)
	if !ok {
		return nil, fault.New(fault.Internal, "the tenant's database keeps no corrections")
	}
	cfg, closeFn, err := es.cfg.Configure(ctx, name, cs)
	if err != nil {
		return nil, err
	}
	cfg.Tenant, cfg.Store = name, cs
	if cfg.Log == nil {
		cfg.Log = es.cfg.Log.With("tenant", name)
	}
	e, err := New(cfg)
	if err != nil {
		if closeFn != nil {
			_ = closeFn()
		}
		return nil, err
	}
	if old, ok := es.runs[name]; ok {
		// The old engine may still be in a request's or a background job's
		// hands: what it opened is closed once the last lets it go.
		es.wg.Add(1)
		go es.retire(name, old)
	}
	work := es.works[name]
	if work == nil {
		work = &sync.Mutex{}
		es.works[name] = work
	}
	r := &engineRun{store: st, engine: e, close: closeFn, work: work, ready: make(chan struct{})}
	r.holds.Add(1)
	es.runs[name] = r
	return r, nil
}

// retire closes what a replaced engine opened once nothing holds it.
func (es *Engines) retire(name string, r *engineRun) {
	defer es.wg.Done()
	r.holds.Wait()
	if r.close == nil {
		return
	}
	if err := r.close(); err != nil {
		es.cfg.Log.Warn("corrections: a replaced engine's storages did not close cleanly", "tenant", name, "error", fault.Message(err))
	}
}

// waitRecovered waits until the tenant's first Recover has succeeded,
// starting one when none runs, up to RecoverWait.
func (es *Engines) waitRecovered(ctx context.Context, name string, r *engineRun) error {
	select {
	case <-r.ready:
		return nil
	default:
	}
	es.mu.Lock()
	if es.stopped {
		es.mu.Unlock()
		return errEnginesStopped
	}
	if !r.recovering {
		r.recovering = true
		r.holds.Add(1)
		es.wg.Add(1)
		go es.recoverAsked(name, r)
	}
	es.mu.Unlock()
	t := time.NewTimer(es.cfg.RecoverWait)
	defer t.Stop()
	select {
	case <-r.ready:
		return nil
	case <-t.C:
		return fault.New(fault.Unavailable, "the tenant's corrections are still taking up what the server's last stop left; try again shortly")
	case <-es.stop:
		return errEnginesStopped
	case <-ctx.Done():
		return fault.Wrap(fault.Unavailable, ctx.Err(), "the request ended while the tenant's corrections took up what the server's last stop left")
	}
}

// recoverAsked runs the tenant's first Recover for a request that waits for
// it, after the tenant's other background work.
func (es *Engines) recoverAsked(name string, r *engineRun) {
	defer es.wg.Done()
	defer r.holds.Done()
	r.work.Lock()
	es.recover(name, r)
	r.work.Unlock()
	es.mu.Lock()
	r.recovering = false
	es.mu.Unlock()
}

// recover runs the tenant's Recover under its gate, unless it has
// succeeded already, and reports whether it has. The caller holds work.
func (es *Engines) recover(name string, r *engineRun) bool {
	if r.recovered {
		return true
	}
	r.gate.Lock()
	err := r.engine.Recover(es.ctx)
	r.gate.Unlock()
	if err != nil {
		if es.ctx.Err() == nil {
			es.cfg.Log.Warn("corrections: what a stop left could not all be taken up; the pass tries again", "tenant", name,
				"error", fault.Message(err))
		}
		return false
	}
	r.recovered = true
	close(r.ready)
	return true
}

// Go runs work in the background on the engine of the tenant named name,
// after the tenant's other background work, until Stop; an error it
// returns is logged. Work asked once Stop has begun is not run: what it
// would have done is taken up by Recover at the next start.
func (es *Engines) Go(name string, st tenant.Store, what string, work func(ctx context.Context, e *Engine) error) {
	r, err := es.acquire(es.ctx, name, st)
	if err != nil {
		es.cfg.Log.Warn("corrections: background work was not started; the next start takes it up", "tenant", name, "work", what,
			"error", fault.Message(err))
		return
	}
	es.mu.Lock()
	if es.stopped {
		es.mu.Unlock()
		r.holds.Done()
		return
	}
	es.wg.Add(1)
	es.mu.Unlock()
	go func() {
		defer es.wg.Done()
		defer r.holds.Done()
		r.work.Lock()
		defer r.work.Unlock()
		if es.ctx.Err() != nil {
			return
		}
		if err := work(es.ctx, r.engine); err != nil && es.ctx.Err() == nil {
			es.cfg.Log.Warn("corrections: background work failed; the pass tries again", "tenant", name, "work", what,
				"error", fault.Message(err))
		}
	}()
}

// Idle waits until the tenant named name runs no background work (a purge,
// a relabel's copy, a Recover), or ctx ends. Work queued after it returns
// still runs; a caller that must see none first stops new work coming (an
// export makes the tenant moved, which every step's Fence then refuses).
func (es *Engines) Idle(ctx context.Context, name string) error {
	es.mu.Lock()
	work := es.works[name]
	es.mu.Unlock()
	if work == nil {
		return nil
	}
	for !work.TryLock() {
		select {
		case <-ctx.Done():
			return fault.Wrap(fault.Unavailable, ctx.Err(), "the tenant's corrections did not finish their background work in time")
		case <-time.After(idlePoll):
		}
	}
	work.Unlock()
	return nil
}

// idlePoll is how often Idle tries the tenant's lock of background work.
const idlePoll = 50 * time.Millisecond

// Start runs the pass now and then every Interval, in the background.
func (es *Engines) Start(context.Context) error {
	es.mu.Lock()
	defer es.mu.Unlock()
	if es.started {
		return fault.New(fault.Internal, "the corrections' pass is already running")
	}
	if es.stopped {
		return errEnginesStopped
	}
	es.started = true
	es.wg.Add(1)
	go es.loop()
	return nil
}

// loop runs the pass until Stop.
func (es *Engines) loop() {
	defer es.wg.Done()
	t := time.NewTicker(es.cfg.Interval)
	defer t.Stop()
	for {
		es.pass()
		select {
		case <-es.stop:
			return
		case <-t.C:
		}
	}
}

// pass goes once over every tenant ready now: Recover, until it has once
// taken up everything; after it, what a request left prepared; then the
// purges due.
func (es *Engines) pass() {
	names, err := es.cfg.Tenants.Names(es.ctx)
	if err != nil {
		if es.ctx.Err() == nil {
			es.cfg.Log.Warn("corrections: the tenants could not be listed; the pass tries again", "error", fault.Message(err))
		}
		return
	}
	for _, name := range names {
		if es.ctx.Err() != nil {
			return
		}
		st, err := es.cfg.Tenants.Store(name)
		if err != nil {
			continue
		}
		r, err := es.acquire(es.ctx, name, st)
		if err != nil {
			if es.ctx.Err() == nil {
				es.cfg.Log.Warn("corrections: the tenant's engine could not be made", "tenant", name, "error", fault.Message(err))
			}
			continue
		}
		es.passOne(name, r)
		r.holds.Done()
	}
}

// passOne runs one tenant's part of the pass, under its work lock.
func (es *Engines) passOne(name string, r *engineRun) {
	r.work.Lock()
	defer r.work.Unlock()
	if !r.recovered {
		if !es.recover(name, r) {
			return
		}
	} else {
		es.abandonLeft(name, r)
	}
	n, err := r.engine.PurgeDue(es.ctx)
	switch {
	case errors.Is(err, tenant.ErrClock):
		es.cfg.Log.Warn("corrections: purges wait: the server's clock is not one they trust", "tenant", name, "reason", fault.Message(err))
	case err != nil && es.ctx.Err() == nil:
		es.cfg.Log.Warn("corrections: a purge did not finish; the pass tries again", "tenant", name, "error", fault.Message(err))
	}
	if n > 0 {
		es.cfg.Log.Info("corrections: purged", "tenant", name, "corrections", n)
	}
}

// abandonLeft abandons the deletes and replaces left prepared with no
// request holding them (a request whose prepare committed though its
// answer was lost, or whose engine call failed past its own abandon), under
// the gate, so no request runs meanwhile. When requests are in flight the
// gate is not waited for, which would hold every new request of the tenant
// behind them: the next pass tries again.
func (es *Engines) abandonLeft(name string, r *engineRun) {
	if !r.gate.TryLock() {
		return
	}
	n, err := r.engine.abandonLeft(es.ctx)
	r.gate.Unlock()
	if err != nil && es.ctx.Err() == nil {
		es.cfg.Log.Warn("corrections: a correction a request left prepared could not be abandoned; the pass tries again", "tenant", name,
			"error", fault.Message(err))
	}
	if n > 0 {
		es.cfg.Log.Info("corrections: abandoned what requests left prepared", "tenant", name, "corrections", n)
	}
}

// Stop ends the pass and the background work, each at its next step, waits
// for the requests that hold an engine to let it go, and closes what the
// engines opened. It returns once all have ended, or when ctx ends, leaving
// open what is still held: storages are never closed under a request.
func (es *Engines) Stop(ctx context.Context) error {
	es.mu.Lock()
	if es.stopped {
		es.mu.Unlock()
		return nil
	}
	es.stopped = true
	es.mu.Unlock()
	close(es.stop)
	es.cancel()
	if err := waitWithin(ctx, es.wg.Wait); err != nil {
		return fault.Wrap(fault.Unavailable, err, "the corrections' background work did not stop in time")
	}
	es.mu.Lock()
	runs := es.runs
	es.runs = map[string]*engineRun{}
	es.mu.Unlock()
	var errs []error
	for _, r := range runs {
		if err := waitWithin(ctx, r.holds.Wait); err != nil {
			errs = append(errs, fault.Wrap(fault.Unavailable, err, "a correction request did not end in time; its engine's storages are left open"))
			continue
		}
		if r.close != nil {
			errs = append(errs, r.close())
		}
	}
	return errors.Join(errs...)
}

// waitWithin runs wait and returns once it has, or ctx's error once ctx
// ends; wait then goes on in the background until it returns.
func waitWithin(ctx context.Context, wait func()) error {
	done := make(chan struct{})
	go func() {
		wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
