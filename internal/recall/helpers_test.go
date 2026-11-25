package recall

import (
	"context"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/driver"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// testTenantID is the fixture's tenant's id.
const testTenantID = "T0000000000001"

// fakeClock is a clock a test moves by hand.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

// Now answers the clock's time.
func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

// Add moves the clock forward.
func (c *fakeClock) Add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// coldStore is a store whose objects a test puts in a cold class: Recall
// and Stat answer from the fields a test sets. The store it embeds answers
// nothing a test reaches.
type coldStore struct {
	blob.Store
	mu sync.Mutex
	// state is what Stat says; until is its OnlineUntil.
	state blob.OnlineState
	until time.Time
	// recallErr and statErr, when set, are what the call answers.
	recallErr, statErr error
	// answer is what a successful Recall says.
	answer blob.Recall
	// onRecall, when set, runs inside Recall before it answers.
	onRecall func(ctx context.Context) error
	// recalls counts the Recall calls; the last request is kept.
	recalls atomic.Int64
	last    blob.RecallRequest
}

// Recall answers as the test set it.
func (s *coldStore) Recall(ctx context.Context, _ string, req blob.RecallRequest) (blob.Recall, error) {
	s.recalls.Add(1)
	s.mu.Lock()
	s.last = req
	hook, err, ans := s.onRecall, s.recallErr, s.answer
	s.mu.Unlock()
	if hook != nil {
		if err := hook(ctx); err != nil {
			return blob.Recall{}, err
		}
	}
	return ans, err
}

// Stat answers as the test set it.
func (s *coldStore) Stat(_ context.Context, _ string) (blob.Info, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.statErr != nil {
		return blob.Info{}, s.statErr
	}
	return blob.Info{Size: 10, State: s.state, OnlineUntil: s.until}, nil
}

// Close does nothing: the test owns what the store embeds.
func (s *coldStore) Close() error { return nil }

// set changes what Stat and Recall answer.
func (s *coldStore) set(f func(*coldStore)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f(s)
}

// plainStore is a store that is not a blob.Recaller.
type plainStore struct{ blob.Store }

// Close does nothing: the test owns what the store embeds.
func (plainStore) Close() error { return nil }

// fixture is a tenant's SQLite database with the storage "main" and its
// archive-tier replica "cold", recalling for 3 days at the bulk speed.
type fixture struct {
	t      testing.TB
	ctx    context.Context
	p      tenant.Pipeline
	rc     tenant.Recalls
	clock  *fakeClock
	seq    atomic.Int64
	store  *coldStore
	opened atomic.Int64
	// open is what the worker's opener answers; a test replaces it.
	open func(ctx context.Context, storage string) (blob.Store, error)
}

// newFixture makes the tenant and a worker's inputs over it.
func newFixture(t testing.TB) *fixture {
	t.Helper()
	f := &fixture{t: t, ctx: context.Background(), clock: &fakeClock{t: time.Now().UTC().Truncate(time.Second)}, store: &coldStore{state: blob.Archived}}
	d := t.TempDir()
	s, err := (driver.SQLiteTenants{DataDir: d}).Open(f.ctx, testTenantID, "lab", "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.Migrate(f.ctx); err != nil {
		t.Fatal(err)
	}
	f.p, f.rc = s.(tenant.Pipeline), s.(tenant.Recalls)
	at := time.Now().Add(-48 * time.Hour)
	if _, err := f.p.AddStorage(f.ctx, tenant.Storage{Name: "main", Type: tenant.StorageDirectory, Tier: tenant.TierOnline,
		Directory: tenant.DirectoryStorage{Root: "archive", Path: "main"}}, at, f.audit(), nil); err != nil {
		t.Fatal(err)
	}
	cold := tenant.Storage{Name: "cold", Type: tenant.StorageS3, Tier: tenant.TierArchive, ReplicaOf: "main", S3: tenant.S3Storage{
		Endpoint: "https://s3.example.org", Region: "us-east-1", Bucket: "lab-archive", Prefix: "cold/", PathStyle: true,
		AccessKeyID: "AKIDEXAMPLE", SealedSecret: []byte("sealed-secret-bytes"), RecallDays: 3, RecallSpeed: tenant.RecallBulk}}
	if _, err := f.p.AddStorage(f.ctx, cold, at, f.audit(), nil); err != nil {
		t.Fatal(err)
	}
	f.open = func(context.Context, string) (blob.Store, error) { return f.store, nil }
	return f
}

// audit returns a new audit entry for a change the test makes.
func (f *fixture) audit() master.AuditEntry {
	return master.AuditEntry{ID: "a" + strconv.FormatInt(f.seq.Add(1), 10) + "x", Time: time.Now(), KeyID: "K1234567890a",
		RemoteAddress: "192.0.2.10:50000", Method: "Test", TenantName: "lab", Outcome: master.OutcomeDone, RequestID: "r1"}
}

// worker returns a worker over the fixture; tune changes its Config first.
func (f *fixture) worker(tune func(*Config)) *Worker {
	f.t.Helper()
	cfg := Config{Queue: f.rc.(Queue), Now: f.clock.Now, Poll: time.Minute, Idle: time.Hour, RetryMin: 30 * time.Second, RetryMax: 10 * time.Minute,
		Open: func(ctx context.Context, name string) (blob.Store, error) {
			f.opened.Add(1)
			return f.open(ctx, name)
		}}
	if tune != nil {
		tune(&cfg)
	}
	w, err := New(cfg)
	if err != nil {
		f.t.Fatal(err)
	}
	return w
}

// ask requests recalls of keys on "cold" at the clock's time, as a job unit
// would.
func (f *fixture) ask(keys ...string) tenant.RecallAsked {
	f.t.Helper()
	got, err := f.rc.RequestRecalls(f.ctx, tenant.RecallAsk{Storage: "cold", Keys: keys, Days: 3, Speed: tenant.RecallBulk, Now: f.clock.Now()})
	if err != nil {
		f.t.Fatal(err)
	}
	return got
}

// row reads the recall of key, failing the test when there is none.
func (f *fixture) row(key string) tenant.Recall {
	f.t.Helper()
	r, found, err := f.rc.Recall(f.ctx, "cold", key)
	if err != nil || !found {
		f.t.Fatalf("Recall(%s) = %+v, %v, %v", key, r, found, err)
	}
	return r
}

// once runs one pass and fails the test on its error.
func (f *fixture) once(w *Worker) Pass {
	f.t.Helper()
	p, err := w.RunOnce(f.ctx)
	if err != nil {
		f.t.Fatal(err)
	}
	return p
}
