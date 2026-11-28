package sqlite

import (
	"context"
	"time"

	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// The tenant's search backfills (tenant.SearchBackfills), on the tables of
// migrations/tenant/0001_baseline.sql: package sqlkit's one implementation.

// QueueSearchBackfill implements tenant.SearchBackfills.
func (t *Tenant) QueueSearchBackfill(ctx context.Context, q tenant.SearchBackfillQueue, audit master.AuditEntry, guard tenant.Guard) (tenant.Job, error) {
	return t.sending().QueueSearchBackfill(ctx, q, audit, guard)
}

// SearchBackfillJob implements tenant.SearchBackfills.
func (t *Tenant) SearchBackfillJob(ctx context.Context, id string) (tenant.SearchBackfillJob, error) {
	return t.sending().SearchBackfillJob(ctx, id)
}

// SearchBackfillScans implements tenant.SearchBackfills.
func (t *Tenant) SearchBackfillScans(ctx context.Context, after int64, limit int) ([]tenant.SearchBackfillScan, error) {
	return t.sending().SearchBackfillScans(ctx, after, limit)
}

// ApplySearchBackfill implements tenant.SearchBackfills.
func (t *Tenant) ApplySearchBackfill(ctx context.Context, a tenant.SearchBackfillApply) (tenant.SearchBackfillJob, error) {
	return t.sending().ApplySearchBackfill(ctx, a)
}

// EndSearchBackfill implements tenant.SearchBackfills.
func (t *Tenant) EndSearchBackfill(ctx context.Context, id string, now time.Time) (tenant.Job, error) {
	return t.sending().EndSearchBackfill(ctx, id, now)
}

// StopSearchBackfill implements tenant.SearchBackfills.
func (t *Tenant) StopSearchBackfill(ctx context.Context, id, reason string, now time.Time) (tenant.Job, error) {
	return t.sending().StopSearchBackfill(ctx, id, reason, now)
}
