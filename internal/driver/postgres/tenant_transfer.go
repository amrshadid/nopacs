package postgres

import (
	"context"
	"time"

	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// The tenant's transfer jobs and the provenance of what came in
// (tenant.Transfers, tenant.TransferTargets), on the tables of
// migrations/tenant/0001_baseline.sql: package sqlkit's one implementation,
// run here in Postgres's transactions.

// QueueTransfer implements tenant.Transfers.
func (t *Tenant) QueueTransfer(ctx context.Context, q tenant.TransferQueue, audit master.AuditEntry, guard tenant.Guard) (tenant.Job, error) {
	return t.sending().QueueTransfer(ctx, q, audit, guard)
}

// TransferJob implements tenant.Transfers.
func (t *Tenant) TransferJob(ctx context.Context, id string) (tenant.TransferJob, error) {
	return t.sending().TransferJob(ctx, id)
}

// ListTransfers implements tenant.Transfers.
func (t *Tenant) ListTransfers(ctx context.Context, page master.Page) ([]tenant.TransferJob, string, error) {
	return t.sending().ListTransfers(ctx, page)
}

// AddTransferItems implements tenant.Transfers.
func (t *Tenant) AddTransferItems(ctx context.Context, id string, items []tenant.TransferNewItem) (int, error) {
	return t.sending().AddTransferItems(ctx, id, items)
}

// StartTransferStep implements tenant.Transfers.
func (t *Tenant) StartTransferStep(ctx context.Context, id string, to tenant.TransferStep, now time.Time) (tenant.TransferJob, error) {
	return t.sending().StartTransferStep(ctx, id, to, now)
}

// TransferItems implements tenant.Transfers.
func (t *Tenant) TransferItems(ctx context.Context, id string, state tenant.TransferItemState, after tenant.TransferItemKey,
	limit int) ([]tenant.TransferItem, error) {
	return t.sending().TransferItems(ctx, id, state, after, limit)
}

// RecordTransferItem implements tenant.Transfers.
func (t *Tenant) RecordTransferItem(ctx context.Context, r tenant.TransferRecord) error {
	return t.sending().RecordTransferItem(ctx, r)
}

// RecordTransferDelete implements tenant.Transfers.
func (t *Tenant) RecordTransferDelete(ctx context.Context, id, correction string) error {
	return t.sending().RecordTransferDelete(ctx, id, correction)
}

// EndTransfer implements tenant.Transfers.
func (t *Tenant) EndTransfer(ctx context.Context, id string, state tenant.JobState, reason string, now time.Time) (tenant.Job, error) {
	return t.sending().EndTransfer(ctx, id, state, reason, now)
}

// FindTransferred implements tenant.TransferTargets.
func (t *Tenant) FindTransferred(ctx context.Context, uids []string) (map[string]tenant.TransferFound, error) {
	return t.sending().FindTransferred(ctx, uids)
}

// RecordTransferProvenance implements tenant.TransferTargets.
func (t *Tenant) RecordTransferProvenance(ctx context.Context, rows []tenant.TransferProvenance) error {
	return t.sending().RecordTransferProvenance(ctx, rows)
}

// TransferProvenanceOf implements tenant.TransferTargets.
func (t *Tenant) TransferProvenanceOf(ctx context.Context, uid string) (tenant.TransferProvenance, bool, error) {
	return t.sending().TransferProvenanceOf(ctx, uid)
}

// ListTransferProvenance implements tenant.TransferTargets.
func (t *Tenant) ListTransferProvenance(ctx context.Context, sourceJob string, page master.Page) ([]tenant.TransferProvenance, string, error) {
	return t.sending().ListTransferProvenance(ctx, sourceJob, page)
}
