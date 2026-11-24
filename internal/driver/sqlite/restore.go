package sqlite

import (
	"context"
	"database/sql"
	"strconv"
	"time"

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
	err := m.write(ctx, "the keys cannot be suspended", func(tx *sql.Tx, s *statements) error {
		// Every row, whatever it held: a value a backup carries is not
		// trusted to mean suspended (master.KeySuspension).
		res, err := tx.ExecContext(ctx, `UPDATE api_keys SET suspended = ?`, micros(now))
		if err != nil {
			return err
		}
		if n, err = res.RowsAffected(); err != nil {
			return err
		}
		return insertAudit(ctx, tx, s, audit.WithDetails(master.DetailSuspended, strconv.FormatInt(n, 10)))
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

// MarkQuarantineLost implements tenant.Restoring.
func (t *Tenant) MarkQuarantineLost(ctx context.Context) (int64, error) {
	return t.sending().MarkQuarantineLost(ctx)
}
