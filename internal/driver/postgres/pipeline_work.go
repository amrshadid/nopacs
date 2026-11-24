package postgres

import (
	"context"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/amrshadid/nopacs/internal/driver/sqlkit"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

const workColumns = `id, kind, subject, reception_id, coalesce(unique_key, ''), state, due, attempt, max_attempts, owner, last_error, created, updated`

// claimRounds bounds how many transactions one ClaimWork runs. Each round
// takes one unit; a unit whose last attempt's lease ended is held as failed
// in a round of its own, which commits, and the next round looks again. The
// bound only keeps a table full of such units from holding one call long.
const claimRounds = 1024

// scanWork reads one row of workColumns.
func scanWork(row scanner) (tenant.Work, error) {
	var w tenant.Work
	var state string
	var due, created, updated time.Time
	err := row.Scan(&w.ID, &w.Kind, &w.Subject, &w.ReceptionID, &w.UniqueKey, &state, &due, &w.Attempt, &w.MaxAttempts, &w.Owner,
		&w.LastError, &created, &updated)
	w.State = tenant.WorkState(state)
	w.Due, w.Created, w.Updated = due.UTC(), created.UTC(), updated.UTC()
	return w, err
}

// workInTx reads a unit inside a change's transaction, or NotFound.
func workInTx(ctx context.Context, tx pgx.Tx, id string) (tenant.Work, error) {
	w, err := scanWork(tx.QueryRow(ctx, `SELECT `+workColumns+` FROM work WHERE id = $1`, id))
	if isNoRow(err) {
		return w, fault.New(fault.NotFound, "the tenant has no unit of work with this id")
	}
	return w, err
}

// enqueueTries bounds the rounds of enqueueInTx. A round that inserts
// nothing met a unit under its key committed meanwhile, which the next round
// reads, or one that finished just after, whose key is then free.
const enqueueTries = 8

// enqueueInTx queues w, or returns the unit queued or leased under its unique
// key. Two units queued at once under one key meet at work_by_key: the second
// insert waits for the first's commit and then does nothing (ON CONFLICT DO
// NOTHING, which covers the id's key as well), and the next round, a
// statement of its own, reads the unit that won.
//
// The unit already held under the key is the answer before anything about
// w is looked at, its reception included: a caller that queues under a key
// asks for the work to be queued once, and the unit that holds the key is
// that work, whatever the new call names.
func enqueueInTx(ctx context.Context, tx pgx.Tx, w tenant.Work, now time.Time) (tenant.Work, bool, error) {
	attempts := w.MaxAttempts
	if attempts == 0 {
		attempts = tenant.DefaultWorkAttempts
	}
	checked := w.ReceptionID == ""
	for range enqueueTries {
		if w.UniqueKey != "" {
			held, err := scanWork(tx.QueryRow(ctx, workByKeySQL, w.UniqueKey))
			if err == nil {
				return held, false, nil
			}
			if !isNoRow(err) {
				return held, false, err
			}
		}
		if !checked {
			has, err := exists(ctx, tx, `SELECT 1 FROM receptions WHERE id = $1`, w.ReceptionID)
			if err != nil {
				return tenant.Work{}, false, err
			}
			if !has {
				return tenant.Work{}, false, tenant.Precondition("a unit of work names a reception the tenant does not have")
			}
			checked = true
		}
		out, err := scanWork(tx.QueryRow(ctx, `INSERT INTO work (id, kind, subject, reception_id, unique_key, state, due, attempt,
			max_attempts, owner, last_error, created, updated) VALUES ($1, $2, $3, $4, $5, 'queued', $6, 0, $7, '', '', $8, $8)
			ON CONFLICT DO NOTHING RETURNING `+workColumns,
			w.ID, w.Kind, w.Subject, w.ReceptionID, sqlkit.NullText(w.UniqueKey), micros(w.Due), attempts, micros(now)))
		if err == nil {
			return out, true, nil
		}
		if !isNoRow(err) {
			return tenant.Work{}, false, err
		}
		taken, err := exists(ctx, tx, `SELECT 1 FROM work WHERE id = $1`, w.ID)
		if err != nil {
			return tenant.Work{}, false, err
		}
		if taken {
			return tenant.Work{}, false, fault.New(fault.Conflict, "a unit of work with this id exists")
		}
	}
	return tenant.Work{}, false, fault.New(fault.Unavailable, "units of work under this key kept changing; try again")
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
	err := t.pipe(ctx, "the unit of work cannot be queued", func(tx pgx.Tx) error {
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
		`SELECT `+workColumns+` FROM work WHERE id = $1`, []any{id}, scanWork)
}

// The outcomes of one round of ClaimWork.
const (
	claimNone = iota
	claimLeased
	claimGaveUp
)

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
	query, kindArgs, err := sqlkit.ReclaimKinds(kinds, func(i int) string { return "$" + strconv.Itoa(i) })
	if err != nil {
		return 0, err
	}
	args := append([]any{micros(now), micros(now)}, kindArgs...)
	var n int64
	err = t.pipe(ctx, "the leased work cannot be reclaimed", func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, query, args...)
		n = tag.RowsAffected()
		return err
	})
	return int(n), err
}

// ClaimNextWork implements tenant.WorkClaims. Each round takes the earliest due
// unit FOR NO KEY UPDATE SKIP LOCKED (claimNextSQL): a unit another claim or
// a fenced write holds is skipped, never waited for, so two claims at once
// take different units and neither waits. A unit whose last allowed attempt
// ended is held as failed, its reception failed with it (giveUp), and that
// round commits on its own, so no transaction holds the rows of two
// receptions. Finding nothing due is answered after the rounds commit.
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
	var c tenant.WorkClaim
	for range claimRounds {
		if len(c.GaveUp) == tenant.MaxGaveUp {
			return c, nil
		}
		var out tenant.Work
		var gave tenant.GaveUpWork
		outcome := claimNone
		err := t.pipe(ctx, "a unit of work cannot be claimed", func(tx pgx.Tx) error {
			outcome, gave = claimNone, tenant.GaveUpWork{}
			w, err := scanWork(tx.QueryRow(ctx, claimNextSQL, kind, micros(now)))
			if isNoRow(err) {
				return nil
			}
			if err != nil {
				return err
			}
			if w.Attempt >= w.MaxAttempts {
				// Its last allowed attempt's lease ended: held as failed.
				if _, err := tx.Exec(ctx, `UPDATE work SET state = 'failed', owner = '',
					last_error = 'the lease of its last attempt ended', updated = $1 WHERE id = $2`, micros(now), w.ID); err != nil {
					return err
				}
				// A unit carries a reception or a job, never both; the job's
				// rows are locked before either writes an event, which
				// takes the feed's lock to the commit.
				events, err := giveUpJobUnit(ctx, tx, w.ID, now)
				if err != nil {
					return err
				}
				if err := writeEvents(ctx, tx, events...); err != nil {
					return err
				}
				failed, err := giveUp(ctx, tx, w.ReceptionID, now)
				if err != nil {
					return err
				}
				gave = tenant.GaveUpWork{WorkID: w.ID, ReceptionID: w.ReceptionID, ReceptionFailed: failed}
				outcome = claimGaveUp
				return nil
			}
			if out, err = scanWork(tx.QueryRow(ctx, `UPDATE work SET state = 'leased', attempt = attempt + 1, owner = $1, due = $2,
				updated = $3 WHERE id = $4 RETURNING `+workColumns, owner, micros(now.Add(lease)), micros(now), w.ID)); err != nil {
				return err
			}
			outcome = claimLeased
			return nil
		})
		if err != nil {
			return tenant.WorkClaim{}, err
		}
		switch outcome {
		case claimLeased:
			c.Work, c.Found = out, true
			return c, nil
		case claimNone:
			return c, nil
		}
		c.GaveUp = append(c.GaveUp, gave)
	}
	return c, nil
}

// fenced runs fn in a change after checking the fence (checkFence), which
// locks the unit's row first.
func (t *Tenant) fenced(ctx context.Context, fence tenant.WorkFence, message string, fn func(tx pgx.Tx) error) error {
	if err := fence.Validate(); err != nil {
		return err
	}
	return t.pipe(ctx, message, func(tx pgx.Tx) error {
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
	err := t.fenced(ctx, fence, "the lease cannot be renewed", func(tx pgx.Tx) error {
		var err error
		out, err = scanWork(tx.QueryRow(ctx, `UPDATE work SET due = $1, updated = $2 WHERE id = $3 AND attempt = $4 RETURNING `+workColumns,
			micros(now.Add(lease)), micros(now), fence.ID, fence.Attempt))
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
	return t.fenced(ctx, fence, "the unit of work cannot be completed", func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE work SET state = 'done', owner = '', updated = $1 WHERE id = $2 AND attempt = $3`,
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
	err := t.fenced(ctx, fence, "the failed attempt cannot be recorded", func(tx pgx.Tx) error {
		w, err := workInTx(ctx, tx, fence.ID)
		if err != nil {
			return err
		}
		state, due := tenant.WorkQueued, now.Add(tenant.RetryDelay(w.Attempt))
		if w.Attempt >= w.MaxAttempts {
			state, due = tenant.WorkFailed, now
		}
		if _, err := tx.Exec(ctx, `UPDATE work SET state = $1, due = $2, owner = '', last_error = $3, updated = $4
			WHERE id = $5 AND attempt = $6`, string(state), micros(due), reason, micros(now), fence.ID, fence.Attempt); err != nil {
			return err
		}
		if state == tenant.WorkFailed {
			// A unit carries a reception or a job, never both.
			events, err := giveUpJobUnit(ctx, tx, w.ID, now)
			if err != nil {
				return err
			}
			if err := writeEvents(ctx, tx, events...); err != nil {
				return err
			}
			if _, err := giveUp(ctx, tx, w.ReceptionID, now); err != nil {
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
