package sqlite

import (
	"context"
	"time"

	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// The tenant's batch deletes (tenant.DeleteBatches), on the tables of
// migrations/tenant/0001_baseline.sql: package sqlkit's one
// implementation, run here in SQLite's transactions.

// PreviewDeleteBatch implements tenant.DeleteBatches.
func (t *Tenant) PreviewDeleteBatch(ctx context.Context, targets []tenant.CorrectionTarget, reason string, now time.Time) (tenant.DeleteBatchPreview, error) {
	return t.sending().PreviewDeleteBatch(ctx, targets, reason, now)
}

// QueueDeleteBatch implements tenant.DeleteBatches.
func (t *Tenant) QueueDeleteBatch(ctx context.Context, q tenant.DeleteBatchQueue, audit master.AuditEntry, guard tenant.Guard) (tenant.Job, bool, error) {
	return t.sending().QueueDeleteBatch(ctx, q, audit, guard)
}

// DeleteBatchJob implements tenant.DeleteBatches.
func (t *Tenant) DeleteBatchJob(ctx context.Context, id string) (tenant.DeleteBatchJob, error) {
	return t.sending().DeleteBatchJob(ctx, id)
}

// DeleteBatchTargets implements tenant.DeleteBatches.
func (t *Tenant) DeleteBatchTargets(ctx context.Context, id string, after, limit int) ([]tenant.DeleteBatchTarget, error) {
	return t.sending().DeleteBatchTargets(ctx, id, after, limit)
}

// RecordBatchTarget implements tenant.DeleteBatches.
func (t *Tenant) RecordBatchTarget(ctx context.Context, r tenant.BatchRecord) (tenant.Job, error) {
	return t.sending().RecordBatchTarget(ctx, r)
}

// EndDeleteBatchJob implements tenant.DeleteBatches.
func (t *Tenant) EndDeleteBatchJob(ctx context.Context, id string, now time.Time) (tenant.Job, error) {
	return t.sending().EndDeleteBatchJob(ctx, id, now)
}

// StopDeleteBatchJob implements tenant.DeleteBatches.
func (t *Tenant) StopDeleteBatchJob(ctx context.Context, id, reason string, now time.Time) (tenant.Job, error) {
	return t.sending().StopDeleteBatchJob(ctx, id, reason, now)
}

// PauseDeleteBatch implements tenant.DeleteBatches.
func (t *Tenant) PauseDeleteBatch(ctx context.Context, id string, paused bool, now time.Time, audit master.AuditEntry,
	guard tenant.Guard) (tenant.DeleteBatchJob, error) {
	return t.sending().PauseDeleteBatch(ctx, id, paused, now, audit, guard)
}
