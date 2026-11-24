package registry

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/amrshadid/nopacs/internal/driver"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
)

// TestCreateWholeOrNothing guards that a creation is whole or not at all, so
// an operator who mistypes an address gets an error and not a broken tenant
// to clean up: a creation whose database cannot be opened or upgraded leaves
// nothing behind (no row in the master, no entry in the registry, no open
// store) and answers the kind the API contract gives each failure; a system
// audit entry records the undoing, with no address.
func TestCreateWholeOrNothing(t *testing.T) {
	cases := []struct {
		name string
		mode mode
		kind fault.Kind
	}{
		{"does-not-answer", down, fault.Unavailable},
		{"migration-fails", broken, fault.Unavailable},
		{"address-refused", refused, fault.Invalid},
		{"plain-error", leaky, fault.Internal},
		{"driver-panics", panicky, fault.Internal},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ctx := context.Background()
			e := newEnv(t)
			r := e.newRegistry(e.config())
			e.flaky.set(c.name, c.mode)
			_, _, err := r.Create(ctx, NewTenant{ID: "id-" + c.name, Name: c.name, Driver: flakyDriver, Address: flakyAddress(c.name)}, e.audit("CreateTenant"), master.LocalOperator())
			e.wantNothingLeft(r, c.name, err, c.kind)
		})
	}
}

// TestCreateSQLiteRefusedFile guards the same for a SQLite tenant whose file
// the driver refuses: a loose file planted at its path, and a symbolic link.
// Both are Invalid, and the creation leaves nothing behind.
func TestCreateSQLiteRefusedFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("file modes and links are not used on Windows here")
	}
	for _, plant := range []string{"loose", "link"} {
		t.Run(plant, func(t *testing.T) {
			ctx := context.Background()
			e := newEnv(t)
			path, err := driver.SQLiteTenants{DataDir: e.data}.Path(plant)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			switch plant {
			case "loose":
				if err := os.WriteFile(path, nil, 0o644); err != nil { //nolint:gosec // G306: the loose mode is what is tested.
					t.Fatal(err)
				}
			case "link":
				target := filepath.Join(e.data, "elsewhere.db")
				if err := os.WriteFile(target, nil, 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, path); err != nil {
					t.Fatal(err)
				}
			}
			r := e.newRegistry(e.config())
			_, _, err = r.Create(ctx, NewTenant{ID: "id-" + plant, Name: plant, Driver: driver.SQLiteDriver}, e.audit("CreateTenant"), master.LocalOperator())
			e.wantNothingLeft(r, plant, err, fault.Invalid)
		})
	}
}

// wantNothingLeft fails unless err has the kind want and the tenant named
// name has no row, no entry and no open store, and the undoing was audited
// by the system without the address.
func (e *env) wantNothingLeft(r *Registry, name string, err error, want fault.Kind) {
	e.t.Helper()
	if fault.KindOf(err) != want {
		e.t.Errorf("Create = %v (kind %v), want %v", err, fault.KindOf(err), want)
	}
	if err != nil {
		mustNotContain(e.t, "the error", err.Error(), append(passwordForms, flakyHost)...)
	}
	if _, merr := e.master.TenantByName(context.Background(), name); fault.KindOf(merr) != fault.NotFound {
		e.t.Errorf("the master still has the tenant (%v)", merr)
	}
	if st, known := r.State(name); known {
		e.t.Errorf("the registry still has the tenant, %s", st.State)
	}
	if _, serr := r.Store(name); fault.KindOf(serr) != fault.NotFound {
		e.t.Errorf("Store after a failed creation = %v, want NotFound", serr)
	}
	if o, c := e.flaky.opens.Load(), e.flaky.closes.Load(); o != c {
		e.t.Errorf("%d stores opened and %d closed", o, c)
	}
	entries, _, lerr := e.master.ListAudit(context.Background(), master.AuditFilter{TenantName: name}, master.Page{Limit: 100})
	if lerr != nil {
		e.t.Fatal(lerr)
	}
	undone := 0
	for _, a := range entries {
		if a.Details[DetailUndone] == "true" {
			undone++
			if a.Method != "CreateTenant" || a.Outcome != master.OutcomeFailed || a.Reason == "" {
				e.t.Errorf("the failed request's audit entry = %+v", a)
			}
			mustNotContain(e.t, "the failed request's audit entry", a.Reason, append(passwordForms, flakyHost)...)
		}
	}
	if undone != 1 {
		e.t.Errorf("%d audit entries say the creation was undone, want 1", undone)
	}
}

// TestCreateRepeatComparesAddresses guards that a repeated creation with the
// same id, name and type is the idempotent repeat only with the same
// address: another address is Conflict and the stored one is kept.
func TestCreateRepeatComparesAddresses(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	r := e.newRegistry(e.config())
	e.create(r, "id-lab", "lab", flakyDriver, flakyAddress("lab"))
	e.create(r, "id-lab", "lab", flakyDriver, flakyAddress("lab"))
	other := "flaky://admin:other@" + flakyHost + ":5432/lab"
	_, _, err := r.Create(ctx, NewTenant{ID: "id-lab", Name: "lab", Driver: flakyDriver, Address: other}, e.audit("CreateTenant"), master.LocalOperator())
	if fault.KindOf(err) != fault.Conflict {
		t.Errorf("a repeat with another address = %v, want Conflict", err)
	}
	stored := e.masterTenant("lab")
	plain, err := e.keys.Open(addressContext(stored.ID), stored.SealedAddress)
	if err != nil || string(plain) != flakyAddress("lab") {
		t.Errorf("the stored address changed (%v)", err)
	}
	// With another key file the stored address does not open, so a repeat
	// cannot be told from another tenant: Internal, nothing changed.
	e.keys = newKeys(t)
	r2 := e.newRegistry(e.config())
	_, _, err = r2.Create(ctx, NewTenant{ID: "id-lab", Name: "lab", Driver: flakyDriver, Address: flakyAddress("lab")}, e.audit("CreateTenant"), master.LocalOperator())
	if fault.KindOf(err) != fault.Internal || !strings.Contains(fault.Message(err), "does not open with this key file") {
		t.Errorf("a repeat under another key file = %v, want Internal saying the address does not open", err)
	}
}

// TestCreateWhoseRowCommittedThenFailed guards a creation whose row the
// master committed but whose call reported an error: the master is read
// again, the row is found as this call's, and the tenant is opened and ready
// rather than left upgrading with nobody to open it.
func TestCreateWhoseRowCommittedThenFailed(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	tricky := &trickyMaster{Store: e.master}
	tricky.createCommitsThenFails.Store(true)
	cfg := e.config()
	cfg.Master = tricky
	r := e.newRegistry(cfg)
	tn, _, err := r.Create(ctx, NewTenant{ID: "id-lab", Name: "lab", Driver: driver.SQLiteDriver}, e.audit("CreateTenant"), master.LocalOperator())
	if err != nil || tn.State != master.StateReady {
		t.Fatalf("Create after a lost answer = %s, %v; want ready", tn.State, err)
	}
	if _, err := r.Store("lab"); err != nil {
		t.Errorf("the tenant is not served: %v", err)
	}
	if got := e.masterTenant("lab"); got.State != master.StateReady {
		t.Errorf("the master holds %s, want ready", got.State)
	}
}

// TestRemoveWhoseCommitWasLost guards a removal that committed in the master
// and then reported an error: the tenant must not stay served. The retry
// finds no tenant, answers NotFound, and unregisters and closes it; and
// without a retry, the next round's reconciliation does.
func TestRemoveWhoseCommitWasLost(t *testing.T) {
	for _, fix := range []string{"retry", "round"} {
		t.Run(fix, func(t *testing.T) {
			ctx := context.Background()
			e := newEnv(t)
			tricky := &trickyMaster{Store: e.master}
			cfg := e.config()
			cfg.Master = tricky
			r := e.newRegistry(cfg)
			e.create(r, "id-lab", "lab", flakyDriver, flakyAddress("lab"))
			tricky.removeCommitsThenFails.Store(true)
			if _, err := r.Remove(ctx, "lab", e.audit("RemoveTenant"), master.LocalOperator()); err == nil {
				t.Fatal("the lost answer was not reported")
			}
			switch fix {
			case "retry":
				_, err := r.Remove(ctx, "lab", e.audit("RemoveTenant"), master.LocalOperator())
				if fault.KindOf(err) != fault.NotFound {
					t.Errorf("the retry = %v, want NotFound", err)
				}
			case "round":
				r.reconcile()
			}
			if _, err := r.Store("lab"); fault.KindOf(err) != fault.NotFound {
				t.Errorf("the removed tenant is still served (%v)", err)
			}
			if o, c := e.flaky.opens.Load(), e.flaky.closes.Load(); o != c {
				t.Errorf("%d stores opened and %d closed", o, c)
			}
		})
	}
}

// TestReconcileWithMaster guards that a round brings the registry to the
// master's list: a tenant another process added is registered and opened, a
// tenant another process removed is unregistered and closed, and a tenant
// whose state write finds no row is retired at once.
func TestReconcileWithMaster(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	e.seed("added", "removed", "gone-on-write")
	// The registry under test starts without "added", which another process
	// then adds back.
	added := e.masterTenant("added")
	e.ok(e.master.RemoveTenant(ctx, added.ID, e.audit("RemoveTenant"), master.LocalOperator()))
	r := e.newRegistry(e.config())
	if err := r.Start(ctx); err != nil {
		t.Fatal(err)
	}
	added.State = master.StateReady
	e.ok(e.master.CreateTenant(ctx, added, e.audit("CreateTenant"), master.LocalOperator()))
	e.ok(e.master.RemoveTenant(ctx, e.masterTenant("removed").ID, e.audit("RemoveTenant"), master.LocalOperator()))
	r.reconcile()
	waitFor(t, "the added tenant to be served", func() bool { _, err := r.Store("added"); return err == nil })
	if _, err := r.Store("removed"); fault.KindOf(err) != fault.NotFound {
		t.Errorf("a tenant the master no longer has is served (%v)", err)
	}

	// A state write that finds no row retires the tenant at once.
	e.ok(e.master.RemoveTenant(ctx, e.masterTenant("gone-on-write").ID, e.audit("RemoveTenant"), master.LocalOperator()))
	e.flaky.set("gone-on-write", down)
	checkNow(r)
	if _, err := r.Store("gone-on-write"); fault.KindOf(err) != fault.NotFound {
		t.Errorf("a tenant whose state write found no row = %v, want NotFound", err)
	}
	if err := r.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if o, c := e.flaky.opens.Load(), e.flaky.closes.Load(); o != c {
		t.Errorf("%d stores opened and %d closed", o, c)
	}
}

// ok fails the test now on an error.
func (e *env) ok(err error) {
	e.t.Helper()
	if err != nil {
		e.t.Fatal(err)
	}
}

// TestCreateReportsItsEntry guards what Create says about the request's
// entry, which its caller uses to record the request exactly once: true when
// the creation settled and wrote it (done, or failed with the undoing), false
// when Create refused before writing anything, and false when the master
// refused the acting key at the row's write, which then wrote nothing at all:
// no row, no entry, nothing registered. Remove by such a key removes nothing.
func TestCreateReportsItsEntry(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	r := e.newRegistry(e.config())
	hasEntry := func(name, id string) bool {
		entries, _, err := e.master.ListAudit(ctx, master.AuditFilter{TenantName: name}, master.Page{Limit: master.MaxPageLimit})
		if err != nil {
			t.Fatal(err)
		}
		for _, a := range entries {
			if a.ID == id {
				return true
			}
		}
		return false
	}

	e.flaky.set("down", down)
	for _, c := range []struct {
		name, drv, address string
		ok, recorded       bool
	}{
		{"made", driver.SQLiteDriver, "", true, true},
		{"down", flakyDriver, flakyAddress("down"), false, true},
		{"nodriver", "oracle", "", false, false},
	} {
		audit := e.audit("CreateTenant")
		_, recorded, err := r.Create(ctx, NewTenant{ID: "id-" + c.name, Name: c.name, Driver: c.drv, Address: c.address}, audit, master.LocalOperator())
		if (err == nil) != c.ok || recorded != c.recorded {
			t.Errorf("%s: err %v, recorded %v; want ok %v, recorded %v", c.name, err, recorded, c.ok, c.recorded)
		}
		if got := hasEntry(c.name, audit.ID); got != recorded {
			t.Errorf("%s: Create reported recorded %v, the log holds the entry: %v", c.name, recorded, got)
		}
	}

	hash := make([]byte, master.KeyHashLength)
	k := master.Key{ID: "revoked-admin", Hash: hash, Holder: "a test", Created: e.clock.Now()}
	if err := e.master.CreateKey(ctx, k, e.audit("Setup"), master.LocalOperator()); err != nil {
		t.Fatal(err)
	}
	if _, err := e.master.SetGrant(ctx, master.Grant{KeyID: k.ID, Scope: master.Installation, Role: master.RoleAdmin}, e.audit("Setup"), master.LocalOperator()); err != nil {
		t.Fatal(err)
	}
	by := master.ActingKey(k.ID, master.Installation, master.RoleAdmin, e.clock.Now)
	if _, err := e.master.RevokeKey(ctx, k.ID, e.clock.Now(), e.audit("Setup"), master.LocalOperator()); err != nil {
		t.Fatal(err)
	}
	audit := e.audit("CreateTenant")
	_, recorded, err := r.Create(ctx, NewTenant{ID: "id-refused", Name: "refused", Driver: driver.SQLiteDriver}, audit, by)
	if fault.KindOf(err) != fault.Unauthenticated || recorded {
		t.Errorf("Create by a revoked key: %v, recorded %v; want Unauthenticated, not recorded", err, recorded)
	}
	if _, ok := master.RefusalOf(err); !ok {
		t.Errorf("Create by a revoked key: %v, want the master's refusal in the chain", err)
	}
	if _, err := e.master.TenantByName(ctx, "refused"); fault.KindOf(err) != fault.NotFound {
		t.Errorf("Create by a revoked key left a row: %v", err)
	}
	if entries, _, _ := e.master.ListAudit(ctx, master.AuditFilter{TenantName: "refused"}, master.Page{Limit: 10}); len(entries) != 0 {
		t.Errorf("Create by a revoked key wrote entries: %+v", entries)
	}
	if _, ok := r.State("refused"); ok {
		t.Error("Create by a revoked key registered the tenant")
	}

	_, err = r.Remove(ctx, "made", e.audit("RemoveTenant"), by)
	if fault.KindOf(err) != fault.Unauthenticated {
		t.Errorf("Remove by a revoked key: %v, want Unauthenticated", err)
	}
	if _, err := e.master.TenantByName(ctx, "made"); err != nil {
		t.Errorf("Remove by a revoked key removed the tenant: %v", err)
	}
	if _, ok := r.State("made"); !ok {
		t.Error("Remove by a revoked key unregistered the tenant")
	}
}

// TestUnregisteredIsToldOfARemoval guards Config.Unregistered: a removed
// tenant's name is passed once, when the registry lets it go, so what runs
// for it elsewhere can stop at once; a tenant made and kept is not.
func TestUnregisteredIsToldOfARemoval(t *testing.T) {
	e := newEnv(t)
	var mu sync.Mutex
	var told []string
	cfg := e.config()
	cfg.Unregistered = func(name string) {
		mu.Lock()
		told = append(told, name)
		mu.Unlock()
	}
	r := e.newRegistry(cfg)
	e.create(r, "id-lab", "lab", flakyDriver, flakyAddress("lab"))
	e.create(r, "id-kept", "kept", flakyDriver, flakyAddress("kept"))
	if _, err := r.Remove(context.Background(), "lab", e.audit("RemoveTenant"), master.LocalOperator()); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if strings.Join(told, ",") != "lab" {
		t.Errorf("told of %v, want [lab]", told)
	}
}
