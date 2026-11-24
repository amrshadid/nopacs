package sqlite

import (
	"context"
	"time"

	"github.com/amrshadid/nopacs/internal/driver/sqlkit"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// The tenant's de-identification profiles and lookup tables
// (tenant.Deidentification), on the tables of
// migrations/tenant/0001_baseline.sql: package sqlkit's one
// implementation, run here in SQLite's transactions. One writer runs at a
// time, so no row lock is taken and the clauses that would take one are
// empty.

// deidentification returns the profiles' and lookups' implementation on t.
func (t *Tenant) deidentification() sqlkit.Deidentification {
	return sqlkit.Deidentification{Host: sendingHost{t}, D: sendingDialect}
}

// CreateProfileVersion implements tenant.Profiles.
func (t *Tenant) CreateProfileVersion(ctx context.Context, v tenant.NewProfileVersion, now time.Time, audit master.AuditEntry, guard tenant.Guard) (tenant.ProfileVersion, bool, error) {
	return t.deidentification().CreateProfileVersion(ctx, v, now, audit, guard)
}

// ProfileVersion implements tenant.Profiles.
func (t *Tenant) ProfileVersion(ctx context.Context, name string, version int32) (tenant.ProfileVersion, error) {
	return t.deidentification().ProfileVersion(ctx, name, version)
}

// ListProfiles implements tenant.Profiles.
func (t *Tenant) ListProfiles(ctx context.Context, page master.Page) ([]tenant.ProfileSummary, string, error) {
	return t.deidentification().ListProfiles(ctx, page)
}

// ListProfileVersions implements tenant.Profiles.
func (t *Tenant) ListProfileVersions(ctx context.Context, name string, page master.Page) ([]tenant.ProfileVersion, string, error) {
	return t.deidentification().ListProfileVersions(ctx, name, page)
}

// InstanceProfile implements tenant.Profiles.
func (t *Tenant) InstanceProfile(ctx context.Context, sopInstanceUID string) (tenant.ProfileRef, error) {
	return t.deidentification().InstanceProfile(ctx, sopInstanceUID)
}

// LookupCode implements tenant.LookupTables.
func (t *Tenant) LookupCode(ctx context.Context, k tenant.LookupKey) (string, bool, error) {
	return t.deidentification().LookupCode(ctx, k)
}

// AssignLookupCodes implements tenant.LookupTables.
func (t *Tenant) AssignLookupCodes(ctx context.Context, keys []tenant.LookupKey, now time.Time) ([]string, error) {
	return t.deidentification().AssignLookupCodes(ctx, keys, now)
}

// ImportLookupCodes implements tenant.LookupImports.
func (t *Tenant) ImportLookupCodes(ctx context.Context, table string, entries []tenant.LookupEntry, now time.Time, audit master.AuditEntry, guard tenant.Guard) (tenant.LookupImport, error) {
	return t.deidentification().ImportLookupCodes(ctx, table, entries, now, audit, guard)
}

// ListLookupTables implements tenant.LookupImports.
func (t *Tenant) ListLookupTables(ctx context.Context, page master.Page) ([]tenant.LookupTableCount, string, error) {
	return t.deidentification().ListLookupTables(ctx, page)
}
