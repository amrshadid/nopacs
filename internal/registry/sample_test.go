package registry

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/driver"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/secret"
	"github.com/amrshadid/nopacs/internal/tenant/tenanttest"
)

// TestWriteTenantSample creates a tenant on SQLite through the registry, as
// the server will, and writes the tenant suite's sample into it, for the
// outside check (scripts/check/tenant-sqlite.sh), which reads the files with
// the sqlite3 tool. It runs only when NOPACS_SAMPLE_DIR names a directory,
// which it uses as a data directory: master.db, key and tenants/sample.db.
func TestWriteTenantSample(t *testing.T) {
	dir := os.Getenv("NOPACS_SAMPLE_DIR")
	if dir == "" {
		t.Skip("NOPACS_SAMPLE_DIR is not set; scripts/check/tenant-sqlite.sh sets it")
	}
	ctx := context.Background()
	keyPath := filepath.Join(dir, "key")
	if err := secret.CreateKeyFile(keyPath); err != nil {
		t.Fatal(err)
	}
	keys, err := secret.LoadKeyFile(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	m, err := driver.OpenMaster(ctx, driver.SQLiteScheme+filepath.Join(dir, "master.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = m.Close() }()
	if err := m.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	r, err := New(Config{
		Master: m, Keys: keys, Drivers: map[string]Driver{driver.SQLiteDriver: driver.SQLiteTenants{DataDir: dir}},
		Now: func() time.Time { return t0 }, Interval: time.Hour, OpenTimeout: 10 * time.Second, PingTimeout: time.Second, MaxOpening: 4,
	})
	if err != nil {
		t.Fatal(err)
	}
	audit := master.AuditEntry{ID: "sample-create", Time: t0, LocalUser: "501", Method: "/nopacs.v1.TenantService/CreateTenant", TenantName: "sample", Outcome: master.OutcomeDone, RequestID: "sample-request"}
	tn, _, err := r.Create(ctx, NewTenant{ID: "sample-tenant", Name: "sample", Driver: driver.SQLiteDriver}, audit, master.LocalOperator())
	if err != nil {
		t.Fatal(err)
	}
	if tn.State != master.StateReady {
		t.Fatalf("the sample tenant is %s %q", tn.State, tn.StateReason)
	}
	s, err := r.Store("sample")
	if err != nil {
		t.Fatal(err)
	}
	tenanttest.WriteSample(t, s)
	if err := r.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}
