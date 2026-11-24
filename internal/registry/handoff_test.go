package registry

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/amrshadid/nopacs/internal/driver"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// TestHandoffMovedIsReadOnly guards a moved tenant: Store refuses it as a
// precondition carrying TENANT_MOVED, Reader still gives its store for
// reads, State says moved; the master keeps moved through the timer's
// checks and across a new registry (a restart); and SetHandoff with ready
// takes it back, Store serving it again.
func TestHandoffMovedIsReadOnly(t *testing.T) {
	e := newEnv(t)
	r := e.newRegistry(e.config())
	if err := r.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	e.create(r, "t1", "lab", driver.SQLiteDriver, "")
	if err := r.SetHandoff(context.Background(), "lab", master.StateMoved, "exported", e.audit("ExportTenant"), master.LocalOperator()); err != nil {
		t.Fatal(err)
	}
	moved := func(r *Registry) {
		t.Helper()
		_, err := r.Store("lab")
		if fault.KindOf(err) != fault.Precondition || !strings.HasPrefix(fault.Message(err), master.ReasonTenantMoved+":") {
			t.Fatalf("Store of a moved tenant = %v, want a precondition naming %s", err, master.ReasonTenantMoved)
		}
		if st, err := r.Reader("lab"); err != nil || st == nil {
			t.Fatalf("Reader of a moved tenant = %v, %v", st, err)
		}
		if s, _ := r.State("lab"); s.State != master.StateMoved || s.Reason != "exported" {
			t.Fatalf("State = %+v", s)
		}
	}
	moved(r)
	r.checkAll().Wait()
	moved(r)
	if row := e.masterTenant("lab"); row.State != master.StateMoved {
		t.Fatalf("the master holds %s after a check", row.State)
	}
	if err := r.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	again := e.newRegistry(e.config())
	if err := again.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	moved(again)
	if err := again.SetHandoff(context.Background(), "lab", master.StateReady, "", e.audit("ReclaimTenant"), master.LocalOperator()); err != nil {
		t.Fatal(err)
	}
	if _, err := again.Store("lab"); err != nil {
		t.Fatalf("Store after the reclaim = %v", err)
	}
	if row := e.masterTenant("lab"); row.State != master.StateReady {
		t.Fatalf("the master holds %s after the reclaim", row.State)
	}
}

// TestHandoffGiveBackOnlyFromMoved guards the end of an export that
// failed: CheckMoved says a moved tenant is moved and refuses a ready one;
// GiveBack makes a moved tenant ready, and refuses, changing nothing, a
// ready one and a fenced one (fenced while the export ran), which stays
// fenced in the registry and the master.
func TestHandoffGiveBackOnlyFromMoved(t *testing.T) {
	e := newEnv(t)
	var reason atomic.Value
	reason.Store("")
	cfg := e.config()
	cfg.Fence = func(context.Context, string, string, tenant.Store) (string, error) {
		return reason.Load().(string), nil
	}
	r := e.newRegistry(cfg)
	if err := r.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	e.create(r, "t1", "lab", driver.SQLiteDriver, "")
	if err := r.CheckMoved("lab"); fault.KindOf(err) != fault.Precondition || !strings.Contains(fault.Message(err), "is ready, no longer moved") {
		t.Fatalf("CheckMoved of a ready tenant = %v", err)
	}
	if err := r.GiveBack(context.Background(), "lab", e.audit("ExportTenant")); fault.KindOf(err) != fault.Precondition {
		t.Fatalf("GiveBack of a ready tenant = %v", err)
	}
	if err := r.SetHandoff(context.Background(), "lab", master.StateMoved, "exported", e.audit("ExportTenant"), master.LocalOperator()); err != nil {
		t.Fatal(err)
	}
	if err := r.CheckMoved("lab"); err != nil {
		t.Fatalf("CheckMoved of a moved tenant = %v", err)
	}
	if err := r.GiveBack(context.Background(), "lab", e.audit("ExportTenant")); err != nil {
		t.Fatalf("GiveBack of a moved tenant = %v", err)
	}
	if _, err := r.Store("lab"); err != nil {
		t.Fatalf("Store after GiveBack = %v", err)
	}
	if err := r.SetHandoff(context.Background(), "lab", master.StateMoved, "exported", e.audit("ExportTenant"), master.LocalOperator()); err != nil {
		t.Fatal(err)
	}
	reason.Store("the storage main's owner record names installation B")
	if err := r.CheckFence("lab"); fault.KindOf(err) != fault.Precondition {
		t.Fatalf("CheckFence = %v", err)
	}
	if err := r.CheckMoved("lab"); fault.KindOf(err) != fault.Precondition || !strings.Contains(fault.Message(err), "is fenced") {
		t.Fatalf("CheckMoved of a fenced tenant = %v", err)
	}
	if err := r.GiveBack(context.Background(), "lab", e.audit("ExportTenant")); fault.KindOf(err) != fault.Precondition {
		t.Fatalf("GiveBack of a fenced tenant = %v", err)
	}
	if s, _ := r.State("lab"); s.State != master.StateFenced {
		t.Fatalf("State after a refused GiveBack = %+v", s)
	}
	if row := e.masterTenant("lab"); row.State != master.StateFenced {
		t.Fatalf("the master holds %s after a refused GiveBack", row.State)
	}
}

// TestHandoffFence guards the fence check: a tenant whose fence says
// nothing is ready; once it names a reason the tenant turns fenced at the
// next check (Store refused with TENANT_FENCED, Reader served, the master
// and its audit say fenced with the reason), a moved tenant turns fenced
// too, and a fenced tenant is not checked again; a check that fails fences
// nothing; CheckFence checks at once.
func TestHandoffFence(t *testing.T) {
	e := newEnv(t)
	var reason atomic.Value
	reason.Store("")
	var calls atomic.Int64
	var fail atomic.Bool
	cfg := e.config()
	cfg.Fence = func(_ context.Context, id, name string, st tenant.Store) (string, error) {
		calls.Add(1)
		if id == "" || name == "" || st == nil {
			t.Errorf("fence called with %q %q %v", id, name, st)
		}
		if fail.Load() {
			return "", fault.New(fault.Unavailable, "the storage does not answer")
		}
		return reason.Load().(string), nil
	}
	r := e.newRegistry(cfg)
	if err := r.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	e.create(r, "t1", "lab", driver.SQLiteDriver, "")
	e.create(r, "t2", "clinic", driver.SQLiteDriver, "")
	if _, err := r.Store("lab"); err != nil {
		t.Fatalf("a tenant no fence names: %v", err)
	}
	fail.Store(true)
	reason.Store("the storage main's owner record names installation B")
	r.checkAll().Wait()
	if _, err := r.Store("lab"); err != nil {
		t.Fatalf("a check that failed fenced the tenant: %v", err)
	}
	fail.Store(false)
	if err := r.SetHandoff(context.Background(), "clinic", master.StateMoved, "", e.audit("ExportTenant"), master.LocalOperator()); err != nil {
		t.Fatal(err)
	}
	r.checkAll().Wait()
	for _, name := range []string{"lab", "clinic"} {
		_, err := r.Store(name)
		if fault.KindOf(err) != fault.Precondition || !strings.HasPrefix(fault.Message(err), master.ReasonTenantFenced+":") ||
			!strings.Contains(fault.Message(err), "names installation B") {
			t.Fatalf("Store of fenced %s = %v", name, err)
		}
		if _, err := r.Reader(name); err != nil {
			t.Fatalf("Reader of fenced %s = %v", name, err)
		}
		if row := e.masterTenant(name); row.State != master.StateFenced || !strings.Contains(row.StateReason, "names installation B") {
			t.Fatalf("the master holds %s (%q) for %s", row.State, row.StateReason, name)
		}
	}
	if es := e.systemEntries("lab"); len(es) == 0 || es[len(es)-1].Details[master.DetailNewState] != "fenced" {
		t.Fatalf("the fence wrote no state change entry: %+v", es)
	}
	before := calls.Load()
	r.checkAll().Wait()
	if calls.Load() != before {
		t.Error("a fenced tenant was checked again")
	}
	reason.Store("")
	e.create(r, "t3", "third", driver.SQLiteDriver, "")
	if err := r.CheckFence("third"); err != nil {
		t.Fatalf("CheckFence of a tenant no fence names = %v", err)
	}
	reason.Store("the storage main's owner record names installation B")
	if err := r.CheckFence("third"); fault.KindOf(err) != fault.Precondition {
		t.Fatalf("CheckFence = %v, want the fence at once", err)
	}
}
