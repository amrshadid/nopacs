package postgres

import (
	"context"

	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// The tenant's change control (tenant.ProfileTrials), on the profile_trials
// table of migrations/tenant/0001_baseline.sql: package sqlkit's one
// implementation, run here in postgres's transactions.

var _ tenant.ProfileTrials = (*Tenant)(nil)

// RecordProfileTrial implements tenant.ProfileTrials.
func (t *Tenant) RecordProfileTrial(ctx context.Context, trial tenant.ProfileTrial, audit master.AuditEntry, guard tenant.Guard) error {
	return t.deidentification().RecordProfileTrial(ctx, trial, audit, guard)
}

// ProfileTrial implements tenant.ProfileTrials.
func (t *Tenant) ProfileTrial(ctx context.Context, receiver string, ref tenant.ProfileRef) (tenant.ProfileTrial, error) {
	return t.deidentification().ProfileTrial(ctx, receiver, ref)
}
