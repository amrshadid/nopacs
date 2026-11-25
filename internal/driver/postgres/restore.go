package postgres

import (
	"context"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/amrshadid/nopacs/internal/core/record"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// What a restore asks of the databases beyond loading them
// (master.KeySuspension, tenant.Restoring).

var (
	_ master.KeySuspension = (*Master)(nil)
	_ tenant.Restoring     = (*Tenant)(nil)
)

// SuspendKeys implements master.KeySuspension.
func (m *Master) SuspendKeys(ctx context.Context, now time.Time, audit master.AuditEntry) (int64, error) {
	if err := master.ValidateTime("suspension time", now); err != nil {
		return 0, err
	}
	if err := audit.Validate(); err != nil {
		return 0, err
	}
	var n int64
	err := m.write(ctx, "the keys cannot be suspended", func(tx pgx.Tx) error {
		// Every row, whatever it held: a value a backup carries is not
		// trusted to mean suspended (master.KeySuspension).
		tag, err := tx.Exec(ctx, `UPDATE api_keys SET suspended = $1`, micros(now))
		if err != nil {
			return err
		}
		n = tag.RowsAffected()
		return insertAudit(ctx, tx, m.red, audit.WithDetails(master.DetailSuspended, strconv.FormatInt(n, 10)))
	})
	return n, err
}

// StorageOwners implements tenant.Restoring.
func (t *Tenant) StorageOwners(ctx context.Context) ([]tenant.StorageOwner, error) {
	return t.sending().StorageOwners(ctx)
}

// SetStorageOwner implements tenant.Restoring.
func (t *Tenant) SetStorageOwner(ctx context.Context, storage string, generation int64, now time.Time) error {
	return t.sending().SetStorageOwner(ctx, storage, generation, now)
}

// AdoptCorrection implements tenant.Restoring.
func (t *Tenant) AdoptCorrection(ctx context.Context, rec record.Correction, storage string, now time.Time) (tenant.Adopted, error) {
	return t.sending().AdoptCorrection(ctx, rec, storage, now)
}

// RelinkHidden implements tenant.Restoring.
func (t *Tenant) RelinkHidden(ctx context.Context) (int64, error) {
	return t.sending().RelinkHidden(ctx)
}

// UnhiddenDeletes implements tenant.Restoring.
func (t *Tenant) UnhiddenDeletes(ctx context.Context) ([]string, error) {
	return t.sending().UnhiddenDeletes(ctx)
}

// HideDeletes implements tenant.Restoring.
func (t *Tenant) HideDeletes(ctx context.Context, deletes []tenant.DeleteToHide) (tenant.Hidden, error) {
	return t.sending().HideDeletes(ctx, deletes)
}

// OwnAdoptedReplace implements tenant.Restoring.
func (t *Tenant) OwnAdoptedReplace(ctx context.Context, rec record.Correction, storage string,
	sizeOf func(ctx context.Context, key string) (int64, error), now time.Time) error {
	return t.sending().OwnAdoptedReplace(ctx, rec, storage, sizeOf, now)
}

// HoldCommittedDeletes implements tenant.Restoring.
func (t *Tenant) HoldCommittedDeletes(ctx context.Context) (int64, error) {
	return t.sending().HoldCommittedDeletes(ctx)
}

// MarkQuarantineLost implements tenant.Restoring.
func (t *Tenant) MarkQuarantineLost(ctx context.Context, keep []string) (int64, error) {
	return t.sending().MarkQuarantineLost(ctx, keep)
}

// HoldEarlierSubjectLabels implements tenant.Restoring.
func (t *Tenant) HoldEarlierSubjectLabels(ctx context.Context) (int64, error) {
	return t.sending().HoldEarlierSubjectLabels(ctx)
}

// EndRebuilds implements tenant.Restoring.
func (t *Tenant) EndRebuilds(ctx context.Context, now time.Time) ([]string, error) {
	return t.sending().EndRebuilds(ctx, now)
}
