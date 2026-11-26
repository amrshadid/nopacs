package sqlkit

import (
	"context"
	"time"

	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// The clock a purge trusts (tenant.Corrections.CheckPurgeClock): a purge is
// refused while the clock reads earlier than a time the corrections
// recorded, and an automatic one while it jumped past the step allowed or
// retention is paused for its clock.

// checkClockIn refuses a purge at now while the clock reads earlier than
// the latest commit, purge or trusted time the corrections recorded, and an
// automatic one while it reads more than retention.max-clock-step (as the
// last check of retention's clock stored it) past the last trusted time;
// else it records now as trusted.
func (k Sending) checkClockIn(ctx context.Context, tx SendingTx, now time.Time, automatic bool) error {
	_, err := k.purgeClockIn(ctx, tx, now, automatic)
	return err
}

// purgeClockIn is checkClockIn, which also answers, with the refusal of an
// automatic purge for a jump, how far the clock jumped (zero for any other
// answer): the caller that commits the refusal pauses retention for it.
func (k Sending) purgeClockIn(ctx context.Context, tx SendingTx, now time.Time, automatic bool) (time.Duration, error) {
	jump, err := k.purgeClockTimes(ctx, tx, now, automatic)
	if err != nil || jump > 0 {
		return jump, err
	}
	_, err = tx.Exec(ctx, `UPDATE correction_clock SET trusted = ? WHERE one = 1`, k.D.Time(now))
	return 0, err
}

// purgeClockTimes makes purgeClockIn's checks, writing nothing: a refusal
// is its error, and a jump past the step allowed is answered with its size
// and its refusal.
func (k Sending) purgeClockTimes(ctx context.Context, tx SendingTx, now time.Time, automatic bool) (time.Duration, error) {
	var trusted, committed, ended, purged DBTime
	if _, err := tx.QueryRow(ctx, `SELECT trusted FROM correction_clock WHERE one = 1`+k.D.Lock, nil, &trusted); err != nil {
		return 0, err
	}
	if _, err := tx.QueryRow(ctx, `SELECT max(committed_at) FROM corrections`, nil, &committed); err != nil {
		return 0, err
	}
	if _, err := tx.QueryRow(ctx, `SELECT max(ended_at) FROM corrections WHERE state = 'purged'`, nil, &ended); err != nil {
		return 0, err
	}
	if _, err := tx.QueryRow(ctx, `SELECT max(purged_at) FROM correction_objects`, nil, &purged); err != nil {
		return 0, err
	}
	for _, t := range []time.Time{trusted.T, committed.T, ended.T, purged.T} {
		if now.Before(t) {
			return 0, tenant.ClockError("the server's clock reads earlier than a time the corrections recorded; no purge runs until it is right")
		}
	}
	if !automatic {
		return 0, nil
	}
	// The step allowed is the tenant's retention.max-clock-step, which
	// every check of retention's clock stores: one setting decides when
	// the clock jumped, for retention and for the purges its time starts.
	clock, err := retentionClockIn(ctx, tx, "")
	if err != nil {
		return 0, err
	}
	if step := now.Sub(trusted.T); !trusted.T.IsZero() && step > clock.MaxStep {
		return step, tenant.ClockError("the server's clock reads more than retention.max-clock-step past the last time it was trusted; " +
			"automatic purges wait for an admin's purge or a confirmation of the time")
	}
	// A clock retention saw jump pauses every purge its purge time starts
	// too, until a person confirms the time: a grace that ended by a jump
	// did not end.
	if clock.Paused {
		return 0, tenant.ClockError("clock jumped: the tenant's retention is paused until an admin confirms the time; automatic purges wait")
	}
	return 0, nil
}

// CheckPurgeClock implements tenant.Corrections. A jump past the step
// allowed pauses retention too, in a transaction that commits before the
// refusal is answered: retention then waits for a confirmation of the time
// at once, not from its next hourly check, which a clock put back before
// that check would never pause.
func (k Sending) CheckPurgeClock(ctx context.Context, now time.Time) error {
	if err := master.ValidateTime("time", now); err != nil {
		return err
	}
	var refused error
	err := k.Host.Write(ctx, "the purge's clock cannot be checked", func(tx SendingTx) error {
		refused = nil
		if err := tx.LockConfig(ctx); err != nil {
			return err
		}
		jump, err := k.purgeClockIn(ctx, tx, now, true)
		if jump == 0 {
			return err
		}
		refused = err
		st, err := retentionClockIn(ctx, tx, k.D.Lock)
		if err != nil || st.Paused {
			return err
		}
		return k.pauseClock(ctx, tx, tenant.PauseClockJumped, jump, now)
	})
	if err != nil {
		return err
	}
	return refused
}
