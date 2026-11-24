package access_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/access"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/secret"
)

// memStore is a store in memory with one key and no grants, for the tests
// that need to control the store's timing (the last-use worker) or run fast
// (the fuzz target). Every audit entry it is given is validated as the
// master would, so an entry the master would refuse fails the test. The
// operations no such test calls fail as Internal.
type memStore struct {
	access.Store // nil: an unexpected call panics, which fails the test
	key          master.Key
	mu           sync.Mutex
	audit        []master.AuditEntry
	badEntry     error
	// mark is called by MarkKeyUsed, when set.
	mark func(ctx context.Context) error
	// appendFn is called by AppendAudit, when set, before anything is
	// kept; an error it returns keeps nothing, as the port's all or none.
	appendFn func(ctx context.Context, entries []master.AuditEntry) error
	// batches is the size of every AppendAudit call, failed ones included.
	batches []int
}

func (m *memStore) Key(_ context.Context, id string) (master.Key, error) {
	if id == m.key.ID {
		return m.key, nil
	}
	return master.Key{}, fault.New(fault.NotFound, "no such key")
}

func (m *memStore) RolesFor(context.Context, string, string) (master.Roles, error) {
	return master.Roles{}, nil
}

func (m *memStore) GrantsByKey(context.Context, string, master.Page) ([]master.Grant, string, error) {
	return nil, "", nil
}

func (m *memStore) TenantByName(_ context.Context, name string) (master.Tenant, error) {
	if err := master.ValidateTenantName(name); err != nil {
		return master.Tenant{}, err
	}
	return master.Tenant{}, fault.New(fault.NotFound, "no such tenant")
}

func (m *memStore) MarkKeyUsed(ctx context.Context, _ string, _ time.Time, _ string) (bool, error) {
	if m.mark != nil {
		return false, m.mark(ctx)
	}
	return true, nil
}

func (m *memStore) AppendAudit(ctx context.Context, entries ...master.AuditEntry) error {
	m.mu.Lock()
	m.batches = append(m.batches, len(entries))
	m.mu.Unlock()
	if m.appendFn != nil {
		if err := m.appendFn(ctx, entries); err != nil {
			return err
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, e := range entries {
		if err := e.Validate(); err != nil && m.badEntry == nil {
			m.badEntry = err
		}
	}
	m.audit = append(m.audit, entries...)
	return nil
}

// newMem returns a memStore holding one key, and the whole key.
func newMem(tb testing.TB) (*memStore, string) {
	tb.Helper()
	made, err := secret.MakeAPIKey()
	if err != nil {
		tb.Fatal(err)
	}
	return &memStore{key: master.Key{ID: made.ID, Hash: made.Hash[:], Holder: "mem", Created: t0}}, made.Key()
}

// newMemAccess returns an Access over m with a queue of n marks.
func newMemAccess(tb testing.TB, m *memStore, n int) *access.Access {
	tb.Helper()
	return newMemAccessWith(tb, m, access.Config{MarkQueue: n})
}

// newMemAccessWith returns an Access over m with cfg, its store, clock and
// id source filled in.
func newMemAccessWith(tb testing.TB, m *memStore, cfg access.Config) *access.Access {
	tb.Helper()
	cfg.Store, cfg.Clock, cfg.NewID = m, func() time.Time { return t0 }, ids("mem")
	a, err := access.New(cfg)
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { _ = a.Close(context.Background()) })
	return a
}

// TestMarkNeverBlocks guards the last-use worker's promise: when the master
// hangs on a mark, requests still answer at once, the marks past the queue
// are dropped and counted, and Close returns at its deadline, canceling
// the write in flight, with no goroutine left behind.
func TestMarkNeverBlocks(t *testing.T) {
	m, full := newMem(t)
	entered := make(chan struct{}, 100)
	m.mark = func(ctx context.Context) error {
		entered <- struct{}{}
		<-ctx.Done()
		return ctx.Err()
	}
	a := newMemAccess(t, m, 1)
	cred := access.APIKey(full)
	// The first mark is taken by the worker and hangs; the second fills the
	// queue of one; the rest are dropped.
	if _, err := a.Check(context.Background(), cred, access.Authenticated(), facts("m")); err != nil {
		t.Fatal(err)
	}
	<-entered
	start := time.Now()
	for range 50 {
		if _, err := a.Check(context.Background(), cred, access.Authenticated(), facts("m")); err != nil {
			t.Fatal(err)
		}
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("50 checks took %v with the master hanging on marks", d)
	}
	if got := a.MarkStats().Dropped; got != 49 {
		t.Fatalf("%d marks dropped, want 49", got)
	}
	// Close waits, after canceling, for the worker to settle the mark the
	// cancel cut short before it counts anything: the order in which that
	// mark used to be counted as failed instead of dropped. Without the
	// wait, which order happened was left to the scheduler.
	access.WaitForWorkersAfterCancel(a)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := a.Close(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Close: %v, want the deadline", err)
	}
	// The mark stuck in the store when the deadline passed is counted as
	// dropped, and its return from the store changes nothing: 49 dropped
	// from the full queue, the one queued, and the one in flight.
	s := a.MarkStats()
	if s.Failed != 0 || s.Dropped != 51 || s.Written != 0 {
		t.Fatalf("stats after Close: %+v", s)
	}
	// Close is safe to call again, and checks after it still answer.
	if err := a.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Check(context.Background(), cred, access.Authenticated(), facts("m")); err != nil {
		t.Fatal(err)
	}
}

// TestMarkFailureDoesNotFailRequest guards that a mark the master refuses
// fails no request, and that Close writes the marks still queued.
func TestMarkFailureDoesNotFailRequest(t *testing.T) {
	m, full := newMem(t)
	m.mark = func(context.Context) error { return fault.New(fault.Unavailable, "down") }
	a := newMemAccess(t, m, 0)
	for range 10 {
		if _, err := a.Check(context.Background(), access.APIKey(full), access.Authenticated(), facts("m")); err != nil {
			t.Fatalf("a failing mark failed the request: %v", err)
		}
	}
	if err := a.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if s := a.MarkStats(); s.Failed+s.Dropped != 10 || s.Failed == 0 {
		t.Fatalf("stats %+v", s)
	}
}

// TestNewRefusesMissing guards that New needs its store, clock and id
// source, since the package reads neither the wall clock nor a global.
func TestNewRefusesMissing(t *testing.T) {
	m, _ := newMem(t)
	for _, cfg := range []access.Config{
		{Clock: time.Now, NewID: ids("x")},
		{Store: m, NewID: ids("x")},
		{Store: m, Clock: time.Now},
		{Store: m, Clock: time.Now, NewID: ids("x"), MarkQueue: -1},
	} {
		if _, err := access.New(cfg); err == nil {
			t.Fatalf("New(%+v) succeeded", cfg)
		}
	}
}

// TestBadIDSource guards that an id source that returns an id the master
// would refuse fails the request as Internal rather than answer it without
// an audit entry.
func TestBadIDSource(t *testing.T) {
	m, full := newMem(t)
	a, err := access.New(access.Config{Store: m, Clock: func() time.Time { return t0 }, NewID: func() string { return "bad id" }})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = a.Close(context.Background()) }()
	_, err = a.Check(context.Background(), access.APIKey(full), access.Authenticated(), facts("m"))
	wantErr(t, "bad id", err, fault.Internal, "internal error")
}
