package settingvalue

import (
	"context"

	"github.com/amrshadid/nopacs/internal/core/settings"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// QuotaBounds returns quota.bytes, quota.instances and quota.warn as they
// apply on the tenant: each bound 0 when none is set.
func (t Tenant) QuotaBounds(ctx context.Context) (tenant.QuotaBounds, error) {
	v, err := t.r.Tenant(ctx, t.st, settings.QuotaBytes)
	bytes, err := typed(settings.QuotaBytes, v, err, settings.ParseQuotaBytes)
	if err != nil {
		return tenant.QuotaBounds{}, err
	}
	v, err = t.r.Tenant(ctx, t.st, settings.QuotaInstances)
	instances, err := typed(settings.QuotaInstances, v, err, settings.ParseQuotaInstances)
	if err != nil {
		return tenant.QuotaBounds{}, err
	}
	v, err = t.r.Tenant(ctx, t.st, settings.QuotaWarn)
	warn, err := typed(settings.QuotaWarn, v, err, settings.ParseQuotaWarn)
	if err != nil {
		return tenant.QuotaBounds{}, err
	}
	return tenant.QuotaBounds{Bytes: bytes, Instances: instances, WarnPercent: warn}, nil
}
