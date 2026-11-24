package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"time"

	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
)

// ReinstateKeys implements master.KeyReinstatement. The keys are read and
// changed, and their entries written, in one write transaction, which holds
// the database's write lock from its start: no other change falls between
// the read of a key's suspension and its clearing.
func (m *Master) ReinstateKeys(ctx context.Context, ids []string, all bool, audit master.AuditEntry, by master.Actor) ([]master.Key, int64, error) {
	ids, err := master.ValidateReinstatement(ids, all, audit, by)
	if err != nil {
		return nil, 0, err
	}
	var out []master.Key
	var reinstated []string
	err = m.write(ctx, "the keys cannot be reinstated", func(tx *sql.Tx, s *statements) error {
		out, reinstated = nil, nil
		if all {
			rows, err := tx.QueryContext(ctx, `UPDATE api_keys SET suspended = NULL WHERE suspended IS NOT NULL RETURNING id`)
			if reinstated, err = collect(rows, err, func(r interface{ Scan(...any) error }) (string, error) {
				var id string
				return id, r.Scan(&id)
			}); err != nil {
				return err
			}
			// RETURNING gives no order; the entries are numbered in the
			// order of the keys' ids (master.ReinstatedEntry).
			slices.Sort(reinstated)
		} else {
			// Every id is read before anything changes, so an unknown id
			// leaves every key as it was.
			var suspended []master.Key
			for _, id := range ids {
				k, err := scanKey(tx.QueryRowContext(ctx, `SELECT `+keyColumns+` FROM api_keys WHERE id = ?`, id))
				if errors.Is(err, sql.ErrNoRows) {
					return fault.New(fault.NotFound, "no key has the id "+id)
				}
				if err != nil {
					return err
				}
				if !k.Suspended.IsZero() {
					suspended = append(suspended, k)
				}
			}
			for _, k := range suspended {
				if _, err := tx.ExecContext(ctx, `UPDATE api_keys SET suspended = NULL WHERE id = ?`, k.ID); err != nil {
					return err
				}
				k.Hash, k.Suspended = nil, time.Time{}
				out = append(out, k)
				reinstated = append(reinstated, k.ID)
			}
		}
		for i, id := range reinstated {
			if err := insertAudit(ctx, tx, s, master.ReinstatedEntry(audit, i+1, id)); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, 0, err
	}
	return out, int64(len(reinstated)), nil
}
