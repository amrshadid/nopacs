package sqlite

import (
	"context"
	"time"

	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// The tenant's edits (tenant.Edits): package sqlkit's one implementation,
// run here in SQLite's transactions, as the other corrections' are.

var _ tenant.Edits = (*Tenant)(nil)

// EditTargets implements tenant.Edits.
func (t *Tenant) EditTargets(ctx context.Context, target tenant.CorrectionTarget, limit int) ([]tenant.EditTarget, error) {
	return t.sending().EditTargets(ctx, target, limit)
}

// CorrectionEdits implements tenant.Edits.
func (t *Tenant) CorrectionEdits(ctx context.Context, id string) ([]tenant.EditedInstance, error) {
	return t.sending().CorrectionEdits(ctx, id)
}

// BeginEditUndo implements tenant.Edits.
func (t *Tenant) BeginEditUndo(ctx context.Context, id, reason string, now time.Time, audit master.AuditEntry,
	guard tenant.Guard) (tenant.Correction, error) {
	return t.sending().BeginEditUndo(ctx, id, reason, now, audit, guard)
}

// UndoEdit implements tenant.Edits.
func (t *Tenant) UndoEdit(ctx context.Context, u tenant.EditUndo) (tenant.Correction, error) {
	return t.sending().UndoEdit(ctx, u)
}

// CheckEditUndo implements tenant.Edits.
func (t *Tenant) CheckEditUndo(ctx context.Context, id string, now time.Time) error {
	return t.sending().CheckEditUndo(ctx, id, now)
}

// AbandonEditUndo implements tenant.Edits.
func (t *Tenant) AbandonEditUndo(ctx context.Context, u tenant.EditUndo) (tenant.Correction, error) {
	return t.sending().AbandonEditUndo(ctx, u)
}
