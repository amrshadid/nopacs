package sqlite

import (
	"context"
	"fmt"
	"testing"

	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/master/mastertest"
	"github.com/amrshadid/nopacs/internal/tenant/tenanttest"
)

// BenchmarkTenantSetting measures reading a tenant's setting by name, in a
// table of 1,000 settings.
func BenchmarkTenantSetting(b *testing.B) {
	s := openTenantTest(b, tenanttest.SuiteTenant)
	ctx := context.Background()
	for i := range 1000 {
		a := master.AuditEntry{ID: fmt.Sprintf("setup-%05d", i), Time: mastertest.T0, Method: "Setup", Outcome: master.OutcomeDone}
		if err := s.SetSetting(ctx, fmt.Sprintf("setting.%04d", i), "v", a, nil); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if _, err := s.Setting(ctx, "setting.0500"); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkTenantAppendAudit measures writing one audit entry to a tenant's
// log in its own transaction, as every request of the tenant does last.
func BenchmarkTenantAppendAudit(b *testing.B) {
	s := openTenantTest(b, tenanttest.SuiteTenant)
	ctx := context.Background()
	entry := master.AuditEntry{
		Time: mastertest.T0, KeyID: "BenchKey0500", RemoteAddress: "192.0.2.10:50000", Method: "/nopacs.v1.SettingsService/GetSetting",
		TenantName: "lab", Outcome: master.OutcomeDone, Details: map[string]string{"name": "x"}, RequestID: "bench",
	}
	i := 0
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		i++
		entry.ID = fmt.Sprintf("bench-%09d", i)
		if err := s.AppendAudit(ctx, entry); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkTenantChanges measures changes to a tenant's database, each a
// setting with its audit entry, from 1, 8, 64 and 256 concurrent callers.
func BenchmarkTenantChanges(b *testing.B) {
	s := openTenantTest(b, tenanttest.SuiteTenant)
	benchChanges(b, settingChange(func(ctx context.Context, name, value string, audit master.AuditEntry) error {
		return s.SetSetting(ctx, name, value, audit, nil)
	}))
}
