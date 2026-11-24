package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/amrshadid/nopacs/internal/driver/sqlkit"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

const workColumns = `id, kind, subject, reception_id, coalesce(unique_key, ''), state, due, attempt, max_attempts, owner, last_error, created, updated`

// claimCandidates is how many due units a claim reads at once. A unit whose
// last attempt's lease ended is held as failed on the way, so a claim may
// have to look past a few.
const claimCandidates = 16

// claimRounds bounds how many times one claim reads claimCandidates units.
const claimRounds = 64

// scanWork reads one row of workColumns.
func scanWork(row interface{ Scan(...any) error }) (tenant.Work, error) {
	var w tenant.Work
	var state string
	var due, created, updated int64
	err := row.Scan(&w.ID, &w.Kind, &w.Subject, &w.ReceptionID, &w.UniqueKey, &state, &due, &w.Attempt, &w.MaxAttempts, &w.Owner,
		&w.LastError, &created, &updated)
	w.State = tenant.WorkState(state)
	w.Due, w.Created, w.Updated = fromMicros(due), fromMicros(created), fromMicros(updated)
	return w, err
}

// workInTx reads a unit inside a change's transaction, or NotFound.
func workInTx(ctx context.Context, tx *sql.Tx, id string) (tenant.Work, error) {
	w, err := scanWork(tx.QueryRowContext(ctx, `SELECT `+workColumns+` FROM work WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return w, fault.New(fault.NotFound, "the tenant has no unit of work with this id")
	}
	return w, err
}

// enqueueInTx queues w, or returns the unit queued or leased under its
// unique key. The key is looked up first with the literal state list of
// work_by_key, the partial unique index that also holds the rule.
func enqueueInTx(ctx context.Context, tx *sql.Tx, w tenant.Work, now time.Time) (tenant.Work, bool, error) {
	if w.UniqueKey != "" {
		held, err := scanWork(tx.QueryRowContext(ctx, workByKeySQL, w.UniqueKey))
		if err == nil {
			return held, false, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return held, false, err
		}
	}
	if w.ReceptionID != "" {
		has, err := exists(ctx, tx, `SELECT 1 FROM receptions WHERE id = ?`, w.ReceptionID)
		if err != nil {
			return tenant.Work{}, false, err
		}
		if !has {
			return tenant.Work{}, false, tenant.Precondition("a unit of work names a reception the tenant does not have")
		}
	}
	attempts := w.MaxAttempts
	if attempts == 0 {
		attempts = tenant.DefaultWorkAttempts
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO work (id, kind, subject, reception_id, unique_key, state, due, attempt, max_attempts,
		owner, last_error, created, updated) VALUES (?, ?, ?, ?, ?, 'queued', ?, 0, ?, '', '', ?, ?)`,
		w.ID, w.Kind, w.Subject, w.ReceptionID, sqlkit.NullText(w.UniqueKey), micros(w.Due), attempts, micros(now), micros(now))
	if isUniqueViolation(err) {
		return tenant.Work{}, false, fault.Wrap(fault.Conflict, err, "a unit of work with this id exists")
	}
	if err != nil {
		return tenant.Work{}, false, err
	}
	out, err := workInTx(ctx, tx, w.ID)
	return out, true, err
}

// EnqueueWork implements tenant.Pipeline.
func (t *Tenant) EnqueueWork(ctx context.Context, w tenant.Work, now time.Time) (tenant.Work, bool, error) {
	if err := w.Validate(); err != nil {
		return tenant.Work{}, false, err
	}
	if err := master.ValidateTime("time", now); err != nil {
		return tenant.Work{}, false, err
	}
	var out tenant.Work
	var created bool
	err := t.write(ctx, "the unit of work cannot be queued", func(tx *sql.Tx, _ *tenantStatements) error {
		var err error
		out, created, err = enqueueInTx(ctx, tx, w, now)
		return err
	})
	if err != nil {
		return tenant.Work{}, false, err
	}
	return out, created, nil
}

// Work implements tenant.Pipeline.
func (t *Tenant) Work(ctx context.Context, id string) (tenant.Work, error) {
	if err := master.ValidateID("work id", id); err != nil {
		return tenant.Work{}, err
	}
	return readOne(ctx, t, "the tenant has no unit of work with this id", "the unit of work cannot be read",
		`SELECT `+workColumns+` FROM work WHERE id = ?`, []any{id}, scanWork)
}

// ClaimWork implements tenant.Pipeline: ClaimNextWork, with nothing due
// answered NotFound.
func (t *Tenant) ClaimWork(ctx context.Context, kind, owner string, now time.Time, lease time.Duration) (tenant.Work, error) {
	c, err := t.ClaimNextWork(ctx, kind, owner, now, lease)
	if err != nil {
		return tenant.Work{}, err
	}
	if !c.Found {
		return tenant.Work{}, fault.New(fault.NotFound, "no unit of work of this kind is due")
	}
	return c.Work, nil
}

// ReclaimWorkOfKinds implements tenant.WorkReclaim: one statement
// (sqlkit.ReclaimKinds), every leased unit of the kinds queued again at now
// with no owner, its attempts kept.
func (t *Tenant) ReclaimWorkOfKinds(ctx context.Context, kinds []string, now time.Time) (int, error) {
	if err := master.ValidateTime("time", now); err != nil {
		return 0, err
	}
	query, kindArgs, err := sqlkit.ReclaimKinds(kinds, func(int) string { return "?" })
	if err != nil {
		return 0, err
	}
	args := append([]any{micros(now), micros(now)}, kindArgs...)
	var n int64
	err = t.write(ctx, "the leased work cannot be reclaimed", func(tx *sql.Tx, _ *tenantStatements) error {
		res, err := tx.ExecContext(ctx, query, args...)
		if err != nil {
			return err
		}
		n, err = res.RowsAffected()
		return err
	})
	return int(n), err
}

// ClaimNextWork implements tenant.WorkClaims. Within one transaction no
// other writer runs (BEGIN IMMEDIATE), and a claim grouped with another in
// one transaction sees that one's lease, so two claims never take one unit.
// Finding nothing due is answered after the transaction commits, since on
// the way it may have held units as failed.
func (t *Tenant) ClaimNextWork(ctx context.Context, kind, owner string, now time.Time, lease time.Duration) (tenant.WorkClaim, error) {
	if err := tenant.ValidateName("work kind", kind); err != nil {
		return tenant.WorkClaim{}, err
	}
	if err := master.ValidateID("worker", owner); err != nil {
		return tenant.WorkClaim{}, err
	}
	if err := tenant.ValidateLease(now, lease); err != nil {
		return tenant.WorkClaim{}, err
	}
	var out tenant.WorkClaim
	err := t.write(ctx, "a unit of work cannot be claimed", func(tx *sql.Tx, s *tenantStatements) error {
		var c tenant.WorkClaim
		defer func() { out = c }()
		// Each round holds as failed or leases what it read, so a round
		// reads new units; the bound only keeps a broken table from holding
		// the writer.
		for range claimRounds {
			rows, err := tx.StmtContext(ctx, s.claimNext).QueryContext(ctx, kind, micros(now), claimCandidates)
			due, err := collect(rows, err, scanWork)
			if err != nil {
				return err
			}
			for _, w := range due {
				if w.Attempt >= w.MaxAttempts {
					if len(c.GaveUp) == tenant.MaxGaveUp {
						return nil
					}
					// Its last allowed attempt's lease ended: held as failed.
					res, err := tx.ExecContext(ctx, `UPDATE work SET state = 'failed', owner = '',
						last_error = 'the lease of its last attempt ended', updated = ? WHERE id = ? AND attempt = ?
						AND state IN ('queued', 'leased')`, micros(now), w.ID, w.Attempt)
					if err != nil {
						return err
					}
					if n, err := res.RowsAffected(); err != nil || n != 1 {
						if err != nil {
							return err
						}
						continue
					}
					failed, err := giveUp(ctx, tx, w.ReceptionID, now)
					if err != nil {
						return err
					}
					if err := giveUpJobUnit(ctx, tx, w.ID, now); err != nil {
						return err
					}
					c.GaveUp = append(c.GaveUp, tenant.GaveUpWork{WorkID: w.ID, ReceptionID: w.ReceptionID, ReceptionFailed: failed})
					continue
				}
				res, err := tx.StmtContext(ctx, s.leaseUnit).ExecContext(ctx, owner, micros(now.Add(lease)), micros(now), w.ID, w.Attempt)
				if err != nil {
					return err
				}
				if n, err := res.RowsAffected(); err != nil || n != 1 {
					if err != nil {
						return err
					}
					continue
				}
				if c.Work, err = workInTx(ctx, tx, w.ID); err != nil {
					return err
				}
				c.Found = true
				return nil
			}
			if len(due) < claimCandidates {
				return nil
			}
		}
		return nil
	})
	if err != nil {
		return tenant.WorkClaim{}, err
	}
	return out, nil
}

// fenced runs fn in a change after checking the fence (checkFence).
func (t *Tenant) fenced(ctx context.Context, fence tenant.WorkFence, message string, fn func(tx *sql.Tx) error) error {
	if err := fence.Validate(); err != nil {
		return err
	}
	return t.write(ctx, message, func(tx *sql.Tx, _ *tenantStatements) error {
		if err := checkFence(ctx, tx, fence); err != nil {
			return err
		}
		return fn(tx)
	})
}

// RenewWork implements tenant.Pipeline.
func (t *Tenant) RenewWork(ctx context.Context, fence tenant.WorkFence, now time.Time, lease time.Duration) (tenant.Work, error) {
	if err := tenant.ValidateLease(now, lease); err != nil {
		return tenant.Work{}, err
	}
	var out tenant.Work
	err := t.fenced(ctx, fence, "the lease cannot be renewed", func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `UPDATE work SET due = ?, updated = ? WHERE id = ? AND attempt = ?`,
			micros(now.Add(lease)), micros(now), fence.ID, fence.Attempt); err != nil {
			return err
		}
		var err error
		out, err = workInTx(ctx, tx, fence.ID)
		return err
	})
	if err != nil {
		return tenant.Work{}, err
	}
	return out, nil
}

// CompleteWork implements tenant.Pipeline.
func (t *Tenant) CompleteWork(ctx context.Context, fence tenant.WorkFence, now time.Time, next *tenant.Work) error {
	if err := master.ValidateTime("time", now); err != nil {
		return err
	}
	if next != nil {
		if err := next.Validate(); err != nil {
			return err
		}
	}
	return t.fenced(ctx, fence, "the unit of work cannot be completed", func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `UPDATE work SET state = 'done', owner = '', updated = ? WHERE id = ? AND attempt = ?`,
			micros(now), fence.ID, fence.Attempt); err != nil {
			return err
		}
		if next == nil {
			return nil
		}
		_, _, err := enqueueInTx(ctx, tx, *next, now)
		return err
	})
}

// FailWork implements tenant.Pipeline.
func (t *Tenant) FailWork(ctx context.Context, fence tenant.WorkFence, reason string, now time.Time) (tenant.Work, error) {
	if err := tenant.ValidateWorkReason(reason); err != nil {
		return tenant.Work{}, err
	}
	if err := master.ValidateTime("time", now); err != nil {
		return tenant.Work{}, err
	}
	var out tenant.Work
	err := t.fenced(ctx, fence, "the failed attempt cannot be recorded", func(tx *sql.Tx) error {
		w, err := workInTx(ctx, tx, fence.ID)
		if err != nil {
			return err
		}
		state, due := tenant.WorkQueued, now.Add(tenant.RetryDelay(w.Attempt))
		if w.Attempt >= w.MaxAttempts {
			state, due = tenant.WorkFailed, now
		}
		if _, err := tx.ExecContext(ctx, `UPDATE work SET state = ?, due = ?, owner = '', last_error = ?, updated = ?
			WHERE id = ? AND attempt = ?`, string(state), micros(due), reason, micros(now), fence.ID, fence.Attempt); err != nil {
			return err
		}
		if state == tenant.WorkFailed {
			if _, err := giveUp(ctx, tx, w.ReceptionID, now); err != nil {
				return err
			}
			if err := giveUpJobUnit(ctx, tx, w.ID, now); err != nil {
				return err
			}
		}
		out, err = workInTx(ctx, tx, fence.ID)
		return err
	})
	if err != nil {
		return tenant.Work{}, err
	}
	return out, nil
}
