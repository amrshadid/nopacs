package supervise

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"sync"
	"time"

	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/receive"
	"github.com/amrshadid/nopacs/internal/settingvalue"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// Tenants is where the supervisor learns which tenants exist and reaches
// each one's database.
type Tenants interface {
	// Names returns the names of every tenant of the installation.
	Names(ctx context.Context) ([]string, error)
	// Store returns the open database of the tenant named name, for as long
	// as it stays open: a store that is replaced (reopened) is no longer
	// used once the supervisor sees the new one. A tenant that is not ready
	// is an error.
	Store(name string) (tenant.Store, error)
}

// Installation reads the installation's settings: master.Store is one.
type Installation = settingvalue.Source

// values returns the reader of the settings that apply (package
// settingvalue, the one the API's handlers read through), so a value is
// read live and means one thing for both.
func (s *Supervisor) values() settingvalue.Reader {
	return settingvalue.Reader{Master: s.cfg.Installation}
}

// Pipeline is one tenant's reception pipeline as the supervisor runs it:
// started, taking files, until Stop.
type Pipeline interface {
	receive.Sink
	// Stop stops it, letting what is in flight finish while ctx lasts.
	Stop(ctx context.Context) error
}

// Gates are the refusals a tenant's pipeline makes itself for any source,
// read from the supervisor's last check of space and limits.
type Gates struct {
	// LowDisk says the incoming area's disk is below its reserve.
	LowDisk receive.Check
	// OverLimit says the tenant holds as many incoming bytes as it may.
	OverLimit receive.Check
}

// StartPipeline makes and starts the reception pipeline of the tenant named
// name over its database st.
type StartPipeline func(ctx context.Context, name string, st tenant.Pipeline, gates Gates) (Pipeline, error)

// Listener is one receiver's listener: started once, stopped once. A Start
// error that has a method Down() (tenant.ReceiverDownReason, string) gives
// the receiver's down reason and phrase; any other is "other".
type Listener interface {
	// Start starts listening; it returns once the listener admits callers
	// or could not start.
	Start(ctx context.Context) error
	// Stop stops it, finishing the stores in flight within its bound or
	// while ctx lasts.
	Stop(ctx context.Context) error
}

// Checker is a Listener that can say it must no longer serve, such as one
// whose TLS certificate has expired since it started. The supervisor asks
// it at every reconciliation, at least every SpaceInterval; an error that
// gives a down reason (a method Down, as a start error) stops the listener
// and shows the receiver down with it, and the listener is started again
// after the start backoff, which reads its files again.
type Checker interface {
	// Check returns nil while the listener may go on serving at now.
	Check(now time.Time) error
}

// ListenerSpec is what a listener is made from.
type ListenerSpec struct {
	// Tenant names the tenant.
	Tenant string
	// Receiver is the receiver's row.
	Receiver tenant.Receiver
	// BindAddress is the address to listen on: the receiver's, or the
	// installation's default; empty for every address.
	BindAddress string
	// Sink takes what it receives: the tenant's pipeline, whichever runs.
	Sink receive.Sink
	// Refusal says why the archive cannot take files now, "" when it can.
	Refusal func() tenant.AssociationFailure
	// Report is told of each caller turned away; it never blocks.
	Report func(failure tenant.AssociationFailure, caller tenant.GroupCaller)
}

// NewListener makes a listener; it does not start it.
type NewListener func(spec ListenerSpec) (Listener, error)

// OpenStorage opens one of a tenant's storages by name, for its free space;
// the caller closes it.
type OpenStorage func(ctx context.Context, tenantName string, st tenant.Pipeline, name string) (blob.Store, error)

// The defaults of a Config.
const (
	// DefaultInterval is how often each tenant's receivers are read again,
	// and the list of tenants: a backstop only, since the API's handlers
	// tell the supervisor of every change of a tenant's receivers at once
	// (Poke), so a change made by a handler of another process, or one
	// whose notice was lost, is still acted on within it.
	DefaultInterval = 30 * time.Second
	// DefaultSpaceInterval is how often free space and the incoming limit
	// are checked: a disk filling at 1 GB/s passes a 10 GB reserve in ten
	// seconds, so a check every five sees it before it is gone.
	DefaultSpaceInterval = 5 * time.Second
	// DefaultMinBackoff and DefaultMaxBackoff bound the wait before a
	// listener that could not start is tried again.
	DefaultMinBackoff = time.Second
	DefaultMaxBackoff = time.Minute
	// DefaultStopTimeout bounds a pipeline's stop when the server stops or
	// its tenant's database is replaced.
	DefaultStopTimeout = 30 * time.Second
	// DefaultStartTimeout bounds a pipeline's start: its Recover reads
	// every file the database names in the incoming area and every file
	// there, so a large leftover after a crash takes longer than a stop,
	// and a start cut short is tried again from the beginning; a bound as
	// short as the stop's could keep a tenant's receivers down for good.
	DefaultStartTimeout = 15 * time.Minute
	// opTimeout bounds one read or write of a tenant's database the
	// supervisor makes, so a database that stops answering holds its own
	// worker only for that long.
	opTimeout = 10 * time.Second
)

// Config is what a Supervisor is handed. Every port is given.
type Config struct {
	// Tenants lists the tenants and opens their databases.
	Tenants Tenants
	// Installation reads the installation's settings.
	Installation Installation
	// StartPipeline starts a tenant's pipeline.
	StartPipeline StartPipeline
	// NewListener makes a receiver's listener.
	NewListener NewListener
	// IncomingUsage reports the space of the incoming area's disk; nil is
	// never full.
	IncomingUsage func(ctx context.Context) (blob.Usage, error)
	// OpenStorage opens a tenant's storage for its free space; nil leaves
	// every storage's space unknown, which is never full.
	OpenStorage OpenStorage
	// Wake is called with a tenant's name after the supervisor wrote events
	// of it; nil does nothing.
	Wake func(tenantName string)
	// Now is the clock; nil is time.Now.
	Now func() time.Time
	// Interval is how often each tenant is read again; 0 is
	// DefaultInterval.
	Interval time.Duration
	// SpaceInterval is how often space and limits are checked; 0 is
	// DefaultSpaceInterval.
	SpaceInterval time.Duration
	// MinBackoff and MaxBackoff bound a failed listener's retry; 0 is the
	// default.
	MinBackoff, MaxBackoff time.Duration
	// StopTimeout bounds a pipeline's stop; 0 is DefaultStopTimeout.
	StopTimeout time.Duration
	// StartTimeout bounds a pipeline's start; 0 is DefaultStartTimeout. A
	// start also ends when the supervisor stops or the tenant goes.
	StartTimeout time.Duration
	// Logger receives the supervisor's lines: names, reasons and counts.
	// Nil discards them.
	Logger *slog.Logger
}

// Supervisor runs every tenant's pipeline and receivers. Make it with New,
// run it with Start, end it with Stop.
type Supervisor struct {
	cfg Config
	log *slog.Logger

	mu      sync.Mutex
	tenants map[string]*tenantRun
	started bool
	stopped bool

	// health is each receiver's health by tenant and name, read by Health
	// on the API's path.
	healthMu sync.RWMutex
	health   map[healthKey]healthEntry

	poke chan struct{}
	stop chan struct{}
	ctx  context.Context
	end  context.CancelFunc
	wg   sync.WaitGroup
}

// New checks cfg and returns a supervisor that runs nothing until Start.
func New(cfg Config) (*Supervisor, error) {
	switch {
	case cfg.Tenants == nil:
		return nil, fault.New(fault.Internal, "the supervisor is given no tenants")
	case cfg.Installation == nil:
		return nil, fault.New(fault.Internal, "the supervisor is given no installation settings")
	case cfg.StartPipeline == nil:
		return nil, fault.New(fault.Internal, "the supervisor is given no way to start a pipeline")
	case cfg.NewListener == nil:
		return nil, fault.New(fault.Internal, "the supervisor is given no way to make a listener")
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Wake == nil {
		cfg.Wake = func(string) {}
	}
	if cfg.Interval <= 0 {
		cfg.Interval = DefaultInterval
	}
	if cfg.SpaceInterval <= 0 {
		cfg.SpaceInterval = DefaultSpaceInterval
	}
	if cfg.MinBackoff <= 0 {
		cfg.MinBackoff = DefaultMinBackoff
	}
	if cfg.MaxBackoff < cfg.MinBackoff {
		cfg.MaxBackoff = max(DefaultMaxBackoff, cfg.MinBackoff)
	}
	if cfg.StopTimeout <= 0 {
		cfg.StopTimeout = DefaultStopTimeout
	}
	if cfg.StartTimeout <= 0 {
		cfg.StartTimeout = DefaultStartTimeout
	}
	log := cfg.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	s := &Supervisor{
		cfg: cfg, log: log, tenants: map[string]*tenantRun{}, health: map[healthKey]healthEntry{},
		poke: make(chan struct{}, 1), stop: make(chan struct{}),
	}
	s.ctx, s.end = context.WithCancel(context.Background())
	return s, nil
}

// Start reads the tenants and starts a worker for each, which starts its
// pipeline and the listeners its rows ask for; it returns without waiting
// for them. A second Start is an error.
func (s *Supervisor) Start(context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.started {
		return fault.New(fault.Internal, "the supervisor is already running")
	}
	s.started = true
	s.wg.Add(1)
	go s.run()
	return nil
}

// Stop stops every worker: each marks its receivers stopping, stops its
// listeners (each finishing its stores in flight within its bound) and then
// its pipeline. It returns once all have ended, or when ctx ends, with the
// workers told to give up waiting.
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
		s.end()
		return nil
	case <-ctx.Done():
		s.end()
		<-done
		return fault.Wrap(fault.Unavailable, ctx.Err(), "the receivers did not all stop in time; stores still in flight were cut short")
	}
}

// Poke makes the worker of the tenant named name read its receivers at
// once, and the supervisor read the list of tenants when it knows no such
// tenant. It never blocks.
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

// Drained reports whether the tenant named name has no reception pipeline
// running, none starting and none stopping: what it held is archived, quarantined or left
// in incoming with its place kept, and nothing more is written for it here
// until a pipeline starts again. A tenant the supervisor runs no worker for
// is drained. An export asks it, after Poke, before it takes its snapshot.
func (s *Supervisor) Drained(name string) bool {
	s.mu.Lock()
	t := s.tenants[name]
	s.mu.Unlock()
	if t == nil {
		return true
	}
	// starting first: restartPipeline sets it before it asks the tenant
	// again and clears it only after it published the pipeline it started,
	// so starting clear here means a start that set it before is over (its
	// pipeline in the view read next), and one that sets it after asks the
	// tenant after it was made read only, and starts nothing. Then the
	// view: stopPipeline sets stopping before it publishes a view without
	// the pipeline, and clears it only once the pipeline has stopped, so a
	// view without one read before stopping is clear means the stop is
	// over.
	if t.starting.Load() {
		return false
	}
	v := t.view.Load()
	if v != nil && v.pipeline != nil {
		return false
	}
	return !t.stopping.Load()
}

// run reads the list of tenants every Interval, or when poked, and starts
// a worker for each new tenant and retires the worker of each one gone.
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

// syncTenants starts a worker for each tenant the list holds that has none,
// and retires the worker of each tenant the list no longer holds. A list
// that cannot be read changes nothing.
func (s *Supervisor) syncTenants() {
	ctx, cancel := context.WithTimeout(s.ctx, opTimeout)
	defer cancel()
	names, err := s.cfg.Tenants.Names(ctx)
	if err != nil {
		s.log.Warn("receivers: the list of tenants could not be read; nothing changes", "reason", fault.Message(err))
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

// retireAll retires every worker, at Stop.
func (s *Supervisor) retireAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for n, t := range s.tenants {
		delete(s.tenants, n)
		t.retire()
	}
}

// newKey returns a random key for an event the supervisor appends.
func newKey() string {
	var b [16]byte
	_, _ = rand.Read(b[:]) // crypto/rand.Read never fails (Go 1.24 and later).
	return "s" + hex.EncodeToString(b[:])
}
