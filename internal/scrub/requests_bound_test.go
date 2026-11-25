package scrub

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/blob/dir"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// failingKey is a store whose read of one object always fails with an
// error that is neither not found nor not online.
type failingKey struct {
	blob.Store
	key string
}

// Read fails the one object.
func (f failingKey) Read(ctx context.Context, key string, offset, length int64) (io.ReadCloser, error) {
	if key == f.key {
		return nil, fault.New(fault.Internal, "the storage failed the read")
	}
	return f.Store.Read(ctx, key, offset, length)
}

// addEmptyStorage adds a directory storage of that name holding nothing,
// and returns its store.
func (f *fixture) addEmptyStorage(name string) blob.Store {
	f.t.Helper()
	root := filepath.Join(f.t.TempDir(), name)
	if err := os.MkdirAll(root, 0o700); err != nil {
		f.t.Fatal(err)
	}
	s, err := dir.Open(root)
	if err != nil {
		f.t.Fatal(err)
	}
	f.t.Cleanup(func() { _ = s.Close() })
	if _, err := f.p.AddStorage(f.ctx, tenant.Storage{Name: name, Type: tenant.StorageDirectory,
		Directory: tenant.DirectoryStorage{Root: "other", Path: name}}, time.Now().Add(-47*time.Hour), f.audit(), nil); err != nil {
		f.t.Fatal(err)
	}
	return s
}

// TestFailingRequestDoesNotHoldBackTheScrub guards that a request whose
// instance cannot be read never holds back the unit that takes it: a
// scrub of another storage, whose units take the request first, ends
// done; and that the request is dropped after it failed a few times,
// rather than tried for ever.
func TestFailingRequestDoesNotHoldBackTheScrub(t *testing.T) {
	f := newFixture(t)
	files := synthetic("1.2.826.0.1.3680043.10.516", 1)
	p := f.archiveEntries("s1", "1.2.826.0.1.3680043.10.516.90", "1.2.826.0.1.3680043.10.516.80", files)
	other := f.addEmptyStorage("other")
	stores := named{"main": readOnly{Store: failingKey{Store: f.store, key: f.keys[files[0].uid]}, writes: &f.writes},
		"other": readOnly{Store: other, writes: &f.writes}}
	cfg := f.config()
	cfg.OpenStorages = func(string, tenant.Pipeline) Storages { return stores }
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if got := s.RequestInstance("lab", InstanceRequest{Path: p, SOPInstanceUID: files[0].uid}); got != RequestQueued {
		t.Fatalf("the request answered %s; want queued", got)
	}
	j := f.createScrub("other")
	if err := s.Start(f.ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = s.Stop(ctx)
	})
	got := f.waitEndedWithin(s, j.ID, 20*time.Second)
	if got.State != tenant.JobDone {
		t.Errorf("the scrub of the other storage ended %s; want done", got.State)
	}
	deadline := time.Now().Add(20 * time.Second)
	for {
		if _, waiting := s.requests.first("lab"); !waiting {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the request that cannot be read is still waiting; want it dropped after a few attempts")
		}
		s.Poke("lab")
		time.Sleep(20 * time.Millisecond)
	}
}

// TestRequestScrubsAreBoundedByTheInterval guards that requests make at
// most one scrub of a storage per scrub.interval when no scrub runs: a
// second request-driven scrub within the interval is not made, even once
// the first has ended, and one is made again once the interval has passed.
func TestRequestScrubsAreBoundedByTheInterval(t *testing.T) {
	f := newFixture(t)
	files := synthetic("1.2.826.0.1.3680043.10.517", 1)
	p := f.archiveEntries("s1", "1.2.826.0.1.3680043.10.517.90", "1.2.826.0.1.3680043.10.517.80", files)
	clock := &stepClock{t: time.Now()}
	cfg := f.config()
	cfg.Now = clock.Now
	cfg.TenantSettings = func(tenant.Store) TenantSettings { return settingsOf{rate: 64 << 30, interval: time.Hour} }
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	tr := s.newTenantRun("lab")
	if got := s.requests.add("lab", InstanceRequest{Path: p, SOPInstanceUID: files[0].uid}); got != RequestQueued {
		t.Fatalf("the request answered %s; want queued", got)
	}
	jobs := func() []tenant.Job {
		t.Helper()
		js, _, err := f.p.ListJobs(f.ctx, tenant.JobFilter{}, pageOf(10))
		if err != nil {
			t.Fatal(err)
		}
		return js
	}
	endAll := func() {
		t.Helper()
		for _, j := range jobs() {
			if !j.State.Ended() {
				if _, err := f.p.CancelJob(f.ctx, j.ID, clock.Now(), f.audit(), nil); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	if !tr.requestJob(f.p, f.p) || len(jobs()) != 1 {
		t.Fatalf("the first request-driven look made %d scrubs; want 1", len(jobs()))
	}
	endAll()
	clock.add(2 * time.Minute)
	if tr.requestJob(f.p, f.p) || len(jobs()) != 1 {
		t.Errorf("a look within scrub.interval made %d scrubs in all; want still 1", len(jobs()))
	}
	clock.add(time.Hour)
	if !tr.requestJob(f.p, f.p) || len(jobs()) != 2 {
		t.Errorf("a look once scrub.interval passed made %d scrubs in all; want 2", len(jobs()))
	}
}

// TestRemovedTenantKeepsNoRequests guards that the requests of a tenant the
// registry let go are dropped, not kept in memory for a tenant gone.
func TestRemovedTenantKeepsNoRequests(t *testing.T) {
	f := newFixture(t)
	s, err := New(f.config())
	if err != nil {
		t.Fatal(err)
	}
	p := f.archiveEntries("s1", "1.2.826.0.1.3680043.10.518.90", "1.2.826.0.1.3680043.10.518.80", synthetic("1.2.826.0.1.3680043.10.518", 1))
	if got := s.RequestInstance("lab", InstanceRequest{Path: p, SOPInstanceUID: "1.2.826.0.1.3680043.10.518.1"}); got != RequestQueued {
		t.Fatalf("the request answered %s; want queued", got)
	}
	s.TenantRemoved("lab")
	if _, waiting := s.requests.first("lab"); waiting {
		t.Error("a removed tenant's request still waits")
	}
}
