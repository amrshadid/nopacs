package sqlite

import (
	"context"
	"time"

	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// The tenant's coverage reports (tenant.DeidCoverages), on the tables of
// migrations/tenant/0001_baseline.sql: package sqlkit's one implementation.

// QueueDeidCoverage implements tenant.DeidCoverages.
func (t *Tenant) QueueDeidCoverage(ctx context.Context, q tenant.DeidCoverageQueue, audit master.AuditEntry, guard tenant.Guard) (tenant.Job, error) {
	return t.sending().QueueDeidCoverage(ctx, q, audit, guard)
}

// DeidCoverageJob implements tenant.DeidCoverages.
func (t *Tenant) DeidCoverageJob(ctx context.Context, id string) (tenant.DeidCoverageJob, error) {
	return t.sending().DeidCoverageJob(ctx, id)
}

// DeidCoverageInProgress implements tenant.DeidCoverages.
func (t *Tenant) DeidCoverageInProgress(ctx context.Context) (tenant.Job, bool, error) {
	return t.sending().DeidCoverageInProgress(ctx)
}

// DeidCoverageScans implements tenant.DeidCoverages.
func (t *Tenant) DeidCoverageScans(ctx context.Context, after int64, limit int) ([]tenant.DeidCoverageScan, error) {
	return t.sending().DeidCoverageScans(ctx, after, limit)
}

// ApplyDeidCoverage implements tenant.DeidCoverages.
func (t *Tenant) ApplyDeidCoverage(ctx context.Context, a tenant.DeidCoverageApply) (tenant.DeidCoverageJob, error) {
	return t.sending().ApplyDeidCoverage(ctx, a)
}

// EndDeidCoverage implements tenant.DeidCoverages.
func (t *Tenant) EndDeidCoverage(ctx context.Context, id string, now time.Time) (tenant.Job, error) {
	return t.sending().EndDeidCoverage(ctx, id, now)
}

// StopDeidCoverage implements tenant.DeidCoverages.
func (t *Tenant) StopDeidCoverage(ctx context.Context, id, reason string, now time.Time) (tenant.Job, error) {
	return t.sending().StopDeidCoverage(ctx, id, reason, now)
}

// DeidCoverageReport implements tenant.DeidCoverages.
func (t *Tenant) DeidCoverageReport(ctx context.Context) (tenant.DeidCoverageReport, bool, error) {
	return t.sending().DeidCoverageReport(ctx)
}

// DeidCoverageElements implements tenant.DeidCoverages.
func (t *Tenant) DeidCoverageElements(ctx context.Context, jobID string, after *tenant.CoverageKey, limit int) ([]tenant.CoverageElement, error) {
	return t.sending().DeidCoverageElements(ctx, jobID, after, limit)
}
