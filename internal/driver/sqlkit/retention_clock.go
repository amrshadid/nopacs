package sqlkit

import (
	"context"
	"time"

	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// Retention's clock (tenant.Retention, "The clock") and the quarantine's
// age, on a SQL database.

// retentionClockIn reads where retention's clock stands, locked with lock.
func retentionClockIn(ctx context.Context, tx SendingTx, lock string) (tenant.RetentionClock, error) {
	var c tenant.RetentionClock
	var trusted, pass, pausedAt DBTime
	var step, maxStep, notice int64
	_, err := tx.QueryRow(ctx, `SELECT trusted, last_pass, paused, pause_reason, pause_step, paused_at, max_step, notice
		FROM retention_state WHERE one = 1`+lock, nil, &trusted, &pass, &c.Paused, &c.Reason, &step, &pausedAt, &maxStep, &notice)
	c.Trusted, c.LastPass, c.PausedAt = trusted.T, pass.T, pausedAt.T
	c.Step, c.MaxStep, c.Notice = time.Duration(step)*time.Microsecond, time.Duration(maxStep)*time.Microsecond,
		time.Duration(notice)*time.Microsecond
	return c, err
}

// effectiveNow returns the time due times are read at for now
// (tenant.RetentionClock.Effective).
func effectiveNow(ctx context.Context, tx SendingTx, now time.Time) (time.Time, error) {
	c, err := retentionClockIn(ctx, tx, "")
	return c.Effective(now), err
}

// CheckRetentionClock implements tenant.Retention.
func (k Sending) CheckRetentionClock(ctx context.Context, c tenant.RetentionClockCheck) (tenant.RetentionClock, error) {
	if err := c.Validate(); err != nil {
		return tenant.RetentionClock{}, err
	}
	var out tenant.RetentionClock
	err := k.Host.Write(ctx, "retention's clock cannot be checked", func(tx SendingTx) error {
		if err := tx.LockConfig(ctx); err != nil {
			return err
		}
		st, err := retentionClockIn(ctx, tx, k.D.Lock)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE retention_state SET max_step = ?, notice = ? WHERE one = 1`, c.MaxStep.Microseconds(),
			c.Notice.Microseconds()); err != nil {
			return err
		}
		switch {
		case st.Paused:
		case !st.Trusted.IsZero() && c.Now.Before(st.Trusted):
			// A step back pauses nothing: the time trusted stays the
			// latest trusted, and due times are read at the clock's earlier
			// reading (tenant.RetentionClock.Effective), so going back only
			// delays what is due; nothing becomes due by it.
		case !st.Trusted.IsZero() && c.Now.Sub(st.Trusted) > c.MaxStep:
			err = k.pauseClock(ctx, tx, tenant.PauseClockJumped, c.Now.Sub(st.Trusted), c.Now)
		default:
			_, err = tx.Exec(ctx, `UPDATE retention_state SET trusted = ? WHERE one = 1`, k.D.Time(c.Now))
			if err == nil && c.Pass {
				_, err = tx.Exec(ctx, `UPDATE retention_state SET last_pass = ? WHERE one = 1`, k.D.Time(c.Now))
			}
		}
		if err != nil {
			return err
		}
		if out, err = retentionClockIn(ctx, tx, ""); err != nil {
			return err
		}
		out.JustPaused = out.Paused && !st.Paused
		return nil
	})
	return out, err
}

// pauseClock pauses retention for why, the clock having moved by step to
// the reading at.
func (k Sending) pauseClock(ctx context.Context, tx SendingTx, why string, step time.Duration, at time.Time) error {
	_, err := tx.Exec(ctx, `UPDATE retention_state SET paused = ?, pause_reason = ?, pause_step = ?, paused_at = ? WHERE one = 1`,
		k.D.Bool(true), why, step.Microseconds(), k.D.Time(at))
	return err
}

// ConfirmClock implements tenant.Retention.
func (k Sending) ConfirmClock(ctx context.Context, c tenant.ClockConfirm, audit master.AuditEntry, guard tenant.Guard) (tenant.RetentionClock,
	bool, error) {
	if err := c.Validate(); err != nil {
		return tenant.RetentionClock{}, false, err
	}
	if err := audit.Validate(); err != nil {
		return tenant.RetentionClock{}, false, err
	}
	if err := tenant.CheckClockAgreement(c.Observed, c.Now); err != nil {
		return tenant.RetentionClock{}, false, err
	}
	var out tenant.RetentionClock
	var was bool
	err := k.Host.Write(ctx, "the clock cannot be confirmed", func(tx SendingTx) error {
		if err := tx.LockConfig(ctx); err != nil {
			return err
		}
		st, err := retentionClockIn(ctx, tx, k.D.Lock)
		if err != nil {
			return err
		}
		was = st.Paused
		if _, err := tx.Exec(ctx, `UPDATE retention_state SET trusted = ?, paused = ?, pause_reason = '', pause_step = 0, paused_at = NULL,
			confirmed_at = ?, observed_at = ? WHERE one = 1`, k.D.Time(c.Now), k.D.Bool(false), k.D.Time(c.Now),
			k.D.Time(c.Observed)); err != nil {
			return err
		}
		// A purge's clock trusts what a person confirmed, too: the purges
		// a jump paused go on.
		if _, err := tx.Exec(ctx, `UPDATE correction_clock SET trusted = ? WHERE one = 1`, k.D.Time(c.Now)); err != nil {
			return err
		}
		if out, err = retentionClockIn(ctx, tx, ""); err != nil {
			return err
		}
		return tx.Audited(ctx, audit, guard)
	})
	return out, was, err
}

// RetentionClock implements tenant.Retention.
func (k Sending) RetentionClock(ctx context.Context) (tenant.RetentionClock, error) {
	var out tenant.RetentionClock
	err := k.Host.Read(ctx, "retention's clock cannot be read", func(tx SendingTx) error {
		var err error
		out, err = retentionClockIn(ctx, tx, "")
		return err
	})
	return out, err
}

// retentionPaused reports whether retention is paused for its clock.
func retentionPaused(ctx context.Context, tx SendingTx) (bool, error) {
	c, err := retentionClockIn(ctx, tx, "")
	return c.Paused, err
}

// QuarantineOldestSQL reads the oldest file held, the first entry of
// quarantine_by_state's range of the held: no read of the table.
const QuarantineOldestSQL = `SELECT at FROM quarantine WHERE state = 'held' ORDER BY at, id LIMIT 1`

// QuarantineAging implements tenant.Retention.
func (k Sending) QuarantineAging(ctx context.Context) (tenant.QuarantineAging, error) {
	var out tenant.QuarantineAging
	err := k.Host.Read(ctx, "the quarantine's age cannot be read", func(tx SendingTx) error {
		var oldest DBTime
		if _, err := tx.QueryRow(ctx, QuarantineOldestSQL, nil, &oldest); err != nil {
			return err
		}
		out.Oldest = oldest.T
		_, err := tx.QueryRow(ctx, `SELECT files, bytes FROM quarantine_totals`, nil, &out.Files, &out.Bytes)
		return err
	})
	return out, err
}
