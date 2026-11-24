package send

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"sync"
	"time"

	"github.com/amrshadid/nopacs/internal/checkedread"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// Supervisor runs every tenant's send workers. Make it with New, run it
// with Start, end it with Stop.
type Supervisor struct {
	cfg  Config
	log  *slog.Logger
	inst *assocLimit
	// reads bounds the senders' share of the server's read slots.
	reads *assocLimit

	mu      sync.Mutex
	tenants map[string]*tenantRun
	started bool
	stopped bool

	poke chan struct{}
	stop chan struct{}
	// hard ends everything in flight: canceled when Stop's ctx ends
	// before the workers have given their units back.
	hard   context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// stopGrace is how long Stop waits, after its bound, for workers that it
// cut off.
const stopGrace = time.Second

// New checks cfg and returns a supervisor that runs nothing until Start.
func New(cfg Config) (*Supervisor, error) {
	cfg, err := cfg.check()
	if err != nil {
		return nil, err
	}
	s := &Supervisor{
		cfg: cfg, log: cfg.Logger, inst: newAssocLimit(DefaultInstallationAssociations), reads: newAssocLimit(MaxSenderReads),
		tenants: map[string]*tenantRun{}, poke: make(chan struct{}, 1), stop: make(chan struct{}),
	}
	s.hard, s.cancel = context.WithCancel(context.Background())
	return s, nil
}

// Start starts the reading of the tenants, which starts a worker for each;
// it returns without waiting for them. A second Start is an error.
func (s *Supervisor) Start(context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.started {
		return fault.New(fault.Internal, "send: the supervisor is already running")
	}
	s.started = true
	s.wg.Add(1)
	go s.run()
	return nil
}

// Stop stops every worker: each finishes the instance it is sending,
// records what the peer answered, and gives its unit back without spending
// an attempt. It returns once all have ended, or when ctx ends, with what is
// still in flight cut short (the next claim resends at most one batch).
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
		// A write blocked on a peer that stopped reading does not see the
		// cancel (go-dicom cannot cut it); its own deadline ends it later.
		// Stop does not wait for that past a short grace: the units of
		// workers cut off write nothing more, and are taken again after
		// their lease.
		t := time.NewTimer(stopGrace)
		defer t.Stop()
		select {
		case <-done:
		case <-t.C:
		}
		return fault.Wrap(fault.Unavailable, ctx.Err(), "the senders did not all stop in time; sends in flight were cut short")
	}
}

// Poke makes the tenant named name read its senders and look for work at
// once: the API calls it after every change of a tenant's senders and
// every job it makes, cancels or retries. It never blocks.
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
// the sends of a tenant the registry has just let go stop now rather than
// at the next Interval: each unit in flight is given back after its
// instance in flight, while the tenant's database is still open to take
// it. It never blocks.
func (s *Supervisor) TenantRemoved(string) {
	select {
	case s.poke <- struct{}{}:
	default:
	}
}

// Health returns the health of the tenant's sender named sender, as the
// contract's SenderHealth with the sender's name; false when the server
// knows nothing of it yet (unknown, since the server started).
func (s *Supervisor) Health(tenantName, sender string) (tenant.SenderHealthChanged, bool) {
	s.mu.Lock()
	t := s.tenants[tenantName]
	s.mu.Unlock()
	if t == nil {
		return tenant.SenderHealthChanged{}, false
	}
	return t.health(sender)
}

// SendersChanged is Poke: the API calls it after a change of a tenant's
// senders or jobs.
func (s *Supervisor) SendersChanged(tenantName string) { s.Poke(tenantName) }

// OpenAssociations returns how many associations (for a stow sender,
// requests) the tenant's sender named sender holds now, its probes
// included.
func (s *Supervisor) OpenAssociations(tenantName, sender string) int {
	s.mu.Lock()
	t := s.tenants[tenantName]
	s.mu.Unlock()
	if t == nil {
		return 0
	}
	t.mu.Lock()
	sr := t.senders[sender]
	t.mu.Unlock()
	if sr == nil {
		return 0
	}
	return int(sr.open.Load())
}

// run reads the list of tenants and the installation's bound every
// Interval, or when poked, starts a worker for each new tenant and retires
// the worker of each one gone.
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

// syncTenants reads the installation's bound, starts a worker for each
// tenant the list holds that has none, and retires the worker of each
// tenant the list no longer holds. A list that cannot be read changes
// nothing.
func (s *Supervisor) syncTenants() {
	ctx, cancel := context.WithTimeout(s.hard, opTimeout)
	defer cancel()
	if n, err := s.cfg.Installation.MaxAssociations(ctx); err == nil {
		s.inst.setMax(n)
	} else {
		s.log.Warn("senders: sender.max-associations could not be read; the last value holds", "reason", fault.Message(err))
	}
	names, err := s.cfg.Tenants.Names(ctx)
	if err != nil {
		s.log.Warn("senders: the list of tenants could not be read; nothing changes", "reason", fault.Message(err))
		return
	}
	want := make(map[string]bool, len(names))
	for _, n := range names {
		want[n] = true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, n := range names {
		if s.tenants[n] != nil {
			continue
		}
		t := newTenantRun(s, n)
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

// MaxSenderReads is the most of the server's read slots
// (checkedread.MaxServerReads, which the API's streams and downloads count
// in too) all senders hold at once: half. The senders may hold up to
// sender.max-associations (1024) associations, each reading an instance; a
// share of their own leaves the other half to people and programs reading
// through the API, whose downloads would otherwise be refused while the
// senders are busy, and half still lets 128 associations send at once,
// four times the installation's default bound.
const MaxSenderReads = checkedread.MaxServerReads / 2

// takeRead takes one read slot for owner, within the senders' share
// (MaxSenderReads) and the server's bounds (checkedread.Slots); a slot
// past either is refused as fault.Exhausted.
func (s *Supervisor) takeRead(owner string) (func(), error) {
	if !s.reads.take() {
		return nil, fault.New(fault.Exhausted, "the senders hold as many reads of archived bytes as their share of the server's allows")
	}
	release, err := s.cfg.Reads.Take(owner)
	if err != nil {
		s.reads.give()
		return nil, err
	}
	return func() {
		release()
		s.reads.give()
	}, nil
}

// newKey returns a random key for an event the supervisor appends.
func newKey() string {
	var b [16]byte
	_, _ = rand.Read(b[:]) // crypto/rand.Read never fails (Go 1.24 and later).
	return "s" + hex.EncodeToString(b[:])
}
