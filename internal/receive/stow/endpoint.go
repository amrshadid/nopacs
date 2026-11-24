package stow

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/receive"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// The defaults of a Config.
const (
	// DefaultReadStall is how long one read of a request's body may wait
	// for the client: 5 minutes, the server's bound on a client that stops
	// reading a long answer, so a stalled sender and a stalled reader are
	// held alike.
	DefaultReadStall = 5 * time.Minute
	// DefaultDrain is how long Stop lets the requests in flight run before
	// it cuts them: as long as a DICOM listener lets its stores run.
	DefaultDrain = 30 * time.Second
)

// Refusal says why the archive cannot take files now, "" when it can: the
// supervisor's gates (the tenant not ready, a disk below its reserve, the
// tenant at its incoming limit).
type Refusal func() tenant.AssociationFailure

// Report is told of each request turned away for a reason the receiver's
// health shows; it must not block.
type Report func(failure tenant.AssociationFailure, caller tenant.GroupCaller)

// Config is what an Endpoint is made from.
type Config struct {
	// Tenant names the tenant.
	Tenant string
	// Receiver is the stow receiver's row.
	Receiver tenant.Receiver
	// Sink takes each part: the tenant's pipeline, whichever runs.
	Sink receive.Sink
	// Directory is where the endpoint is found while it runs.
	Directory *Directory
	// Refusal and Report are the supervisor's; nil never refuses and
	// reports nothing.
	Refusal Refusal
	Report  Report
	// ReadStall bounds one read of a body; 0 is DefaultReadStall.
	ReadStall time.Duration
	// Drain bounds how long Stop waits for the requests in flight; 0 is
	// DefaultDrain.
	Drain time.Duration
	// Logger receives the endpoint's lines: names, counts and reasons,
	// never a UID, a key or an address. Nil discards.
	Logger *slog.Logger
}

// Endpoint is one stow receiver while it runs: found in its Directory
// between Start and Stop, serving the requests of the keys it lists.
type Endpoint struct {
	cfg Config
	log *slog.Logger

	mu       sync.Mutex
	started  bool
	stopping bool
	// active cuts each request in flight: its context canceled and its
	// body's reads ended, for a Stop past its drain.
	active map[*inFlight]struct{}
	wg     sync.WaitGroup
}

// inFlight is one request in flight, as Stop can cut it.
type inFlight struct {
	// cancel ends the request's context, and with it a part the Sink
	// holds; deadline ends a read of the body waiting for the client.
	cancel, deadline func()
	// stopped says the request was cut: its body's reads fail from then
	// on (stallReader) and it is answered as the receiver stops.
	stopped atomic.Bool
	// refusedBy is the request's Request.Refused, nil for none.
	refusedBy func() bool
}

// refused reports whether the request's caller was refused while it was
// open.
func (f *inFlight) refused() bool { return f.refusedBy != nil && f.refusedBy() }

// cut ends the request: set first, so no read after it extends the
// deadline it then sets.
func (f *inFlight) cut() {
	f.stopped.Store(true)
	f.cancel()
	f.deadline()
}

// New checks cfg and returns an endpoint that serves nothing until Start.
func New(cfg Config) (*Endpoint, error) {
	switch {
	case cfg.Receiver.Type != tenant.ReceiverSTOW:
		return nil, fault.New(fault.Internal, "stow: the receiver is not a stow receiver")
	case cfg.Sink == nil || cfg.Directory == nil || cfg.Tenant == "":
		return nil, fault.New(fault.Internal, "stow: a tenant, a sink and a directory are required")
	}
	if cfg.ReadStall <= 0 {
		cfg.ReadStall = DefaultReadStall
	}
	if cfg.Drain <= 0 {
		cfg.Drain = DefaultDrain
	}
	if cfg.Refusal == nil {
		cfg.Refusal = func() tenant.AssociationFailure { return "" }
	}
	if cfg.Report == nil {
		cfg.Report = func(tenant.AssociationFailure, tenant.GroupCaller) {}
	}
	log := cfg.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Endpoint{cfg: cfg, log: log.With("receiver", cfg.Receiver.Name), active: map[*inFlight]struct{}{}}, nil
}

// Receiver returns the row the endpoint was made from.
func (e *Endpoint) Receiver() tenant.Receiver { return e.cfg.Receiver }

// Start makes the endpoint found in its Directory. An endpoint is started
// once; a second Start, or a Start after Stop, is refused.
func (e *Endpoint) Start(context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.started || e.stopping {
		return fault.New(fault.Internal, "stow: an endpoint is started once")
	}
	e.started = true
	e.cfg.Directory.add(e)
	e.log.Info("receiver: started")
	return nil
}

// Stop removes the endpoint from its Directory, so no new request reaches
// it, lets the requests in flight finish within the drain bound or while
// ctx lasts, then cuts the rest: a part not yet kept is not kept, and its
// request is answered as the server stops. It returns once no request is
// in flight.
func (e *Endpoint) Stop(ctx context.Context) error {
	e.mu.Lock()
	already := e.stopping
	e.stopping = true
	e.mu.Unlock()
	if already {
		return nil
	}
	e.cfg.Directory.remove(e)
	done := make(chan struct{})
	go func() { e.wg.Wait(); close(done) }()
	bounded, cancel := context.WithTimeout(ctx, e.cfg.Drain)
	defer cancel()
	cut := false
	select {
	case <-done:
	case <-bounded.Done():
		cut = true
		e.mu.Lock()
		for f := range e.active {
			f.cut()
		}
		e.mu.Unlock()
		<-done
	}
	e.log.Info("receiver: stopped", "requests_cut_short", cut)
	return nil
}

// enter counts a request in flight, or reports that the endpoint is
// stopping and the request is to be refused. Checked under mu, where Stop
// sets stopping, so no request is counted after Stop starts waiting.
func (e *Endpoint) enter(f *inFlight) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.stopping {
		return false
	}
	e.wg.Add(1)
	e.active[f] = struct{}{}
	return true
}

// leave ends a request entered.
func (e *Endpoint) leave(f *inFlight) {
	e.mu.Lock()
	delete(e.active, f)
	e.mu.Unlock()
	e.wg.Done()
}

// Directory holds the stow endpoints that run, by tenant, for the HTTP
// route to find the one that takes a key's requests. It is safe for use by
// many goroutines.
type Directory struct {
	mu       sync.RWMutex
	byTenant map[string]map[*Endpoint]struct{}
}

// NewDirectory returns an empty Directory.
func NewDirectory() *Directory {
	return &Directory{byTenant: map[string]map[*Endpoint]struct{}{}}
}

// Find returns the running endpoint of the tenant named tenantName whose
// receiver takes requests made with the key whose id is keyID, and whether
// one does. A key is listed by at most one stow receiver of a tenant (the
// tenant port refuses a second), so at most one endpoint answers; while a
// change of receivers is being applied two may for an instant, and the
// first by name is taken.
func (d *Directory) Find(tenantName, keyID string) (*Endpoint, bool) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	var found *Endpoint
	for e := range d.byTenant[tenantName] {
		if e.cfg.Receiver.STOW.Accepts(keyID) && (found == nil || e.cfg.Receiver.Name < found.cfg.Receiver.Name) {
			found = e
		}
	}
	return found, found != nil
}

// add makes e found.
func (d *Directory) add(e *Endpoint) {
	d.mu.Lock()
	defer d.mu.Unlock()
	m := d.byTenant[e.cfg.Tenant]
	if m == nil {
		m = map[*Endpoint]struct{}{}
		d.byTenant[e.cfg.Tenant] = m
	}
	m[e] = struct{}{}
}

// remove makes e no longer found.
func (d *Directory) remove(e *Endpoint) {
	d.mu.Lock()
	defer d.mu.Unlock()
	m := d.byTenant[e.cfg.Tenant]
	delete(m, e)
	if len(m) == 0 {
		delete(d.byTenant, e.cfg.Tenant)
	}
}
