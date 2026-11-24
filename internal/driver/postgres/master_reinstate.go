package postgres

import (
	"context"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
)

// ReinstateKeys implements master.KeyReinstatement. The keys' rows are
// locked first, in the order of their ids (the lock RevokeKey and
// RotateKey take, in the order lockKeys takes several), before anything
// changes: with ids, the rows named; with all, every suspended row. Taking
// rows in one order is what keeps two reinstatements, or a reinstatement
// and another change of keys, from waiting on each other in a cycle. Every
// entry is written with one statement.
func (m *Master) ReinstateKeys(ctx context.Context, ids []string, all bool, audit master.AuditEntry, by master.Actor) ([]master.Key, int64, error) {
	ids, err := master.ValidateReinstatement(ids, all, audit, by)
	if err != nil {
		return nil, 0, err
	}
	var out []master.Key
	var reinstated []string
	err = m.write(ctx, "the keys cannot be reinstated", func(tx pgx.Tx) error {
		out, reinstated = nil, nil
		if all {
			// The rows are locked in the order of their ids before any is
			// changed, as the ids path locks them: an UPDATE alone would
			// lock them in the order it meets them on disk, and two calls
			// taking the same rows in two orders can each hold a row the
			// other waits for, which Postgres ends by failing one as a
			// deadlock.
			rows, err := tx.Query(ctx, `SELECT id FROM api_keys WHERE suspended IS NOT NULL ORDER BY id FOR NO KEY UPDATE`)
			locked, err := collect(rows, err, func(r scanner) (string, error) {
				var id string
				return id, r.Scan(&id)
			})
			if err != nil || len(locked) == 0 {
				return err
			}
			rows, err = tx.Query(ctx, `UPDATE api_keys SET suspended = NULL WHERE id = ANY($1) AND suspended IS NOT NULL RETURNING id`, locked)
			if reinstated, err = collect(rows, err, func(r scanner) (string, error) {
				var id string
				return id, r.Scan(&id)
			}); err != nil {
				return err
			}
		} else {
			// Every id is found, and locked, before anything changes, so
			// an unknown id leaves every key as it was.
			rows, err := tx.Query(ctx, `SELECT id FROM api_keys WHERE id = ANY($1) ORDER BY id FOR NO KEY UPDATE`, ids)
			found, err := collect(rows, err, func(r scanner) (string, error) {
				var id string
				return id, r.Scan(&id)
			})
			if err != nil {
				return err
			}
			if len(found) != len(ids) {
				return fault.New(fault.NotFound, "no key has the id "+firstMissing(ids, found))
			}
			rows, err = tx.Query(ctx, `UPDATE api_keys SET suspended = NULL WHERE id = ANY($1) AND suspended IS NOT NULL RETURNING `+keyColumns, ids)
			if out, err = collect(rows, err, scanKey); err != nil {
				return err
			}
			slices.SortFunc(out, func(a, b master.Key) int { return strings.Compare(a.ID, b.ID) })
			for i := range out {
				// RETURNING gives the row as the UPDATE left it, so the
				// suspension already reads as cleared.
				out[i].Hash, out[i].Suspended = nil, time.Time{}
				reinstated = append(reinstated, out[i].ID)
			}
		}
		if len(reinstated) == 0 {
			return nil
		}
		// RETURNING gives no order; the entries are numbered in the order
		// of the keys' ids (master.ReinstatedEntry), by their bytes.
		slices.Sort(reinstated)
		entries := make([]master.AuditEntry, len(reinstated))
		for i, id := range reinstated {
			entries[i] = master.ReinstatedEntry(audit, i+1, id)
		}
		return execAudit(ctx, tx, m.red, entries)
	})
	if err != nil {
		return nil, 0, err
	}
	return out, int64(len(reinstated)), nil
}

// firstMissing returns the first of ids, sorted, that found, sorted and
// drawn from ids, lacks.
func firstMissing(ids, found []string) string {
	for i, id := range ids {
		if i >= len(found) || found[i] != id {
			return id
		}
	}
	return ""
}
