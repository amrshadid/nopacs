package holds

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/blob/dir"
	"github.com/amrshadid/nopacs/internal/core/layout"
	"github.com/amrshadid/nopacs/internal/core/record"
	"github.com/amrshadid/nopacs/internal/driver"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/secret"
	"github.com/amrshadid/nopacs/internal/tenant"
	"github.com/amrshadid/nopacs/internal/testkit"
)

// rigTenantID is the rig's tenant's id, which its records are sealed for.
const rigTenantID = "T0000000000001"

// replica is the name of the replica the rig attaches to its primary.
const replica = "copy"

// db is a tenant's database as the writer and the tests reach it.
type db interface {
	Store
	tenant.Pipeline
	tenant.CatalogReads
	tenant.Replication
	tenant.Corrections
}

// rig is a tenant on SQLite with a synthetic catalog on the primary
// testkit.GenStorage and a replica of it, each storage a directory the
// writer opens through a store the test can make fail.
type rig struct {
	t      *testing.T
	ctx    context.Context
	keys   *secret.Keys
	db     db
	stores map[string]*testStore
	seq    atomic.Int64
	// subjects are the catalog's subjects' labels.
	subjects []string
}

// testStore is one storage of the rig: a directory, a switch to make its
// writes fail, and a call before each write.
type testStore struct {
	blob.Store
	fail    atomic.Bool
	mu      sync.Mutex
	beforeP func(key string)
}

// Put fails while fail is set, else calls beforeP and writes.
func (s *testStore) Put(ctx context.Context, key string, r io.Reader) (blob.Written, error) {
	if s.fail.Load() {
		return blob.Written{}, fault.New(fault.Unavailable, "the storage does not answer")
	}
	s.mu.Lock()
	before := s.beforeP
	s.mu.Unlock()
	if before != nil {
		before(key)
	}
	return s.Store.Put(ctx, key, r)
}

// Close keeps the directory open: the rig closes it at the test's end.
func (s *testStore) Close() error { return nil }

// openTenant opens and migrates a SQLite tenant in a new directory and
// writes into it the synthetic catalog of seed, its scans on
// testkit.GenStorage, with the replica attached.
func openTenant(t *testing.T, seed uint64) db {
	t.Helper()
	ctx := context.Background()
	st, err := (driver.SQLiteTenants{DataDir: t.TempDir()}).Open(ctx, rigTenantID, "lab", "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	d, ok := st.(db)
	if !ok {
		t.Fatal("the SQLite tenant lacks the holds' parts")
	}
	var profiles []testkit.FileProfile
	for _, name := range []string{"MR_small.dcm", "ct5n-2062.dcm"} {
		b, err := os.ReadFile(filepath.Join("..", "receive", "testdata", name)) // #nosec G304 -- a test's own file names.
		if err != nil {
			t.Fatal(err)
		}
		profiles = append(profiles, testkit.ProfileOf(t, b))
	}
	// Two seeds, two studies of a subject each: a hold of one subject and
	// a record changed to name the other.
	for _, s := range []uint64{seed, seed + 1000} {
		if _, err := testkit.GenerateTenant(ctx, d, testkit.TenantShape{Instances: 20, Seed: s, Profiles: profiles}); err != nil {
			t.Fatal(err)
		}
	}
	audit := master.AuditEntry{ID: "a-replica", Time: time.Now(), LocalUser: "tester", Method: "Test", TenantName: "lab", Outcome: master.OutcomeDone}
	if _, err := d.AddStorage(ctx, tenant.Storage{Name: replica, Type: tenant.StorageDirectory, Directory: tenant.DirectoryStorage{Root: "replicas",
		Path: replica}}, time.Now(), audit, nil); err != nil {
		t.Fatal(err)
	}
	audit.ID = "a-attach"
	if _, _, _, err := d.AttachReplica(ctx, replica, testkit.GenStorage, time.Now(), audit, nil); err != nil {
		t.Fatal(err)
	}
	return d
}

// newRig makes the rig over a new tenant of seed 1.
func newRig(t *testing.T) *rig {
	t.Helper()
	r := &rig{t: t, ctx: context.Background(), keys: testkit.InstallationKeys(t), stores: map[string]*testStore{}}
	r.db = openTenant(t, 1)
	for _, name := range []string{testkit.GenStorage, replica} {
		root := filepath.Join(t.TempDir(), name)
		if err := os.MkdirAll(root, 0o700); err != nil {
			t.Fatal(err)
		}
		s, err := dir.Open(root)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = s.Close() })
		r.stores[name] = &testStore{Store: s}
	}
	page, err := r.db.ListSubjects(r.ctx, tenant.SubjectFilter{Order: tenant.OrderLabel}, master.Page{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range page.Items {
		r.subjects = append(r.subjects, s.Label)
	}
	if len(r.subjects) < 2 {
		t.Fatalf("the synthetic tenant has subjects %v, want two or more", r.subjects)
	}
	return r
}

// config returns a writer's configuration over the rig's database st,
// with the rig's storages.
func (r *rig) config(st Store) Config {
	return Config{Tenant: "lab", TenantID: rigTenantID, Keys: r.keys, Store: st, Interval: 20 * time.Millisecond,
		Open: func(_ context.Context, name string) (blob.Store, error) {
			s, ok := r.stores[name]
			if !ok {
				return nil, fault.New(fault.NotFound, "no storage "+name)
			}
			return s, nil
		}}
}

// writer returns a writer over the rig's database.
func (r *rig) writer() *Writer {
	r.t.Helper()
	w, err := New(r.config(r.db))
	if err != nil {
		r.t.Fatal(err)
	}
	return w
}

// audit returns a new audit entry for a change the test makes.
func (r *rig) audit() master.AuditEntry {
	return master.AuditEntry{ID: fmt.Sprintf("a%08d", r.seq.Add(1)), Time: time.Now(), KeyID: "K1234567890a", Method: "Test",
		TenantName: "lab", Outcome: master.OutcomeDone}
}

// place places a hold of the subject through w, failing the test on an
// error.
func (r *rig) place(w *Writer, subject string) tenant.Hold {
	r.t.Helper()
	h, made, err := w.Place(r.ctx, Placing{Target: tenant.HoldTarget{Subject: subject}, Reason: "litigation 2026-17", KeyID: "K1234567890a",
		Audit: r.audit()})
	if err != nil || !made {
		r.t.Fatalf("Place = %+v, %v, %v", h, made, err)
	}
	return h
}

// record reads the hold's record on the storage named, or fails the test.
func (r *rig) record(storage, id string) record.Hold {
	r.t.Helper()
	key, err := layout.HoldKey(id)
	if err != nil {
		r.t.Fatal(err)
	}
	rc, err := r.stores[storage].Read(r.ctx, key, 0, -1)
	if err != nil {
		r.t.Fatalf("the record of %s on %s: %v", id, storage, err)
	}
	defer func() { _ = rc.Close() }()
	h, err := record.DecodeHold(rc, rigTenantID, r.keys)
	if err != nil {
		r.t.Fatalf("the record of %s on %s: %v", id, storage, err)
	}
	return h
}

// hasRecord reports whether the storage holds a record of the hold.
func (r *rig) hasRecord(storage, id string) bool {
	r.t.Helper()
	key, _ := layout.HoldKey(id)
	_, err := r.stores[storage].Stat(r.ctx, key)
	if err != nil && fault.KindOf(err) != fault.NotFound {
		r.t.Fatal(err)
	}
	return err == nil
}

// rawRecord returns the bytes of the hold's record on the storage named.
func (r *rig) rawRecord(storage, id string) []byte {
	r.t.Helper()
	key, _ := layout.HoldKey(id)
	rc, err := r.stores[storage].Read(r.ctx, key, 0, -1)
	if err != nil {
		r.t.Fatal(err)
	}
	defer func() { _ = rc.Close() }()
	b, err := io.ReadAll(rc)
	if err != nil {
		r.t.Fatal(err)
	}
	return b
}

// putRaw writes b as the hold's record on the storage named.
func (r *rig) putRaw(storage, id string, b []byte) {
	r.t.Helper()
	key, _ := layout.HoldKey(id)
	if _, err := r.stores[storage].Store.Put(r.ctx, key, bytes.NewReader(b)); err != nil {
		r.t.Fatal(err)
	}
}

// holds lists every hold of the database.
func holdsOf(t *testing.T, st tenant.Holds) []tenant.Hold {
	t.Helper()
	out, _, err := st.ListHolds(context.Background(), tenant.HoldFilter{}, master.Page{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	return out
}
