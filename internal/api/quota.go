package api

import (
	"context"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/core/settings"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// reasonQuotaReached is the phrase of an import job held back while its
// tenant is at its storage quota, from the fixed set the contract has
// (job.proto, Job.reason).
const reasonQuotaReached = "quota reached"

// messageQuotaMasked refuses a change of a storage quota by a masked key.
const messageQuotaMasked = "a storage quota (quota.bytes, quota.instances) is set by a person, never by a masked key"

// quotaSettings are the settings that bound what a tenant may hold: set on
// a tenant by the installation's admin only, and never by a masked key,
// whatever its grants. A masked key is held by a service outside the site,
// and how much of a storage shared by every tenant one tenant may take is
// the installation's decision, never that service's.
var quotaSettings = map[string]bool{settings.QuotaBytes: true, settings.QuotaInstances: true}

// quotaStateMessage returns a quota's state as the contract writes it.
func quotaStateMessage(s tenant.QuotaState) v1.QuotaState {
	return v1.QuotaState(word("QUOTA_STATE_", string(s), v1.QuotaState_value))
}

// tenantQuota returns what the tenant whose database is st holds against
// its storage quota, and where that stands, or nil when its database keeps
// no counts or does not answer: a part of an answer that cannot be read is
// left out, never answered as an error.
func (c *core) tenantQuota(ctx context.Context, st tenant.Store) *v1.TenantQuota {
	q, ok := st.(tenant.Quotas)
	if !ok {
		return nil
	}
	bounds, err := c.values().OnTenant(st).QuotaBounds(ctx)
	if err != nil {
		return nil
	}
	use, err := q.QuotaUse(ctx)
	if err != nil {
		return nil
	}
	return &v1.TenantQuota{
		UsedBytes: use.Bytes(), UsedInstances: use.Instances(), BytesBound: bounds.Bytes, InstancesBound: bounds.Instances,
		WarnPercent: int32(bounds.WarnPercent), //nolint:gosec // G115: 1 to 100.
		State:       quotaStateMessage(bounds.State(use)), PurgeBytes: use.PurgeBytes, PurgeInstances: use.PurgeInstances,
	}
}

// namedTenantQuota is tenantQuota for the tenant named name, read through
// the registry: nil when the tenant's database is not open or serves no
// data (fenced), as GetTenant answers such a tenant from the master alone.
func (c *core) namedTenantQuota(ctx context.Context, name string) *v1.TenantQuota {
	st, err := c.reg.Store(name)
	if fault.KindOf(err) == fault.Precondition {
		// A moved tenant's counts are still read; a fenced one serves none.
		st, err = c.reg.DataReader(name)
	}
	if err != nil || st == nil {
		return nil
	}
	return c.tenantQuota(ctx, st)
}

// quotaHeld returns the phrase of an import job of the tenant whose
// database is st held back by its storage quota: reasonQuotaReached while
// the tenant is at a bound, else "".
func (c *core) quotaHeld(ctx context.Context, st tenant.Store) string {
	if q := c.tenantQuota(ctx, st); q != nil && q.GetState() == v1.QuotaState_QUOTA_STATE_REACHED {
		return reasonQuotaReached
	}
	return ""
}
