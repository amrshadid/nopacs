package tui

import (
	"testing"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/testkit/fakeserver"
)

// quotaTenant is tenant t000 with a storage quota past its warning.
func quotaTenant(state v1.QuotaState) func(*fakeserver.Server) {
	return func(f *fakeserver.Server) {
		f.AddTenant(&v1.Tenant{Name: "t000", DatabaseType: v1.DatabaseType_DATABASE_TYPE_SQLITE, State: v1.TenantState_TENANT_STATE_READY, CreateTime: timestamp(-5),
			Quota: &v1.TenantQuota{UsedBytes: 900 << 20, BytesBound: 1000 << 20, UsedInstances: 90, InstancesBound: 100, State: state, WarnPercent: 80,
				PurgeBytes: 10 << 20, PurgeInstances: 5}})
	}
}

// TestTenantScreenShowsItsQuotaAndItsWarning guards the quota of a tenant: the
// list leaves it out, so opening the tenant reads it; the screen shows the use
// against each bound, what its purges will give back and the warning for the state
// it stands in, and none for a tenant under its warning.
func TestTenantScreenShowsItsQuotaAndItsWarning(t *testing.T) {
	forSizes(t, func(t *testing.T, w, h int) {
		r := newRig(t, w, h, quotaTenant(v1.QuotaState_QUOTA_STATE_WARNED))
		r.key("enter", "enter")
		if r.calls("GetTenant") != 1 {
			t.Errorf("the tenant was read %d times, want once, for its quota", r.calls("GetTenant"))
		}
		r.expect("Tenant t000", "Quota:", "900.0 MiB of 1000.0 MiB", "90 instances of 100", "(warned)", "To purge:", "awaiting purge", "Quota warning:", "past quota.warn (80%)")

		r = newRig(t, w, h, quotaTenant(v1.QuotaState_QUOTA_STATE_REACHED))
		r.key("enter", "enter")
		r.expect("(reached)", "at its storage quota")

		r = newRig(t, w, h, quotaTenant(v1.QuotaState_QUOTA_STATE_UNDER))
		r.key("enter", "enter")
		r.expect("Quota:", "(under)")
		r.refuse("Quota warning:")

		r = newRig(t, w, h, func(f *fakeserver.Server) { addTenants(f, 1) })
		r.key("enter", "enter")
		r.expect("Tenant t000")
		r.refuse("Quota:")
	})
}

// TestStatusShowsTheQuota guards the quota on the tenant's status, which follows
// the tenant's events: the same use and the same warning.
func TestStatusShowsTheQuota(t *testing.T) {
	r := newRig(t, 120, 40, quotaTenant(v1.QuotaState_QUOTA_STATE_WARNED))
	r.key("enter", "enter", "t")
	r.expect("Status of t000", "Quota:", "(warned)", "Quota warning:", "past quota.warn (80%)")
}
