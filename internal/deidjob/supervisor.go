package deidjob

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// Supervisor runs every tenant's deid jobs. Make it with New, run it with
// Start, end it with Stop.
type Supervisor struct {
	cfg Config
	log *slog.Logger
	// units holds one token per unit worked now, at most MaxUnits.
	units chan struct{}

	mu      sync.Mutex
	tenants map[string]*tenantRun
	started bool
	stopped bool

	poke chan struct{}
	stop chan struct{}
	// hard ends everything in flight: canceled when Stop's ctx ends
	// before the units have been given back.
	hard   context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// stopGrace is how long Stop waits, after its bound, for units it cut off.
const stopGrace = time.Second

// New checks cfg and returns a supervisor that runs nothing until Start.
func New(cfg Config) (*Supervisor, error) {
	cfg, err := cfg.check()
	if err != nil {
		return nil, err
	}
	s := &Supervisor{cfg: cfg, log: cfg.Logger, units: make(chan struct{}, MaxUnits), tenants: map[string]*tenantRun{},
		poke: make(chan struct{}, 1), stop: make(chan struct{})}
	s.hard, s.cancel = context.WithCancel(context.Background())
	return s, nil
}

// Start starts the reading of the tenants; it returns without waiting. A
// second Start is an error.
func (s *Supervisor) Start(context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.started {
		return fault.New(fault.Internal, "deidjob: the supervisor is already running")
	}
	s.started = true
	s.wg.Add(1)
	go s.run()
	return nil
}

// Stop stops every unit: each finishes the instance it is judging, records
// what it judged, and gives its unit back without spending an attempt. It
// returns once all have ended, or when ctx ends, with what is still in
// flight cut short (the next claim judges again at most one batch).
func (s *Supervisor) Stop(ctx context.Context) error {
	s.mu.Lock()
	if s.stopped || !s.started {
		s.stopped = true
		s.mu.Unlock()
		return nil
	}
	s.stopped = true
	s.mu.Unlock()
	close(s.stop)
	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		s.cancel()
		return nil
	case <-ctx.Done():
		s.cancel()
		t := time.NewTimer(stopGrace)
		defer t.Stop()
		select {
		case <-done:
		case <-t.C:
		}
		return fault.Wrap(fault.Unavailable, ctx.Err(), "the deid jobs did not all stop in time; units in flight were cut short")
	}
}

// Poke makes the tenant named name look for work at once: the API calls it
// after every job it makes, cancels or retries. It never blocks.
func (s *Supervisor) Poke(name string) {
	s.mu.Lock()
	t := s.tenants[name]
	s.mu.Unlock()
	if t != nil {
		t.wake()
		return
	}
	select {
	case s.poke <- struct{}{}:
	default:
	}
}

// TenantRemoved makes the supervisor read the list of tenants at once, so
// the units of a tenant the registry let go are given back now. It never
// blocks.
func (s *Supervisor) TenantRemoved(string) {
	select {
	case s.poke <- struct{}{}:
	default:
	}
}

// wakeAll wakes every tenant, after a unit ended and freed a place.
func (s *Supervisor) wakeAll() {
	s.mu.Lock()
	all := make([]*tenantRun, 0, len(s.tenants))
	for _, t := range s.tenants {
		all = append(all, t)
	}
	s.mu.Unlock()
	for _, t := range all {
		t.wake()
	}
}

// run reads the list of tenants every Interval, or when poked, starts a
// worker for each new tenant and retires the worker of each one gone.
func (s *Supervisor) run() {
	defer s.wg.Done()
	for {
		s.syncTenants()
		t := time.NewTimer(s.cfg.Interval)
		select {
		case <-s.stop:
			t.Stop()
			s.retireAll()
			return
		case <-s.poke:
		case <-t.C:
		}
		t.Stop()
	}
}

// syncTenants starts a worker for each tenant the list holds that has
// none, and retires the worker of each tenant it no longer holds. A list
// that cannot be read changes nothing.
func (s *Supervisor) syncTenants() {
	ctx, cancel := context.WithTimeout(s.hard, opTimeout)
	defer cancel()
	names, err := s.cfg.Tenants.Names(ctx)
	if err != nil {
		s.log.Warn("deid jobs: the list of tenants could not be read; nothing changes", "reason", fault.Message(err))
		return
	}
	want := make(map[string]bool, len(names))
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, n := range names {
		want[n] = true
		if s.tenants[n] != nil {
			continue
		}
		t := &tenantRun{sup: s, name: n, log: s.log.With("tenant", n), poke: make(chan struct{}, 1), retired: make(chan struct{})}
		s.tenants[n] = t
		s.wg.Add(1)
		go t.run()
	}
	for n, t := range s.tenants {
		if !want[n] {
			delete(s.tenants, n)
			t.retire()
		}
	}
}

// retireAll retires every tenant's worker, at Stop.
func (s *Supervisor) retireAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for n, t := range s.tenants {
		delete(s.tenants, n)
		t.retire()
	}
}

// tenantRun is the worker of one tenant: it claims the tenant's units of
// deid jobs while places are free and works each on its own goroutine.
type tenantRun struct {
	sup  *Supervisor
	name string
	log  *slog.Logger

	poke    chan struct{}
	retired chan struct{}
	once    sync.Once
	// running counts the tenant's units in flight.
	running atomic.Int32
	// reclaimed says the tenant's units were reclaimed once, as the
	// process started working it.
	reclaimed bool
}

// wake makes the worker look for work at once. It never blocks.
func (t *tenantRun) wake() {
	select {
	case t.poke <- struct{}{}:
	default:
	}
}

// retire ends the worker: its units in flight give their units back.
func (t *tenantRun) retire() { t.once.Do(func() { close(t.retired) }) }

// isRetired reports whether the worker was retired.
func (t *tenantRun) isRetired() bool {
	select {
	case <-t.retired:
		return true
	default:
		return false
	}
}

// run looks for work until retired.
func (t *tenantRun) run() {
	defer t.sup.wg.Done()
	wait := t.sup.cfg.PollInterval
	for {
		if t.claimAll() {
			wait = t.sup.cfg.PollInterval
		}
		timer := time.NewTimer(wait)
		select {
		case <-t.retired:
			timer.Stop()
			return
		case <-t.poke:
		case <-timer.C:
			wait = min(2*wait, MaxPollInterval)
		}
		timer.Stop()
	}
}

// pipeline returns the tenant's database, or nil while it is not ready.
func (t *tenantRun) pipeline() (tenant.Pipeline, tenant.Store) {
	st, err := t.sup.cfg.Tenants.Store(t.name)
	if err != nil {
		return nil, nil
	}
	p, _ := st.(tenant.Pipeline)
	return p, st
}

// claimAll claims and starts units while the tenant's and the server's
// places allow, and reports whether it started any.
func (t *tenantRun) claimAll() bool {
	p, st := t.pipeline()
	if p == nil || t.isRetired() {
		return false
	}
	t.reclaim(p)
	started := false
	for t.running.Load() < MaxTenantUnits && !t.isRetired() {
		select {
		case t.sup.units <- struct{}{}:
		default:
			return started
		}
		ctx, cancel := context.WithTimeout(t.sup.hard, opTimeout)
		w, err := p.ClaimWork(ctx, tenant.DeidWorkKind, "deid-"+t.name, t.sup.cfg.Now(), Lease)
		cancel()
		if err != nil {
			<-t.sup.units
			if fault.KindOf(err) != fault.NotFound && t.sup.hard.Err() == nil {
				t.log.Warn("deid jobs: looking for work failed", "reason", fault.Message(err))
			}
			return started
		}
		started = true
		t.running.Add(1)
		t.sup.wg.Add(1)
		go func() {
			defer t.sup.wg.Done()
			defer func() {
				t.running.Add(-1)
				<-t.sup.units
				t.sup.wakeAll()
			}()
			t.work(p, st, w)
		}()
	}
	return started
}

// reclaim gives back, once per process and before the tenant's first
// claim, the units of deid jobs a server that is gone left leased, so they
// are taken up at once rather than when their lease runs out.
func (t *tenantRun) reclaim(p tenant.Pipeline) {
	if t.reclaimed {
		return
	}
	t.reclaimed = true
	ctx, cancel := context.WithTimeout(t.sup.hard, opTimeout)
	defer cancel()
	n, err := p.ReclaimWorkOfKinds(ctx, []string{tenant.DeidWorkKind}, t.sup.cfg.Now())
	if err != nil {
		t.log.Warn("deid jobs: the units a stopped process held could not be taken back; their leases run out", "reason", fault.Message(err))
		return
	}
	if n > 0 {
		t.log.Info("deid jobs: units a stopped process held were taken back", "units", n)
	}
}
