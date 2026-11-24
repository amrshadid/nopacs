package tenanttest

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// WriteSample writes a small tenant into s: two setting overrides, one of
// them set twice and one removed, and the audit entries of each change plus
// one refusal, so that a tool other than NoPACs can read what the driver
// wrote. It leaves 2 settings and 5 audit entries.
func WriteSample(tb testing.TB, s tenant.Store) {
	tb.Helper()
	ctx := context.Background()
	n := 0
	audit := func(method, object string, outcome master.Outcome) master.AuditEntry {
		n++
		return master.AuditEntry{
			ID: fmt.Sprintf("sample-tenant-audit-%02d", n), Time: T0.Add(time.Duration(n) * time.Second),
			KeyID: "SampleKey001", RemoteAddress: "192.0.2.10:50000", Method: method,
			TenantName: "sample", Object: object, Outcome: outcome,
			Details: map[string]string{"name": object}, RequestID: "sample-request",
		}
	}
	must := func(err error) {
		tb.Helper()
		if err != nil {
			tb.Fatal(err)
		}
	}
	must(s.SetSetting(ctx, "reception.quiet_seconds", "60", audit("SetSetting", "reception.quiet_seconds", master.OutcomeDone), nil))
	must(s.SetSetting(ctx, "reception.quiet_seconds", "90", audit("SetSetting", "reception.quiet_seconds", master.OutcomeDone), nil))
	must(s.SetSetting(ctx, "label.prefix", "Zoë-山田", audit("SetSetting", "label.prefix", master.OutcomeDone), nil))
	_, err := s.UnsetSetting(ctx, "never.set", audit("UnsetSetting", "never.set", master.OutcomeDone), nil)
	must(err)
	must(s.AppendAudit(ctx, audit("SetSetting", "Bad Name", master.OutcomeRefused)))
}
