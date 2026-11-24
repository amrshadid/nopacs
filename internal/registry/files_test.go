package registry

import (
	"context"
	"os"
	"runtime"
	"strings"
	"testing"

	"github.com/amrshadid/nopacs/internal/driver"
	"github.com/amrshadid/nopacs/internal/master"
)

// TestLoosenedTenantFile guards what happens to a tenant whose database file
// a person made readable by others: the driver refuses it as Invalid, so the
// tenant is failed, with a reason that says how to fix it and names no path,
// and the other tenants start. Once the mode is fixed, the next start
// serves it again.
func TestLoosenedTenantFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("file modes are not used on Windows")
	}
	ctx := context.Background()
	e := newEnv(t)
	setup := e.newRegistry(e.config())
	e.create(setup, "id-loose", "loose", driver.SQLiteDriver, "")
	e.create(setup, "id-fine", "fine", driver.SQLiteDriver, "")
	if err := setup.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	path, err := driver.SQLiteTenants{DataDir: e.data}.Path("loose")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil { //nolint:gosec // G302: the loosened mode is what is tested.
		t.Fatal(err)
	}
	r := e.newRegistry(e.config())
	if err := r.Start(ctx); err != nil {
		t.Fatal(err)
	}
	st, _ := r.State("loose")
	if st.State != master.StateFailed || !strings.Contains(st.Reason, "chmod 600") {
		t.Errorf("loose is %s %q, want failed with the fix in its reason", st.State, st.Reason)
	}
	mustNotContain(t, "the reason", st.Reason, e.data)
	if got := e.masterTenant("loose"); got.State != master.StateFailed {
		t.Errorf("the master holds %s for loose, want failed", got.State)
	}
	if _, err := r.Store("fine"); err != nil {
		t.Errorf("the other tenant did not start: %v", err)
	}
	t.Logf("reason: %s", st.Reason)
	if err := r.Close(context.Background()); err != nil {
		t.Fatal(err)
	}

	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	again := e.newRegistry(e.config())
	if err := again.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := again.Store("loose"); err != nil {
		t.Errorf("after the mode is fixed the tenant is not served: %v", err)
	}
}

// TestRepeatWithTheSameAuditEntry guards a creation repeated with the very
// request the first one recorded (the same audit entry id, as a client that
// retries does): the master refuses the entry as a duplicate, which the
// registry reads as "already recorded", and the creation succeeds.
func TestRepeatWithTheSameAuditEntry(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	r := e.newRegistry(e.config())
	audit := e.audit("CreateTenant")
	nt := NewTenant{ID: "id-lab", Name: "lab", Driver: driver.SQLiteDriver}
	if _, _, err := r.Create(ctx, nt, audit, master.LocalOperator()); err != nil {
		t.Fatal(err)
	}
	tn, _, err := r.Create(ctx, nt, audit, master.LocalOperator())
	if err != nil || tn.State != master.StateReady {
		t.Errorf("the same creation again = %s, %v; want ready", tn.State, err)
	}
}
