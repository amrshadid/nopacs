package servecmd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/driver"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/place"
	"github.com/amrshadid/nopacs/internal/receive/supervise"
	"github.com/amrshadid/nopacs/internal/registry"
	"github.com/amrshadid/nopacs/internal/server"
	"github.com/amrshadid/nopacs/internal/tenant"
	"github.com/amrshadid/nopacs/internal/testkit"
)

// countedParts is every part of a tenant's database the registry and the
// background work reach.
type countedParts interface {
	tenantDB
	tenant.Secrets
	RebuildRunning(ctx context.Context) (bool, error)
}

// countingDB is a tenant's database that counts the writes of the
// background work that reach it: the claims of the replicator and of the
// repairs, and retention's check of its clock, which an idle tenant's work
// makes over and over.
type countingDB struct {
	countedParts
	writes atomic.Int64
}

// ClaimReplicaScans counts, then claims.
func (c *countingDB) ClaimReplicaScans(ctx context.Context, owner string, now time.Time, lease time.Duration, limit int) ([]tenant.ReplicaClaim, error) {
	c.writes.Add(1)
	return c.countedParts.ClaimReplicaScans(ctx, owner, now, lease, limit)
}

// ClaimWork counts, then claims.
func (c *countingDB) ClaimWork(ctx context.Context, kind, owner string, now time.Time, lease time.Duration) (tenant.Work, error) {
	c.writes.Add(1)
	return c.countedParts.ClaimWork(ctx, kind, owner, now, lease)
}

// CheckRetentionClock counts, then checks.
func (c *countingDB) CheckRetentionClock(ctx context.Context, rc tenant.RetentionClockCheck) (tenant.RetentionClock, error) {
	c.writes.Add(1)
	return c.countedParts.CheckRetentionClock(ctx, rc)
}

// countingDriver is the SQLite driver whose every store opened counts the
// background work's writes, in the order opened.
type countingDriver struct {
	inner  registry.Driver
	mu     sync.Mutex
	opened []*countingDB
}

// Open opens through the SQLite driver and keeps the counting store.
func (d *countingDriver) Open(ctx context.Context, id, name, address string) (tenant.Store, error) {
	st, err := d.inner.Open(ctx, id, name, address)
	if err != nil {
		return nil, err
	}
	parts, ok := st.(countedParts)
	if !ok {
		_ = st.Close()
		return nil, errors.New("the store lacks a part the test counts through")
	}
	c := &countingDB{countedParts: parts}
	d.mu.Lock()
	d.opened = append(d.opened, c)
	d.mu.Unlock()
	return c, nil
}

// Location is the SQLite driver's.
func (d *countingDriver) Location(name, address string) (string, error) {
	return d.inner.Location(name, address)
}

// store returns the i-th store opened.
func (d *countingDriver) store(t *testing.T, i int) *countingDB {
	t.Helper()
	d.mu.Lock()
	defer d.mu.Unlock()
	if i >= len(d.opened) {
		t.Fatalf("only %d stores were opened, want store %d", len(d.opened), i)
	}
	return d.opened[i]
}

// registryRig is a real registry over a SQLite master, its tenants opened
// by countingDriver, and the background work as serve builds it over the
// registry, its readings of the tenants made by the test.
type registryRig struct {
	t      *testing.T
	ctx    context.Context
	reg    *registry.Registry
	drv    *countingDriver
	w      *tenantWork
	fenced atomic.Bool
	seq    atomic.Int64
	// fences counts the fence's checks of the owner records.
	fences atomic.Int64
}

// newRegistryRig makes the rig with the tenant lab created.
func newRegistryRig(t *testing.T) *registryRig {
	t.Helper()
	r := &registryRig{t: t, ctx: context.Background()}
	data := t.TempDir()
	m, err := driver.OpenMaster(r.ctx, driver.SQLiteScheme+filepath.Join(data, "master.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })
	if err := m.Migrate(r.ctx); err != nil {
		t.Fatal(err)
	}
	keys := testkit.InstallationKeys(t)
	r.drv = &countingDriver{inner: driver.SQLiteTenants{DataDir: data}}
	var work atomic.Pointer[tenantWork]
	r.reg, err = registry.New(registry.Config{Master: m, Keys: keys, Drivers: map[string]registry.Driver{driver.SQLiteDriver: r.drv},
		Now: time.Now, Interval: time.Hour, OpenTimeout: 30 * time.Second, PingTimeout: 10 * time.Second, MaxOpening: 2,
		Fence: fenceNotice(func(context.Context, string, string, tenant.Store) (string, error) {
			r.fences.Add(1)
			if r.fenced.Load() {
				return "the storage main is owned by another installation", nil
			}
			return "", nil
		}, func(name string) {
			if w := work.Load(); w != nil {
				w.Fenced(name)
			}
		}),
		Unregistered: func(name string) {
			if w := work.Load(); w != nil {
				w.TenantRemoved(name)
			}
		}})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.reg.Start(r.ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		_ = r.reg.Close(ctx)
	})
	r.create()
	r.w, err = newTenantWork(Deps{Keys: keys, Master: m, Log: slog.New(slog.DiscardHandler)}, r.reg, &place.Cleanups{}, server.NewID(),
		&ownerLocks{})
	if err != nil {
		t.Fatal(err)
	}
	r.w.interval = time.Hour
	r.w.replicaIdle = 20 * time.Millisecond
	work.Store(r.w)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		_ = r.w.Stop(ctx)
	})
	return r
}

// audit returns a new audit entry for a change the test makes.
func (r *registryRig) audit(method string) master.AuditEntry {
	id := fmt.Sprintf("req-%06d", r.seq.Add(1))
	return master.AuditEntry{ID: id, Time: time.Now(), LocalUser: "501", Method: method, Outcome: master.OutcomeDone, RequestID: "request-" + id}
}

// create creates the tenant lab, with a new id.
func (r *registryRig) create() {
	r.t.Helper()
	if _, _, err := r.reg.Create(r.ctx, registry.NewTenant{ID: server.NewID(), Name: "lab", Driver: driver.SQLiteDriver},
		r.audit("CreateTenant"), master.LocalOperator()); err != nil {
		r.t.Fatal(err)
	}
}

// wantWrites waits until the store's count of writes has grown past from.
func (r *registryRig) wantWrites(what string, st *countingDB, from int64) {
	r.t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for st.writes.Load() <= from {
		if time.Now().After(deadline) {
			r.t.Fatalf("%s: the background work wrote nothing", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// wantNoWrites fails when a write of the background work lands on st in
// the time the replicator makes many claims: a write let through by the
// fence just before the change may still land, so the count is taken once
// that has had its time.
func (r *registryRig) wantNoWrites(what string, st *countingDB) {
	r.t.Helper()
	time.Sleep(100 * time.Millisecond)
	before := st.writes.Load()
	time.Sleep(500 * time.Millisecond)
	if after := st.writes.Load(); after != before {
		r.t.Fatalf("%s: %d writes of the background work landed after it", what, after-before)
	}
}

// TestTheBackgroundWorkWritesNothingForATenantNoLongerReady guards the
// fence of a tenant's background work over a real registry: the work
// started by a reading of the tenants writes; once the tenant is moved by
// an export no write of it lands (each asks the registry's fence) and the
// drain's Quiesce stops it; given back, it starts again; once a check
// fences the tenant its work is stopped at once (fenceNotice) and no write
// lands; taken back, it starts again; removed, nothing more lands on its
// database; made again, the work runs over the new database only.
func TestTheBackgroundWorkWritesNothingForATenantNoLongerReady(t *testing.T) {
	r := newRegistryRig(t)
	r.w.reconcile(r.ctx)
	first := r.drv.store(t, 0)
	r.wantWrites("started", first, 0)

	if err := r.reg.SetHandoff(r.ctx, "lab", master.StateMoved, "handed off", r.audit("ExportTenant"), master.LocalOperator()); err != nil {
		t.Fatal(err)
	}
	r.wantNoWrites("moved", first)
	if err := r.w.Quiesce(r.ctx, "lab"); err != nil || r.w.set("lab") != nil {
		t.Fatalf("the drain's quiesce of a moved tenant: %v, work left %v", err, r.w.set("lab") != nil)
	}
	r.w.reconcile(r.ctx)
	if r.w.set("lab") != nil {
		t.Fatal("a reading of the tenants started the work of a moved tenant")
	}
	if err := r.reg.GiveBack(r.ctx, "lab", r.audit("ExportTenant")); err != nil {
		t.Fatal(err)
	}
	r.w.reconcile(r.ctx)
	r.wantWrites("given back", first, first.writes.Load())

	r.fenced.Store(true)
	if err := r.reg.CheckFence("lab"); fault.KindOf(err) != fault.Precondition {
		t.Fatalf("the fence check: %v, want the tenant fenced", err)
	}
	if r.w.set("lab") != nil {
		t.Fatal("the fence check left the tenant's work running")
	}
	r.wantNoWrites("fenced", first)
	r.fenced.Store(false)
	if err := r.reg.SetHandoff(r.ctx, "lab", master.StateReady, "", r.audit("ReclaimTenant"), master.LocalOperator()); err != nil {
		t.Fatal(err)
	}
	r.w.reconcile(r.ctx)
	r.wantWrites("taken back", first, first.writes.Load())

	if _, err := r.reg.Remove(r.ctx, "lab", r.audit("RemoveTenant"), master.LocalOperator()); err != nil {
		t.Fatal(err)
	}
	if r.w.set("lab") != nil {
		t.Fatal("the removal left the tenant's work running")
	}
	r.wantNoWrites("removed", first)

	r.create()
	r.w.reconcile(r.ctx)
	second := r.drv.store(t, 1)
	r.wantWrites("made again", second, 0)
	r.wantNoWrites("the first database once the tenant was made again", first)
}

// TestAStartReadBeforeAQuiesceDoesNotStart guards the epoch of a tenant's
// work: a reading of the tenants that read the tenant before an export's
// drain stopped its work does not start the work after it.
func TestAStartReadBeforeAQuiesceDoesNotStart(t *testing.T) {
	r := newWorkRig(t)
	epoch := r.w.epochOf("lab")
	if err := r.w.Quiesce(r.ctx, "lab"); err != nil {
		t.Fatal(err)
	}
	if err := r.w.startSet(r.ctx, "lab", r.db, epoch); err != nil {
		t.Fatal(err)
	}
	if r.w.set("lab") != nil {
		t.Fatal("a start read before the quiesce started the tenant's work")
	}
	r.start()
	if r.w.set("lab") == nil {
		t.Fatal("a start read after the quiesce did not start the work")
	}
}

// TestNoWorkStartsWhileARebuildRuns guards the reading of the tenants
// during a rebuild: the work a rebuild's drain stopped is not started again
// while the rebuild is queued or running.
func TestNoWorkStartsWhileARebuildRuns(t *testing.T) {
	r := newWorkRig(t)
	if !rebuildingOK(r) {
		t.Skip("the tenant's database keeps no rebuilds")
	}
	if rebuilding(r.ctx, r.db) {
		t.Fatal("a tenant with no rebuild reads as rebuilding")
	}
	if _, err := r.db.CreateJob(r.ctx, tenant.NewJob{ID: "rb-1", Kind: tenant.JobRebuild, Rebuild: &tenant.RebuildJob{Mode: tenant.RebuildReplace},
		Created: time.Now()}, r.audit(), nil); err != nil {
		t.Fatal(err)
	}
	if !rebuilding(r.ctx, r.db) {
		t.Fatal("a tenant with a rebuild queued does not read as rebuilding")
	}
	r.w.tenants = oneTenantStore{st: r.db}
	r.w.reconcile(r.ctx)
	if r.w.set("lab") != nil {
		t.Fatal("a reading of the tenants started the work while a rebuild was queued")
	}
}

// rebuildingOK reports whether the rig's database keeps rebuilds.
func rebuildingOK(r *workRig) bool {
	_, ok := r.db.(interface {
		RebuildRunning(ctx context.Context) (bool, error)
	})
	return ok
}

// TestEachDrainStopsTheTenantsBackgroundWork guards an export's drain and a
// rebuild's: each stops the tenant's background work and waits for it
// before it answers, so nothing of it writes after the snapshot or during
// the build.
func TestEachDrainStopsTheTenantsBackgroundWork(t *testing.T) {
	r := newWorkRig(t)
	sup, err := supervise.New(supervise.Config{Tenants: oneTenantStore{st: r.db}, Installation: noSettings{},
		StartPipeline: func(context.Context, string, tenant.Pipeline, supervise.Gates) (supervise.Pipeline, error) {
			return nil, errors.New("no pipeline in this test")
		},
		NewListener: func(supervise.ListenerSpec) (supervise.Listener, error) {
			return nil, errors.New("no listener in this test")
		}})
	if err != nil {
		t.Fatal(err)
	}
	h := handoffs{sup: sup, engines: retentionEngines(t, r), locks: r.w.owners, work: r.w}
	for name, drain := range map[string]func(context.Context, string) error{"export": h.Drain, "rebuild": h.DrainForRebuild} {
		r.start()
		s := r.w.set("lab")
		if s == nil {
			t.Fatalf("%s: the work did not start", name)
		}
		_ = drain(r.ctx, "lab")
		if r.w.set("lab") != nil {
			t.Fatalf("%s: the drain left the tenant's work running", name)
		}
		done := make(chan struct{})
		go func() {
			s.wg.Wait()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatalf("%s: the drain answered before the tenant's workers returned", name)
		}
	}
}

// refusingFence is a fence that refuses while refuse is set.
type refusingFence struct{ refuse atomic.Bool }

// check refuses as the registry refuses a moved tenant.
func (f *refusingFence) check() error {
	if f.refuse.Load() {
		return master.ReadOnlyError("lab", master.StateMoved, "handed off")
	}
	return nil
}

// TestAFencedStoreRefusesEveryWrite guards fencedOpen: while the fence
// refuses, a put, a delete, a purge and a recall through a store it opened
// reach nothing and answer the fence's refusal; reads pass; a store that
// takes recalls keeps that part; once the fence lets writes through they
// land.
func TestAFencedStoreRefusesEveryWrite(t *testing.T) {
	r := newWorkRig(t)
	row, err := r.db.Storage(r.ctx, "main")
	if err != nil {
		t.Fatal(err)
	}
	inner, err := r.w.opener.OpenBeside(r.ctx, place0(r), row)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = inner.Close() })
	f := &refusingFence{}
	counted := &countingStore{Store: inner}
	open := fencedOpen(func(context.Context, string) (blob.Store, error) { return counted, nil }, f.check)
	st, err := open(r.ctx, "main")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := st.(blob.Recaller); !ok {
		t.Fatal("the fenced store lost the recalls of the store it wraps")
	}
	f.refuse.Store(true)
	writes := map[string]func() error{
		"put":    func() error { _, err := st.Put(r.ctx, "data/x", bytes.NewReader([]byte("x"))); return err },
		"delete": func() error { return st.Delete(r.ctx, "data/x") },
		"purge":  func() error { _, err := st.Purge(r.ctx, "data/x"); return err },
		"recall": func() error {
			_, err := st.(blob.Recaller).Recall(r.ctx, "data/x", blob.RecallRequest{})
			return err
		},
	}
	for name, write := range writes {
		if err := write(); fault.KindOf(err) != fault.Precondition {
			t.Fatalf("a %s while the tenant is moved: %v, want the fence's refusal", name, err)
		}
	}
	if n := counted.writes.Load(); n != 0 {
		t.Fatalf("%d writes reached the storage while the fence refused", n)
	}
	if _, err := st.Stat(r.ctx, "data/x"); fault.KindOf(err) != fault.NotFound {
		t.Fatalf("a read while the tenant is moved: %v, want it passed to the storage", err)
	}
	f.refuse.Store(false)
	if _, err := st.Put(r.ctx, "data/x", bytes.NewReader([]byte("x"))); err != nil {
		t.Fatal(err)
	}
	if n := counted.writes.Load(); n != 1 {
		t.Fatalf("%d writes reached the storage once the fence let them through, want 1", n)
	}
}

// countingStore counts the writes that reach a storage, and takes recalls.
type countingStore struct {
	blob.Store
	writes atomic.Int64
}

// Put counts, then puts.
func (s *countingStore) Put(ctx context.Context, key string, r io.Reader) (blob.Written, error) {
	s.writes.Add(1)
	return s.Store.Put(ctx, key, r)
}

// Delete counts, then deletes.
func (s *countingStore) Delete(ctx context.Context, key string) error {
	s.writes.Add(1)
	return s.Store.Delete(ctx, key)
}

// Purge counts, then purges.
func (s *countingStore) Purge(ctx context.Context, key string) ([]blob.Removal, error) {
	s.writes.Add(1)
	return s.Store.Purge(ctx, key)
}

// Recall counts and answers nothing restored.
func (s *countingStore) Recall(context.Context, string, blob.RecallRequest) (blob.Recall, error) {
	s.writes.Add(1)
	return blob.Recall{}, nil
}

// TestAFencedDatabaseRefusesEveryWorkWrite guards fencedDB: while the
// fence refuses, the writes of the background work (a claim of the
// replicator, a claim of a repair unit, a job made, retention's clock)
// answer the fence's refusal and reach nothing; reads pass.
func TestAFencedDatabaseRefusesEveryWorkWrite(t *testing.T) {
	r := newWorkRig(t)
	f := &refusingFence{}
	f.refuse.Store(true)
	db := fenceDB(r.db, f.check)
	now := time.Now()
	writes := map[string]func() error{
		"replica claim": func() error { _, err := db.ClaimReplicaScans(r.ctx, "w-1", now, time.Minute, 1); return err },
		"repair claim": func() error {
			_, err := db.ClaimWork(r.ctx, string(tenant.JobRepair), "w-1", now, time.Minute)
			return err
		},
		"job": func() error {
			_, err := db.CreateJob(r.ctx, tenant.NewJob{ID: "j-1", Kind: tenant.JobRepair, Created: now,
				Repair: &tenant.RepairJob{Storage: "main", All: true}}, serverAudit("lab", "repair.automatic", "main", now), nil)
			return err
		},
		"retention clock": func() error {
			_, err := db.CheckRetentionClock(r.ctx, tenant.RetentionClockCheck{Now: now, MaxStep: time.Hour, Notice: time.Hour})
			return err
		},
	}
	for name, write := range writes {
		if err := write(); fault.KindOf(err) != fault.Precondition {
			t.Fatalf("a %s while the tenant is moved: %v, want the fence's refusal", name, err)
		}
	}
	if jobs, _, err := r.db.ListJobs(r.ctx, tenant.JobFilter{}, master.Page{Limit: 10}); err != nil || len(jobs) != 0 {
		t.Fatalf("the refused job was written: %d jobs (%v)", len(jobs), err)
	}
	if _, err := db.Storage(r.ctx, "main"); err != nil {
		t.Fatalf("a read while the tenant is moved: %v", err)
	}
}
