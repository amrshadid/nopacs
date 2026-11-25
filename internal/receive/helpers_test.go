package receive

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/blob/dir"
	"github.com/amrshadid/nopacs/internal/core/naming"
	"github.com/amrshadid/nopacs/internal/core/reception"
	"github.com/amrshadid/nopacs/internal/driver"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/receive/header"
	"github.com/amrshadid/nopacs/internal/receive/incoming"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// t0 is the fake clock's start.
var t0 = time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)

// fakeClock is a clock a test moves by hand; its waits are real and short,
// scaled down a thousand times, so a worker polls without the test
// sleeping.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) After(d time.Duration) <-chan time.Time {
	return time.After(max(d/1000, time.Millisecond))
}

// Advance moves the clock forward.
func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

// env is a tenant on the real SQLite driver with a directory storage, a
// receiver, and a pipeline over them.
type env struct {
	t        testing.TB
	ctx      context.Context
	dataDir  string
	store    tenant.Pipeline
	storage  blob.Store
	storeDir string
	clock    *fakeClock
	p        *Pipeline
	cfg      Config
	notify   chan struct{}
	seq      atomic.Int64
	// wrap, when set, wraps the storage each step opens.
	wrap func(blob.Store) blob.Store
	// dbWrap, when set, wraps the database the pipeline is given.
	dbWrap func(tenant.Pipeline) tenant.Pipeline
	// manual says the pipeline is not started.
	manual bool
	// hook runs at each of the pipeline's named points.
	hook func(point string)
	// running counts the steps in flight, so a wait moves the clock only
	// between steps: a jump past a lease while a step runs would take its
	// unit over, which is a test of its own, not of what is waited for.
	running atomic.Int64
	// check is the check after de-identification, when the receiver
	// de-identifies (deid_helpers_test.go).
	check *recordingCheck
	// deadline bounds a wait in real time; 0 is a minute. A scale test,
	// whose speed is the disk's, gives itself more.
	deadline time.Duration
	// others are the storages of names other than "main", each a
	// directory of its own, opened when first asked for (storageNamed).
	othersMu sync.Mutex
	others   map[string]blob.Store
}

// envOptions change what newEnv sets up.
type envOptions struct {
	maxFileBytes int64
	naming       *naming.Spec
	sopClasses   []string
	workers      int
	// storage and db wrap the storage and the database the pipeline is
	// given, for a test of their failures.
	storage func(blob.Store) blob.Store
	db      func(tenant.Pipeline) tenant.Pipeline
	// manual makes the pipeline but does not start it: the test runs its
	// steps by hand.
	manual bool
	// hook runs at each of the pipeline's named points.
	hook func(point string)
	// postgres, when set, is the address of an empty Postgres schema the
	// tenant's database is made in, instead of a SQLite file.
	postgres string
	// headerLimit, when set, is the pipeline's reading of the header bound
	// (Config.HeaderLimit).
	headerLimit Limit
	// deid, when set, makes the receiver de-identify (deid_helpers_test.go).
	deid *deidOptions
	// forwarded, when set, is the pipeline's Config.Forwarded.
	forwarded func()
	// routing, when set, is the receiver's routing, set before the
	// pipeline starts, with the storages and senders it names added
	// (route_test.go).
	routing *tenant.Routing
	// pause, when set, is the pipeline's Config.Pause (pause_test.go).
	pause WritePause
}

// newEnv makes a tenant, its storage and receiver, and a started pipeline.
func newEnv(t testing.TB, opts envOptions) *env {
	t.Helper()
	return newEnvAt(t, t.TempDir(), t0, opts)
}

// newEnvAt is newEnv in the data directory given, with the clock starting
// at start; the tenant, its storage and its receiver are made only when
// they are not there, so a second process opens what the first made.
func newEnvAt(t testing.TB, dataDir string, start time.Time, opts envOptions) *env {
	t.Helper()
	e := &env{t: t, ctx: context.Background(), dataDir: dataDir, clock: &fakeClock{now: start}, notify: make(chan struct{}, 1),
		wrap: opts.storage, dbWrap: opts.db, manual: opts.manual, hook: opts.hook}
	e.storeDir = filepath.Join(e.dataDir, "storage")
	if err := os.MkdirAll(e.storeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if opts.postgres != "" {
		s, err := (driver.PostgresTenants{}).Open(e.ctx, "T0000000000001", "lab", opts.postgres)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Migrate(e.ctx); err != nil {
			t.Fatal(err)
		}
		e.store = s.(tenant.Pipeline)
		t.Cleanup(func() { _ = s.Close() })
	} else {
		e.openStore()
	}
	ds, err := dir.Open(e.storeDir)
	if err != nil {
		t.Fatal(err)
	}
	e.storage = ds
	t.Cleanup(func() { _ = ds.Close() })
	t.Cleanup(func() {
		e.othersMu.Lock()
		defer e.othersMu.Unlock()
		for _, o := range e.others {
			_ = o.Close()
		}
	})
	if _, err := e.store.AddStorage(e.ctx, tenant.Storage{Name: "main", Type: tenant.StorageDirectory,
		Directory: tenant.DirectoryStorage{Root: "archive"}}, t0, e.audit(), nil); err != nil && fault.KindOf(err) != fault.Conflict {
		t.Fatal(err)
	}
	if opts.maxFileBytes == 0 {
		opts.maxFileBytes = 1 << 34
	}
	if _, err := e.store.AddReceiver(e.ctx, tenant.Receiver{
		Name: "dimse", Type: tenant.ReceiverDIMSE, Enabled: true, Storage: "main", Naming: opts.naming,
		QuietPeriod: 5 * time.Second, MaxOpenTime: time.Minute, MaxFileBytes: opts.maxFileBytes,
		DIMSE: tenant.DIMSEReceiver{Port: 11112, AETitle: "ARCHIVE", AnyCaller: true, SOPClasses: opts.sopClasses},
	}, t0, e.audit(), nil); err != nil && fault.KindOf(err) != fault.Conflict {
		t.Fatal(err)
	}
	hasher, err := naming.NewHasher(bytes.Repeat([]byte{9}, naming.MinKeyLength))
	if err != nil {
		t.Fatal(err)
	}
	workers := opts.workers
	if workers == 0 {
		workers = 2
	}
	e.cfg = Config{
		Tenant: "lab", Store: e.store, Hasher: hasher, Clock: e.clock, HeaderLimit: opts.headerLimit,
		OpenStorage: func(_ context.Context, name string) (blob.Store, error) {
			s := e.storage
			if name != "main" {
				var err error
				if s, err = e.storageNamed(name); err != nil {
					return nil, err
				}
			}
			if e.wrap != nil {
				return e.wrap(s), nil
			}
			return s, nil
		},
		IncomingRoot: filepath.Join(e.dataDir, "incoming"), Workers: workers, Lease: 30 * time.Second,
		PollInterval: 2 * time.Second, BatchInterval: 5 * time.Second, RetryInterval: 20 * time.Second,
		Forwarded: opts.forwarded, Pause: opts.pause,
	}
	if opts.deid != nil {
		e.setUpDeid(opts.deid)
	}
	if opts.routing != nil {
		e.setRouting(*opts.routing)
	}
	e.start()
	return e
}

// storageNamed returns the storage of a name other than "main": a
// directory of its own beside main's, opened once. It is called from the
// pipeline's workers, so it returns its error rather than failing the test.
func (e *env) storageNamed(name string) (blob.Store, error) {
	e.othersMu.Lock()
	defer e.othersMu.Unlock()
	if s, ok := e.others[name]; ok {
		return s, nil
	}
	root := filepath.Join(e.dataDir, "storage-"+name)
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, err
	}
	s, err := dir.Open(root)
	if err != nil {
		return nil, err
	}
	if e.others == nil {
		e.others = map[string]blob.Store{}
	}
	e.others[name] = s
	return s, nil
}

// openStore opens the tenant's SQLite database through the driver package.
func (e *env) openStore() {
	s, err := (driver.SQLiteTenants{DataDir: e.dataDir}).Open(e.ctx, "T0000000000001", "lab", "")
	if err != nil {
		e.t.Fatal(err)
	}
	if err := s.Migrate(e.ctx); err != nil {
		e.t.Fatal(err)
	}
	ps, ok := s.(tenant.Pipeline)
	if !ok {
		e.t.Fatal("the SQLite tenant store does not implement tenant.Pipeline")
	}
	e.store = ps
	e.t.Cleanup(func() { _ = s.Close() })
}

// start makes and starts a pipeline over the env's store.
func (e *env) start() {
	e.t.Helper()
	cfg := e.cfg
	cfg.Store = e.store
	if e.dbWrap != nil {
		cfg.Store = e.dbWrap(e.store)
	}
	p, err := New(cfg)
	if err != nil {
		e.t.Fatal(err)
	}
	p.hook = func(point string) {
		if e.hook != nil {
			e.hook(point)
		}
		switch {
		case strings.HasPrefix(point, "step.start:"):
			e.running.Add(1)
		case strings.HasPrefix(point, "step.done:"), strings.HasPrefix(point, "step.failed:"),
			strings.HasPrefix(point, "step.lease-lost:"), strings.HasPrefix(point, "step.stopped:"):
			e.running.Add(-1)
		}
		select {
		case e.notify <- struct{}{}:
		default:
		}
	}
	e.p = p
	if e.manual {
		e.t.Cleanup(func() { _ = p.area.Close() })
		return
	}
	if _, err := p.Start(e.ctx); err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(func() { _ = p.Stop(context.Background()) })
}

// audit returns a new audit entry for a change the test makes.
func (e *env) audit() master.AuditEntry {
	return master.AuditEntry{
		ID: fmt.Sprintf("a%d-%08d", e.clock.Now().UnixNano(), e.seq.Add(1)), Time: t0, KeyID: "K1234567890a", RemoteAddress: "192.0.2.10:50000",
		Method: "Test", TenantName: "lab", Outcome: master.OutcomeDone, RequestID: "r1",
	}
}

// corpus reads a public test file.
func corpus(t testing.TB, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name)) // #nosec G703 -- a test's own file names.
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// studyFiles are the seven files of one CT study in two series.
var studyFiles = []string{"ct2n-6293.dcm", "ct2n-6924.dcm", "ct5n-2062.dcm", "ct5n-2392.dcm", "ct5n-2693.dcm", "ct5n-3023.dcm", "ct5n-3353.dcm"}

// send hands each named corpus file to the pipeline and requires each to be
// received.
func (e *env) send(names ...string) {
	e.t.Helper()
	for _, n := range names {
		e.sendBytes(corpus(e.t, n), Received)
	}
}

// sendBytes hands bytes to the pipeline and checks the outcome.
func (e *env) sendBytes(b []byte, want Status) Outcome {
	e.t.Helper()
	out := e.p.Accept(e.ctx, Arrival{Receiver: "dimse", Body: bytes.NewReader(b),
		Caller: tenant.Caller{CallingAETitle: "SCANNER", Address: "192.0.2.7:40000"}})
	if out.Status != want {
		e.t.Fatalf("outcome %v (%s), want %v", out.Status, out.Reason, want)
	}
	return out
}

// waitFor waits until cond holds, moving the clock forward by step each
// time the pipeline does nothing for a moment (no step running, no received
// file waiting for its row), and fails the test after a generous real
// deadline. It waits on the pipeline's hooks, never on a
// fixed sleep.
func (e *env) waitFor(what string, step time.Duration, cond func() bool) {
	e.t.Helper()
	limit := e.deadline
	if limit == 0 {
		limit = 60 * time.Second
	}
	deadline := time.Now().Add(limit)
	for !cond() {
		if time.Now().After(deadline) {
			e.t.Fatalf("timed out waiting for %s", what)
		}
		select {
		case <-e.notify:
		case <-time.After(20 * time.Millisecond):
			// Not while a received file waits for its row: the row is
			// written at the clock's time, so a clock moved past the quiet
			// period first would put the study's later files in a second
			// reception, as a slow database does on a loaded machine.
			if e.running.Load() == 0 && e.p.batch.queued.Load() == 0 {
				e.clock.Advance(step)
			}
		}
	}
}

// receptions lists every reception of the tenant.
func (e *env) receptions() []tenant.Reception {
	e.t.Helper()
	var out []tenant.Reception
	if err := e.p.eachReception(e.ctx, tenant.ReceptionFilter{}, func(r tenant.Reception) error {
		out = append(out, r)
		return nil
	}); err != nil {
		e.t.Fatal(err)
	}
	return out
}

// allEnded reports whether there are n receptions and every one has ended.
func (e *env) allEnded(n int) func() bool {
	return func() bool {
		rs := e.receptions()
		if len(rs) < n {
			return false
		}
		for _, r := range rs {
			if !r.State.Ended() {
				return false
			}
		}
		return true
	}
}

// settle waits until n receptions exist and all have ended.
func (e *env) settle(n int) []tenant.Reception {
	e.t.Helper()
	e.waitFor(fmt.Sprintf("%d receptions to end", n), 3*time.Second, e.allEnded(n))
	return e.receptions()
}

// incomingFiles counts the files in the incoming area.
func (e *env) incomingFiles() int {
	e.t.Helper()
	n := 0
	if _, err := e.p.area.Scan(func(incoming.Entry) error { n++; return nil }); err != nil {
		e.t.Fatal(err)
	}
	return n
}

// held lists the quarantine records of the tenant.
func (e *env) held() []tenant.QuarantinedFile {
	e.t.Helper()
	qs, _, err := e.store.ListQuarantined(e.ctx, tenant.QuarantineFilter{}, master.Page{Limit: 1000})
	if err != nil {
		e.t.Fatal(err)
	}
	return qs
}

// uidOf returns a corpus file's SOP Instance UID.
func uidOf(t *testing.T, b []byte) string {
	t.Helper()
	h, err := header.Read(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		t.Fatal(err)
	}
	u, _ := h.UID(header.SOPInstanceUID)
	return u
}

// done reports whether r reached done.
func done(r tenant.Reception) bool { return r.State == reception.Done }

// bytesReader returns a reader of b.
func bytesReader(b []byte) *bytes.Reader { return bytes.NewReader(b) }

// sendReader hands a stream to the pipeline and requires it received.
func (e *env) sendReader(r io.Reader) {
	e.t.Helper()
	out := e.p.Accept(e.ctx, Arrival{Receiver: "dimse", Body: r})
	if out.Status != Received {
		e.t.Fatalf("outcome %v (%s)", out.Status, out.Reason)
	}
}
