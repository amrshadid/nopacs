package sqlite

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/core/settings"
	"github.com/amrshadid/nopacs/internal/driver/sqlkit"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// TestAllowAsReceivedMigration guards the migration that lets receivers
// de-identify: a tenant that had a receiver before it is given
// receiver.allow-as-received "true", so its receivers, which archive as
// received, may stay so; a tenant with none, and a tenant made after,
// hold no value and take the built-in default, "false".
func TestAllowAsReceivedMigration(t *testing.T) {
	ctx := context.Background()
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	audit := func(id string) master.AuditEntry {
		return master.AuditEntry{ID: id, Time: at, Method: "test.setup", Outcome: master.OutcomeDone}
	}
	open := func(name string, upTo int) *Tenant {
		s, err := OpenTenant(ctx, filepath.Join(t.TempDir(), name+".db"), "t1")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = s.Close() })
		if _, err := sqlkit.Migrate(ctx, s.p.write, dialect, tenantMigrations[:upTo]); err != nil {
			t.Fatal(err)
		}
		return s
	}
	// The migration that adds the setting is the thirteenth.
	before := 12
	used := open("used", before)
	lendImportArrivals(t, used)
	if _, err := used.AddStorage(ctx, tenant.Storage{Name: "main", Type: tenant.StorageDirectory,
		Directory: tenant.DirectoryStorage{Root: "archive"}}, at, audit("a1"), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := used.AddReceiver(ctx, tenant.Receiver{Name: "scanner", Type: tenant.ReceiverDIMSE, Storage: "main",
		QuietPeriod: tenant.DefaultQuietPeriod, MaxOpenTime: tenant.DefaultMaxOpenTime, MaxFileBytes: tenant.DefaultMaxFileBytes,
		DIMSE: tenant.DIMSEReceiver{Port: 11112, AETitle: "NOPACS", AnyCaller: true}}, at, audit("a2"), nil); err != nil {
		t.Fatal(err)
	}
	takeBackImportArrivals(t, used)
	unused := open("unused", before)
	fresh := open("fresh", 0)
	for _, s := range []*Tenant{used, unused, fresh} {
		if err := s.Migrate(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if v, err := used.Setting(ctx, settings.ReceiverAllowAsReceived); err != nil || v != "true" {
		t.Errorf("a tenant with a receiver holds %q (%v), want true", v, err)
	}
	for name, s := range map[string]*Tenant{"with no receiver": unused, "made after": fresh} {
		if _, err := s.Setting(ctx, settings.ReceiverAllowAsReceived); fault.KindOf(err) != fault.NotFound {
			t.Errorf("a tenant %s holds a value (%v)", name, err)
		}
	}
	r, err := used.Receiver(ctx, "scanner")
	if err != nil || !r.Profile.IsZero() {
		t.Errorf("the receiver after the migration: %v, %v", r.Profile, err)
	}
}
