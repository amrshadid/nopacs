package supervise_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/driver"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/receive"
	"github.com/amrshadid/nopacs/internal/receive/supervise"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// t0 is when the test's rows are made.
var t0 = time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)

// clock is a clock a test moves by hand.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

// tenants is the supervisor's view of one tenant, "lab", on the real
// SQLite driver; its store can be swapped for a reopened one, or made
// unavailable.
type tenants struct {
	mu      sync.Mutex
	store   tenant.Store
	down    bool
	dataDir string
	// calls counts the asks for the store; from the ask movedFrom on (0 for
	// never) the tenant is refused as read only, as a moved one is.
	calls, movedFrom int
}

func (ts *tenants) Names(context.Context) ([]string, error) { return []string{"lab"}, nil }

func (ts *tenants) Store(name string) (tenant.Store, error) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	if name != "lab" {
		return nil, fault.New(fault.NotFound, "no such tenant")
	}
	if ts.down {
		return nil, fault.New(fault.Unavailable, "the tenant lab is unreachable")
	}
	ts.calls++
	if ts.movedFrom > 0 && ts.calls >= ts.movedFrom {
		return nil, fault.New(fault.Precondition, "the tenant lab is moved")
	}
	return ts.store, nil
}

// pipe returns the tenant's store as the pipeline's port.
func (ts *tenants) pipe() tenant.Pipeline {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	return ts.store.(tenant.Pipeline)
}

// reopen opens the tenant's database again and makes it the one served.
func (ts *tenants) reopen(t *testing.T) {
	t.Helper()
	s := openStore(t, ts.dataDir)
	ts.mu.Lock()
	ts.store = s
	ts.mu.Unlock()
}

// openStore opens the tenant's SQLite database.
func openStore(t *testing.T, dataDir string) tenant.Store {
	t.Helper()
	s, err := (driver.SQLiteTenants{DataDir: dataDir}).Open(context.Background(), "T0000000000001", "lab", "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	return s
}

// installation is the installation's settings.
type installation struct {
	mu sync.Mutex
	v  map[string]string
}

func (i *installation) Setting(_ context.Context, name string) (string, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if v, ok := i.v[name]; ok {
		return v, nil
	}
	return "", fault.New(fault.NotFound, "no such setting")
}

// listener is a fake listener: it records its spec and its stops, and
// fails to start while its port is in taken.
type listener struct {
	spec    supervise.ListenerSpec
	stopped atomic.Bool
	// expires, when set, is when its certificate ends (Check).
	expires *time.Time
}

// Check answers as a listener whose certificate ends at expires.
func (l *listener) Check(now time.Time) error {
	if l.expires != nil && now.After(*l.expires) {
		return &downError{reason: tenant.DownTLS, detail: tenant.PhraseCertificateExpired}
	}
	return nil
}

func (l *listener) Start(context.Context) error { return nil }
func (l *listener) Stop(context.Context) error  { l.stopped.Store(true); return nil }

// downError is a start failure that says its reason as a listener's does.
type downError struct {
	reason tenant.ReceiverDownReason
	detail string
}

func (e *downError) Error() string                             { return e.detail }
func (e *downError) Down() (tenant.ReceiverDownReason, string) { return e.reason, e.detail }

// pipeline is a fake pipeline.
type pipeline struct {
	store   tenant.Pipeline
	stopped atomic.Bool
	gates   supervise.Gates
	// duringStop, when set, runs inside Stop, before it returns.
	duringStop func()

	mu sync.Mutex
	// removed lists the removals handed to it, in order: "rejected:<path>"
	// and "discarded:<reception>".
	removed []string
}

// removals returns the removals handed to the fake.
func (p *pipeline) removals() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.removed)
}

func (p *pipeline) Accept(context.Context, receive.Arrival) receive.Outcome {
	return receive.Outcome{Status: receive.Received}
}
func (p *pipeline) Stop(context.Context) error {
	if p.duringStop != nil {
		p.duringStop()
	}
	p.stopped.Store(true)
	return nil
}

// The fake takes decisions as the reception pipeline does, doing nothing.
func (p *pipeline) Release(context.Context, tenant.Release, master.AuditEntry, tenant.Guard) (tenant.QuarantinedFile, error) {
	return tenant.QuarantinedFile{}, nil
}
func (p *pipeline) Retry(context.Context, string, string, string, master.AuditEntry, tenant.Guard) (tenant.Reception, tenant.Reception, error) {
	return tenant.Reception{}, tenant.Reception{}, nil
}
func (p *pipeline) RemoveRejected(path string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.removed = append(p.removed, "rejected:"+path)
	return nil
}
func (p *pipeline) RemoveDiscarded(_ context.Context, id string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.removed = append(p.removed, "discarded:"+id)
	return nil
}

// rig is a supervisor over one tenant with fake listeners and pipelines.
type rig struct {
	t       *testing.T
	ts      *tenants
	inst    *installation
	clock   *clock
	sup     *supervise.Supervisor
	free    atomic.Int64 // the incoming disk's free bytes
	total   int64
	storage atomic.Int64 // the storage's free bytes, -1 for unknown

	mu        sync.Mutex
	listeners []*listener
	pipelines []*pipeline
	taken     map[int]bool
	seq       atomic.Int64
	wakes     atomic.Int64
	// expires, when set, is the certificate end of every listener made.
	expires atomic.Pointer[time.Time]
	// startWait, when set, runs at the start of every pipeline, which
	// fails with its error; edit, when set, changes the supervisor's
	// configuration before it is made.
	startWait func(ctx context.Context) error
	edit      func(*supervise.Config)
}

// newRig makes the tenant with a storage "main" and starts nothing.
func newRig(t *testing.T) *rig {
	t.Helper()
	if testing.Short() {
		t.Skip("opens a database and waits on the supervisor's workers; left out under -short")
	}
	dir := t.TempDir()
	r := &rig{t: t, ts: &tenants{dataDir: dir}, inst: &installation{v: map[string]string{}}, clock: &clock{now: t0},
		total: 1 << 40, taken: map[int]bool{}}
	r.free.Store(1 << 39)
	r.storage.Store(-1)
	r.ts.store = openStore(t, dir)
	if _, err := r.ts.pipe().AddStorage(context.Background(), tenant.Storage{Name: "main", Type: tenant.StorageDirectory,
		Directory: tenant.DirectoryStorage{Root: "archive"}}, t0, r.audit(), nil); err != nil {
		t.Fatal(err)
	}
	return r
}

// start makes and starts the supervisor.
func (r *rig) start() {
	r.t.Helper()
	cfg := supervise.Config{
		Tenants: r.ts, Installation: r.inst, Now: r.clock.Now,
		Interval: time.Hour, SpaceInterval: time.Nanosecond, MinBackoff: time.Millisecond, MaxBackoff: 5 * time.Millisecond,
		StartPipeline: func(ctx context.Context, _ string, st tenant.Pipeline, g supervise.Gates) (supervise.Pipeline, error) {
			if r.startWait != nil {
				if err := r.startWait(ctx); err != nil {
					return nil, err
				}
			}
			p := &pipeline{store: st, gates: g}
			r.mu.Lock()
			r.pipelines = append(r.pipelines, p)
			r.mu.Unlock()
			return p, nil
		},
		NewListener: func(spec supervise.ListenerSpec) (supervise.Listener, error) {
			r.mu.Lock()
			defer r.mu.Unlock()
			if r.taken[spec.Receiver.DIMSE.Port] {
				return nil, &downError{reason: "address_in_use", detail: "the port is taken by another program"}
			}
			l := &listener{spec: spec, expires: r.expires.Load()}
			r.listeners = append(r.listeners, l)
			return l, nil
		},
		IncomingUsage: func(context.Context) (blob.Usage, error) {
			return blob.Usage{Known: true, Total: r.total, Free: r.free.Load(), Used: r.total - r.free.Load()}, nil
		},
		OpenStorage: func(context.Context, string, tenant.Pipeline, string) (blob.Store, error) {
			if r.storage.Load() < 0 {
				return nil, errors.New("unknown")
			}
			return usageStore{free: r.storage.Load(), total: r.total}, nil
		},
		Wake: func(string) { r.wakes.Add(1) },
	}
	if r.edit != nil {
		r.edit(&cfg)
	}
	sup, err := supervise.New(cfg)
	if err != nil {
		r.t.Fatal(err)
	}
	r.sup = sup
	if err := sup.Start(context.Background()); err != nil {
		r.t.Fatal(err)
	}
	r.t.Cleanup(func() { _ = sup.Stop(context.Background()) })
}

// usageStore is a storage that only reports its space.
type usageStore struct {
	blob.Store
	free, total int64
}

func (u usageStore) Usage(context.Context) (blob.Usage, error) {
	return blob.Usage{Known: true, Total: u.total, Free: u.free, Used: u.total - u.free}, nil
}
func (u usageStore) Close() error { return nil }

// audit returns a new audit entry for a change the test makes.
func (r *rig) audit() master.AuditEntry {
	return master.AuditEntry{ID: fmt.Sprintf("a-%d", r.seq.Add(1)), Time: t0, KeyID: "K1234567890a", RemoteAddress: "192.0.2.10:50000",
		Method: "Test", TenantName: "lab", Outcome: master.OutcomeDone, RequestID: "r1"}
}

// add adds a receiver on port.
func (r *rig) add(name string, port int, enabled bool) tenant.Receiver {
	r.t.Helper()
	row, err := r.ts.pipe().AddReceiver(context.Background(), tenant.Receiver{
		Name: name, Type: tenant.ReceiverDIMSE, Enabled: enabled, Storage: "main",
		QuietPeriod: 5 * time.Second, MaxOpenTime: time.Minute, MaxFileBytes: 1 << 30,
		DIMSE: tenant.DIMSEReceiver{Port: port, AETitle: "ARCHIVE", AnyCaller: true},
	}, r.clock.Now(), r.audit(), nil)
	if err != nil {
		r.t.Fatal(err)
	}
	return row
}

// set changes fields of a receiver, at a time after every earlier change.
func (r *rig) set(change tenant.Receiver, fields ...tenant.ReceiverField) tenant.Receiver {
	r.t.Helper()
	r.clock.advance(time.Second)
	row, err := r.ts.pipe().SetReceiver(context.Background(), change, fields, r.clock.Now(), r.audit(), nil)
	if err != nil {
		r.t.Fatal(err)
	}
	return row
}

// row reads a receiver's row.
func (r *rig) row(name string) tenant.Receiver {
	r.t.Helper()
	row, err := r.ts.pipe().Receiver(context.Background(), name)
	if err != nil {
		r.t.Fatal(err)
	}
	return row
}

// health asks the supervisor for a receiver's health as the API does, with
// its row as stored.
func (r *rig) health(name string) supervise.Health { return r.sup.Health("lab", r.row(name)) }

// running returns the listeners of a receiver not stopped.
func (r *rig) running(name string) []*listener {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []*listener
	for _, l := range r.listeners {
		if l.spec.Receiver.Name == name && !l.stopped.Load() {
			out = append(out, l)
		}
	}
	return out
}

// made returns every listener made for a receiver.
func (r *rig) made(name string) []*listener {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []*listener
	for _, l := range r.listeners {
		if l.spec.Receiver.Name == name {
			out = append(out, l)
		}
	}
	return out
}

// waitFor waits until cond holds, poking the tenant's worker each time, and
// fails after a generous real deadline. It polls, never sleeping for a
// result.
func (r *rig) waitFor(what string, cond func() bool) {
	r.t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			r.t.Fatalf("timed out waiting for %s", what)
		}
		r.sup.Poke("lab")
		time.Sleep(5 * time.Millisecond)
	}
}

// isUp reports whether a receiver's health is up.
func (r *rig) isUp(name string) func() bool {
	return func() bool { return r.health(name).State == tenant.ReceiverUp }
}

// healthEvents reads the tenant's receiver health events.
func (r *rig) healthEvents() []*tenant.ReceiverHealthChanged {
	r.t.Helper()
	var out []*tenant.ReceiverHealthChanged
	for _, e := range r.events(tenant.EventReceiverHealth) {
		out = append(out, e.Payload.(*tenant.ReceiverHealthChanged))
	}
	return out
}

// events reads the tenant's events of type typ.
func (r *rig) events(typ tenant.EventType) []tenant.Event {
	r.t.Helper()
	feed := r.ts.pipe().(tenant.Events)
	page, err := feed.ReadEvents(context.Background(), "", []tenant.EventType{typ}, tenant.MaxEventPage)
	if err != nil {
		r.t.Fatal(err)
	}
	return page.Events
}

// receivedStatus is what the fake pipeline answers.
const receivedStatus = receive.Received

// receiveArrival is a file handed in by a test.
func receiveArrival() receive.Arrival { return receive.Arrival{Receiver: "ct"} }
