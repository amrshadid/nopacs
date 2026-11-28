package postgres

import (
	"context"

	"github.com/amrshadid/nopacs/internal/tenant"
)

// The tenant's use against its quota (tenant.Quotas), on the running totals
// of migrations/tenant/0001_baseline.sql: package sqlkit's one
// implementation, read here by one statement, whose snapshot makes the
// three totals agree at read committed.

var _ tenant.Quotas = (*Tenant)(nil)

// QuotaUse implements tenant.Quotas.
func (t *Tenant) QuotaUse(ctx context.Context) (tenant.QuotaUse, error) {
	return t.sending().QuotaUse(ctx)
}
